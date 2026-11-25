package placement

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
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
	Code      FindingCode `json:"code" enum:"R1_OWNER_UNREGISTERED,R2_OWNER_DISABLED,R3_OWNER_DRAINING,R4_OWNER_NO_LIVE_LEASE,R5_CLUSTER_ACTIVE_NO_LIVE_LEASE,R6_CLUSTER_NO_NOISE_KEY,R7_CLUSTER_OVER_CAPACITY,R8_DEDICATED_MULTIPLE_TAILNETS,R9_TAILNET_PENDING_TOO_LONG" doc:"anomaly; see evidence/source-inventory-P12.md §3.2"`
	ClusterID string      `json:"cluster_id,omitempty" doc:"cluster concerned (owner of the tailnet for R1-R4, R9)"`
	TailnetID string      `json:"tailnet_id,omitempty" doc:"tailnet concerned (R1-R4, R9)"`
	Count     int64       `json:"count,omitempty" doc:"R7 devices, R8 live tailnets, R9 seconds PENDING"`
	Limit     int64       `json:"limit,omitempty" doc:"R7 max_devices, R8 1, R9 seconds allowed"`
}

// Report is the cross-cluster reconcile report.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Findings    []Finding `json:"findings"`
}

// Reconcile reads the registry, the cluster leases (by the database clock)
// and the device counts, and reports R1–R9 ordered by code, cluster and
// tailnet. A read only: on PostgreSQL one READ ONLY, REPEATABLE READ
// transaction, so every finding comes from the same snapshot; nothing is
// moved, changed or audited (Q4).
func (r *Registry) Reconcile(ctx context.Context) (Report, error) {
	var (
		cs    []Cluster
		tns   []types.Tailnet
		usage map[string]usageRow
		live  map[string]bool
	)

	read := func(tx *gorm.DB) error {
		err := tx.Order("id").Find(&cs).Error
		if err != nil {
			return fmt.Errorf("listing clusters: %w", err)
		}

		err = tx.Select("id", "owner_cluster_id", "status", "created_at").
			Where("deleted_at IS NULL").Order("id").Find(&tns).Error
		if err != nil {
			return fmt.Errorf("listing tailnets: %w", err)
		}

		usage, err = clusterUsage(tx)
		if err != nil {
			return err
		}

		live, err = liveLeases(tx)

		return err
	}

	db := r.db.WithContext(ctx)

	var opts []*sql.TxOptions
	if isPostgres(db) {
		opts = append(opts, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	}

	err := db.Transaction(read, opts...)
	if err != nil {
		return Report{}, fmt.Errorf("reconcile: %w", err)
	}

	now := r.now()
	out := []Finding{}
	byID := make(map[string]Cluster, len(cs))

	for _, c := range cs {
		byID[c.ID] = c
	}

	for _, t := range tns {
		owner := t.OwnerClusterID

		c, registered := byID[owner]

		switch {
		case !registered:
			out = append(out, Finding{Code: FindingOwnerUnregistered, ClusterID: owner, TailnetID: t.ID})
		case c.State == ClusterDisabled:
			out = append(out, Finding{Code: FindingOwnerDisabled, ClusterID: owner, TailnetID: t.ID})
		case c.State == ClusterDraining:
			out = append(out, Finding{Code: FindingOwnerDraining, ClusterID: owner, TailnetID: t.ID})
		}

		if t.Status == types.TailnetActive && !live[owner] {
			out = append(out, Finding{Code: FindingOwnerNoLease, ClusterID: owner, TailnetID: t.ID})
		}

		if age := now.Sub(t.CreatedAt); t.Status == types.TailnetPending && age > r.cfg.PendingMaxAge {
			out = append(out, Finding{
				Code: FindingPendingTooLong, ClusterID: owner, TailnetID: t.ID,
				Count: int64(age / time.Second), Limit: int64(r.cfg.PendingMaxAge / time.Second),
			})
		}
	}

	for _, c := range cs {
		u := usage[c.ID]

		if c.State == ClusterActive && !live[c.ID] {
			out = append(out, Finding{Code: FindingClusterNoLease, ClusterID: c.ID})
		}

		if live[c.ID] && c.NoisePublicKey == "" {
			out = append(out, Finding{Code: FindingClusterNoNoiseKey, ClusterID: c.ID})
		}

		if u.Devices > c.MaxDevices {
			out = append(out, Finding{Code: FindingClusterOverCapacity, ClusterID: c.ID, Count: u.Devices, Limit: c.MaxDevices})
		}

		if c.Class == ClassDedicated && u.Tailnets > 1 {
			out = append(out, Finding{Code: FindingDedicatedShared, ClusterID: c.ID, Count: u.Tailnets, Limit: 1})
		}
	}

	slices.SortFunc(out, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.ClusterID, b.ClusterID), cmp.Compare(a.TailnetID, b.TailnetID))
	})

	return Report{GeneratedAt: now, Findings: out}, nil
}
