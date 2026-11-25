// Package placement is the registry of the shared database (P10, PLAN §4
// "Registry"): which clusters exist, which cluster owns each tailnet at which
// epoch, where a new tailnet goes, and how many devices a tailnet may enroll.
//
// It works only on platform tables (clusters, tailnets, platform_audit,
// platform_idempotency). It touches tenant tables in exactly two ways, both
// raw statements reviewed by hand (tenantscope does not parse raw SQL):
//
//   - the deny-all policy row of a tailnet being created, in the same
//     transaction as the tailnet row (AGENTS rule 8);
//   - device counts (count of live nodes per tailnet or per owning cluster).
//     Counting is the only cross-tenant read; it returns numbers, never rows.
//
// Every mutation runs in one transaction together with its audit row and, when
// the caller gives an idempotency key, the stored result of the call: a retry
// with the same key and request returns that result without a second effect.
package placement

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"
)

// Class is the shard class of a cluster (ADR 0003).
type Class string

const (
	// ClassShared hosts many tailnets, each limited to Config.SharedTenantLimit devices.
	ClassShared Class = "shared"
	// ClassDedicated hosts one tailnet, limited to the cluster's MaxDevices.
	ClassDedicated Class = "dedicated"
)

// Valid reports whether c is a known class.
func (c Class) Valid() bool { return c == ClassShared || c == ClassDedicated }

// ClusterState is the operator-set state of a cluster.
type ClusterState string

const (
	// ClusterActive clusters receive new tailnets.
	ClusterActive ClusterState = "ACTIVE"
	// ClusterDraining clusters keep their tailnets and receive no new ones.
	ClusterDraining ClusterState = "DRAINING"
	// ClusterDisabled clusters receive nothing; a new cluster starts so.
	ClusterDisabled ClusterState = "DISABLED"
)

// Valid reports whether s is a known state.
func (s ClusterState) Valid() bool {
	return s == ClusterActive || s == ClusterDraining || s == ClusterDisabled
}

// Cluster is a row of the clusters table. Revision grows by one on every
// change and never decreases (a database trigger refuses it).
type Cluster struct {
	ID             string       `gorm:"primaryKey" json:"id"`
	EndpointURL    string       `gorm:"column:endpoint_url;not null" json:"endpoint_url"`
	Class          Class        `gorm:"not null" json:"class"`
	State          ClusterState `gorm:"not null" json:"state"`
	NoisePublicKey string       `gorm:"column:noise_public_key" json:"noise_public_key,omitempty"`
	MaxDevices     int64        `gorm:"column:max_devices;not null" json:"max_devices"`
	Revision       uint64       `gorm:"not null" json:"revision"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// TableName pins the table name.
func (Cluster) TableName() string { return "clusters" }

// ClusterUsage is a cluster with the number of live tailnets it owns and
// their live devices.
type ClusterUsage struct {
	Cluster

	Tailnets int64 `json:"tailnets"`
	Devices  int64 `json:"devices"`
}

// ClusterSpec describes a cluster to register. An empty State registers it
// DISABLED, so it receives no tailnet until an operator activates it.
type ClusterSpec struct {
	ID             string
	EndpointURL    string
	Class          Class
	State          ClusterState
	NoisePublicKey string
	MaxDevices     int64
}

// ClusterPatch changes the descriptive fields of a cluster; nil fields stay.
type ClusterPatch struct {
	EndpointURL    *string
	NoisePublicKey *string
	MaxDevices     *int64
}

// Op identifies who makes a mutation and how to deduplicate it. Actor is
// required (the verified operator identity, recorded in the audit). With an
// IdempotencyKey, RequestHash must fingerprint the whole request: the same
// key with another request is refused with ErrIdempotencyConflict.
type Op struct {
	Actor          string
	IdempotencyKey string
	RequestHash    string
}

// Quota is the enrollment decision for one tailnet.
type Quota struct {
	TailnetID string `json:"tailnet_id"`
	ClusterID string `json:"cluster_id"`
	Class     Class  `json:"class"`
	// Devices is the number of live devices now.
	Devices int64 `json:"devices"`
	// Limit is the hard limit of the tailnet.
	Limit int64 `json:"limit"`
	// Allowed reports whether one more device may enroll.
	Allowed bool `json:"allowed"`
	// Warn reports that the tailnet is at or above the warning threshold
	// (Config.WarnPercent of Limit, counting the enrolling device).
	Warn bool `json:"warn"`
	// ClusterDevices and ClusterLimit are the live devices of every
	// tailnet on the owner cluster and its max_devices (shared clusters,
	// P10d); zero on a dedicated cluster, whose tailnet limit is the
	// cluster's.
	ClusterDevices int64 `json:"cluster_devices,omitempty"`
	ClusterLimit   int64 `json:"cluster_limit,omitempty"`
	// ClusterWarn reports that the shared cluster is at or above
	// Config.WarnPercent of max_devices, counting the enrolling device.
	ClusterWarn bool `json:"cluster_warn,omitempty"`
}

// DefaultSharedTenantLimit is the hard device limit of a tailnet on a shared
// cluster (PLAN §1.6, ADR 0003).
const DefaultSharedTenantLimit = 5000

// DefaultWarnPercent is the share of the limit at which Quota.Warn is set.
const DefaultWarnPercent = 80

// Config tunes the registry. Zero values take the defaults.
type Config struct {
	SharedTenantLimit int64
	WarnPercent       int64
	// PendingMaxAge is how long a tailnet may stay PENDING before the
	// reconcile report lists it (R9); zero takes DefaultPendingMaxAge.
	PendingMaxAge time.Duration
	// MoveMaxAge is how long a requested move may stay pending before the
	// reconcile report lists it (R10, ADR 0010); zero takes
	// DefaultMoveMaxAge.
	MoveMaxAge time.Duration
}

var (
	// ErrNotFound reports an unknown or deleted cluster or tailnet.
	ErrNotFound = errors.New("not found")
	// ErrInvalid reports a malformed request.
	ErrInvalid = errors.New("invalid request")
	// ErrStaleEpoch reports a compare-and-swap whose expected owner or epoch
	// is no longer current: nothing changed.
	ErrStaleEpoch = errors.New("stale owner or epoch")
	// ErrStaleRevision reports an UpdateClusterState whose expected revision
	// is no longer current: nothing changed.
	ErrStaleRevision = errors.New("stale cluster revision")
	// ErrNoCapacity reports that no ACTIVE cluster of the class can take a
	// tailnet.
	ErrNoCapacity = errors.New("no cluster with capacity")
	// ErrNoLiveLease reports a named cluster (CreateTailnetOn, MoveOwner to
	// another cluster) with no process holding its lease: nothing would
	// serve the tailnet. Always returned joined with ErrNoCapacity (P12,
	// owner decision 2026-11-25).
	ErrNoLiveLease = errors.New("no process holds the cluster lease")
	// ErrClusterFull reports that the shared cluster owning the tailnet has
	// reached its max_devices (P10d). It is returned joined with
	// ErrQuotaExceeded, so callers that refuse on a quota refuse on it too.
	ErrClusterFull = errors.New("cluster has reached its device limit")
	// ErrInvalidTransition reports a status change the tailnet's current
	// status does not allow.
	ErrInvalidTransition = errors.New("invalid status transition")
	// ErrClusterInUse reports deleting a cluster that tailnets reference.
	ErrClusterInUse = errors.New("cluster owns tailnets")
	// ErrConflict reports a uniqueness conflict (slug, cluster id, endpoint).
	ErrConflict = errors.New("already exists")
	// ErrIdempotencyConflict reports an idempotency key reused for another
	// request.
	ErrIdempotencyConflict = errors.New("idempotency key reused for another request")
	// ErrQuotaExceeded reports that a tailnet reached its device limit:
	// enrollment must be refused.
	ErrQuotaExceeded = errors.New("device quota exceeded")
	// ErrTailnetNotActive reports enrollment into a tailnet that is not
	// ACTIVE: refused.
	ErrTailnetNotActive = errors.New("tailnet is not active")
	// ErrUnknownCluster reports a tailnet whose owner cluster is not
	// registered: its class and limit are unknown, so enrollment is refused.
	ErrUnknownCluster = errors.New("owner cluster is not registered")
)

// Registry is the placement service over the shared database.
type Registry struct {
	db  *gorm.DB
	cfg Config
	now func() time.Time

	// failAt, when set (tests only), is called at each named step of a
	// mutation; an error aborts the transaction there.
	failAt func(step string) error
}

// New returns a registry on db, the platform pool (db.Platform.RegistryDB)
// whose schema the v0.1 migrations created.
func New(db *gorm.DB, cfg Config) (*Registry, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil database", ErrInvalid)
	}

	if cfg.SharedTenantLimit == 0 {
		cfg.SharedTenantLimit = DefaultSharedTenantLimit
	}

	if cfg.WarnPercent == 0 {
		cfg.WarnPercent = DefaultWarnPercent
	}

	if cfg.PendingMaxAge == 0 {
		cfg.PendingMaxAge = DefaultPendingMaxAge
	}

	if cfg.MoveMaxAge == 0 {
		cfg.MoveMaxAge = DefaultMoveMaxAge
	}

	if cfg.PendingMaxAge < 0 || cfg.MoveMaxAge < 0 {
		return nil, fmt.Errorf("%w: pending max age %s, move max age %s", ErrInvalid, cfg.PendingMaxAge, cfg.MoveMaxAge)
	}

	if cfg.SharedTenantLimit < 0 || cfg.WarnPercent < 0 || cfg.WarnPercent > 100 {
		return nil, fmt.Errorf("%w: shared limit %d, warn %d%%", ErrInvalid, cfg.SharedTenantLimit, cfg.WarnPercent)
	}

	return &Registry{db: db, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Config returns the effective configuration.
func (r *Registry) Config() Config { return r.cfg }

var (
	slugRE      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	clusterIDRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)
)

func (r *Registry) step(name string) error {
	if r.failAt == nil {
		return nil
	}

	return r.failAt(name)
}

func isPostgres(db *gorm.DB) bool { return db.Name() == "postgres" }
