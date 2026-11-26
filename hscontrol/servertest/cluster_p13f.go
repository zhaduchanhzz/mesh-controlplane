//go:build p05cluster

package servertest

import (
	"testing"
	"time"
)

// StoreRawPolicyForTest writes raw as the newest policy row of slug without
// validation, as a hand edit of the database would (P13f L9). With bump the
// policy revision is bumped too, so the running cluster reloads it; without
// it the cluster only sees the row if it reloads anyway.
func (tc *TestCluster) StoreRawPolicyForTest(tb testing.TB, slug, raw string, bump bool) {
	tb.Helper()

	id := tc.Tailnet(tb, slug).ID.String()
	now := time.Now().UTC()

	err := tc.raw.Exec(`INSERT INTO policies (tailnet_id, data, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, raw, now, now).Error
	if err != nil {
		tb.Fatalf("servertest: raw policy write: %v", err)
	}

	if !bump {
		return
	}

	err = tc.raw.Exec(`UPDATE tailnets SET policy_revision = policy_revision + 1 WHERE id = ?`, id).Error
	if err != nil {
		tb.Fatalf("servertest: bump policy revision: %v", err)
	}
}
