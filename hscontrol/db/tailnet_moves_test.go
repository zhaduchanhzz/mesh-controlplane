package db

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
)

func TestTailnetMovesDDLInSchemaFilesAndInChain(t *testing.T) {
	norm := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	assert.Contains(t, norm(dbSchema), TailnetMovesSQLiteDDL)
	assert.Contains(t, norm(dbSchemaPostgres), TailnetMovesPostgresDDL)

	// P12c A8 appended 202611261200-tenant-id-immutable after it: committed
	// migrations keep their place, new ones go after them.
	chain := v01Migrations()

	ids := make([]string, 0, len(chain))
	for _, m := range chain {
		ids = append(ids, m.ID)
	}

	assert.Contains(t, ids, TailnetMovesMigrationID, "in the v0.1 chain")
	assert.Less(t, slices.Index(ids, TailnetMovesMigrationID), slices.Index(ids, TenantIDImmutableMigrationID),
		"appended before the P12c A8 migration")
}

func insertMove(tx *gorm.DB, id, tailnetID, state string) error {
	now := time.Now().UTC()

	return tx.Exec(`INSERT INTO tailnet_moves (id, tailnet_id, from_cluster_id, to_cluster_id, from_epoch,
		scheduled_at, state, created_at, updated_at) VALUES (?, ?, 'c1', 'c2', 1, ?, ?, ?, ?)`,
		id, tailnetID, now, state, now, now).Error
}

// P12b Q2: a fresh database has tailnet_moves; one from before the migration
// is upgraded on open and keeps its rows; at most one open move per tailnet;
// a move names an existing tailnet.
func TestMigrationTailnetMoves(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			cfg := b.cfg(t, "", "")

			p := openPlatform(t, cfg)
			require.True(t, p.db.Migrator().HasTable("tailnet_moves"), "fresh database")

			id := platformTailnet(t, p, "local", types.TailnetActive, false)

			require.NoError(t, tailnetMovesMigration().Rollback(p.db))
			require.NoError(t, p.db.Exec(`DELETE FROM migrations WHERE id = ?`, TailnetMovesMigrationID).Error)
			require.False(t, p.db.Migrator().HasTable("tailnet_moves"))
			require.NoError(t, p.Close())

			p = openPlatform(t, cfg)
			require.True(t, p.db.Migrator().HasTable("tailnet_moves"), "upgraded on open")

			var n int64
			require.NoError(t, p.db.Raw(`SELECT count(*) FROM tailnets WHERE id = ?`, id.String()).Scan(&n).Error)
			assert.EqualValues(t, 1, n, "rows survive")

			require.NoError(t, insertMove(p.db, "m1", id.String(), "SCHEDULED"))
			require.Error(t, insertMove(p.db, "m2", id.String(), "REQUESTED"), "a second open move")
			require.NoError(t, insertMove(p.db, "m3", id.String(), "CANCELLED"), "closed moves do not count")
			require.Error(t, insertMove(p.db, "m4", id.String(), "LOST"), "unknown state")

			if b.name == "postgres" {
				require.Error(t, insertMove(p.db, "m5", "00000000-0000-4000-8000-00000000dead", "CANCELLED"), "unknown tailnet")
			}
		})
	}
}

// MoveNotices: only open moves of ACTIVE tailnets this cluster owns and
// moves away; never another cluster's, a closed move, or a deleted tailnet.
func TestMoveNoticesOnlyOwnOpenMoves(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

			ins := func(id string, tn tailnet.ID, from, state string) {
				now := time.Now().UTC()
				require.NoError(t, p.db.Exec(`INSERT INTO tailnet_moves (id, tailnet_id, from_cluster_id, to_cluster_id,
					from_epoch, scheduled_at, reason, state, created_at, updated_at)
					VALUES (?, ?, ?, 'cx', 1, ?, 'rebalance', ?, ?, ?)`, id, tn.String(), from, at, state, now, now).Error)
			}

			mine := platformTailnet(t, p, "local", types.TailnetActive, false)
			requested := platformTailnet(t, p, "local", types.TailnetActive, false)
			closed := platformTailnet(t, p, "local", types.TailnetActive, false)
			other := platformTailnet(t, p, "other", types.TailnetActive, false)
			gone := platformTailnet(t, p, "local", types.TailnetActive, true)

			ins("m1", mine, "local", "SCHEDULED")
			ins("m2", requested, "local", "REQUESTED")
			ins("m3", closed, "local", "CANCELLED")
			ins("m4", other, "other", "SCHEDULED")
			ins("m5", gone, "local", "SCHEDULED")

			got, err := p.MoveNotices("local")
			require.NoError(t, err)
			require.Len(t, got, 2)
			assert.True(t, got[mine.String()].At.Equal(at))
			assert.Equal(t, "rebalance", got[mine.String()].Reason)
			assert.Contains(t, got, requested.String())

			_, err = p.MoveNotices("")
			require.Error(t, err)
		})
	}
}
