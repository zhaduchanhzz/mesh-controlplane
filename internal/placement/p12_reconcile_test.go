package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// setNoiseKey registers a Noise public key for cluster id, as a running
// cluster does from P12 step 4 (Q6).
func setNoiseKey(t *testing.T, gdb *gorm.DB, id string) {
	t.Helper()
	require.NoError(t, gdb.Exec(`UPDATE clusters SET noise_public_key = ? WHERE id = ?`, "mkey:"+id, id).Error)
}

// healthyCluster registers an ACTIVE cluster with a live lease and a Noise
// key: nothing to report about it.
func healthyCluster(t *testing.T, r *Registry, id string, class Class, maxDevices int64) {
	t.Helper()
	addCluster(t, r, id, class, ClusterActive, maxDevices)
	setNoiseKey(t, r.db, id)
}

// reconcileBase is a clean registry: cluster ok (shared, 100 devices), an
// ACTIVE tailnet with 2 devices and a fresh PENDING tailnet on it.
func reconcileBase(t *testing.T, gdb *gorm.DB) *Registry {
	t.Helper()

	r := newRegistry(t, gdb, Config{})
	healthyCluster(t, r, "ok", ClassShared, 100)

	tn := activeTailnet(t, r, "base", ClassShared)
	addNodes(t, gdb, tn.ID, 2)

	_, err := r.CreateTailnet(context.Background(), testOp, "fresh", ClassShared)
	require.NoError(t, err)

	return r
}

func reconcile(t *testing.T, r *Registry) []Finding {
	t.Helper()

	rep, err := r.Reconcile(context.Background())
	require.NoError(t, err)
	require.NotNil(t, rep.Findings, "an empty report is [], not null")

	return rep.Findings
}

// P12 step 2, owner decision Q4: a clean registry reports nothing.
func TestReconcileCleanIsEmpty(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := reconcileBase(t, gdb)
			healthyCluster(t, r, "ded", ClassDedicated, 10)
			activeTailnet(t, r, "solo", ClassDedicated)

			assert.Empty(t, reconcile(t, r))
		})
	}
}

// P12 step 2: each anomaly R1–R9 on top of a clean registry gives exactly
// its findings, with identifiers and counts only.
func TestReconcileFindsEachAnomaly(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *Registry) []Finding
	}{
		{"R1 owner unregistered", func(t *testing.T, r *Registry) []Finding {
			id := rawTailnet(t, r.db, "ghost", types.TailnetActive)
			setLease(t, r.db, "ghost", true) // its process runs: only R1

			return []Finding{{Code: FindingOwnerUnregistered, ClusterID: "ghost", TailnetID: id}}
		}},
		{"R2 owner disabled", func(t *testing.T, r *Registry) []Finding {
			addCluster(t, r, "off", ClassShared, ClusterDisabled, 100)
			setNoiseKey(t, r.db, "off")
			id := rawTailnet(t, r.db, "off", types.TailnetActive)

			return []Finding{{Code: FindingOwnerDisabled, ClusterID: "off", TailnetID: id}}
		}},
		{"R3 owner draining", func(t *testing.T, r *Registry) []Finding {
			addCluster(t, r, "drain", ClassShared, ClusterDraining, 100)
			setNoiseKey(t, r.db, "drain")
			id := rawTailnet(t, r.db, "drain", types.TailnetSuspended)

			return []Finding{{Code: FindingOwnerDraining, ClusterID: "drain", TailnetID: id}}
		}},
		{"R4 owner without live lease (with R5)", func(t *testing.T, r *Registry) []Finding {
			healthyCluster(t, r, "dead", ClassShared, 100)
			id := rawTailnet(t, r.db, "dead", types.TailnetActive)
			setLease(t, r.db, "dead", false)

			return []Finding{
				{Code: FindingOwnerNoLease, ClusterID: "dead", TailnetID: id},
				{Code: FindingClusterNoLease, ClusterID: "dead"},
			}
		}},
		{"R5 active cluster never started", func(t *testing.T, r *Registry) []Finding {
			healthyCluster(t, r, "new", ClassShared, 100)
			require.NoError(t, r.db.Exec(`DELETE FROM cluster_leases WHERE cluster_id = 'new'`).Error)

			return []Finding{{Code: FindingClusterNoLease, ClusterID: "new"}}
		}},
		{"R6 running cluster without Noise key", func(t *testing.T, r *Registry) []Finding {
			addCluster(t, r, "nokey", ClassShared, ClusterActive, 100)

			return []Finding{{Code: FindingClusterNoNoiseKey, ClusterID: "nokey"}}
		}},
		{"R7 cluster over capacity", func(t *testing.T, r *Registry) []Finding {
			healthyCluster(t, r, "full", ClassShared, 3)
			addNodes(t, r.db, rawTailnet(t, r.db, "full", types.TailnetActive), 4)

			return []Finding{{Code: FindingClusterOverCapacity, ClusterID: "full", Count: 4, Limit: 3}}
		}},
		{"R8 dedicated with two tailnets", func(t *testing.T, r *Registry) []Finding {
			healthyCluster(t, r, "ded", ClassDedicated, 100)
			rawTailnet(t, r.db, "ded", types.TailnetActive)
			rawTailnet(t, r.db, "ded", types.TailnetPending)

			return []Finding{{Code: FindingDedicatedShared, ClusterID: "ded", Count: 2, Limit: 1}}
		}},
		{"R9 pending too long", func(t *testing.T, r *Registry) []Finding {
			id := rawTailnet(t, r.db, "ok", types.TailnetPending)
			old := time.Now().UTC().Add(-25 * time.Hour)
			require.NoError(t, r.db.Exec(`UPDATE tailnets SET created_at = ? WHERE id = ?`, old, id).Error)

			// Count is the age in seconds: 25 h, give or take the test run.
			return []Finding{{Code: FindingPendingTooLong, ClusterID: "ok", TailnetID: id, Count: -1, Limit: int64(DefaultPendingMaxAge / time.Second)}}
		}},
	}

	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					r := reconcileBase(t, b.open(t))
					want := tc.setup(t, r)

					got := reconcile(t, r)
					require.Len(t, got, len(want), "%+v", got)

					for i := range want {
						if want[i].Count == -1 {
							age := got[i].Count
							assert.InDelta(t, 25*3600, age, 600, "age in seconds")
							got[i].Count = -1
						}
					}

					assert.ElementsMatch(t, want, got)
				})
			}
		})
	}
}

// P12 step 2: the report is ordered (code, cluster, tailnet) so two reads
// of the same registry compare equal, and a deleted tailnet is not live.
func TestReconcileOrderedAndIgnoresDeleted(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			gdb := b.open(t)
			r := reconcileBase(t, gdb)

			b2 := rawTailnet(t, gdb, "zz-ghost", types.TailnetActive)
			a2 := rawTailnet(t, gdb, "aa-ghost", types.TailnetActive)
			gone := rawTailnet(t, gdb, "aa-ghost", types.TailnetActive)
			require.NoError(t, gdb.Exec(`UPDATE tailnets SET deleted_at = ? WHERE id = ?`, time.Now().UTC(), gone).Error)
			setLease(t, gdb, "zz-ghost", true)
			setLease(t, gdb, "aa-ghost", true)

			got := reconcile(t, r)
			assert.Equal(t, []Finding{
				{Code: FindingOwnerUnregistered, ClusterID: "aa-ghost", TailnetID: a2},
				{Code: FindingOwnerUnregistered, ClusterID: "zz-ghost", TailnetID: b2},
			}, got)
			assert.Equal(t, got, reconcile(t, r))
		})
	}
}
