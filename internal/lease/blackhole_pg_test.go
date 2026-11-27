package lease

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// blackholeProxy forwards TCP to a PostgreSQL server until blackholed; then
// every byte in either direction is dropped, on the connections already
// open and on new ones, with no reset: what a client sees when nft drops
// a PgBouncer's port or its VM is stopped hard (P13 drills K3, F05.3).
type blackholeProxy struct {
	ln       net.Listener
	upstream func() (net.Conn, error)
	dropped  atomic.Bool
	accepted atomic.Int64

	mu    sync.Mutex
	conns []net.Conn
}

func newBlackholeProxy(t *testing.T, upstream func() (net.Conn, error)) *blackholeProxy {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	p := &blackholeProxy{ln: ln, upstream: upstream}
	t.Cleanup(p.close)

	go p.serve()

	return p
}

func (p *blackholeProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port } //nolint:forcetypeassert // a TCP listener

func (p *blackholeProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

func (p *blackholeProxy) close() {
	_ = p.ln.Close()

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range p.conns {
		_ = c.Close()
	}
}

func (p *blackholeProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}

		p.accepted.Add(1)
		p.track(c)

		if p.dropped.Load() {
			go func() { _, _ = io.Copy(io.Discard, c) }() // never answers

			continue
		}

		u, err := p.upstream()
		if err != nil {
			_ = c.Close()

			continue
		}

		p.track(u)

		go p.pipe(u, c)
		go p.pipe(c, u)
	}
}

// pipe copies src to dst, dropping everything once blackholed.
func (p *blackholeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)

	for {
		n, err := src.Read(buf)
		if n > 0 && !p.dropped.Load() {
			_, werr := dst.Write(buf[:n])
			if werr != nil {
				return
			}
		}

		if err != nil {
			return
		}
	}
}

// pgKV parses a key=value DSN (MESH_TEST_POSTGRES_DSN).
func pgKV(dsn string) map[string]string {
	kv := map[string]string{}

	for f := range strings.FieldsSeq(dsn) {
		k, v, ok := strings.Cut(f, "=")
		if ok {
			kv[k] = v
		}
	}

	return kv
}

// TestRenewSkipsBlackholedPgBouncer (PostgreSQL, real pgx): the lease pool
// lists two servers (one PgBouncer per PostgreSQL node, ADR 0011 §D); the
// first, which every connection uses, starts dropping packets without a
// reset while the process holds the lease with the default timing. The hung
// renewal is abandoned after its attempt timeout, its connection discarded,
// and the retry opens a fresh connection that gives up on the dead host
// after the lease pool's connect_timeout (1 s) and renews through the second.
// No self-fence, no new epoch (D12: killing a PgBouncer causes 0 failovers).
func TestRenewSkipsBlackholedPgBouncer(t *testing.T) {
	dsn := os.Getenv("MESH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MESH_TEST_POSTGRES_DSN not set")
	}

	if testing.Short() {
		t.Skip("runs for about 25 s")
	}

	kv := pgKV(dsn)

	var dialer net.Dialer

	upstream := func() (net.Conn, error) {
		host, port := kv["host"], kv["port"]
		if port == "" {
			port = "5432"
		}

		if strings.HasPrefix(host, "/") {
			return dialer.DialContext(t.Context(), "unix", filepath.Join(host, ".s.PGSQL."+port))
		}

		return dialer.DialContext(t.Context(), "tcp", net.JoinHostPort(host, port))
	}

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}

	sch := fmt.Sprintf("lease_bh_%d", time.Now().UnixNano())

	err = admin.Exec("CREATE SCHEMA " + sch).Error
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + sch + " CASCADE").Error

		d, err := admin.DB()
		if err == nil {
			_ = d.Close()
		}
	})

	err = admin.Exec("SET search_path = " + sch + "; " + PostgresDDL).Error
	if err != nil {
		t.Fatalf("ddl: %v", err)
	}

	first := newBlackholeProxy(t, upstream)
	second := newBlackholeProxy(t, upstream)

	// The lease pool as hscontrol/db opens it: host list, read-write,
	// connect_timeout 1 (leaseDatabase), 2 connections.
	pdsn := fmt.Sprintf("host=127.0.0.1,127.0.0.1 port=%d,%d user=%s dbname=%s sslmode=disable "+
		"connect_timeout=1 target_session_attrs=read-write search_path=%s",
		first.port(), second.port(), kv["user"], kv["dbname"], sch)

	db, err := gorm.Open(postgres.Open(pdsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}

	sqlDB.SetMaxOpenConns(2)
	sqlDB.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = sqlDB.Close() })

	s := NewStore(db)

	m, err := newManager(s, "c1", "a", DefaultConfig)
	if err != nil {
		t.Fatal(err)
	}

	lost := runHeld(t, m)

	time.Sleep(DefaultConfig.RenewInterval + 500*time.Millisecond) //nolint:forbidigo // one renewal through the first host

	if second.accepted.Load() != 0 {
		t.Fatalf("the second host was used before the first failed (%d connections)", second.accepted.Load())
	}

	first.dropped.Store(true)

	dropAt := time.Now()

	select {
	case r := <-lost:
		t.Fatalf("lease ended %s after the first PgBouncer was blackholed: %s",
			time.Since(dropAt).Round(time.Millisecond), r)
	case <-time.After(2 * DefaultConfig.TTL):
	}

	l, ok := m.Current()
	if !ok || l.Epoch != 1 {
		t.Fatalf("Current %+v ok=%v, want epoch 1 still held", l, ok)
	}

	if second.accepted.Load() == 0 {
		t.Fatal("no connection reached the second host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var rows []struct {
		Holder string
		Epoch  int64
		Left   float64
	}

	err = admin.WithContext(ctx).Raw("SELECT holder, epoch, EXTRACT(EPOCH FROM expires_at - clock_timestamp()) AS left FROM " +
		sch + ".cluster_leases WHERE cluster_id = 'c1'").Scan(&rows).Error
	if err != nil || len(rows) != 1 {
		t.Fatalf("reading the lease row: %v (%d rows)", err, len(rows))
	}

	if rows[0].Holder != "a" || rows[0].Epoch != 1 || rows[0].Left <= 0 {
		t.Fatalf("lease row %+v: want holder a, epoch 1, live", rows[0])
	}

	t.Logf("blackholed the first PgBouncer for %s: lease kept at epoch 1, %d connection(s) to the second, row live for %.1fs more",
		time.Since(dropAt).Round(time.Second), second.accepted.Load(), rows[0].Left)
}
