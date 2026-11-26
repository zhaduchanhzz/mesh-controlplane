package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
)

// P13d (ADR 0013): migrations are an operator step under a lock, and a
// binary runs on a database that has newer "expand" migrations.

// Test migrations of a newer binary (never in the production chain).
const (
	p13dExpandID    = "209912310000-p13d-test-expand"
	p13dContractID  = "209912310100-p13d-test-contract"
	p13dUnmarkedID  = "209912310200-p13d-test-unmarked"
	p13dHelperEnv   = "MESH_P13D_HELPER"
	p13dDBEnv       = "MESH_P13D_DB"
	p13dStartEnv    = "MESH_P13D_START"
	p13dHelperDelay = time.Second
)

// The schema.sql lines of the test migrations.
const (
	p13dExpandDDL   = `CREATE TABLE p13d_canary(id integer PRIMARY KEY, note text);`
	p13dContractDDL = `CREATE TABLE p13d_canary(id integer PRIMARY KEY, note text, contracted integer);`
)

// p13dExpand creates a table no binary of the current chain uses; with
// delay it sleeps inside its transaction, after the DDL, so concurrent runs
// overlap.
func p13dExpand(delay time.Duration) *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: p13dExpandID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				err := tx.Exec(p13dExpandDDL).Error
				if err == nil && delay > 0 {
					time.Sleep(delay)
				}

				return err
			})
		},
	}
}

// p13dContract plays a migration the current binary may not run with.
func p13dContract() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: p13dContractID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`ALTER TABLE p13d_canary ADD COLUMN contracted integer`).Error
			})
		},
	}
}

// p13dChain is the chain of binary "N+1": the current chain plus extra,
// each declared with its min_reader_id; sqliteDDL is what extra adds to its
// schema.sql.
func p13dChain(extra []*gormigrate.Migration, minReader map[string]string, sqliteDDL string) chain {
	ch := currentChain()
	ch.migrations = append(ch.migrations, extra...)
	ch.sqliteSchema += "\n" + sqliteDDL

	compat := map[string]string{}
	for k, v := range ch.compat {
		compat[k] = v
	}

	for k, v := range minReader {
		compat[k] = v
	}

	ch.compat = compat

	return ch
}

// p13dHelperChain is the chain a helper process runs as.
func p13dHelperChain(mode string) chain {
	n := LatestMigrationID()

	switch mode {
	case "expand":
		return p13dChain([]*gormigrate.Migration{p13dExpand(0)}, map[string]string{p13dExpandID: n}, p13dExpandDDL)
	case "expand-slow":
		return p13dChain([]*gormigrate.Migration{p13dExpand(p13dHelperDelay)}, map[string]string{p13dExpandID: n}, p13dExpandDDL)
	case "contract":
		return p13dChain([]*gormigrate.Migration{p13dExpand(0), p13dContract()},
			map[string]string{p13dExpandID: n, p13dContractID: p13dContractID}, p13dContractDDL)
	default:
		return currentChain()
	}
}

// p13dDBSpec carries a test database to a helper process (no secrets: the
// test servers trust local connections).
type p13dDBSpec struct {
	SQLite string `json:"sqlite,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	User   string `json:"user,omitempty"`
	Pass   string `json:"pass,omitempty"`
	Name   string `json:"name,omitempty"`
}

func p13dSpecOf(cfg *types.Config) p13dDBSpec {
	if cfg.Database.IsSQLite() {
		return p13dDBSpec{SQLite: cfg.SQLitePath()}
	}

	pg := cfg.Database.Postgres

	return p13dDBSpec{Host: pg.Host, Port: pg.Port, User: pg.User, Pass: string(pg.Pass), Name: pg.Name}
}

func (s p13dDBSpec) config() *types.Config {
	if s.SQLite != "" {
		return sqliteTestConfig(s.SQLite)
	}

	return &types.Config{
		Database: types.DatabaseConfig{
			Type: types.DatabasePostgres,
			Postgres: types.PostgresConfig{
				Host: s.Host, Port: s.Port, User: s.User, Pass: types.Secret(s.Pass), Name: s.Name, Ssl: "disable",
			},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
	}
}

// TestP13dHelperProcess is the body of a helper process ("another binary"):
// it migrates the database of MESH_P13D_DB with the chain MESH_P13D_HELPER
// names, starting at MESH_P13D_START (Unix ns), and prints the result.
func TestP13dHelperProcess(t *testing.T) {
	mode := os.Getenv(p13dHelperEnv)
	if mode == "" {
		t.Skip("helper process of the P13d tests")
	}

	var spec p13dDBSpec
	if err := json.Unmarshal([]byte(os.Getenv(p13dDBEnv)), &spec); err != nil { //nolint:noinlineerr
		fmt.Fprintln(os.Stderr, "helper: spec:", err)
		os.Exit(2)
	}

	if ns, err := strconv.ParseInt(os.Getenv(p13dStartEnv), 10, 64); err == nil { //nolint:noinlineerr
		time.Sleep(time.Until(time.Unix(0, ns)))
	}

	res, err := migrateChain(context.Background(), spec.config(), p13dHelperChain(mode))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: migrate:", err)
		os.Exit(1)
	}

	_ = json.NewEncoder(os.Stdout).Encode(res)

	os.Exit(0)
}

type p13dRun struct {
	res    MigrateResult
	err    error
	stderr string
}

// p13dSpawn starts a helper process running chain mode on cfg's database at
// start; wait returns its outcome.
func p13dSpawn(t *testing.T, cfg *types.Config, mode string, start time.Time) func() p13dRun {
	t.Helper()

	spec, err := json.Marshal(p13dSpecOf(cfg))
	require.NoError(t, err)

	cmd := exec.Command(os.Args[0], "-test.run=^TestP13dHelperProcess$", "-test.count=1") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(),
		p13dHelperEnv+"="+mode,
		p13dDBEnv+"="+string(spec),
		p13dStartEnv+"="+strconv.FormatInt(start.UnixNano(), 10),
	)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Start())

	return func() p13dRun {
		err := cmd.Wait()

		var r p13dRun

		r.err, r.stderr = err, stderr.String()

		// The test framework prints PASS/ok lines after the JSON on some
		// paths; the result is the first line that parses.
		for line := range strings.SplitSeq(stdout.String(), "\n") {
			if json.Unmarshal([]byte(line), &r.res) == nil && r.res.Latest != "" {
				break
			}
		}

		return r
	}
}

// p13dRunHelper runs one helper to completion.
func p13dRunHelper(t *testing.T, cfg *types.Config, mode string) p13dRun {
	t.Helper()

	return p13dSpawn(t, cfg, mode, time.Now())()
}

func migrationRows(t *testing.T, conn *gorm.DB) map[string]int {
	t.Helper()

	var ids []string
	require.NoError(t, conn.Table("migrations").Pluck("id", &ids).Error)

	out := map[string]int{}
	for _, id := range ids {
		out[id]++
	}

	return out
}

// rawConn opens cfg's database without any check (closed at cleanup).
func rawConn(t *testing.T, cfg *types.Config) *gorm.DB {
	t.Helper()

	conn, err := openDB(cfg.Database, cfg.SQLitePath())
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, err := conn.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return conn
}

// ADR 0013 §1: OpenPlatform (cluster, mesh-placement, admin CLI) never
// creates or migrates the schema; it refuses a database behind the binary
// with ErrSchemaNeedsMigration and leaves it unchanged.
func TestOpenPlatformDoesNotMigrate(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			cfg := bk.cfg(t, "", "")

			p, err := OpenPlatform(cfg)
			if err == nil {
				_ = p.Close()
			}

			require.ErrorIs(t, err, ErrSchemaNeedsMigration, "a fresh database is refused, not created")
			assert.Contains(t, err.Error(), "migrate")

			present, err := tableExists(rawConn(t, cfg), "tailnets")
			require.NoError(t, err)
			assert.False(t, present, "OpenPlatform created the schema")

			res, err := Migrate(context.Background(), cfg)
			require.NoError(t, err)
			assert.True(t, res.Fresh)
			assert.Equal(t, LatestMigrationID(), res.Latest)

			p = openPlatform(t, cfg)

			// Rewind the last migration: the binary needs it, OpenPlatform
			// refuses and does not run it.
			last := v01Migrations()[len(v01Migrations())-1]
			require.NoError(t, last.Rollback(p.db))
			require.NoError(t, p.db.Exec(`DELETE FROM migrations WHERE id = ?`, last.ID).Error)
			require.NoError(t, p.Close())

			p, err = OpenPlatform(cfg)
			if err == nil {
				_ = p.Close()
			}

			require.ErrorIs(t, err, ErrSchemaNeedsMigration)
			assert.Contains(t, err.Error(), last.ID)
			assert.Zero(t, migrationRows(t, rawConn(t, cfg))[last.ID], "OpenPlatform ran the pending migration")

			res, err = Migrate(context.Background(), cfg)
			require.NoError(t, err)
			assert.Equal(t, []string{last.ID}, res.Applied)

			_ = openPlatform(t, cfg)
		})
	}
}

// L7 (ADR 0013 §2): two migrate runs at once, in two processes, both
// succeed and every migration is recorded once: on a fresh database, and
// for a new migration (whose transaction sleeps so the runs overlap).
func TestMigrateTwoProcessesAtOnce(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			cfg := bk.cfg(t, "", "")

			start := time.Now().Add(1500 * time.Millisecond)
			wa := p13dSpawn(t, cfg, "current", start)
			wb := p13dSpawn(t, cfg, "current", start)
			a, b := wa(), wb()

			require.NoError(t, a.err, "fresh run A: %s", a.stderr)
			require.NoError(t, b.err, "fresh run B: %s", b.stderr)
			assert.True(t, a.res.Fresh != b.res.Fresh, "exactly one run created the schema: A=%+v B=%+v", a.res, b.res)

			rows := migrationRows(t, rawConn(t, cfg))
			for _, m := range v01Migrations() {
				assert.Equal(t, 1, rows[m.ID], "migration %s recorded once", m.ID)
			}

			start = time.Now().Add(1500 * time.Millisecond)
			wa = p13dSpawn(t, cfg, "expand-slow", start)
			wb = p13dSpawn(t, cfg, "expand-slow", start)
			a, b = wa(), wb()

			require.NoError(t, a.err, "N+1 run A: %s", a.stderr)
			require.NoError(t, b.err, "N+1 run B: %s", b.stderr)

			applied := append(append([]string{}, a.res.Applied...), b.res.Applied...)
			assert.Equal(t, []string{p13dExpandID}, applied, "the new migration ran exactly once")
			assert.Equal(t, 1, migrationRows(t, rawConn(t, cfg))[p13dExpandID])
		})
	}
}

// ADR 0013 §4 (canary): a running binary N keeps working while binary N+1
// migrates an expand migration in another process, and N starts again on
// that database (cluster, mesh-placement, mesh-bootstrap). After a contract
// migration N is refused with the cause.
func TestCanaryOldBinaryAcrossExpandMigration(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			cfg := bk.cfg(t, "", "")

			// Binary N, running.
			pN, err := MigrateAndOpenPlatform(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { _ = pN.Close() })

			id1 := platformTailnet(t, pN, "local", types.TailnetActive, false)

			// Binary N+1 migrates (another process).
			up := p13dRunHelper(t, cfg, "expand")
			require.NoError(t, up.err, "N+1 migrate: %s", up.stderr)
			require.Equal(t, []string{p13dExpandID}, up.res.Applied)

			// N keeps running ...
			id2 := platformTailnet(t, pN, "local", types.TailnetActive, false)
			owned, err := pN.OwnedTailnets("local")
			require.NoError(t, err)
			assert.Len(t, owned, 2)

			_, err = pN.TenantDB(id1)
			require.NoError(t, err)

			// ... and starts again on the database N+1 migrated.
			restarted, err := OpenPlatform(cfg)
			require.NoError(t, err, "binary N refused a database with only a new expand migration")
			t.Cleanup(func() { _ = restarted.Close() })

			_, err = restarted.TenantDB(id2)
			require.NoError(t, err)

			ro, err := OpenReadOnly(cfg)
			require.NoError(t, err, "mesh-bootstrap N refused a database with only a new expand migration")
			closeGorm(ro)

			// A contract migration: N may not start any more.
			down := p13dRunHelper(t, cfg, "contract")
			require.NoError(t, down.err, "N+1 contract migrate: %s", down.stderr)

			_, err = OpenPlatform(cfg)
			require.ErrorIs(t, err, ErrSchemaTooNew)
			assert.Contains(t, err.Error(), p13dContractID)

			_, err = OpenReadOnly(cfg)
			require.ErrorIs(t, err, ErrSchemaNotCurrent)
			require.ErrorIs(t, err, ErrSchemaTooNew)

			// N may not migrate it either (it cannot know what N+1 did).
			_, err = Migrate(context.Background(), cfg)
			require.ErrorIs(t, err, ErrSchemaTooNew)
		})
	}
}

// A migration without a schema_compat row (e.g. written by hand) is treated
// as contract: refused, with the cause.
func TestUnmarkedUnknownMigrationRefused(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			cfg := bk.cfg(t, "", "")
			p := openPlatform(t, cfg)
			require.NoError(t, p.db.Exec(`INSERT INTO migrations (id) VALUES (?)`, p13dUnmarkedID).Error)

			_, err := OpenPlatform(cfg)
			require.ErrorIs(t, err, ErrSchemaTooNew)
			assert.Contains(t, err.Error(), p13dUnmarkedID)
			require.NotErrorIs(t, err, ErrNotV01Database, "a newer v0.1 database is not a Headscale database")
		})
	}
}

// Every migration from SchemaCompatMigrationID on declares its
// min_reader_id (ADR 0013 §3): a migration this binary already knows, at or
// before itself; the earlier, committed migrations declare none (they are
// contract for every binary that knows schema_compat).
func TestSchemaCompatDeclarations(t *testing.T) {
	ms := v01Migrations()
	pos := map[string]int{}

	for i, m := range ms {
		pos[m.ID] = i
	}

	first, ok := pos[SchemaCompatMigrationID]
	require.True(t, ok, "schema_compat migration is in the chain")

	for i, m := range ms {
		reader, declared := migrationCompat[m.ID]
		if i < first {
			assert.False(t, declared, "%s is committed before schema_compat: no declaration", m.ID)

			continue
		}

		require.True(t, declared, "%s must declare expand/contract in migrationCompat", m.ID)

		at, known := pos[reader]
		require.True(t, known, "%s: min_reader %s is not in the chain", m.ID, reader)
		assert.LessOrEqual(t, at, i, "%s: min_reader %s is later than the migration", m.ID, reader)
		assert.GreaterOrEqual(t, at, first, "%s: min_reader %s does not know schema_compat", m.ID, reader)
	}

	assert.Len(t, migrationCompat, len(ms)-first, "no declaration for a migration outside the chain")
	assert.Equal(t, SchemaCompatMigrationID, migrationCompat[SchemaCompatMigrationID],
		"schema_compat is contract: binaries before it refuse every unknown migration")

	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), SchemaCompatSQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), SchemaCompatPostgresDDL)
}

// TestSchemaCompatMigrationUpgrades: a database from before schema_compat
// (rewound with the migration's own Rollback) gets the table and the rows of
// every declared migration when migrated, keeps its data, and opens.
func TestSchemaCompatMigrationUpgrades(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			cfg := bk.cfg(t, "", "")
			p := openPlatform(t, cfg)
			id := platformTailnet(t, p, "local", types.TailnetActive, false)

			// Rewind every migration from schema_compat on, newest first.
			ms := v01Migrations()
			require.Contains(t, currentChain().known(), SchemaCompatMigrationID, "schema_compat migration is in the chain")

			for i := len(ms) - 1; i >= 0; i-- {
				require.NoError(t, ms[i].Rollback(p.db), ms[i].ID)
				require.NoError(t, p.db.Exec(`DELETE FROM migrations WHERE id = ?`, ms[i].ID).Error)

				if ms[i].ID == SchemaCompatMigrationID {
					break
				}
			}

			present, err := tableExists(p.db, "schema_compat")
			require.NoError(t, err)
			require.False(t, present, "rolled back")
			require.NoError(t, p.Close())

			res, err := Migrate(context.Background(), cfg)
			require.NoError(t, err)
			require.NotEmpty(t, res.Applied)
			assert.Equal(t, SchemaCompatMigrationID, res.Applied[0])

			p = openPlatform(t, cfg)

			var rows []struct{ MigrationID, MinReaderID string }
			require.NoError(t, p.db.Raw(`SELECT migration_id, min_reader_id FROM schema_compat ORDER BY migration_id`).
				Scan(&rows).Error)
			require.Len(t, rows, len(migrationCompat))

			for _, r := range rows {
				assert.Equal(t, migrationCompat[r.MigrationID], r.MinReaderID, r.MigrationID)
			}

			_, err = p.TenantDB(id)
			require.NoError(t, err, "data kept")
		})
	}
}

func closeGorm(conn *gorm.DB) {
	sqlDB, err := conn.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}
