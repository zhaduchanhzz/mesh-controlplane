package placement

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// EnrollmentAllowed decides whether one more device may enroll into tailnet
// id, which the caller took from a verified scope (AGENTS rule 6). It fails
// closed: the result is allowed only when the error is nil.
//
//   - shared cluster: hard limit Config.SharedTenantLimit (5000) per tailnet;
//     other tailnets on the cluster do not count;
//   - shared cluster, also: the cluster's max_devices over all its tailnets
//     (P10d) -> ErrClusterFull joined with ErrQuotaExceeded;
//   - dedicated cluster: the cluster's max_devices;
//   - Quota.Warn at Config.WarnPercent (80%) of the limit, counting the
//     enrolling device;
//   - unknown/deleted tailnet → ErrNotFound; tailnet not ACTIVE →
//     ErrTailnetNotActive; owner cluster not registered → ErrUnknownCluster;
//     limit reached → ErrQuotaExceeded.
//
// Outside a transaction, two concurrent enrollments at limit-1 may both pass;
// call EnrollmentAllowedTx inside the transaction that inserts the node for
// an exact limit.
func (r *Registry) EnrollmentAllowed(ctx context.Context, id tailnet.ID) (Quota, error) {
	var (
		q   Quota
		err error
	)

	txErr := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q, err = r.EnrollmentAllowedTx(tx, id)

		return nil // a refusal is an answer, not a reason to roll back
	})
	if txErr != nil {
		return Quota{}, txErr
	}

	return q, err
}

// EnrollmentAllowedTx is EnrollmentAllowed inside the caller's transaction
// tx (the registration transaction). On PostgreSQL it first takes a
// transaction-scoped advisory lock on the tailnet (allowed with PgBouncer in
// transaction mode, PLAN §2), so concurrent enrollments into one tailnet
// serialise until the caller commits and the limit is exact. Other tailnets
// are never blocked.
func (r *Registry) EnrollmentAllowedTx(tx *gorm.DB, id tailnet.ID) (Quota, error) {
	if id.IsZero() {
		return Quota{}, fmt.Errorf("%w: empty tailnet id", ErrNotFound)
	}

	tid := id.String()

	if isPostgres(tx) {
		err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('placement/quota/' || ?, 0))`, tid).Error
		if err != nil {
			return Quota{}, fmt.Errorf("locking quota: %w", err)
		}
	}

	tn, err := loadTailnet(tx, tid)
	if err != nil {
		return Quota{}, err
	}

	q := Quota{TailnetID: tid, ClusterID: tn.OwnerClusterID}

	if tn.Status != types.TailnetActive {
		return q, fmt.Errorf("%w: %s", ErrTailnetNotActive, tn.Status)
	}

	c, err := loadCluster(tx, tn.OwnerClusterID)
	if errors.Is(err, ErrNotFound) {
		return q, fmt.Errorf("%w: %q", ErrUnknownCluster, tn.OwnerClusterID)
	}

	if err != nil {
		return q, err
	}

	q.Class = c.Class

	switch c.Class {
	case ClassShared:
		q.Limit = r.cfg.SharedTenantLimit
	case ClassDedicated:
		q.Limit = c.MaxDevices
	default:
		return q, fmt.Errorf("%w: class %q", ErrUnknownCluster, c.Class)
	}

	q.Devices, err = deviceCount(tx, tid)
	if err != nil {
		return q, err
	}

	q.Warn = (q.Devices+1)*100 >= q.Limit*r.cfg.WarnPercent

	if q.Devices >= q.Limit {
		return q, fmt.Errorf("%w: %d of %d devices", ErrQuotaExceeded, q.Devices, q.Limit)
	}

	if c.Class == ClassShared {
		err = clusterBudget(tx, c, r.cfg.WarnPercent, &q)
		if err != nil {
			return q, err
		}
	}

	q.Allowed = true

	return q, nil
}

// clusterBudget checks the shared cluster c against its max_devices (P10d):
// the tailnet quota alone lets the tenants of one cluster together grow past
// what the cluster was sized for, since max_devices was otherwise only
// checked when a tailnet is placed. On PostgreSQL a transaction-scoped
// advisory lock on the cluster, taken after the tailnet's, serialises the
// enrollments of the cluster's tailnets until the caller commits, so the
// limit is exact. Lock order is always tailnet, then cluster.
func clusterBudget(tx *gorm.DB, c Cluster, warnPercent int64, q *Quota) error {
	if isPostgres(tx) {
		err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('placement/cluster/' || ?, 0))`, c.ID).Error
		if err != nil {
			return fmt.Errorf("locking cluster budget: %w", err)
		}
	}

	err := tx.Raw(`SELECT count(n.id) FROM nodes n JOIN tailnets t ON t.id = n.tailnet_id
		WHERE t.owner_cluster_id = ? AND t.deleted_at IS NULL AND n.deleted_at IS NULL`, c.ID).
		Scan(&q.ClusterDevices).Error
	if err != nil {
		return fmt.Errorf("counting cluster devices: %w", err)
	}

	q.ClusterLimit = c.MaxDevices
	q.ClusterWarn = (q.ClusterDevices+1)*100 >= q.ClusterLimit*warnPercent

	if q.ClusterDevices >= q.ClusterLimit {
		q.Allowed = false

		return fmt.Errorf("%w: %w: cluster %s has %d of %d devices",
			ErrQuotaExceeded, ErrClusterFull, c.ID, q.ClusterDevices, q.ClusterLimit)
	}

	return nil
}
