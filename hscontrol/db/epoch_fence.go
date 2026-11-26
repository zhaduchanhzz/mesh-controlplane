package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"gorm.io/gorm"
)

// EpochFenceMigrationID (P13c, ADR 0012 §1-2) adds platform_epoch_fences,
// the record of every epoch fence, and a trigger that refuses DELETE (and,
// on PostgreSQL, TRUNCATE) of cluster_leases: a deleted row would be
// re-created at epoch 1. Expand (ADR 0013): no binary deletes lease rows,
// and the new table is only written by the fence.
const EpochFenceMigrationID = "202611271100-epoch-fence"

// EpochFenceSQLiteDDL and EpochFencePostgresDDL are appended verbatim to
// schema.sql / schema_postgres.sql.
const (
	EpochFenceSQLiteDDL = `CREATE TABLE platform_epoch_fences(
  id integer PRIMARY KEY AUTOINCREMENT,
  epoch_floor integer NOT NULL CHECK(epoch_floor >= 1),
  fenced_at datetime NOT NULL,
  reason text NOT NULL,
  operator text NOT NULL,
  leases integer NOT NULL,
  tailnets integer NOT NULL,
  moves_cancelled integer NOT NULL
);
CREATE TRIGGER trg_cluster_leases_no_delete BEFORE DELETE ON cluster_leases
BEGIN
  SELECT RAISE(ABORT, 'cluster_leases rows are never deleted: the epoch would restart at 1 (ADR 0012)');
END;
`

	EpochFencePostgresDDL = `CREATE TABLE platform_epoch_fences(
  id bigserial PRIMARY KEY,
  epoch_floor bigint NOT NULL CHECK(epoch_floor >= 1),
  fenced_at timestamptz NOT NULL,
  reason text NOT NULL,
  operator text NOT NULL,
  leases bigint NOT NULL,
  tailnets bigint NOT NULL,
  moves_cancelled bigint NOT NULL
);
CREATE FUNCTION cluster_leases_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'cluster_leases rows are never deleted: the epoch would restart at 1 (ADR 0012)' USING ERRCODE = 'check_violation';
END;
$$;
CREATE TRIGGER trg_cluster_leases_no_delete BEFORE DELETE ON cluster_leases
  FOR EACH ROW EXECUTE FUNCTION cluster_leases_no_delete();
CREATE TRIGGER trg_cluster_leases_no_truncate BEFORE TRUNCATE ON cluster_leases
  FOR EACH STATEMENT EXECUTE FUNCTION cluster_leases_no_delete();
`
)

const (
	epochFenceSQLiteRollback = `DROP TRIGGER IF EXISTS trg_cluster_leases_no_delete;
DROP TABLE IF EXISTS platform_epoch_fences;
`
	epochFencePostgresRollback = `DROP TRIGGER IF EXISTS trg_cluster_leases_no_truncate ON cluster_leases;
DROP TRIGGER IF EXISTS trg_cluster_leases_no_delete ON cluster_leases;
DROP FUNCTION IF EXISTS cluster_leases_no_delete();
DROP TABLE IF EXISTS platform_epoch_fences;
`
)

func epochFenceMigration() *gormigrate.Migration {
	return &gormigrate.Migration{
		ID: EpochFenceMigrationID,
		Migrate: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := EpochFenceSQLiteDDL
				if tx.Name() == dialectPostgres {
					ddl = EpochFencePostgresDDL
				}

				return tx.Exec(ddl).Error
			})
		},
		Rollback: func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				ddl := epochFenceSQLiteRollback
				if tx.Name() == dialectPostgres {
					ddl = epochFencePostgresRollback
				}

				return tx.Exec(ddl).Error
			})
		},
	}
}

// Epoch floor (ADR 0012 §1): an epoch fence at Unix second s raises every
// epoch to at least s × 2^20. Epochs issued after a fence at t0 are
// floor(t0) + k, k the number of takeovers and moves since; a later fence
// at t1 starts (t1 - t0) × 2^20 higher, above k unless more than 2^20 took
// place per second. Floors stay below 2^53 (JSON/JS numbers) until 2242.
const (
	epochFloorShift = 20
	maxSafeEpoch    = 1<<53 - 1
)

var (
	// ErrEpochFloor reports a time outside the range of the epoch floor.
	ErrEpochFloor = errors.New("time outside the epoch floor range (after 1970, before 2242)")

	errFenceRequest = errors.New("epoch fence: reason and operator are required")
)

// EpochFloor is the epoch floor of a fence at t.
func EpochFloor(t time.Time) (uint64, error) {
	s := t.Unix()
	if s <= 0 || uint64(s) > maxSafeEpoch>>epochFloorShift {
		return 0, fmt.Errorf("%w: %s", ErrEpochFloor, t.UTC().Format(time.RFC3339))
	}

	return uint64(s) << epochFloorShift, nil
}

// EpochFenceRequest is one run of the epoch fence.
type EpochFenceRequest struct {
	// Reason and Operator are recorded in platform_epoch_fences.
	Reason   string
	Operator string
	// OperatorNow is the operator machine's clock: the floor uses the later
	// of it and the database clock, so one clock set back cannot lower it.
	OperatorNow time.Time
	// ExternalHWM is the highest epoch recorded outside the database (for
	// example before the restore); 0 when there is none.
	ExternalHWM uint64
}

// EpochFenceResult says what the fence did.
type EpochFenceResult struct {
	// Floor is the fence's floor F: every lease epoch and owner_epoch is
	// now max(its value + 1, F), and F is above every epoch present before.
	Floor uint64 `json:"floor"`
	// Holder is the holder name the lease rows now carry (expired).
	Holder         string    `json:"holder"`
	Leases         int64     `json:"leases"`
	Tailnets       int64     `json:"tailnets"`
	MovesCancelled int64     `json:"moves_cancelled"`
	FencedAt       time.Time `json:"fenced_at"`
}

// The fence's statements; each dialect's form is chosen by the caller.
const (
	fenceLockSQL        = `SELECT pg_advisory_xact_lock(hashtextextended('mesh/epoch-fence', 0))`
	fenceLockTimeoutSQL = `SELECT set_config('lock_timeout', ?, true)`
	fenceStatePostgres  = `SELECT floor(extract(epoch FROM clock_timestamp()))::bigint AS now_s,
  (SELECT COALESCE(max(epoch), 0) FROM cluster_leases) AS max_lease,
  (SELECT COALESCE(max(owner_epoch), 0) FROM tailnets) AS max_owner`
	fenceStateSQLite = `SELECT CAST(strftime('%s', 'now') AS integer) AS now_s,
  (SELECT COALESCE(max(epoch), 0) FROM cluster_leases) AS max_lease,
  (SELECT COALESCE(max(owner_epoch), 0) FROM tailnets) AS max_owner`
	fenceTailnetsPostgres = `UPDATE tailnets SET owner_epoch = GREATEST(owner_epoch + 1, ?),
  moving_to_cluster_id = NULL, updated_at = ?`
	fenceTailnetsSQLite = `UPDATE tailnets SET owner_epoch = max(owner_epoch + 1, ?),
  moving_to_cluster_id = NULL, updated_at = ?`
	fenceMovesSQL = `UPDATE tailnet_moves SET state = 'CANCELLED', error = ?, finished_at = ?, updated_at = ?
  WHERE state IN ('SCHEDULED', 'REQUESTED')`
	fenceLeaseClustersSQL = `SELECT cluster_id FROM cluster_leases ORDER BY cluster_id`
	fenceLeasesPostgres   = `UPDATE cluster_leases SET epoch = GREATEST(epoch + 1, ?), holder = ?, expires_at = ` +
		lease.PostgresNow + `, updated_at = ` + lease.PostgresNow
	fenceLeasesSQLite = `UPDATE cluster_leases SET epoch = max(epoch + 1, ?), holder = ?, expires_at = ` +
		lease.SQLiteNow + `, updated_at = ` + lease.SQLiteNow
	fenceRecordSQL = `INSERT INTO platform_epoch_fences (epoch_floor, fenced_at, reason, operator, leases, tailnets,
  moves_cancelled) VALUES (?, ?, ?, ?, ?, ?, ?)`

	// epochFenceLockTimeout bounds each lock wait of the fence.
	epochFenceLockTimeout = "30s"
)

// EpochFence is the restore fence of ADR 0012 §1, run by the operator after
// a restore or a promote that may have lost commits, before the cluster
// processes reach the database again. In one transaction it computes
//
//	F = max(floor(max(database clock, OperatorNow)), every epoch + 1, ExternalHWM + 1)
//
// and sets every tailnets.owner_epoch to max(owner_epoch + 1, F) (clearing
// requested moves: they carry the old epoch), cancels every SCHEDULED or
// REQUESTED tailnet_moves row, sets every cluster_leases row to epoch
// max(epoch + 1, F) under holder "fence:<unix time>", expired (the holder
// before it loses its renewal; any process then takes the lease above F),
// and records the fence. Epochs only grow: running it again raises them
// again. The monotonic triggers allow every change.
//
// Locks follow the order of a fenced write (tailnets rows, then the cluster
// lease locks) so the fence and the writes in flight never deadlock; each
// wait is bounded by epochFenceLockTimeout. On PostgreSQL a fixed advisory
// lock serialises fences.
func (p *Platform) EpochFence(ctx context.Context, req EpochFenceRequest) (EpochFenceResult, error) {
	if req.Reason == "" || req.Operator == "" {
		return EpochFenceResult{}, errFenceRequest
	}

	pg := p.db.Name() == dialectPostgres

	stateSQL := fenceStateSQLite
	tailnetsSQL := fenceTailnetsSQLite
	leasesSQL := fenceLeasesSQLite

	if pg {
		stateSQL = fenceStatePostgres
		tailnetsSQL = fenceTailnetsPostgres
		leasesSQL = fenceLeasesPostgres
	}

	res := EpochFenceResult{FencedAt: time.Now().UTC()}
	res.Holder = "fence:" + strconv.FormatInt(res.FencedAt.Unix(), 10)

	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if pg {
			err := tx.Exec(fenceLockTimeoutSQL, epochFenceLockTimeout).Error
			if err != nil {
				return fmt.Errorf("setting lock_timeout: %w", err)
			}

			err = tx.Exec(fenceLockSQL).Error
			if err != nil {
				return fmt.Errorf("waiting for another epoch fence: %w", err)
			}
		}

		var st struct {
			NowS     int64
			MaxLease int64
			MaxOwner int64
		}

		err := tx.Raw(stateSQL).Scan(&st).Error
		if err != nil {
			return fmt.Errorf("reading the database clock and epochs: %w", err)
		}

		floor, err := fenceFloor(st.NowS, req, st.MaxLease, st.MaxOwner)
		if err != nil {
			return err
		}

		res.Floor = floor
		f := int64(floor) //nolint:gosec // floor <= maxSafeEpoch < 2^63

		upd := tx.Exec(tailnetsSQL, f, res.FencedAt)
		if upd.Error != nil {
			return fmt.Errorf("raising owner_epoch: %w", upd.Error)
		}

		res.Tailnets = upd.RowsAffected

		upd = tx.Exec(fenceMovesSQL, "cancelled by epoch fence "+res.Holder+": "+req.Reason, res.FencedAt, res.FencedAt)
		if upd.Error != nil {
			return fmt.Errorf("cancelling pending moves: %w", upd.Error)
		}

		res.MovesCancelled = upd.RowsAffected

		if pg {
			var clusters []string

			err = tx.Raw(fenceLeaseClustersSQL).Scan(&clusters).Error
			if err != nil {
				return fmt.Errorf("listing cluster leases: %w", err)
			}

			// As a takeover does: wait for the fenced writes in flight.
			for _, c := range clusters {
				err = tx.Exec(lease.PostgresTakeoverLock, c).Error
				if err != nil {
					return fmt.Errorf("locking the lease of cluster %s: %w", c, err)
				}
			}
		}

		upd = tx.Exec(leasesSQL, f, res.Holder)
		if upd.Error != nil {
			return fmt.Errorf("raising lease epochs: %w", upd.Error)
		}

		res.Leases = upd.RowsAffected

		err = tx.Exec(fenceRecordSQL, f, res.FencedAt, req.Reason, req.Operator,
			res.Leases, res.Tailnets, res.MovesCancelled).Error
		if err != nil {
			return fmt.Errorf("recording the fence: %w", err)
		}

		return nil
	})
	if err != nil {
		return EpochFenceResult{}, fmt.Errorf("epoch fence: %w", err)
	}

	return res, nil
}

// fenceFloor is F of EpochFence: the clock floor of the later of the
// database's and the operator's clock, above every epoch present and above
// the external high-water mark.
func fenceFloor(dbNowS int64, req EpochFenceRequest, maxLease, maxOwner int64) (uint64, error) {
	at := time.Unix(dbNowS, 0)
	if req.OperatorNow.After(at) {
		at = req.OperatorNow
	}

	f, err := EpochFloor(at)
	if err != nil {
		return 0, err
	}

	for _, e := range []int64{maxLease, maxOwner} {
		if e >= 0 && uint64(e)+1 > f {
			f = uint64(e) + 1
		}
	}

	if req.ExternalHWM+1 > f {
		f = req.ExternalHWM + 1
	}

	if f > maxSafeEpoch {
		return 0, fmt.Errorf("%w: floor %d is above 2^53", ErrEpochFloor, f)
	}

	return f, nil
}
