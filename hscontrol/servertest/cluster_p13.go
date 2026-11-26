//go:build p05cluster

package servertest

import (
	"path/filepath"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
)

// P13 helpers.

// NewClusterOn builds, without starting it, a process of tc's cluster on
// the SQLite database at path (its own Noise key), and returns NewCluster's
// error: what `headscale cluster serve` does at start on that database.
func (tc *TestCluster) NewClusterOn(tb testing.TB, path string) error {
	tb.Helper()

	cfg := *tc.cfg
	cfg.Database.Type = types.DatabaseSqlite
	cfg.Database.Sqlite.Path = path
	cfg.Noise.PrivateKeyPath = filepath.Join(tb.TempDir(), "noise_private.key")

	_, err := hscontrol.NewCluster(&cfg)

	return err
}

// ownerEpochTrigger is trg_tailnets_owner_epoch_monotonic of schema.sql.
const ownerEpochTrigger = `CREATE TRIGGER trg_tailnets_owner_epoch_monotonic BEFORE UPDATE OF owner_epoch, owner_cluster_id ON tailnets
WHEN NEW.owner_epoch < OLD.owner_epoch OR (NEW.owner_cluster_id <> OLD.owner_cluster_id AND NEW.owner_epoch <= OLD.owner_epoch)
BEGIN
  SELECT RAISE(ABORT, 'owner_epoch must grow with an owner change and never decrease');
END;`

// RestoreOwnerEpochForTest sets slug's owner_epoch the way a database
// restore does (ADR 0012): it may go down, the monotonic trigger does not
// run (dropped and created again in the same transaction).
func (tc *TestCluster) RestoreOwnerEpochForTest(tb testing.TB, slug string, epoch uint64) {
	tb.Helper()

	err := tc.raw.Transaction(func(tx *gorm.DB) error {
		err := tx.Exec(`DROP TRIGGER trg_tailnets_owner_epoch_monotonic`).Error
		if err != nil {
			return err
		}

		err = tx.Exec(`UPDATE tailnets SET owner_epoch = ? WHERE id = ?`, int64(epoch), tc.Tailnet(tb, slug).ID.String()).Error //nolint:gosec // test epochs < 2^63
		if err != nil {
			return err
		}

		return tx.Exec(ownerEpochTrigger).Error
	})
	if err != nil {
		tb.Fatalf("servertest: restore owner_epoch of %q: %v", slug, err)
	}
}
