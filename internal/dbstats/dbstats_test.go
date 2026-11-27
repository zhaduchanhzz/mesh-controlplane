package dbstats

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/metricstest"
	_ "modernc.org/sqlite" // the driver the platform uses for SQLite
)

func TestPoolStatsExportedAndSummedByName(t *testing.T) {
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = db.Close() })
		db.SetMaxOpenConns(3)

		return db
	}

	a, b := open(), open()
	ua := Register("dbstats-test", a)
	ub := Register("dbstats-test", b)

	conn, err := a.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	pool := map[string]string{"pool": "dbstats-test"}

	if v, _ := metricstest.Value(t, "headscale_db_pool_max_open_connections", pool); v != 6 {
		t.Errorf("max_open summed over two pools = %v, want 6", v)
	}

	if v, _ := metricstest.Value(t, "headscale_db_pool_in_use_connections", pool); v != 1 {
		t.Errorf("in_use = %v, want 1", v)
	}

	_ = conn.Close()

	ua()
	ub()
	ub() // idempotent

	if _, ok := metricstest.Value(t, "headscale_db_pool_open_connections", pool); ok {
		t.Error("unregistered pools are still exported")
	}
}
