//go:build benchdb

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"gorm.io/gorm"

	hsdb "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// registryDesign is the real P12b path (ADR 0010): mesh-placement's
// Registry.MoveOwner (moving_to set, owner and epoch kept), then the owner
// cluster's Platform.HandOver (owner = target, epoch + 1), each a short
// transaction on the tailnets row under LockOwnership with its audit/lease
// checks. Its probes write like a cluster runtime (FOR SHARE fence).
const registryDesign = "registry"

// regEnv holds the real move path: the registry and one Platform per
// cluster, each holding a (bench) lease of its cluster.
type regEnv struct {
	reg  *placement.Registry
	plat map[string]*hsdb.Platform
	// lastTo is the target cluster of the last move: MoveOwner counts the
	// devices the target already owns (capacity check), so it matters.
	lastTo string
}

var benchOp = placement.Op{Actor: "spiffe://mesh/operator/bench-b3"}

func setupRegistry(t pgTarget, adb *gorm.DB) (*regEnv, error) {
	reg, err := placement.New(adb, placement.Config{})
	if err != nil {
		return nil, err
	}

	env := &regEnv{reg: reg, plat: map[string]*hsdb.Platform{}}

	for _, c := range []string{"src", "dst"} {
		_, err := reg.CreateCluster(context.Background(), benchOp, placement.ClusterSpec{
			ID: c, EndpointURL: "https://" + c + ".bench.invalid:8443", Class: placement.ClassShared,
			State: placement.ClusterActive, MaxDevices: 1_000_000,
		})
		if err != nil && !errors.Is(err, placement.ErrConflict) {
			return nil, fmt.Errorf("cluster %s: %w", c, err)
		}

		// A lease held by "bench-<c>" at epoch 1 for a day: MoveOwner needs
		// the target's live lease, HandOver the owner's own.
		err = adb.Exec(`INSERT INTO cluster_leases (cluster_id, holder, epoch, expires_at, updated_at)
			VALUES (?, ?, 1, now() + interval '1 day', now())
			ON CONFLICT (cluster_id) DO UPDATE SET expires_at = EXCLUDED.expires_at, updated_at = now()`, c, "bench-"+c).Error
		if err != nil {
			return nil, fmt.Errorf("lease %s: %w", c, err)
		}

		p, err := hsdb.OpenPlatform(t.config(c, "", 4, 4))
		if err != nil {
			return nil, fmt.Errorf("platform %s: %w", c, err)
		}

		p.SetLease("bench-"+c, 1)
		env.plat[c] = p
	}

	return env, nil
}

func (e *regEnv) close() {
	for _, p := range e.plat {
		_ = p.Close()
	}
}

// move runs the real two-step move of tailnet id away from its current
// owner and returns the time of each step.
func (e *regEnv) move(ctx context.Context, db *sql.DB, id string) (float64, float64, error) {
	var cur ownerRow
	if err := db.QueryRowContext(ctx, `SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id = $1`, id).Scan(&cur.owner, &cur.epoch); err != nil {
		return 0, 0, err
	}

	e.lastTo = flip(cur.owner)
	t0 := time.Now()

	if _, err := e.reg.MoveOwner(ctx, benchOp, id, cur.owner, uint64(cur.epoch), flip(cur.owner)); err != nil { //nolint:gosec
		return 0, 0, fmt.Errorf("MoveOwner: %w", err)
	}

	request := msSince(t0)
	t1 := time.Now()

	if _, err := e.plat[cur.owner].HandOver(tailnet.MustParseID(id), uint64(cur.epoch)); err != nil { //nolint:gosec
		return 0, 0, fmt.Errorf("HandOver: %w", err)
	}

	return request, msSince(t1), nil
}

// blogTables are the tenant tables that the per-row design ("cluster_id on
// every row", ownding.com blog) must rewrite when a tenant moves.
var blogTables = []string{"users", "credentials", "nodes", "policies"}

type b3Trial struct {
	Design   string           `json:"design"`
	Size     int              `json:"size"`
	Trial    int              `json:"trial"`
	StmtMs   float64          `json:"stmt_ms"`
	CommitMs float64          `json:"commit_ms"`
	TotalMs  float64          `json:"total_ms"`
	Rows     int64            `json:"rows"`
	WALBytes float64          `json:"wal_bytes"`
	Locks    map[string]int64 `json:"locks,omitempty"`  // pg_locks of the moving backend before COMMIT
	Target   string           `json:"target,omitempty"` // registry: cluster the tailnet moved to
}

type b3Probe struct {
	Design      string  `json:"design"`
	Size        int     `json:"size"`
	Trial       int     `json:"trial"`
	MoveMs      float64 `json:"move_ms"`
	SameBaseP50 float64 `json:"same_tenant_baseline_p50_ms"`
	SameMaxIn   float64 `json:"same_tenant_max_during_move_ms"`
	OtherBase   float64 `json:"other_tenant_baseline_p50_ms"`
	OtherMaxIn  float64 `json:"other_tenant_max_during_move_ms"`
	SameN       int     `json:"same_ops"`
	Target      string  `json:"target,omitempty"` // registry: cluster the tailnet moved to
}

type b3Result struct {
	Started    time.Time         `json:"started"`
	Params     map[string]string `json:"params"`
	Tenants    map[string]int64  `json:"tenant_nodes"`
	SeedS      float64           `json:"seed_s"`
	Trials     []b3Trial         `json:"trials"`
	Probes     []b3Probe         `json:"probes"`
	TableStats []map[string]any  `json:"table_stats"`
	Sizes      []relSize         `json:"sizes"`
}

func runB3(args []string) error {
	fs := flag.NewFlagSet("b3", flag.ExitOnError)

	var t pgTarget

	var (
		others       = fs.Int("others", 9, "other tenants (load the tables like a real shared DB)")
		otherDevices = fs.Int("other-devices", 5000, "devices per other tenant")
		sizesFlag    = fs.String("sizes", "1000,5000,10000", "device counts of the tenants that are moved")
		users        = fs.Int("users", 20, "users per tenant")
		trials       = fs.Int("trials", 7, "trials per design and size")
		designsFlag  = fs.String("designs", "owner_cas,row_rewrite,registry", "designs to measure (registry: the real MoveOwner + HandOver path)")
		out          = fs.String("out", "", "write JSON result here")
	)

	fs.StringVar(&t.Host, "host", "127.0.0.1", "PostgreSQL host (direct)")
	fs.IntVar(&t.Port, "port", 5432, "port")
	fs.StringVar(&t.DB, "db", "bench_b3", "database")
	fs.StringVar(&t.User, "user", "bench", "user")
	fs.StringVar(&t.PassFile, "passfile", "", "password file")
	_ = fs.Parse(args)

	res := b3Result{Started: time.Now().UTC(), Params: map[string]string{}, Tenants: map[string]int64{}}
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name != "passfile" {
			res.Params[f.Name] = f.Value.String()
		}
	})

	var moveSizes []int

	for s := range strings.SplitSeq(*sizesFlag, ",") {
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}

		moveSizes = append(moveSizes, n)
	}

	adb, err := openAdmin(t, false)
	if err != nil {
		return err
	}

	sqlDB, _ := adb.DB()
	sqlDB.SetMaxOpenConns(8)

	if !adb.Migrator().HasTable("tailnets") {
		h, err := hsdb.NewHeadscaleDatabase(t.config("src", "", 2, 2))
		if err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}

		_ = h.Close()
	}

	// Tenants: the moved ones (one per size) and the others.
	want := map[string]int{}
	for _, s := range moveSizes {
		want[fmt.Sprintf("mv-%05d", s)] = s
	}

	for i := range *others {
		want[fmt.Sprintf("other-%02d", i)] = *otherDevices
	}

	ids := map[string]string{}

	for slug := range want {
		var tn types.Tailnet

		err := adb.Where("slug = ?", slug).First(&tn).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			now := time.Now().UTC()
			tn = types.Tailnet{
				ID: uuid.NewV4().String(), Slug: slug, Status: types.TailnetActive,
				OwnerClusterID: "src", OwnerEpoch: 1, PolicyRevision: 1, ConfigRevision: 1, CreatedAt: now, UpdatedAt: now,
			}
			err = adb.Create(&tn).Error
		}

		if err != nil {
			return err
		}

		ids[slug] = tn.ID
	}

	seedStart := time.Now()

	if err := seedB3(t, adb, ids, want, *users); err != nil {
		return err
	}

	res.SeedS = time.Since(seedStart).Seconds()

	for slug, id := range ids {
		var n int64

		_ = adb.Raw(`SELECT count(*) FROM nodes WHERE tailnet_id = ?`, id).Scan(&n).Error
		res.Tenants[slug] = n
	}

	if err := buildBlogSim(adb); err != nil {
		return fmt.Errorf("blogsim: %w", err)
	}

	_ = adb.Exec(`VACUUM ANALYZE`).Error
	res.Sizes, _ = sizes(adb)
	_ = adb.Exec(`SELECT pg_stat_reset()`).Error

	ctx := context.Background()

	designs := strings.Split(*designsFlag, ",")

	var env *regEnv

	if slices.Contains(designs, registryDesign) {
		env, err = setupRegistry(t, adb)
		if err != nil {
			return fmt.Errorf("registry design: %w", err)
		}
		defer env.close()
	}

	// Phase 1: isolated trials (time, WAL, locks).
	for _, size := range moveSizes {
		slug := fmt.Sprintf("mv-%05d", size)

		for trial := range *trials {
			order := slices.Clone(designs)
			if trial%2 == 1 {
				slices.Reverse(order)
			}

			for _, d := range order {
				tr, err := moveOnce(ctx, sqlDB, env, d, ids[slug], size, trial)
				if err != nil {
					return fmt.Errorf("%s %s trial %d: %w", d, slug, trial, err)
				}

				res.Trials = append(res.Trials, tr)
				fmt.Printf("isolated %-11s size=%5d trial=%d total=%7.2fms stmt=%7.2fms commit=%6.2fms rows=%5d wal=%8.0fB locks=%v\n",
					d, size, trial, tr.TotalMs, tr.StmtMs, tr.CommitMs, tr.Rows, tr.WALBytes, tr.Locks)
			}
		}
	}

	// Closing the pool ends the backends, which flushes their table stats.
	_ = sqlDB.Close()

	adb, err = openAdmin(t, false)
	if err != nil {
		return err
	}

	sqlDB, _ = adb.DB()
	sqlDB.SetMaxOpenConns(8)

	// The registry held the pool closed above: give it the new one.
	if env != nil {
		env.reg, err = placement.New(adb, placement.Config{})
		if err != nil {
			return fmt.Errorf("registry design: %w", err)
		}
	}

	time.Sleep(1500 * time.Millisecond)

	_ = adb.Raw(`SELECT schemaname || '.' || relname AS rel, n_tup_upd, n_tup_hot_upd, n_dead_tup, n_live_tup
		FROM pg_stat_user_tables WHERE relname IN ('tailnets','users','credentials','nodes','policies')
		ORDER BY 1`).Scan(&res.TableStats).Error

	// Phase 2: the same moves with concurrent writers on the moving tenant
	// and on another tenant, to see who is blocked and for how long.
	other := ids["other-00"]

	for _, size := range moveSizes {
		slug := fmt.Sprintf("mv-%05d", size)

		for trial := range min(*trials, 5) {
			for _, d := range designs {
				p, err := probeMove(ctx, sqlDB, env, d, ids[slug], other, size, trial)
				if err != nil {
					return fmt.Errorf("probe %s %s: %w", d, slug, err)
				}

				res.Probes = append(res.Probes, p)
				fmt.Printf("concurrent %-11s size=%5d trial=%d move=%7.2fms same-tenant p50=%6.2fms max-during=%7.2fms | other p50=%6.2fms max-during=%6.2fms\n",
					d, size, trial, p.MoveMs, p.SameBaseP50, p.SameMaxIn, p.OtherBase, p.OtherMaxIn)
			}
		}
	}

	if *out != "" {
		b, _ := json.MarshalIndent(res, "", " ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}

	return nil
}

// seedB3 fills each tenant through the scoped handle (tenantscope stamps
// tailnet_id) with users, one pre-auth key and devices in batches.
func seedB3(t pgTarget, adb *gorm.DB, ids map[string]string, want map[string]int, users int) error {
	var first string
	for _, id := range ids {
		first = id
		break
	}

	base, err := hsdb.NewHeadscaleDatabase(t.config("src", first, 8, 8))
	if err != nil {
		return err
	}
	defer base.Close()

	for slug, id := range ids {
		var tn types.Tailnet
		if err := adb.Where("id = ?", id).First(&tn).Error; err != nil {
			return err
		}

		r, err := bindRuntime(base, id, tn.OwnerClusterID)
		if err != nil {
			return err
		}

		var n int64
		if err := r.h.DB.Model(&types.Node{}).Count(&n).Error; err != nil {
			return err
		}

		if int(n) >= want[slug] {
			continue
		}

		if err := r.seed(users, 0, false); err != nil {
			return err
		}

		pak, err := r.h.GetPreAuthKey(r.pakKey)
		if err != nil {
			return err
		}

		batch := make([]types.Node, 0, 500)

		for i := int(n); i < want[slug]; i++ {
			batch = append(batch, newNode(uint32(i+1), r.users[i%len(r.users)], pak.ID))
			if len(batch) == cap(batch) || i == want[slug]-1 {
				if err := r.h.DB.CreateInBatches(&batch, 500).Error; err != nil {
					return fmt.Errorf("seeding %s: %w", slug, err)
				}

				batch = batch[:0]
			}
		}

		fmt.Printf("seeded %s: %d devices\n", slug, want[slug])
	}

	return nil
}

// buildBlogSim copies the tenant tables into schema blogsim with an extra
// NOT NULL cluster_id column (and an index on it, as a per-row ownership
// filter needs), populated from tailnets.owner_cluster_id.
func buildBlogSim(adb *gorm.DB) error {
	var exists bool
	if err := adb.Raw(`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'blogsim')`).Scan(&exists).Error; err != nil {
		return err
	}

	if exists {
		return nil
	}

	return adb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{`CREATE SCHEMA blogsim`}
		for _, tbl := range blogTables {
			stmts = append(stmts,
				fmt.Sprintf(`CREATE TABLE blogsim.%s (LIKE public.%s INCLUDING ALL)`, tbl, tbl),
				fmt.Sprintf(`ALTER TABLE blogsim.%s ADD COLUMN cluster_id text`, tbl),
				fmt.Sprintf(`INSERT INTO blogsim.%s SELECT p.*, tn.owner_cluster_id FROM public.%s p JOIN tailnets tn ON tn.id = p.tailnet_id`, tbl, tbl),
				fmt.Sprintf(`ALTER TABLE blogsim.%s ALTER COLUMN cluster_id SET NOT NULL`, tbl),
				fmt.Sprintf(`CREATE INDEX ON blogsim.%s (cluster_id, tailnet_id)`, tbl),
			)
		}

		for _, s := range stmts {
			if err := tx.Exec(s).Error; err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}

		return nil
	})
}

type ownerRow struct {
	owner string
	epoch int64
}

func currentOwner(ctx context.Context, c *sql.Conn, id string) (ownerRow, error) {
	var o ownerRow
	err := c.QueryRowContext(ctx, `SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id = $1`, id).Scan(&o.owner, &o.epoch)

	return o, err
}

func blogOwner(ctx context.Context, c *sql.Conn, id string) (string, error) {
	var o string
	err := c.QueryRowContext(ctx, `SELECT cluster_id FROM blogsim.nodes WHERE tailnet_id = $1 LIMIT 1`, id).Scan(&o)

	return o, err
}

func flip(owner string) string {
	if owner == "src" {
		return "dst"
	}

	return "src"
}

// moveStatements runs one move inside tx and returns the rows it changed.
func moveStatements(ctx context.Context, tx *sql.Tx, design, id string, cur ownerRow, blogCur string) (int64, error) {
	switch design {
	case "owner_cas":
		// Simplified owner move: one CAS statement (the registry design runs
		// the real two-step path of ADR 0010).
		r, err := tx.ExecContext(ctx, `UPDATE tailnets SET owner_cluster_id = $1, owner_epoch = owner_epoch + 1, updated_at = now()
			WHERE id = $2 AND owner_cluster_id = $3 AND owner_epoch = $4`, flip(cur.owner), id, cur.owner, cur.epoch)
		if err != nil {
			return 0, err
		}

		n, _ := r.RowsAffected()
		if n != 1 {
			return n, fmt.Errorf("owner CAS changed %d rows", n)
		}

		return n, nil
	default:
		var total int64

		for _, tbl := range blogTables {
			r, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE blogsim.%s SET cluster_id = $1 WHERE tailnet_id = $2 AND cluster_id = $3`, tbl),
				flip(blogCur), id, blogCur)
			if err != nil {
				return 0, err
			}

			n, _ := r.RowsAffected()
			total += n
		}

		return total, nil
	}
}

func moveOnce(ctx context.Context, db *sql.DB, env *regEnv, design, id string, size, trial int) (b3Trial, error) {
	tr := b3Trial{Design: design, Size: size, Trial: trial, Locks: map[string]int64{}}

	if design == registryDesign {
		return moveRegistryOnce(ctx, db, env, id, tr)
	}

	mc, err := db.Conn(ctx)
	if err != nil {
		return tr, err
	}
	defer mc.Close()

	oc, err := db.Conn(ctx)
	if err != nil {
		return tr, err
	}
	defer oc.Close()

	cur, err := currentOwner(ctx, oc, id)
	if err != nil {
		return tr, err
	}

	blogCur, err := blogOwner(ctx, oc, id)
	if err != nil {
		return tr, err
	}

	if _, err := oc.ExecContext(ctx, `CHECKPOINT`); err != nil {
		return tr, err
	}

	var pid int
	if err := mc.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return tr, err
	}

	var lsn0, lsn1 string
	_ = oc.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn0)

	t0 := time.Now()

	tx, err := mc.BeginTx(ctx, nil)
	if err != nil {
		return tr, err
	}

	tr.Rows, err = moveStatements(ctx, tx, design, id, cur, blogCur)
	if err != nil {
		_ = tx.Rollback()
		return tr, err
	}

	tr.StmtMs = msSince(t0)

	rows, err := oc.QueryContext(ctx, `SELECT locktype || ':' || mode || coalesce(':' || relation::regclass::text, ''), count(*)
		FROM pg_locks WHERE pid = $1 GROUP BY 1`, pid)
	if err == nil {
		for rows.Next() {
			var k string

			var n int64
			if rows.Scan(&k, &n) == nil {
				tr.Locks[k] = n
			}
		}

		_ = rows.Close()
	}

	t1 := time.Now()

	if err := tx.Commit(); err != nil {
		return tr, err
	}

	tr.CommitMs = msSince(t1)
	tr.TotalMs = tr.StmtMs + tr.CommitMs

	_ = oc.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn1)
	_ = oc.QueryRowContext(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)`, lsn1, lsn0).Scan(&tr.WALBytes)

	return tr, nil
}

// moveRegistryOnce: StmtMs is MoveOwner (the request), CommitMs HandOver,
// each its own committed transaction; WAL covers both.
func moveRegistryOnce(ctx context.Context, db *sql.DB, env *regEnv, id string, tr b3Trial) (b3Trial, error) {
	oc, err := db.Conn(ctx)
	if err != nil {
		return tr, err
	}
	defer oc.Close()

	if _, err := oc.ExecContext(ctx, `CHECKPOINT`); err != nil {
		return tr, err
	}

	var lsn0, lsn1 string
	_ = oc.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn0)

	tr.StmtMs, tr.CommitMs, err = env.move(ctx, db, id)
	if err != nil {
		return tr, err
	}

	tr.TotalMs = tr.StmtMs + tr.CommitMs
	tr.Rows = 2
	tr.Target = env.lastTo

	_ = oc.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn1)
	_ = oc.QueryRowContext(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)`, lsn1, lsn0).Scan(&tr.WALBytes)

	return tr, nil
}

type probeSample struct {
	start time.Time
	dur   time.Duration
}

// probeLoop writes to one random device of tenant id until stop, the way a
// cluster process would (owner_cas: fencing read + update in one tx;
// row_rewrite: per-row cluster filter on the copy).
func probeLoop(ctx context.Context, db *sql.DB, design, id string, stop <-chan struct{}, out *[]probeSample) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	var ids []int64

	table := "nodes"
	if design == "row_rewrite" {
		table = "blogsim.nodes"
	}

	rows, err := c.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tailnet_id = $1 LIMIT 2000`, table), id)
	if err != nil {
		return err
	}

	for rows.Next() {
		var n int64
		if rows.Scan(&n) == nil {
			ids = append(ids, n)
		}
	}

	_ = rows.Close()

	for {
		select {
		case <-stop:
			return nil
		default:
		}

		nid := ids[rand.IntN(len(ids))]
		s := time.Now()

		if design == "owner_cas" || design == registryDesign {
			tx, err := c.BeginTx(ctx, nil)
			if err != nil {
				return err
			}

			var owner string

			var epoch int64

			// owner_cas keeps the 2026-11-22 probe (FOR SHARE only) so its
			// numbers stay comparable; registry uses the fence of
			// tenantscope.checkFence (shared advisory lock, then FOR SHARE),
			// which waits on LockOwnership of a move like a cluster write.
			q := `SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id = $1 FOR SHARE`
			if design == registryDesign {
				q = `SELECT t.owner_cluster_id, t.owner_epoch
					FROM pg_advisory_xact_lock_shared(hashtextextended('tenantscope/ownership/' || $1, 0)) AS l, tailnets t
					WHERE t.id = $1 FOR SHARE OF t`
			}

			err = tx.QueryRowContext(ctx, q, id).Scan(&owner, &epoch)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE nodes SET last_seen = now() WHERE tailnet_id = $1 AND id = $2`, id, nid)
			}

			if err != nil {
				_ = tx.Rollback()
				return err
			}

			if err := tx.Commit(); err != nil {
				return err
			}
		} else {
			// Updates whatever cluster currently owns the row; the probe
			// measures blocking, not fencing outcome.
			if _, err := c.ExecContext(ctx, `UPDATE blogsim.nodes SET last_seen = now() WHERE tailnet_id = $1 AND id = $2`, id, nid); err != nil {
				return err
			}
		}

		*out = append(*out, probeSample{start: s, dur: time.Since(s)})

		time.Sleep(2 * time.Millisecond)
	}
}

func probeMove(ctx context.Context, db *sql.DB, env *regEnv, design, id, other string, size, trial int) (b3Probe, error) {
	p := b3Probe{Design: design, Size: size, Trial: trial}
	stop := make(chan struct{})

	var (
		same, oth []probeSample
		wg        sync.WaitGroup
		perr      = make(chan error, 2)
	)

	for _, x := range []struct {
		id  string
		out *[]probeSample
	}{{id, &same}, {other, &oth}} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := probeLoop(ctx, db, design, x.id, stop, x.out); err != nil {
				perr <- err
			}
		}()
	}

	time.Sleep(500 * time.Millisecond)

	mStart := time.Now()

	var (
		tr  float64
		err error
	)

	if design == registryDesign {
		var req, ho float64

		req, ho, err = env.move(ctx, db, id)
		tr = req + ho
		p.Target = env.lastTo
	} else {
		tr, err = moveNoCheckpoint(ctx, db, design, id)
	}

	mEnd := time.Now()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(perr)

	if err != nil {
		return p, err
	}

	if e := <-perr; e != nil {
		return p, e
	}

	p.MoveMs = tr
	p.SameBaseP50, p.SameMaxIn, p.SameN = window(same, mStart, mEnd)
	p.OtherBase, p.OtherMaxIn, _ = window(oth, mStart, mEnd)

	return p, nil
}

func moveNoCheckpoint(ctx context.Context, db *sql.DB, design, id string) (float64, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	cur, err := currentOwner(ctx, c, id)
	if err != nil {
		return 0, err
	}

	blogCur, err := blogOwner(ctx, c, id)
	if err != nil {
		return 0, err
	}

	t0 := time.Now()

	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}

	if _, err := moveStatements(ctx, tx, design, id, cur, blogCur); err != nil {
		_ = tx.Rollback()
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return msSince(t0), nil
}

// window returns the p50 latency of probe ops finished before the move
// started, and the max latency of ops overlapping the move window.
func window(s []probeSample, a, b time.Time) (float64, float64, int) {
	var base []float64

	maxIn := 0.0
	n := 0

	for _, x := range s {
		end := x.start.Add(x.dur)

		switch {
		case end.Before(a):
			base = append(base, float64(x.dur.Microseconds())/1000)
		case x.start.Before(b):
			n++
			maxIn = max(maxIn, float64(x.dur.Microseconds())/1000)
		}
	}

	slices.Sort(base)

	p50 := 0.0
	if len(base) > 0 {
		p50 = base[len(base)/2]
	}

	return p50, maxIn, n
}
