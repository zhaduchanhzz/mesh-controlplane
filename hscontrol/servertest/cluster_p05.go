//go:build p05cluster

// This file and the other files tagged p05cluster are the RED test suite of
// P04+P05 (design/adr/0007-multi-tenant-runtime-and-session-routing.md): two
// or more tailnets served by ONE cluster process, exercised by real Tailscale
// control clients in-process.
//
// They are written against the ADR 0007 code contract (hscontrol.Cluster,
// db.Platform, db.ErrUnknownTenant, hscontrol.ErrSessionTenantSwitch), which
// does not exist yet. The build tag keeps `go build ./...`, `go vet ./...`
// and CI green until the implementation lands:
//
//	go test -tags p05cluster ./hscontrol/servertest/ -run Cluster -count=1 -v
//
// Remove the tag (or add it to CI) in the PR that implements the contract.
// See evidence/P05-servertest-red-suite.md.
package servertest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"uuid"

	"github.com/glebarez/sqlite"
	hscontrol "github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// ClusterBaseDomain is the MagicDNS base domain of every [TestCluster].
const ClusterBaseDomain = "mesh.test"

// ClusterTailnetDomain is the MagicDNS domain of tailnet slug in a
// [TestCluster]: <slug>.<ClusterBaseDomain> (P06, ADR 0008).
func ClusterTailnetDomain(slug string) string { return slug + "." + ClusterBaseDomain }

// TailnetSeed describes one row of the tailnets table written before the
// cluster starts, as the future CreateTailnet/registry (P10) will.
type TailnetSeed struct {
	Slug string
	// Owner is owner_cluster_id; "" means the cluster under test.
	Owner string
	// Status is the lifecycle state; "" means ACTIVE.
	Status types.TailnetStatus
	// MintKey mints one reusable pre-auth key in the tailnet before the
	// cluster starts, through a handle scoped to it. Use it for tailnets the
	// cluster must NOT serve (foreign, not ACTIVE): they get no runtime to
	// mint keys with.
	MintKey bool
	// SeedNode writes one registered node row into the tailnet before the
	// cluster starts, so a map request for it can be attempted.
	SeedNode bool
}

// SeededTailnet is a tailnet written by [NewTestCluster].
type SeededTailnet struct {
	ID   tailnet.ID
	Slug string
	// AuthKey is set when the seed asked for MintKey.
	AuthKey string
	// NodeMachine and NodeKey identify the seeded node (SeedNode).
	NodeMachine key.MachinePrivate
	NodeKey     key.NodePublic
	NodeDisco   key.DiscoPublic
}

// NodeRow is a node as the shared database stores it, read without any
// tenant scope: the oracle for "no node was created anywhere".
type NodeRow struct {
	ID         uint64
	TailnetID  string
	Hostname   string
	MachineKey string
	NodeKey    string
}

// TestCluster is one cluster process (ADR 0007 §1) serving several tailnets
// on one listener, one hostname and one Noise key, started through the
// contract API: [hscontrol.NewCluster] and [hscontrol.Cluster.Serve].
//
// Unlike [TestServer] it listens on a real loopback TCP port, because
// Serve owns its listener. The shared database is a SQLite file; the
// PostgreSQL/PgBouncer path is covered by the Docker/E2E harness.
type TestCluster struct {
	Cluster   *hscontrol.Cluster
	URL       string
	ClusterID string

	cfg      *types.Config
	tailnets map[string]*SeededTailnet
	raw      *gorm.DB
}

// NewTestCluster writes seeds into a fresh shared database, then starts one
// cluster process over it and waits until it answers /health.
func NewTestCluster(tb testing.TB, seeds ...TailnetSeed) *TestCluster {
	tb.Helper()

	if len(seeds) == 0 {
		tb.Fatal("servertest: NewTestCluster needs at least one tailnet")
	}

	tmpDir := tb.TempDir()

	// Unix socket paths are limited to ~100 bytes; t.TempDir embeds the
	// test name.
	sockDir, err := os.MkdirTemp("", "p05")
	if err != nil {
		tb.Fatalf("servertest: socket dir: %v", err)
	}

	tb.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	addr := freeLoopbackAddr(tb)
	clusterID := "p05-cluster-" + uuid.NewV4().String()[:8]

	prefixV4 := netip.MustParsePrefix("100.64.0.0/10")
	prefixV6 := netip.MustParsePrefix("fd7a:115c:a1e0::/48")

	cfg := config.Default()
	// The cluster listener answers only its server_url hostname, never an IP
	// literal (P08, S11c), so clients use localhost; it still listens on
	// 127.0.0.1 only.
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		tb.Fatalf("servertest: %v", err)
	}

	cfg.ServerURL = "http://localhost:" + port
	cfg.Addr = types.ListenAddr(addr)
	cfg.MetricsAddr = ""
	cfg.UnixSocket = filepath.Join(sockDir, "hs.sock")
	cfg.Noise.PrivateKeyPath = filepath.Join(tmpDir, "noise_private.key")
	cfg.Prefixes.V4 = &prefixV4
	cfg.Prefixes.V6 = &prefixV6
	cfg.Prefixes.Allocation = types.IPAllocationStrategySequential
	cfg.Database.Type = types.DatabaseSqlite
	cfg.Database.Sqlite.Path = filepath.Join(tmpDir, "cluster.db")
	cfg.Policy.Mode = types.PolicyModeDB
	cfg.PolicyEmptyDenies = true
	cfg.ClusterID = clusterID
	cfg.TailnetID = ""
	cfg.Tuning.BatchChangeDelay = 50 * time.Millisecond
	cfg.Tuning.BatcherWorkers = 1
	cfg.Tuning.NodeMapSessionBufferedChanSize = 30
	cfg.DNSConfig.MagicDNS = true
	cfg.DNSConfig.BaseDomain = ClusterBaseDomain
	cfg.DNSConfig.OverrideLocalDNS = false
	cfg.DERP.Server.Enabled = false
	cfg.DERP.URLs = nil
	cfg.DERP.Paths = nil
	cfg.DERP.AutoUpdate = false
	cfg.DERP.DERPMap = &tailcfg.DERPMap{
		Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{
			900: {
				RegionID:   900,
				RegionCode: "test",
				RegionName: "Test Region",
				Nodes: []*tailcfg.DERPNode{{
					Name:     "test0",
					RegionID: 900,
					HostName: "127.0.0.1",
					IPv4:     "127.0.0.1",
					DERPPort: -1,
				}},
			},
		},
	}

	tc := &TestCluster{
		URL:       cfg.ServerURL,
		ClusterID: clusterID,
		cfg:       &cfg,
		tailnets:  make(map[string]*SeededTailnet),
	}

	tc.seed(tb, seeds)
	// P10: new devices are admitted only into tailnets of a registered
	// cluster (quota). Register the cluster under test as shared.
	tc.registerCluster(tb)

	cluster, err := hscontrol.NewCluster(&cfg)
	if err != nil {
		tb.Fatalf("servertest: NewCluster: %v", err)
	}

	tc.Cluster = cluster

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)

	go func() { serveErr <- cluster.Serve(ctx) }()

	// Registered after the seed cleanups, so it runs first: the cluster
	// stops before the raw handle closes.
	tb.Cleanup(func() {
		cancel()

		select {
		case <-serveErr:
		case <-time.After(30 * time.Second):
			tb.Errorf("servertest: cluster Serve did not return 30s after cancel")
		}
	})

	tc.waitHealthy(tb, serveErr)

	return tc
}

// seed creates the v0.1 schema through the existing single-tenant opener
// (which also creates the default tailnet, reused for seeds[0]), then writes
// the other tailnets with an unscoped handle and mints keys/nodes through
// handles scoped to each tailnet.
func (tc *TestCluster) seed(tb testing.TB, seeds []TailnetSeed) {
	tb.Helper()

	first := *tc.cfg

	hs, err := db.NewHeadscaleDatabase(&first)
	if err != nil {
		tb.Fatalf("servertest: create schema: %v", err)
	}

	defaultID := hs.Tailnet().ID

	err = hs.Close()
	if err != nil {
		tb.Fatalf("servertest: close schema handle: %v", err)
	}

	raw, err := gorm.Open(
		sqlite.Open(tc.cfg.Database.Sqlite.Path+"?_pragma=busy_timeout(10000)"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		tb.Fatalf("servertest: open raw database: %v", err)
	}

	tc.raw = raw

	tb.Cleanup(func() {
		if sqlDB, err := raw.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Now().UTC()

	for i, s := range seeds {
		owner := s.Owner
		if owner == "" {
			owner = tc.ClusterID
		}

		status := s.Status
		if status == "" {
			status = types.TailnetActive
		}

		id := defaultID
		if i == 0 {
			// An owner change must raise owner_epoch (trigger
			// trg_tailnets_owner_epoch_monotonic, P10).
			err = raw.Model(&types.Tailnet{}).Where("id = ?", id).Updates(map[string]any{
				"slug": s.Slug, "status": status, "owner_cluster_id": owner,
				"owner_epoch": gorm.Expr("owner_epoch + 1"), "updated_at": now,
			}).Error
		} else {
			id = uuid.NewV4().String()
			err = raw.Create(&types.Tailnet{
				ID: id, Slug: s.Slug, Status: status,
				OwnerClusterID: owner, OwnerEpoch: 1, PolicyRevision: 1, ConfigRevision: 1,
				CreatedAt: now, UpdatedAt: now,
			}).Error
		}

		if err != nil {
			tb.Fatalf("servertest: seed tailnet %q: %v", s.Slug, err)
		}

		st := &SeededTailnet{ID: tailnet.MustParseID(id), Slug: s.Slug}
		tc.tailnets[s.Slug] = st

		if s.MintKey || s.SeedNode {
			// Tenant data is written through the fenced tenant handle, which
			// refuses a tailnet that is not ACTIVE (P10b, A1): seed it while
			// ACTIVE, then set the requested status, as an operator would.
			if status != types.TailnetActive {
				tc.setSeedStatus(tb, id, types.TailnetActive)
			}

			tc.seedTenantData(tb, st, owner, s)

			if status != types.TailnetActive {
				tc.setSeedStatus(tb, id, status)
			}
		}
	}
}

func (tc *TestCluster) setSeedStatus(tb testing.TB, id string, status types.TailnetStatus) {
	tb.Helper()

	err := tc.raw.Model(&types.Tailnet{}).Where("id = ?", id).Update("status", status).Error
	if err != nil {
		tb.Fatalf("servertest: set seed status: %v", err)
	}
}

func (tc *TestCluster) seedTenantData(tb testing.TB, st *SeededTailnet, owner string, s TailnetSeed) {
	tb.Helper()

	scoped := *tc.cfg
	scoped.TailnetID = st.ID.String()
	scoped.ClusterID = owner

	hs, err := db.NewHeadscaleDatabase(&scoped)
	if err != nil {
		tb.Fatalf("servertest: scoped handle for %q: %v", s.Slug, err)
	}
	defer hs.Close()

	user, err := hs.CreateUser(types.User{Name: "seed-" + s.Slug})
	if err != nil {
		tb.Fatalf("servertest: seed user in %q: %v", s.Slug, err)
	}

	if s.MintKey {
		uid := types.UserID(user.ID)

		pak, err := hs.CreatePreAuthKey(&uid, true, false, nil, nil)
		if err != nil {
			tb.Fatalf("servertest: seed key in %q: %v", s.Slug, err)
		}

		st.AuthKey = pak.Key
	}

	if s.SeedNode {
		st.NodeMachine = key.NewMachine()
		st.NodeKey = key.NewNode().Public()
		st.NodeDisco = key.NewDisco().Public()

		n := &types.Node{
			MachineKey: st.NodeMachine.Public(), NodeKey: st.NodeKey, DiscoKey: st.NodeDisco,
			Hostname: "seed-" + s.Slug, GivenName: "seed-" + s.Slug,
			UserID: &user.ID, RegisterMethod: util.RegisterMethodAuthKey,
		}

		err = hs.DB.Create(n).Error
		if err != nil {
			tb.Fatalf("servertest: seed node in %q: %v", s.Slug, err)
		}
	}
}

func (tc *TestCluster) waitHealthy(tb testing.TB, serveErr <-chan error) {
	tb.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case err := <-serveErr:
			tb.Fatalf("servertest: cluster Serve returned before it was healthy: %v", err)
		default:
		}

		resp, err := client.Get(tc.URL + "/health") //nolint:noctx // bounded by client timeout
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return
			}
		}

		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // polling a listener we do not own
	}

	tb.Fatalf("servertest: cluster at %s not healthy after 20s", tc.URL)
}

// Tailnet returns the seeded tailnet with slug.
func (tc *TestCluster) Tailnet(tb testing.TB, slug string) *SeededTailnet {
	tb.Helper()

	st, ok := tc.tailnets[slug]
	if !ok {
		tb.Fatalf("servertest: no seeded tailnet %q", slug)
	}

	return st
}

// HasRuntime reports whether the cluster loaded a runtime for slug.
func (tc *TestCluster) HasRuntime(tb testing.TB, slug string) bool {
	tb.Helper()

	_, ok := tc.Cluster.Runtime(tc.Tailnet(tb, slug).ID)

	return ok
}

// Runtime returns the runtime of slug and fails the test when there is none.
func (tc *TestCluster) Runtime(tb testing.TB, slug string) *hscontrol.Headscale {
	tb.Helper()

	rt, ok := tc.Cluster.Runtime(tc.Tailnet(tb, slug).ID)
	if !ok || rt == nil {
		tb.Fatalf("servertest: cluster has no runtime for tailnet %q", slug)
	}

	return rt
}

// Tenant returns a [TestServer] view of slug's runtime: State/CreateUser/
// CreatePreAuthKey act on that tailnet only, and URL is the cluster's single
// URL. It has no listener of its own; use it with [NewClusterClient].
func (tc *TestCluster) Tenant(tb testing.TB, slug string) *TestServer {
	tb.Helper()

	rt := tc.Runtime(tb, slug)

	return &TestServer{App: rt, URL: tc.URL, st: rt.GetState(), cfg: tc.cfg}
}

// Front is the cluster's front door with no tenant: only URL is set. Use it
// for clients whose credential must not resolve to any tailnet.
func (tc *TestCluster) Front() *TestServer {
	return &TestServer{URL: tc.URL}
}

// SetPolicy stores pol in slug's runtime and sends the resulting changes.
func (tc *TestCluster) SetPolicy(tb testing.TB, slug, pol string) {
	tb.Helper()

	rt := tc.Runtime(tb, slug)

	_, changes, err := rt.GetState().UpdatePolicy([]byte(pol))
	if err != nil {
		tb.Fatalf("servertest: UpdatePolicy(%s): %v", slug, err)
	}

	rt.Change(changes...)
}

// Platform opens the unscoped platform handle of the contract on the shared
// database (closed at cleanup when it has a Close method).
func (tc *TestCluster) Platform(tb testing.TB) *db.Platform {
	tb.Helper()

	p, err := db.OpenPlatform(tc.cfg)
	if err != nil {
		tb.Fatalf("servertest: OpenPlatform: %v", err)
	}

	if c, ok := any(p).(interface{ Close() error }); ok {
		tb.Cleanup(func() { _ = c.Close() })
	}

	return p
}

// NoiseKey fetches the cluster's Noise public key from /key, as clients do.
func (tc *TestCluster) NoiseKey(tb testing.TB) key.MachinePublic {
	tb.Helper()

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(tc.URL + "/key?v=" + strconv.Itoa(int(tailcfg.CurrentCapabilityVersion))) //nolint:noctx
	if err != nil {
		tb.Fatalf("servertest: GET /key: %v", err)
	}
	defer resp.Body.Close()

	var k tailcfg.OverTLSPublicKeyResponse

	err = json.NewDecoder(resp.Body).Decode(&k)
	if err != nil || k.PublicKey.IsZero() {
		tb.Fatalf("servertest: GET /key: status %d, decode %v", resp.StatusCode, err)
	}

	return k.PublicKey
}

// NodeRows reads every live node of every tailnet straight from the shared
// database, bypassing runtimes, caches and the tenantscope plugin.
func (tc *TestCluster) NodeRows(tb testing.TB) []NodeRow {
	tb.Helper()

	var rows []NodeRow

	err := tc.raw.Raw(`SELECT id, tailnet_id, COALESCE(hostname, '') AS hostname,
		COALESCE(machine_key, '') AS machine_key, COALESCE(node_key, '') AS node_key
		FROM nodes WHERE deleted_at IS NULL ORDER BY id`).Scan(&rows).Error
	if err != nil {
		tb.Fatalf("servertest: read node rows: %v", err)
	}

	return rows
}

func freeLoopbackAddr(tb testing.TB) string {
	tb.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("servertest: pick port: %v", err)
	}

	addr := ln.Addr().String()
	_ = ln.Close()

	return addr
}

// String formats a row for failure messages.
func (r NodeRow) String() string {
	return fmt.Sprintf("node %d tailnet=%s host=%q", r.ID, r.TailnetID, r.Hostname)
}
