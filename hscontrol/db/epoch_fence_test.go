package db

import (
	"context"
	"regexp"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

// P13c (ADR 0012): epochs never go backwards after a restore.

// simulateRestore applies fn to conn the way a restore changes rows: the
// monotonic triggers do not run (a restore replaces the data wholesale). On
// PostgreSQL triggers are skipped for the transaction (session replication
// role, superuser test server); on SQLite the two epoch triggers are
// dropped and created again from the schema file.
func simulateRestore(t *testing.T, conn *gorm.DB, fn func(tx *gorm.DB) error) {
	t.Helper()

	if conn.Name() == dialectPostgres {
		require.NoError(t, conn.Transaction(func(tx *gorm.DB) error {
			err := tx.Exec(`SET LOCAL session_replication_role = replica`).Error
			if err != nil {
				return err
			}

			return fn(tx)
		}))

		return
	}

	triggers := []string{"trg_cluster_leases_epoch_monotonic", "trg_tailnets_owner_epoch_monotonic"}
	for _, trg := range triggers {
		require.NoError(t, conn.Exec(`DROP TRIGGER `+trg).Error)
	}

	require.NoError(t, fn(conn))

	for _, trg := range triggers {
		ddl := regexp.MustCompile(`(?s)CREATE TRIGGER ` + trg + `\b.*?\nEND;`).FindString(normalizeNewlines(dbSchema))
		require.NotEmpty(t, ddl, trg)
		require.NoError(t, conn.Exec(ddl).Error)
	}
}

func normalizeNewlines(s string) string {
	return regexp.MustCompile("\r\n").ReplaceAllString(s, "\n")
}

func leaseRow(t *testing.T, p *Platform, cluster string) lease.Row {
	t.Helper()

	row, ok, err := p.LeaseStore().Get(context.Background(), cluster)
	require.NoError(t, err)
	require.True(t, ok, "lease row of %s", cluster)

	return row
}

func ownerEpoch(t *testing.T, p *Platform, id tailnet.ID) uint64 {
	t.Helper()

	var e int64
	require.NoError(t, p.db.Raw(`SELECT owner_epoch FROM tailnets WHERE id = ?`, id.String()).Scan(&e).Error)

	return uint64(e) //nolint:gosec // CHECK(owner_epoch >= 1)
}

// ADR 0012 §2: a cluster_leases row is never deleted (DELETE + acquire
// would restart its epoch at 1), whatever SQL tries.
func TestClusterLeaseRowsCannotBeDeleted(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			p := openPlatform(t, bk.cfg(t, "", ""))
			ctx := context.Background()

			_, err := p.LeaseStore().Acquire(ctx, "c1", "h1", time.Minute)
			require.NoError(t, err)

			require.Error(t, p.db.Exec(`DELETE FROM cluster_leases WHERE cluster_id = 'c1'`).Error, "DELETE of a lease row")
			require.Error(t, p.db.Exec(`DELETE FROM cluster_leases`).Error, "DELETE of every lease row")

			if p.db.Name() == dialectPostgres {
				require.Error(t, p.db.Exec(`TRUNCATE cluster_leases`).Error, "TRUNCATE of the lease table")
			}

			assert.Equal(t, uint64(1), leaseRow(t, p, "c1").Epoch, "row kept")
		})
	}
}

// ADR 0012 §1: floor = Unix seconds × 2^20, below 2^53.
func TestEpochFloor(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)

	f, err := EpochFloor(at)
	require.NoError(t, err)
	assert.Equal(t, uint64(1_800_000_000)<<20, f)
	assert.Less(t, f, uint64(1)<<53, "fits a JSON/JS number")

	later, err := EpochFloor(at.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, uint64(1)<<20, later-f, "one second apart: 2^20 epochs apart")

	_, err = EpochFloor(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))
	require.Error(t, err, "past 2^53")

	_, err = EpochFloor(time.Unix(0, 0))
	require.Error(t, err)
}

type fenceWorld struct {
	p                *Platform
	cfg              *types.Config
	moved, plain     tailnet.ID
	scheduledMoveID  string
	requestedMoveID  string
	staleHolder      string
	staleLeaseEpoch  uint64
	staleOwnerEpochs map[tailnet.ID]uint64
}

// fenceSetup: cluster c1's lease taken over once (epoch 2) and c2's live; a
// tailnet with a move requested (moving_to set, REQUESTED row), another with
// a SCHEDULED move, owner epochs 1 and 7.
func fenceSetup(t *testing.T, cfg *types.Config) fenceWorld {
	t.Helper()

	p := openPlatform(t, cfg)
	ctx := context.Background()
	st := p.LeaseStore()

	_, err := st.Acquire(ctx, "c1", "h1", time.Millisecond)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // let the 1 ms lease expire by the database clock

	l, err := st.Acquire(ctx, "c1", "h2", time.Hour)
	require.NoError(t, err)
	require.Equal(t, uint64(2), l.Epoch)

	_, err = st.Acquire(ctx, "c2", "k1", time.Hour)
	require.NoError(t, err)

	moved := platformTailnet(t, p, "c1", types.TailnetActive, false)
	plain := platformTailnet(t, p, "c1", types.TailnetActive, false)
	require.NoError(t, p.db.Exec(`UPDATE tailnets SET owner_epoch = 7 WHERE id = ?`, plain.String()).Error)
	require.NoError(t, p.db.Exec(`UPDATE tailnets SET moving_to_cluster_id = 'c2' WHERE id = ?`, moved.String()).Error)

	now := time.Now().UTC()
	w := fenceWorld{
		p: p, cfg: cfg, moved: moved, plain: plain,
		scheduledMoveID: uuid.NewV4().String(), requestedMoveID: uuid.NewV4().String(),
		staleHolder: "h2", staleLeaseEpoch: 2,
		staleOwnerEpochs: map[tailnet.ID]uint64{moved: 1, plain: 7},
	}

	for _, m := range []struct {
		id    string
		tn    tailnet.ID
		state string
		epoch int
	}{{w.requestedMoveID, moved, "REQUESTED", 1}, {w.scheduledMoveID, plain, "SCHEDULED", 7}} {
		require.NoError(t, p.db.Exec(`INSERT INTO tailnet_moves (id, tailnet_id, from_cluster_id, to_cluster_id, from_epoch,
			scheduled_at, state, created_at, updated_at) VALUES (?, ?, 'c1', 'c2', ?, ?, ?, ?, ?)`,
			m.id, m.tn.String(), m.epoch, now, m.state, now, now).Error)
	}

	return w
}

// ADR 0012 §1: one fence raises every lease epoch and owner_epoch to at
// least the floor and above every epoch present, ends every lease (the old
// holder's renewal fails, its writes are fenced), cancels the pending moves
// and records itself; run again, it raises them again.
func TestEpochFenceRaisesEveryEpoch(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			w := fenceSetup(t, bk.cfg(t, "", "c1"))
			p, ctx := w.p, context.Background()

			// A runtime of c1 holding the lease and a tenant handle on
			// "plain" at epoch 7, as before a restore.
			p.SetLease(w.staleHolder, w.staleLeaseEpoch)
			tdb, err := p.TenantDB(w.plain)
			require.NoError(t, err)
			_, err = tdb.CreateUser(types.User{Name: "before-fence"})
			require.NoError(t, err, "the holder writes before the fence")

			start := time.Now()
			res, err := p.EpochFence(ctx, EpochFenceRequest{Reason: "test restore", Operator: "test", OperatorNow: start})
			require.NoError(t, err)

			floor, err := EpochFloor(start)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, res.Floor, floor)
			assert.Less(t, res.Floor, uint64(1)<<53)
			assert.EqualValues(t, 2, res.Leases)
			assert.EqualValues(t, 2, res.Tailnets)
			assert.EqualValues(t, 2, res.MovesCancelled)

			for _, c := range []string{"c1", "c2"} {
				row := leaseRow(t, p, c)
				assert.GreaterOrEqual(t, row.Epoch, res.Floor, "lease %s", c)
				assert.True(t, row.Expired, "lease %s ended by the fence", c)
				assert.Equal(t, res.Holder, row.Holder)
			}

			for id, before := range w.staleOwnerEpochs {
				assert.GreaterOrEqual(t, ownerEpoch(t, p, id), res.Floor)
				assert.Greater(t, ownerEpoch(t, p, id), before)
			}

			var moving int64
			require.NoError(t, p.db.Raw(`SELECT count(*) FROM tailnets WHERE moving_to_cluster_id IS NOT NULL`).Scan(&moving).Error)
			assert.Zero(t, moving, "requested move cleared")

			var states []string
			require.NoError(t, p.db.Raw(`SELECT state FROM tailnet_moves ORDER BY id`).Scan(&states).Error)
			assert.Equal(t, []string{"CANCELLED", "CANCELLED"}, states)

			var fences int64
			require.NoError(t, p.db.Raw(`SELECT count(*) FROM platform_epoch_fences WHERE epoch_floor = ? AND reason = ?`,
				int64(res.Floor), "test restore").Scan(&fences).Error) //nolint:gosec // < 2^53
			assert.EqualValues(t, 1, fences)

			// The holder from before the fence: renewal lost, writes fenced.
			err = p.LeaseStore().Renew(ctx, lease.Lease{Cluster: "c1", Holder: w.staleHolder, Epoch: w.staleLeaseEpoch}, time.Hour)
			require.ErrorIs(t, err, lease.ErrLost)

			_, err = tdb.CreateUser(types.User{Name: "after-fence"})
			require.ErrorIs(t, err, tenantscope.ErrFenced, "a write under the pre-fence owner epoch")

			// The next acquire is above the fence.
			l, err := p.LeaseStore().Acquire(ctx, "c1", "h3", time.Hour)
			require.NoError(t, err)
			assert.Greater(t, l.Epoch, res.Floor)

			// Run again: everything grows again, monotonic.
			again, err := p.EpochFence(ctx, EpochFenceRequest{Reason: "again", Operator: "test", OperatorNow: time.Now()})
			require.NoError(t, err)
			assert.Greater(t, again.Floor, l.Epoch)
			assert.Greater(t, leaseRow(t, p, "c1").Epoch, l.Epoch)

			// An external high-water mark above the clock floor wins.
			hwm := again.Floor + 1<<30
			third, err := p.EpochFence(ctx, EpochFenceRequest{Reason: "hwm", Operator: "test", OperatorNow: time.Now(), ExternalHWM: hwm})
			require.NoError(t, err)
			assert.Equal(t, hwm+1, third.Floor)
		})
	}
}

// ADR 0012 §Kiểm chứng: a restore takes the lease and owner epochs back
// below what processes saw; the fence puts every epoch above them again.
func TestEpochFenceAfterSimulatedRestore(t *testing.T) {
	for _, bk := range backends(t) {
		t.Run(bk.name, func(t *testing.T) {
			w := fenceSetup(t, bk.cfg(t, "", "c1"))
			p, ctx := w.p, context.Background()

			// Restore to before the takeover and the epoch bump.
			simulateRestore(t, p.db, func(tx *gorm.DB) error {
				err := tx.Exec(`UPDATE cluster_leases SET epoch = 1, holder = 'h1' WHERE cluster_id = 'c1'`).Error
				if err != nil {
					return err
				}

				return tx.Exec(`UPDATE tailnets SET owner_epoch = 1 WHERE id = ?`, w.plain.String()).Error
			})
			require.Equal(t, uint64(1), leaseRow(t, p, "c1").Epoch)

			res, err := p.EpochFence(ctx, EpochFenceRequest{Reason: "restore", Operator: "test", OperatorNow: time.Now()})
			require.NoError(t, err)

			assert.Greater(t, leaseRow(t, p, "c1").Epoch, w.staleLeaseEpoch)
			assert.Greater(t, ownerEpoch(t, p, w.plain), w.staleOwnerEpochs[w.plain])
			assert.GreaterOrEqual(t, ownerEpoch(t, p, w.plain), res.Floor)
		})
	}
}

// The fence migration is appended and declared expand: a binary of the
// schema_compat migration runs on a database that has it.
func TestEpochFenceMigrationDeclared(t *testing.T) {
	ms := v01Migrations()
	require.Equal(t, EpochFenceMigrationID, ms[len(ms)-1].ID, "appended at the end of the chain")
	assert.Equal(t, SchemaCompatMigrationID, migrationCompat[EpochFenceMigrationID], "expand")
}
