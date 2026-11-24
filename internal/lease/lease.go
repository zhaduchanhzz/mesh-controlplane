// Package lease is the cluster lease of P11b (ADR 0005, Bổ sung 2 §2): one
// row per cluster in the shared database names the process allowed to serve
// that cluster (active), until an expiry set by the database clock. A
// standby takes the row over once it has expired, with a higher epoch.
//
// Two layers keep two processes from serving one cluster at once:
//
//   - the database: every tenant write of a cluster process carries the
//     holder and epoch it holds (tailnet.Fence.WithLease); the write rolls
//     back unless the row still names them and has not expired by the
//     database's now() (hscontrol/db/tenantscope);
//   - the process: [Manager] stops serving at a deadline on the monotonic
//     clock, taken from the start of its last successful acquire or renew
//     plus the TTL minus a safety margin, so it stops before the row can
//     expire, even without reaching the database (paused, partitioned).
//
// Only the database clock decides expiry; process clocks are never compared
// with stored times.
package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrHeld reports that another holder's lease has not expired.
var ErrHeld = errors.New("cluster lease is held by another process")

// ErrLost reports that a lease is no longer ours: expired, or taken over.
var ErrLost = errors.New("cluster lease lost")

// MigrationID is the v0.1 migration that creates cluster_leases.
const MigrationID = "202611251000-cluster-leases"

// The DDL is byte-identical to the tail of hscontrol/db/schema.sql and
// schema_postgres.sql (TestClusterLeasesDDLInSchemaFiles).
//
// cluster_id has no foreign key to clusters, like tailnets.owner_cluster_id:
// a cluster that is not registered yet still elects one process.
const (
	SQLiteDDL = `CREATE TABLE cluster_leases(
  cluster_id text PRIMARY KEY,
  holder text NOT NULL,
  epoch integer NOT NULL CHECK(epoch >= 1),
  expires_at datetime NOT NULL,
  updated_at datetime NOT NULL
);
CREATE TRIGGER trg_cluster_leases_epoch_monotonic BEFORE UPDATE OF epoch, holder ON cluster_leases
WHEN NEW.epoch < OLD.epoch OR (NEW.holder <> OLD.holder AND NEW.epoch <= OLD.epoch)
BEGIN
  SELECT RAISE(ABORT, 'lease epoch must grow with a holder change and never decrease');
END;`

	PostgresDDL = `CREATE TABLE cluster_leases(
  cluster_id text PRIMARY KEY,
  holder text NOT NULL,
  epoch bigint NOT NULL CHECK(epoch >= 1),
  expires_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE FUNCTION cluster_leases_epoch_monotonic() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.epoch < OLD.epoch OR (NEW.holder <> OLD.holder AND NEW.epoch <= OLD.epoch) THEN
    RAISE EXCEPTION 'lease epoch must grow with a holder change and never decrease' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER trg_cluster_leases_epoch_monotonic BEFORE UPDATE OF epoch, holder ON cluster_leases
  FOR EACH ROW EXECUTE FUNCTION cluster_leases_epoch_monotonic();`
)

// SQLiteNow is the database clock on SQLite, as the text stored in
// expires_at (millisecond precision, comparable as strings).
const SQLiteNow = `strftime('%Y-%m-%d %H:%M:%f','now')`

// PostgresNow is the database clock on PostgreSQL (see Store.now).
const PostgresNow = `clock_timestamp()`

// Lease is a lease held by this process.
type Lease struct {
	Cluster string
	Holder  string
	Epoch   uint64
}

// Row is the stored lease of a cluster.
type Row struct {
	Holder  string
	Epoch   uint64
	Expired bool
}

// Store reads and writes cluster_leases. It never filters by tenant: the
// table belongs to the platform.
type Store struct {
	db       *gorm.DB
	postgres bool
}

// NewStore wraps db (SQLite or PostgreSQL).
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db, postgres: db.Dialector.Name() == "postgres"}
}

// nowPlus is the database expression for now() + d.
func (s *Store) nowPlus(d time.Duration) string {
	if s.postgres {
		return fmt.Sprintf("clock_timestamp() + interval '%d milliseconds'", d.Milliseconds())
	}

	return fmt.Sprintf(`strftime('%%Y-%%m-%%d %%H:%%M:%%f','now','+%.3f seconds')`, d.Seconds())
}

// now is the database clock. On PostgreSQL clock_timestamp(), not now():
// now() is the start of the transaction, which would let a long transaction
// see a lease as live after it expired.
func (s *Store) now() string {
	if s.postgres {
		return PostgresNow
	}

	return SQLiteNow
}

// Acquire takes the lease of cluster for holder with ttl: a first lease has
// epoch 1; an expired lease (whoever held it) is taken over at epoch+1. A
// lease that has not expired is never taken: [ErrHeld].
func (s *Store) Acquire(ctx context.Context, cluster, holder string, ttl time.Duration) (Lease, error) {
	q := `INSERT INTO cluster_leases(cluster_id, holder, epoch, expires_at, updated_at)
VALUES (?, ?, 1, ` + s.nowPlus(ttl) + `, ` + s.now() + `)
ON CONFLICT (cluster_id) DO UPDATE SET
  holder = excluded.holder,
  epoch = cluster_leases.epoch + 1,
  expires_at = excluded.expires_at,
  updated_at = excluded.updated_at
WHERE cluster_leases.expires_at <= ` + s.now() + `
RETURNING epoch`

	var epochs []int64

	err := s.db.WithContext(ctx).Raw(q, cluster, holder).Scan(&epochs).Error
	if err != nil {
		return Lease{}, fmt.Errorf("acquiring cluster lease %s: %w", cluster, err)
	}

	if len(epochs) == 0 {
		return Lease{}, ErrHeld
	}

	if epochs[0] < 1 {
		return Lease{}, fmt.Errorf("acquiring cluster lease %s: invalid epoch %d", cluster, epochs[0])
	}

	return Lease{Cluster: cluster, Holder: holder, Epoch: uint64(epochs[0])}, nil
}

// Renew extends l by ttl from the database's now(). It fails with [ErrLost]
// if l has expired or another holder took the lease.
func (s *Store) Renew(ctx context.Context, l Lease, ttl time.Duration) error {
	res := s.db.WithContext(ctx).Exec(`UPDATE cluster_leases SET expires_at = `+s.nowPlus(ttl)+`, updated_at = `+s.now()+`
WHERE cluster_id = ? AND holder = ? AND epoch = ? AND expires_at > `+s.now(),
		l.Cluster, l.Holder, int64(l.Epoch)) //nolint:gosec // epochs are positive int64 in the database
	if res.Error != nil {
		return fmt.Errorf("renewing cluster lease %s: %w", l.Cluster, res.Error)
	}

	if res.RowsAffected != 1 {
		return ErrLost
	}

	return nil
}

// Release ends l now, so a standby can take over without waiting for the
// TTL. Releasing a lease already lost is not an error.
func (s *Store) Release(ctx context.Context, l Lease) error {
	err := s.db.WithContext(ctx).Exec(`UPDATE cluster_leases SET expires_at = `+s.now()+`, updated_at = `+s.now()+`
WHERE cluster_id = ? AND holder = ? AND epoch = ? AND expires_at > `+s.now(),
		l.Cluster, l.Holder, int64(l.Epoch)).Error //nolint:gosec // see Renew
	if err != nil {
		return fmt.Errorf("releasing cluster lease %s: %w", l.Cluster, err)
	}

	return nil
}

// Get reads the stored lease of cluster; ok is false when there is none.
func (s *Store) Get(ctx context.Context, cluster string) (Row, bool, error) {
	var rows []struct {
		Holder  string
		Epoch   int64
		Expired bool
	}

	err := s.db.WithContext(ctx).Raw(`SELECT holder, epoch, expires_at <= `+s.now()+` AS expired
FROM cluster_leases WHERE cluster_id = ?`, cluster).Scan(&rows).Error
	if err != nil {
		return Row{}, false, fmt.Errorf("reading cluster lease %s: %w", cluster, err)
	}

	if len(rows) == 0 {
		return Row{}, false, nil
	}

	return Row{Holder: rows[0].Holder, Epoch: uint64(rows[0].Epoch), Expired: rows[0].Expired}, true, nil //nolint:gosec // CHECK(epoch >= 1)
}
