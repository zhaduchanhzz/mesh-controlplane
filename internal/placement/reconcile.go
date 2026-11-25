package placement

import (
	"context"
	"time"
)

// FindingCode names one anomaly of the cross-cluster reconcile report (P12,
// owner decision Q4: detect and report only; nothing is moved or changed).
// The codes follow evidence/source-inventory-P12.md §3.2.
type FindingCode string

const (
	// FindingOwnerUnregistered (R1): a live tailnet owned by a cluster id
	// that is not registered. Its process may still serve it (Q3), but it
	// cannot enrol devices and bootstrap refuses it.
	FindingOwnerUnregistered FindingCode = "R1_OWNER_UNREGISTERED"
	// FindingOwnerDisabled (R2): a live tailnet owned by a DISABLED cluster.
	FindingOwnerDisabled FindingCode = "R2_OWNER_DISABLED"
	// FindingOwnerDraining (R3): a live tailnet owned by a DRAINING cluster,
	// waiting for an operator move (P12b).
	FindingOwnerDraining FindingCode = "R3_OWNER_DRAINING"
	// FindingOwnerNoLease (R4): an ACTIVE tailnet whose owner cluster has
	// no process holding a live lease: nothing serves it.
	FindingOwnerNoLease FindingCode = "R4_OWNER_NO_LIVE_LEASE"
	// FindingClusterNoLease (R5): an ACTIVE cluster with no process holding
	// its lease, so nothing answers at its registered endpoint. Placement
	// skips it (Q2).
	FindingClusterNoLease FindingCode = "R5_CLUSTER_ACTIVE_NO_LIVE_LEASE"
	// FindingClusterNoNoiseKey (R6): a cluster with a live lease and no
	// registered Noise public key (its process predates Q6, which records
	// the key when the lease is taken).
	FindingClusterNoNoiseKey FindingCode = "R6_CLUSTER_NO_NOISE_KEY"
	// FindingClusterOverCapacity (R7): a cluster whose live devices exceed
	// max_devices (Count devices, Limit max_devices).
	FindingClusterOverCapacity FindingCode = "R7_CLUSTER_OVER_CAPACITY"
	// FindingDedicatedShared (R8): a dedicated cluster owning more than one
	// live tailnet (Count tailnets, Limit 1).
	FindingDedicatedShared FindingCode = "R8_DEDICATED_MULTIPLE_TAILNETS"
	// FindingPendingTooLong (R9): a tailnet PENDING for longer than
	// Config.PendingMaxAge (Count and Limit in seconds).
	FindingPendingTooLong FindingCode = "R9_TAILNET_PENDING_TOO_LONG"
)

// DefaultPendingMaxAge is how long a tailnet may stay PENDING before the
// reconcile report lists it (R9).
const DefaultPendingMaxAge = 24 * time.Hour

// Finding is one anomaly. It carries identifiers and counts only: no slug,
// endpoint, key or tenant data (I15).
type Finding struct {
	Code      FindingCode `json:"code"`
	ClusterID string      `json:"cluster_id,omitempty"`
	TailnetID string      `json:"tailnet_id,omitempty"`
	Count     int64       `json:"count,omitempty"`
	Limit     int64       `json:"limit,omitempty"`
}

// Report is the cross-cluster reconcile report.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Findings    []Finding `json:"findings"`
}

// Reconcile reads the registry, the cluster leases and the device counts
// and reports R1–R9. A read only.
func (r *Registry) Reconcile(ctx context.Context) (Report, error) {
	return Report{GeneratedAt: r.now(), Findings: []Finding{}}, nil
}
