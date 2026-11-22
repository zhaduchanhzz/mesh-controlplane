//go:build benchdb

package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	hsdb "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

type b2Result struct {
	Label      string                 `json:"label"`
	Started    time.Time              `json:"started"`
	Params     map[string]string      `json:"params"`
	WallS      float64                `json:"wall_s"`
	Ops        map[string]histSummary `json:"ops"`
	OpsPerSec  map[string]float64     `json:"ops_per_s"`
	TotalOps   float64                `json:"total_ops_per_s"`
	Standby    histSummary            `json:"standby_poll"`
	OpenMs     []float64              `json:"open_ms"`
	LoadMs     []float64              `json:"startup_load_ms"`
	HarnessCPU float64                `json:"harness_cpu_cores"`
	PG         pgDelta                `json:"pg"`
	Conns      connStats              `json:"conns"`
	Bouncer    *bouncerStats          `json:"bouncer,omitempty"`
	CPU        map[string]float64     `json:"cgroup_cpu_cores"`
	Top        []stmtStat             `json:"top_statements"`
	Sizes      []relSize              `json:"sizes"`
	DBBytes    int64                  `json:"db_bytes"`
	Rows       map[string]int64       `json:"rows"`
	Errors     []string               `json:"errors,omitempty"`
}

type pgDelta struct {
	CommitsPerS   float64 `json:"commits_per_s"`
	RollbacksPerS float64 `json:"rollbacks_per_s"`
	TupInsertedPS float64 `json:"tup_inserted_per_s"`
	TupUpdatedPS  float64 `json:"tup_updated_per_s"`
	TupDeletedPS  float64 `json:"tup_deleted_per_s"`
	TupFetchedPS  float64 `json:"tup_fetched_per_s"`
	TupReturnedPS float64 `json:"tup_returned_per_s"`
	BlksReadPS    float64 `json:"blks_read_per_s"`
	HitRatio      float64 `json:"cache_hit_ratio"`
	WALMBPerS     float64 `json:"wal_mb_per_s"`
	StmtCallsPerS float64 `json:"statements_per_s"`
}

type connStats struct {
	Samples      int     `json:"samples"`
	MaxTotal     int64   `json:"max_total"`
	MeanTotal    float64 `json:"mean_total"`
	MaxActive    int64   `json:"max_active"`
	MeanActive   float64 `json:"mean_active"`
	MaxIdleTx    int64   `json:"max_idle_in_tx"`
	MaxLockWait  int64   `json:"max_lock_wait"`
	MeanLockWait float64 `json:"mean_lock_wait"`
}

type bouncerStats struct {
	MaxClActive      int64   `json:"max_cl_active"`
	MaxClWaiting     int64   `json:"max_cl_waiting"`
	MeanClWaiting    float64 `json:"mean_cl_waiting"`
	MaxSvActive      int64   `json:"max_sv_active"`
	MaxWaitMs        float64 `json:"max_maxwait_ms"`
	XactPerS         float64 `json:"xact_per_s"`
	QueryPerS        float64 `json:"query_per_s"`
	AvgWaitPerXactUs float64 `json:"avg_client_wait_us_per_xact"`
	Error            string  `json:"error,omitempty"`
}

type stmtStat struct {
	Calls   int64   `json:"calls"`
	TotalMs float64 `json:"total_ms"`
	MeanMs  float64 `json:"mean_ms"`
	StdMs   float64 `json:"stddev_ms"`
	MaxMs   float64 `json:"max_ms"`
	Rows    int64   `json:"rows"`
	Query   string  `json:"query"`
}

type relSize struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
}

type proc struct {
	idx    int
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	ready  chan struct{}
	result chan workerResult
	failed chan string
}

func runB2(args []string) error {
	fs := flag.NewFlagSet("b2", flag.ExitOnError)

	var admin, wt pgTarget

	var (
		clusters    = fs.Int("clusters", 1, "active cluster processes (each also gets a standby unless -standby-pool 0)")
		tenants     = fs.Int("tenants", 10, "tenants per cluster")
		devices     = fs.Int("devices", 250, "devices per tenant")
		users       = fs.Int("users", 10, "users per tenant")
		workers     = fs.Int("workers", 8, "op goroutines per active process")
		pool        = fs.Int("pool", 8, "pool size per active process")
		standbyPool = fs.Int("standby-pool", 2, "pool size per standby process (0 = no standby)")
		rate        = fs.Float64("rate", 0, "target ops/s per active process (0 = unthrottled)")
		dur         = fs.Duration("duration", 30*time.Second, "measured window")
		fence       = fs.Bool("fence", true, "simulated P10 fencing read in every write")
		mix         = fs.String("mix", defaultMix, "operation weights")
		cpus        = fs.String("cpus", "", "taskset CPU list for worker processes (empty = no pinning)")
		bouncerPort = fs.Int("bouncer-admin-port", 0, "PgBouncer port for SHOW POOLS/STATS (0 = direct run)")
		cgroups     = fs.String("cgroups", "", "name=path-to-cpu.stat,... sampled before/after")
		label       = fs.String("label", "", "run label")
		out         = fs.String("out", "", "append JSON result to this file")
	)

	fs.StringVar(&admin.Host, "admin-host", "127.0.0.1", "PostgreSQL host (direct, for stats)")
	fs.IntVar(&admin.Port, "admin-port", 5432, "PostgreSQL port (direct)")
	fs.StringVar(&wt.Host, "host", "127.0.0.1", "host the cluster processes connect to (PostgreSQL or PgBouncer)")
	fs.IntVar(&wt.Port, "port", 5432, "port the cluster processes connect to")
	fs.StringVar(&admin.DB, "db", "bench", "database")
	fs.StringVar(&admin.User, "user", "bench", "user")
	fs.StringVar(&admin.PassFile, "passfile", "", "password file")
	_ = fs.Parse(args)

	wt.DB, wt.User, wt.PassFile = admin.DB, admin.User, admin.PassFile

	res := b2Result{Label: *label, Started: time.Now().UTC(), Params: map[string]string{}, CPU: map[string]float64{}, Rows: map[string]int64{}}
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name != "passfile" {
			res.Params[f.Name] = f.Value.String()
		}
	})

	adb, err := openAdmin(admin, false)
	if err != nil {
		return err
	}

	if !adb.Migrator().HasTable("tailnets") {
		// First run: the real open path creates the v0.1 schema.
		h, err := hsdb.NewHeadscaleDatabase(admin.config("platform", "", 2, 2))
		if err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}

		_ = h.Close()
	}

	if err := adb.Exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`).Error; err != nil {
		return err
	}

	owned, err := ensureTailnets(adb, *clusters, *tenants)
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}

	common := []string{
		"-host", wt.Host, "-port", strconv.Itoa(wt.Port), "-db", wt.DB, "-user", wt.User, "-passfile", wt.PassFile,
		"-duration", dur.String(), "-devices", strconv.Itoa(*devices), "-users", strconv.Itoa(*users),
	}

	var procs []*proc

	for c := range *clusters {
		cl := clusterName(c)
		a := append([]string{"worker", "-cluster", cl, "-tailnets", strings.Join(owned[cl], ","),
			"-pool", strconv.Itoa(*pool), "-workers", strconv.Itoa(*workers),
			"-rate", strconv.FormatFloat(*rate, 'f', -1, 64), "-fence=" + strconv.FormatBool(*fence), "-mix", *mix}, common...)
		procs = append(procs, spawn(len(procs), self, *cpus, a))

		if *standbyPool > 0 {
			s := append([]string{"standby", "-cluster", cl, "-tailnets", owned[cl][0], "-pool", strconv.Itoa(*standbyPool)}, common...)
			procs = append(procs, spawn(len(procs), self, *cpus, s))
		}
	}

	for _, p := range procs {
		if p.cmd == nil {
			return errors.New("spawn failed")
		}

		select {
		case <-p.ready:
		case msg := <-p.failed:
			killAll(procs)
			return fmt.Errorf("process %d failed before READY: %s", p.idx, msg)
		case <-time.After(60 * time.Minute):
			killAll(procs)
			return fmt.Errorf("process %d not ready", p.idx)
		}
	}

	// Let seeding's dead tuples and stats settle so they are not counted.
	_ = adb.Exec(`VACUUM ANALYZE`).Error
	time.Sleep(2 * time.Second)

	_ = adb.Exec(`SELECT pg_stat_statements_reset()`).Error
	before, err := readDBStats(adb)
	if err != nil {
		return err
	}

	cg0 := readCgroups(*cgroups)

	var bdb *gorm.DB

	var bstat0 map[string]float64

	if *bouncerPort > 0 {
		bt := admin
		bt.Port, bt.DB = *bouncerPort, "pgbouncer"

		bdb, err = openAdmin(bt, true)
		if err == nil {
			bstat0, err = bouncerStatsRow(bdb, admin.DB)
		}

		if err != nil {
			res.Bouncer = &bouncerStats{Error: err.Error()}
			bdb = nil
		} else {
			res.Bouncer = &bouncerStats{}
		}
	}

	stop := make(chan struct{})

	var sw sync.WaitGroup

	sw.Add(1)

	go func() {
		defer sw.Done()
		sampleLoop(adb, bdb, admin.DB, &res, stop)
	}()

	t0 := time.Now()

	for _, p := range procs {
		_, _ = io.WriteString(p.stdin, "GO\n")
	}

	var results []workerResult

	for _, p := range procs {
		select {
		case r := <-p.result:
			results = append(results, r)
		case msg := <-p.failed:
			res.Errors = append(res.Errors, fmt.Sprintf("process %d: %s", p.idx, msg))
		case <-time.After(*dur + 10*time.Minute):
			res.Errors = append(res.Errors, fmt.Sprintf("process %d: no result", p.idx))
		}
	}

	res.WallS = time.Since(t0).Seconds()

	close(stop)
	sw.Wait()

	for _, p := range procs {
		_ = p.stdin.Close()
		_ = p.cmd.Wait()
	}

	cg1 := readCgroups(*cgroups)
	for k, v := range cg1 {
		res.CPU[k] = (v - cg0[k]) / res.WallS
	}

	time.Sleep(1500 * time.Millisecond) // backends flush their stats on exit

	after, err := readDBStats(adb)
	if err != nil {
		return err
	}

	res.PG = diffStats(adb, before, after, res.WallS)

	if bdb != nil {
		if bstat1, err := bouncerStatsRow(bdb, admin.DB); err == nil {
			dx := bstat1["total_xact_count"] - bstat0["total_xact_count"]
			res.Bouncer.XactPerS = dx / res.WallS
			res.Bouncer.QueryPerS = (bstat1["total_query_count"] - bstat0["total_query_count"]) / res.WallS

			if dx > 0 {
				res.Bouncer.AvgWaitPerXactUs = (bstat1["total_wait_time"] - bstat0["total_wait_time"]) / dx
			}
		}
	}

	aggregate(&res, results)
	res.Top = topStatements(adb)
	res.PG.StmtCallsPerS = stmtCalls(adb) / res.WallS
	res.Sizes, res.DBBytes = sizes(adb)

	for _, tbl := range []string{"tailnets", "users", "credentials", "nodes"} {
		var n int64

		_ = adb.Raw(`SELECT count(*) FROM ` + tbl).Scan(&n).Error
		res.Rows[tbl] = n
	}

	printB2(res)

	if *out != "" {
		f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()

		b, _ := json.Marshal(res)
		_, _ = f.Write(append(b, '\n'))
	}

	if len(res.Errors) > 0 {
		return fmt.Errorf("%d process errors (see result)", len(res.Errors))
	}

	return nil
}

func clusterName(c int) string { return fmt.Sprintf("c%02d", c) }

func openAdmin(t pgTarget, simple bool) (*gorm.DB, error) {
	dsn, err := t.dsn()
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: simple}),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("admin connection to %s:%d/%s: %w", t.Host, t.Port, t.DB, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}

	sqlDB.SetMaxOpenConns(2)

	return db, nil
}

// ensureTailnets creates, through the platform (unscoped) handle as the
// future CreateTailnet API will, the tenants of every cluster that do not
// exist yet; existing ones are reused so the database grows run by run.
func ensureTailnets(adb *gorm.DB, clusters, tenants int) (map[string][]string, error) {
	owned := map[string][]string{}

	for c := range clusters {
		cl := clusterName(c)
		for t := range tenants {
			slug := fmt.Sprintf("%s-t%03d", cl, t)

			var tn types.Tailnet

			err := adb.Where("slug = ?", slug).First(&tn).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				now := time.Now().UTC()
				tn = types.Tailnet{
					ID: uuid.NewV4().String(), Slug: slug, Status: types.TailnetActive,
					OwnerClusterID: cl, OwnerEpoch: 1, PolicyRevision: 1, ConfigRevision: 1,
					CreatedAt: now, UpdatedAt: now,
				}
				err = adb.Create(&tn).Error
			}

			if err != nil {
				return nil, fmt.Errorf("tailnet %s: %w", slug, err)
			}

			owned[cl] = append(owned[cl], tn.ID)
		}
	}

	return owned, nil
}

func spawn(idx int, self, cpus string, args []string) *proc {
	p := &proc{idx: idx, ready: make(chan struct{}), result: make(chan workerResult, 1), failed: make(chan string, 1)}

	name, argv := self, args
	if cpus != "" {
		name, argv = "taskset", append([]string{"-c", cpus, self}, args...)
	}

	p.cmd = exec.Command(name, argv...)
	p.cmd.Stderr = &prefixWriter{prefix: fmt.Sprintf("[p%02d] ", idx), w: os.Stderr}

	var err error

	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		p.cmd = nil
		return p
	}

	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		p.cmd = nil
		return p
	}

	if err := p.cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		p.cmd = nil

		return p
	}

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)

		gotResult := false

		for sc.Scan() {
			line := sc.Text()

			switch {
			case line == "READY":
				close(p.ready)
			case strings.HasPrefix(line, "RESULT "):
				var r workerResult
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "RESULT ")), &r); err != nil {
					p.failed <- "bad result: " + err.Error()
					return
				}

				gotResult = true
				p.result <- r
			}
		}

		if !gotResult {
			p.failed <- "exited without result"
		}
	}()

	return p
}

func killAll(procs []*proc) {
	for _, p := range procs {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
}

type prefixWriter struct {
	prefix string
	w      io.Writer
	mu     sync.Mutex
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		fmt.Fprintln(p.w, p.prefix+line)
	}

	return len(b), nil
}

type dbStats struct {
	XactCommit, XactRollback, TupReturned, TupFetched, TupInserted, TupUpdated, TupDeleted, BlksRead, BlksHit int64
	LSN                                                                                                       string
}

func readDBStats(adb *gorm.DB) (dbStats, error) {
	var s dbStats

	err := adb.Raw(`SELECT xact_commit, xact_rollback, tup_returned, tup_fetched, tup_inserted, tup_updated,
		tup_deleted, blks_read, blks_hit, pg_current_wal_lsn()::text AS lsn
		FROM pg_stat_database WHERE datname = current_database()`).Scan(&s).Error

	return s, err
}

func diffStats(adb *gorm.DB, a, b dbStats, wall float64) pgDelta {
	var wal float64

	_ = adb.Raw(`SELECT pg_wal_lsn_diff(?::pg_lsn, ?::pg_lsn)`, b.LSN, a.LSN).Scan(&wal).Error

	d := pgDelta{
		CommitsPerS:   float64(b.XactCommit-a.XactCommit) / wall,
		RollbacksPerS: float64(b.XactRollback-a.XactRollback) / wall,
		TupInsertedPS: float64(b.TupInserted-a.TupInserted) / wall,
		TupUpdatedPS:  float64(b.TupUpdated-a.TupUpdated) / wall,
		TupDeletedPS:  float64(b.TupDeleted-a.TupDeleted) / wall,
		TupFetchedPS:  float64(b.TupFetched-a.TupFetched) / wall,
		TupReturnedPS: float64(b.TupReturned-a.TupReturned) / wall,
		BlksReadPS:    float64(b.BlksRead-a.BlksRead) / wall,
		WALMBPerS:     wal / wall / 1e6,
	}

	if hit, rd := b.BlksHit-a.BlksHit, b.BlksRead-a.BlksRead; hit+rd > 0 {
		d.HitRatio = float64(hit) / float64(hit+rd)
	}

	return d
}

func sampleLoop(adb, bdb *gorm.DB, dbName string, res *b2Result, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	var sumTotal, sumActive, sumLock, sumWait float64

	for {
		select {
		case <-stop:
			if n := float64(res.Conns.Samples); n > 0 {
				res.Conns.MeanTotal, res.Conns.MeanActive, res.Conns.MeanLockWait = sumTotal/n, sumActive/n, sumLock/n
				if res.Bouncer != nil {
					res.Bouncer.MeanClWaiting = sumWait / n
				}
			}

			return
		case <-tick.C:
		}

		var s struct{ Total, Active, IdleTx, LockWait int64 }

		err := adb.Raw(`SELECT count(*) AS total,
			count(*) FILTER (WHERE state = 'active') AS active,
			count(*) FILTER (WHERE state LIKE 'idle in transaction%') AS idle_tx,
			count(*) FILTER (WHERE wait_event_type = 'Lock') AS lock_wait
			FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()
			AND backend_type = 'client backend'`).Scan(&s).Error
		if err != nil {
			continue
		}

		res.Conns.Samples++
		sumTotal += float64(s.Total)
		sumActive += float64(s.Active)
		sumLock += float64(s.LockWait)
		res.Conns.MaxTotal = max(res.Conns.MaxTotal, s.Total)
		res.Conns.MaxActive = max(res.Conns.MaxActive, s.Active)
		res.Conns.MaxIdleTx = max(res.Conns.MaxIdleTx, s.IdleTx)
		res.Conns.MaxLockWait = max(res.Conns.MaxLockWait, s.LockWait)

		if bdb != nil {
			if p, err := showRow(bdb, "SHOW POOLS", dbName); err == nil {
				res.Bouncer.MaxClActive = max(res.Bouncer.MaxClActive, int64(p["cl_active"]))
				res.Bouncer.MaxClWaiting = max(res.Bouncer.MaxClWaiting, int64(p["cl_waiting"]))
				res.Bouncer.MaxSvActive = max(res.Bouncer.MaxSvActive, int64(p["sv_active"]))
				res.Bouncer.MaxWaitMs = max(res.Bouncer.MaxWaitMs, p["maxwait"]*1000+p["maxwait_us"]/1000)
				sumWait += p["cl_waiting"]
			}
		}
	}
}

// showRow runs a PgBouncer admin SHOW command and returns the numeric
// columns of the row whose "database" is db.
func showRow(bdb *gorm.DB, cmd, db string) (map[string]float64, error) {
	rows, err := bdb.Raw(cmd).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))

		for i := range vals {
			ptrs[i] = &vals[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		m := map[string]float64{}
		name := ""

		for i, c := range cols {
			if c == "database" {
				name = vals[i].String
			}

			if f, err := strconv.ParseFloat(vals[i].String, 64); err == nil {
				m[c] = f
			}
		}

		if name == db {
			return m, nil
		}
	}

	return nil, fmt.Errorf("%s: no row for %s", cmd, db)
}

func bouncerStatsRow(bdb *gorm.DB, db string) (map[string]float64, error) {
	return showRow(bdb, "SHOW STATS", db)
}

func readCgroups(spec string) map[string]float64 {
	out := map[string]float64{}

	for part := range strings.SplitSeq(spec, ",") {
		name, path, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}

		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		for line := range strings.SplitSeq(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
				f, _ := strconv.ParseFloat(v, 64)
				out[name] = f / 1e6
			}
		}
	}

	return out
}

func aggregate(res *b2Result, results []workerResult) {
	ops := map[string]*hist{}
	standby := newHist()

	var cpu float64

	for _, r := range results {
		cpu += r.CPUS

		if r.Role == "standby" {
			if h := r.Ops[opPoll]; h != nil {
				standby.merge(h)
			}

			continue
		}

		res.OpenMs = append(res.OpenMs, r.OpenMs)
		res.LoadMs = append(res.LoadMs, r.LoadMs)

		for k, h := range r.Ops {
			if ops[k] == nil {
				ops[k] = newHist()
			}

			ops[k].merge(h)
		}
	}

	res.HarnessCPU = cpu / res.WallS
	res.Standby = standby.summary()
	res.Ops = map[string]histSummary{}
	res.OpsPerSec = map[string]float64{}

	for k, h := range ops {
		res.Ops[k] = h.summary()
		res.OpsPerSec[k] = float64(h.Count) / res.WallS
		res.TotalOps += float64(h.Count) / res.WallS
	}
}

func topStatements(adb *gorm.DB) []stmtStat {
	var out []stmtStat

	_ = adb.Raw(`SELECT calls, total_exec_time AS total_ms, mean_exec_time AS mean_ms, stddev_exec_time AS std_ms,
		max_exec_time AS max_ms, rows, left(regexp_replace(query, '\s+', ' ', 'g'), 200) AS query
		FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		ORDER BY total_exec_time DESC LIMIT 15`).Scan(&out).Error

	return out
}

func stmtCalls(adb *gorm.DB) float64 {
	var n float64

	_ = adb.Raw(`SELECT coalesce(sum(calls), 0) FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n).Error

	return n
}

func sizes(adb *gorm.DB) ([]relSize, int64) {
	var out []relSize

	_ = adb.Raw(`SELECT c.relname AS name, c.relkind::text AS kind, pg_relation_size(c.oid) AS bytes
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'i')
		ORDER BY bytes DESC`).Scan(&out).Error

	var total int64

	_ = adb.Raw(`SELECT pg_database_size(current_database())`).Scan(&total).Error

	return out, total
}

func printB2(r b2Result) {
	fmt.Printf("== %s  clusters=%s tenants/cluster=%s devices/tenant=%s rate/proc=%s target=%s:%s wall=%.1fs\n",
		r.Label, r.Params["clusters"], r.Params["tenants"], r.Params["devices"], r.Params["rate"], r.Params["host"], r.Params["port"], r.WallS)
	fmt.Printf("   total ops/s=%.0f  harness CPU cores=%.2f  cgroup CPU cores=%v\n", r.TotalOps, r.HarnessCPU, r.CPU)

	keys := make([]string, 0, len(r.Ops))
	for k := range r.Ops {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		s := r.Ops[k]
		fmt.Printf("   %-12s %8.1f/s  p50=%7.2fms p95=%7.2fms p99=%7.2fms max=%8.2fms err=%d %s\n",
			k, r.OpsPerSec[k], s.P50Ms, s.P95Ms, s.P99Ms, s.MaxMs, s.Errors, s.FirstErr)
	}

	fmt.Printf("   standby poll: n=%d p99=%.2fms err=%d\n", r.Standby.Count, r.Standby.P99Ms, r.Standby.Errors)
	fmt.Printf("   PG: commits/s=%.0f stmts/s=%.0f ins/s=%.0f upd/s=%.0f fetched/s=%.0f returned/s=%.0f WAL=%.2fMB/s hit=%.4f\n",
		r.PG.CommitsPerS, r.PG.StmtCallsPerS, r.PG.TupInsertedPS, r.PG.TupUpdatedPS, r.PG.TupFetchedPS, r.PG.TupReturnedPS, r.PG.WALMBPerS, r.PG.HitRatio)
	fmt.Printf("   conns: max=%d mean=%.1f active max=%d mean=%.1f lockwait max=%d\n",
		r.Conns.MaxTotal, r.Conns.MeanTotal, r.Conns.MaxActive, r.Conns.MeanActive, r.Conns.MaxLockWait)

	if r.Bouncer != nil {
		fmt.Printf("   bouncer: %+v\n", *r.Bouncer)
	}

	fmt.Printf("   rows=%v db=%.1fMB\n", r.Rows, float64(r.DBBytes)/1e6)
}
