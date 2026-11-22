//go:build benchdb

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	hsdb "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// nodeUpdateColumns mirrors hscontrol/state.nodeUpdateColumns (unexported):
// the column set State.persistNodeWith writes after a MapRequest.
var nodeUpdateColumns = []string{
	"MachineKey", "NodeKey", "DiscoKey", "Endpoints", "Hostinfo", "IPv4", "IPv6",
	"LastControlAddress", "Hostname", "GivenName", "UserID", "RegisterMethod", "Tags",
	"Ephemeral", "Expiry", "KeyExpiryDisabled", "LastSeen", "ApprovedRoutes",
	"AutoApprovedRoutes", "UpdatedAt",
}

var errFenced = errors.New("fenced: tailnet not owned at this epoch")

// Operation names (weights per 1000 operations, see -mix).
const (
	opMapUpdate  = "map_update"   // Write: [fence] + Select(nodeUpdateColumns).Updates (state.persistNodeWith)
	opLastSeen   = "last_seen"    // Write: [fence] + db.SetLastSeen
	opNodeByKey  = "node_by_key"  // HSDatabase.GetNodeByNodeKey (3 statements: node + preloads)
	opRegister   = "register"     // GetPreAuthKey + Write: [fence] + Omit("AuthKey").Save(new node)
	opListNodes  = "list_nodes"   // HSDatabase.ListNodes of one tenant (runtime reload)
	opCreateUser = "create_user"  // HSDatabase.CreateUser
	opPoll       = "standby_poll" // standby: simulated lease/ownership poll
)

const defaultMix = "map_update=450,last_seen=250,node_by_key=240,register=50,list_nodes=5,create_user=5"

type nodeRef struct {
	node types.Node // full row as loaded; copied before each update
}

// runtime is one tenant's slice of a cluster process: a scoped HSDatabase
// sharing the process's connection pool (as P05 plans: one pool per process).
type runtime struct {
	id      string
	epoch   uint64
	h       *hsdb.HSDatabase
	mu      sync.Mutex
	nodes   []types.Node
	users   []uint
	pakKey  string // plaintext of a reusable pre-auth key made at start; memory only
	ipNext  uint32
	cluster string
}

type workerResult struct {
	Cluster    string           `json:"cluster"`
	Role       string           `json:"role"`
	Tenants    int              `json:"tenants"`
	OpenMs     float64          `json:"open_ms"`
	SeedS      float64          `json:"seed_s"`
	LoadMs     float64          `json:"load_ms"` // ListNodes of every tenant at start
	NodesStart int              `json:"nodes_start"`
	WallS      float64          `json:"wall_s"`
	CPUS       float64          `json:"cpu_s"` // process CPU during the measured window
	Ops        map[string]*hist `json:"ops"`
}

func runWorker(args []string, standby bool) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)

	var (
		t        pgTarget
		cluster  = fs.String("cluster", "", "cluster ID owning -tailnets")
		ids      = fs.String("tailnets", "", "comma-separated tailnet IDs owned by -cluster")
		devices  = fs.Int("devices", 200, "devices per tenant (seeded when missing)")
		users    = fs.Int("users", 10, "users per tenant")
		pool     = fs.Int("pool", 8, "max open (and idle) connections of the process pool")
		workers  = fs.Int("workers", 8, "concurrent operation goroutines")
		rate     = fs.Float64("rate", 0, "target operations/s for the process (0 = as fast as possible)")
		dur      = fs.Duration("duration", 30*time.Second, "measured window")
		fence    = fs.Bool("fence", true, "simulated P10 fencing: SELECT tailnets ... FOR SHARE in every write tx")
		mix      = fs.String("mix", defaultMix, "operation weights")
		poll     = fs.Duration("poll", time.Second, "standby poll interval")
		seedOnly = fs.Bool("seed-only", false, "seed and exit")
	)

	fs.StringVar(&t.Host, "host", "127.0.0.1", "PostgreSQL or PgBouncer host")
	fs.IntVar(&t.Port, "port", 5432, "port")
	fs.StringVar(&t.DB, "db", "bench", "database")
	fs.StringVar(&t.User, "user", "bench", "user")
	fs.StringVar(&t.PassFile, "passfile", "", "file holding the password")
	_ = fs.Parse(args)

	tailnetIDs := strings.Split(*ids, ",")
	if *cluster == "" || len(tailnetIDs) == 0 || tailnetIDs[0] == "" {
		return errors.New("-cluster and -tailnets are required")
	}

	res := workerResult{Cluster: *cluster, Role: "active", Tenants: len(tailnetIDs), Ops: map[string]*hist{}}
	if standby {
		res.Role = "standby"
	}

	start := time.Now()

	// The real open path: schema check, version check, tenantscope, and the
	// ownership check of the runtime's (first) tailnet.
	base, err := hsdb.NewHeadscaleDatabase(t.config(*cluster, tailnetIDs[0], *pool, *pool))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer base.Close()

	res.OpenMs = msSince(start)

	if standby {
		return standbyLoop(base, *cluster, *poll, *dur, &res)
	}

	rts := make([]*runtime, 0, len(tailnetIDs))

	for _, id := range tailnetIDs {
		r, err := bindRuntime(base, id, *cluster)
		if err != nil {
			return err
		}

		rts = append(rts, r)
	}

	seedStart := time.Now()

	var wg sync.WaitGroup

	errs := make(chan error, len(rts))

	sem := make(chan struct{}, max(1, *pool/2))
	for _, r := range rts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			if err := r.seed(*users, *devices, *fence); err != nil {
				errs <- fmt.Errorf("seed %s: %w", r.id, err)
			}
		}()
	}

	wg.Wait()
	close(errs)

	if err := <-errs; err != nil {
		return err
	}

	res.SeedS = time.Since(seedStart).Seconds()

	if *seedOnly {
		fmt.Println("SEEDED")
		return nil
	}

	loadStart := time.Now()

	for _, r := range rts {
		if err := r.load(); err != nil {
			return fmt.Errorf("load %s: %w", r.id, err)
		}

		res.NodesStart += len(r.nodes)
	}

	res.LoadMs = msSince(loadStart)

	weights, err := parseMix(*mix)
	if err != nil {
		return err
	}

	if err := waitGo(); err != nil {
		return err
	}

	cpu0, t0 := cpuSeconds(), time.Now()
	hists := runLoad(rts, weights, *workers, *rate, *dur, *fence)
	res.WallS = time.Since(t0).Seconds()
	res.CPUS = cpuSeconds() - cpu0
	res.Ops = hists

	return emit(res)
}

func bindRuntime(base *hsdb.HSDatabase, id, cluster string) (*runtime, error) {
	var tn types.Tailnet
	if err := base.DB.Where("id = ?", id).First(&tn).Error; err != nil {
		return nil, fmt.Errorf("tailnet %s: %w", id, err)
	}

	if tn.OwnerClusterID != cluster {
		return nil, fmt.Errorf("tailnet %s owned by %q, not %q", id, tn.OwnerClusterID, cluster)
	}

	tid, err := tailnet.ParseID(id)
	if err != nil {
		return nil, err
	}

	scope, err := tailnet.NewScope(tid, tailnet.SourceInternalService, "runtime@"+cluster, tailnet.Epoch(tn.OwnerEpoch))
	if err != nil {
		return nil, err
	}

	ctx, err := tailnet.WithScope(context.Background(), scope)
	if err != nil {
		return nil, err
	}

	// Only the exported DB field is set: every HSDatabase method used here
	// reads hsdb.DB alone. The pool and the tenantscope plugin are shared.
	return &runtime{id: id, epoch: tn.OwnerEpoch, cluster: cluster, h: &hsdb.HSDatabase{DB: base.DB.WithContext(ctx)}}, nil
}

// fenceCheck is the planned P10 check (PLAN §4): owner and epoch read under a
// shared row lock in the same transaction as the mutation. Simulated: the
// cluster_leases half of the check does not exist in the v0.1 schema yet.
func fenceCheck(tx *gorm.DB, id, cluster string, epoch uint64) error {
	var row struct {
		OwnerClusterID string
		OwnerEpoch     uint64
	}

	err := tx.Raw(`SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id = ? FOR SHARE`, id).Scan(&row).Error
	if err != nil {
		return err
	}

	if row.OwnerClusterID != cluster || row.OwnerEpoch != epoch {
		return errFenced
	}

	return nil
}

func (r *runtime) seed(users, devices int, fence bool) error {
	existing, err := r.h.ListUsers(nil)
	if err != nil {
		return err
	}

	for i := len(existing); i < users; i++ {
		if _, err := r.h.CreateUser(types.User{Name: fmt.Sprintf("user%03d", i)}); err != nil {
			return err
		}
	}

	existing, err = r.h.ListUsers(nil)
	if err != nil {
		return err
	}

	for _, u := range existing {
		r.users = append(r.users, u.ID)
	}

	if err := r.newKey(); err != nil {
		return err
	}

	var n int64
	if err := r.h.DB.Model(&types.Node{}).Count(&n).Error; err != nil {
		return err
	}

	r.ipNext = uint32(n)

	for i := int(n); i < devices; i++ {
		if _, err := r.register(fence); err != nil {
			return err
		}
	}

	return nil
}

// newKey makes a reusable pre-auth key through the real CreatePreAuthKey.
func (r *runtime) newKey() error {
	uid := types.UserID(r.users[0])

	pak, err := r.h.CreatePreAuthKey(&uid, true, false, nil, nil)
	if err != nil {
		return err
	}

	r.pakKey = pak.Key

	return nil
}

func (r *runtime) load() error {
	nodes, err := r.h.ListNodes()
	if err != nil {
		return err
	}

	r.nodes = r.nodes[:0]
	for _, n := range nodes {
		c := *n
		c.User, c.AuthKey = nil, nil
		r.nodes = append(r.nodes, c)
	}

	r.ipNext = uint32(len(r.nodes))

	return nil
}

// register runs the registration path of State.HandleNodeFromPreAuthKey
// (state.go: Write { Omit("AuthKey").Save(node); UsePreAuthKey if single-use }).
func (r *runtime) register(fence bool) (*types.Node, error) {
	pak, err := r.h.GetPreAuthKey(r.pakKey)
	if err != nil {
		return nil, fmt.Errorf("pre-auth key: %w", err)
	}

	r.mu.Lock()
	r.ipNext++
	seq := r.ipNext
	uid := r.users[int(seq)%len(r.users)]
	r.mu.Unlock()

	node := newNode(seq, uid, pak.ID)

	saved, err := hsdb.Write(r.h.DB, func(tx *gorm.DB) (*types.Node, error) {
		if fence {
			if err := fenceCheck(tx, r.id, r.cluster, r.epoch); err != nil {
				return nil, err
			}
		}

		if err := tx.Omit("AuthKey").Save(&node).Error; err != nil {
			return nil, fmt.Errorf("saving node: %w", err)
		}

		if !pak.Reusable {
			if err := hsdb.UsePreAuthKey(tx, pak); err != nil {
				return nil, err
			}
		}

		return &node, nil
	})
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.nodes = append(r.nodes, *saved)
	r.mu.Unlock()

	return saved, nil
}

func (r *runtime) pick(rng *rand.Rand) types.Node {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.nodes[rng.IntN(len(r.nodes))]
}

// newNode builds a device row of realistic size (hostinfo with netinfo and
// services, three endpoints, v4+v6 addresses).
func newNode(seq uint32, uid uint, pakID uint64) types.Node {
	now := time.Now().UTC()
	ip4 := netip.AddrFrom4([4]byte{100, 64 + byte(seq>>16&0x3f), byte(seq >> 8), byte(seq)})
	ip6 := netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0, 12: byte(seq >> 24), 13: byte(seq >> 16), 14: byte(seq >> 8), 15: byte(seq)})
	host := fmt.Sprintf("device-%05d", seq)
	pk := pakID

	return types.Node{
		MachineKey: key.NewMachine().Public(),
		NodeKey:    key.NewNode().Public(),
		DiscoKey:   key.NewDisco().Public(),
		Endpoints:  endpoints(seq, 41641),
		Hostinfo: &tailcfg.Hostinfo{
			IPNVersion: "1.92.3-t0123456789-g0123456789", FrontendLogID: "bench", BackendLogID: "bench",
			OS: "linux", OSVersion: "Ubuntu 24.04.3 LTS", Distro: "ubuntu", DistroVersion: "24.04",
			Hostname: host, GoArch: "amd64", GoVersion: "go1.25.5", Machine: "x86_64",
			Services: []tailcfg.Service{{Proto: "tcp", Port: 22, Description: "sshd"}, {Proto: "peerapi4", Port: 41234}},
			NetInfo: &tailcfg.NetInfo{
				PreferredDERP: 1, LinkType: "wired",
				DERPLatency: map[string]float64{"1-v4": 0.0123, "2-v4": 0.0456, "9-v4": 0.0789, "1-v6": 0.0131},
			},
		},
		IPv4: &ip4, IPv6: &ip6,
		Hostname: host, GivenName: host,
		UserID: &uid, RegisterMethod: util.RegisterMethodAuthKey,
		AuthKeyID: &pk, LastSeen: &now,
	}
}

func endpoints(seq uint32, port uint16) types.AddrPorts {
	return types.AddrPorts{
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(seq)}), port),
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 168, byte(seq >> 8), byte(seq)}), 41641),
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{172, 17, 0, byte(seq)}), 41641),
	}
}

func parseMix(s string) ([]string, error) {
	var table []string

	for part := range strings.SplitSeq(s, ",") {
		name, w, ok := strings.Cut(part, "=")

		var n int
		if _, err := fmt.Sscanf(w, "%d", &n); !ok || err != nil {
			return nil, fmt.Errorf("bad mix entry %q", part)
		}

		for range n {
			table = append(table, name)
		}
	}

	if len(table) == 0 {
		return nil, errors.New("empty mix")
	}

	return table, nil
}

func runLoad(rts []*runtime, table []string, workers int, rate float64, dur time.Duration, fence bool) map[string]*hist {
	var (
		tokens <-chan time.Time
		wg     sync.WaitGroup
		mu     sync.Mutex
		out    = map[string]*hist{}
	)

	if rate > 0 {
		tick := time.NewTicker(time.Duration(float64(time.Second) / rate))
		defer tick.Stop()

		tokens = tick.C
	}

	deadline := time.Now().Add(dur)

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(w)))
			local := map[string]*hist{}
			userSeq := 0

			for time.Now().Before(deadline) {
				if tokens != nil {
					select {
					case <-tokens:
					case <-time.After(time.Until(deadline)):
						continue
					}
				}

				op := table[rng.IntN(len(table))]
				r := rts[rng.IntN(len(rts))]
				t0 := time.Now()
				err := doOp(op, r, rng, fence, &userSeq, w)

				h := local[op]
				if h == nil {
					h = newHist()
					local[op] = h
				}

				h.add(time.Since(t0), err)
			}

			mu.Lock()
			defer mu.Unlock()

			for k, h := range local {
				if out[k] == nil {
					out[k] = newHist()
				}

				out[k].merge(h)
			}
		}()
	}

	wg.Wait()

	return out
}

func doOp(op string, r *runtime, rng *rand.Rand, fence bool, userSeq *int, w int) error {
	switch op {
	case opMapUpdate:
		n := r.pick(rng)
		now := time.Now().UTC()
		n.Endpoints = endpoints(uint32(n.ID), uint16(1024+rng.IntN(60000)))
		n.LastSeen = &now

		return r.h.Write(func(tx *gorm.DB) error {
			if fence {
				if err := fenceCheck(tx, r.id, r.cluster, r.epoch); err != nil {
					return err
				}
			}

			return tx.Select(nodeUpdateColumns).Updates(&n).Error
		})
	case opLastSeen:
		n := r.pick(rng)

		return r.h.Write(func(tx *gorm.DB) error {
			if fence {
				if err := fenceCheck(tx, r.id, r.cluster, r.epoch); err != nil {
					return err
				}
			}

			return hsdb.SetLastSeen(tx, n.ID, time.Now())
		})
	case opNodeByKey:
		n := r.pick(rng)
		_, err := r.h.GetNodeByNodeKey(n.NodeKey)

		return err
	case opRegister:
		_, err := r.register(fence)
		return err
	case opListNodes:
		_, err := r.h.ListNodes()
		return err
	case opCreateUser:
		*userSeq++
		_, err := r.h.CreateUser(types.User{Name: fmt.Sprintf("load-%d-%d-%d-%d", os.Getpid(), w, *userSeq, rng.IntN(1e6))})

		return err
	default:
		return fmt.Errorf("unknown op %q", op)
	}
}

// standbyLoop holds a minimal pool and polls ownership the way the planned
// lease client would (simulated: cluster_leases does not exist yet).
func standbyLoop(base *hsdb.HSDatabase, cluster string, every, dur time.Duration, res *workerResult) error {
	if err := waitGo(); err != nil {
		return err
	}

	h := newHist()
	cpu0, t0 := cpuSeconds(), time.Now()
	tick := time.NewTicker(every)

	defer tick.Stop()

	for time.Since(t0) < dur {
		s := time.Now()

		var n int64
		err := base.DB.Raw(`SELECT count(*) FROM tailnets WHERE owner_cluster_id = ? AND status = 'ACTIVE'`, cluster).Scan(&n).Error
		h.add(time.Since(s), err)

		<-tick.C
	}

	res.WallS = time.Since(t0).Seconds()
	res.CPUS = cpuSeconds() - cpu0
	res.Ops[opPoll] = h

	return emit(*res)
}

func waitGo() error {
	fmt.Println("READY")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("waiting for GO: %w", err)
	}

	if strings.TrimSpace(line) != "GO" {
		return fmt.Errorf("expected GO, got %q", line)
	}

	return nil
}

func emit(res workerResult) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}

	fmt.Println("RESULT " + string(b))

	return nil
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
