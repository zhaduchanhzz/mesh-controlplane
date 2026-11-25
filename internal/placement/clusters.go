package placement

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func validateEndpoint(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("%w: endpoint_url must be an https URL", ErrInvalid)
	}

	return nil
}

func (s ClusterSpec) validate() error {
	if !clusterIDRE.MatchString(s.ID) {
		return fmt.Errorf("%w: cluster id %q", ErrInvalid, s.ID)
	}

	err := validateEndpoint(s.EndpointURL)
	if err != nil {
		return err
	}

	if !s.Class.Valid() {
		return fmt.Errorf("%w: class %q", ErrInvalid, s.Class)
	}

	if s.State != "" && !s.State.Valid() {
		return fmt.Errorf("%w: state %q", ErrInvalid, s.State)
	}

	if s.MaxDevices <= 0 {
		return fmt.Errorf("%w: max_devices must be positive", ErrInvalid)
	}

	return nil
}

// CreateCluster registers a cluster (DISABLED unless spec.State says
// otherwise).
func (r *Registry) CreateCluster(ctx context.Context, op Op, spec ClusterSpec) (Cluster, error) {
	err := spec.validate()
	if err != nil {
		return Cluster{}, err
	}

	if spec.State == "" {
		spec.State = ClusterDisabled
	}

	return mutate(ctx, r, op, "CreateCluster", spec.ID, spec, func(tx *gorm.DB) (Cluster, string, error) {
		now := r.now()
		c := Cluster{
			ID: spec.ID, EndpointURL: spec.EndpointURL, Class: spec.Class, State: spec.State,
			NoisePublicKey: spec.NoisePublicKey, MaxDevices: spec.MaxDevices, Revision: 1,
			CreatedAt: now, UpdatedAt: now,
		}

		err := tx.Create(&c).Error
		if isDuplicate(err) {
			return Cluster{}, "", fmt.Errorf("%w: cluster %q or its endpoint", ErrConflict, spec.ID)
		}

		if err != nil {
			return Cluster{}, "", fmt.Errorf("creating cluster: %w", err)
		}

		return c, "", nil
	})
}

// UpdateCluster changes the descriptive fields of cluster id; its revision
// grows by one.
func (r *Registry) UpdateCluster(ctx context.Context, op Op, id string, p ClusterPatch) (Cluster, error) {
	set := map[string]any{}

	if p.EndpointURL != nil {
		err := validateEndpoint(*p.EndpointURL)
		if err != nil {
			return Cluster{}, err
		}

		set["endpoint_url"] = *p.EndpointURL
	}

	if p.NoisePublicKey != nil {
		set["noise_public_key"] = *p.NoisePublicKey
	}

	if p.MaxDevices != nil {
		if *p.MaxDevices <= 0 {
			return Cluster{}, fmt.Errorf("%w: max_devices must be positive", ErrInvalid)
		}

		set["max_devices"] = *p.MaxDevices
	}

	if len(set) == 0 {
		return Cluster{}, fmt.Errorf("%w: nothing to update", ErrInvalid)
	}

	return mutate(ctx, r, op, "UpdateCluster", id, set, func(tx *gorm.DB) (Cluster, string, error) {
		set["revision"] = gorm.Expr("revision + 1")
		set["updated_at"] = r.now()

		res := tx.Model(&Cluster{}).Where("id = ?", id).Updates(set)
		if isDuplicate(res.Error) {
			return Cluster{}, "", fmt.Errorf("%w: endpoint", ErrConflict)
		}

		if res.Error != nil {
			return Cluster{}, "", fmt.Errorf("updating cluster: %w", res.Error)
		}

		if res.RowsAffected != 1 {
			return Cluster{}, "", fmt.Errorf("%w: cluster %q", ErrNotFound, id)
		}

		c, err := loadCluster(tx, id)

		return c, "", err
	})
}

// UpdateClusterState sets the state of cluster id. With expectedRevision >
// 0 it is a compare-and-swap on the revision. The revision grows by one; it
// never touches tailnets (owner_epoch is changed only by MoveOwner).
func (r *Registry) UpdateClusterState(ctx context.Context, op Op, id string, state ClusterState, expectedRevision uint64) (Cluster, error) {
	if !state.Valid() {
		return Cluster{}, fmt.Errorf("%w: state %q", ErrInvalid, state)
	}

	detail := map[string]any{"state": state, "expected_revision": expectedRevision}

	return mutate(ctx, r, op, "UpdateClusterState", id, detail, func(tx *gorm.DB) (Cluster, string, error) {
		q := tx.Model(&Cluster{}).Where("id = ?", id)
		if expectedRevision > 0 {
			q = q.Where("revision = ?", expectedRevision)
		}

		res := q.Updates(map[string]any{
			"state": state, "revision": gorm.Expr("revision + 1"), "updated_at": r.now(),
		})
		if res.Error != nil {
			return Cluster{}, "", fmt.Errorf("updating cluster state: %w", res.Error)
		}

		if res.RowsAffected != 1 {
			_, err := loadCluster(tx, id)
			if err != nil {
				return Cluster{}, "", err
			}

			return Cluster{}, "", fmt.Errorf("%w: cluster %q", ErrStaleRevision, id)
		}

		c, err := loadCluster(tx, id)

		return c, "", err
	})
}

// DeleteCluster removes cluster id. A cluster that any tailnet row (deleted
// ones included) names as owner is never removed.
func (r *Registry) DeleteCluster(ctx context.Context, op Op, id string) error {
	_, err := mutate(ctx, r, op, "DeleteCluster", id, nil, func(tx *gorm.DB) (struct{}, string, error) {
		// Lock the row first: a concurrent CreateTailnet/MoveOwner holds it
		// while it adds a tailnet, and the count below (a new statement,
		// so a new snapshot under READ COMMITTED) sees that tailnet.
		_, err := lockClusters(tx, "UPDATE", "id = ?", id)
		if err != nil {
			return struct{}{}, "", err
		}

		var n int64

		// Owner, or target of a pending move (ADR 0010).
		err = tx.Raw(`SELECT count(*) FROM tailnets WHERE owner_cluster_id = ? OR moving_to_cluster_id = ?`, id, id).Scan(&n).Error
		if err != nil {
			return struct{}{}, "", fmt.Errorf("counting tailnets: %w", err)
		}

		if n > 0 {
			return struct{}{}, "", fmt.Errorf("%w: %d tailnets", ErrClusterInUse, n)
		}

		res := tx.Where("id = ?", id).Delete(&Cluster{})
		if res.Error != nil {
			return struct{}{}, "", fmt.Errorf("deleting cluster: %w", res.Error)
		}

		if res.RowsAffected != 1 {
			return struct{}{}, "", fmt.Errorf("%w: cluster %q", ErrNotFound, id)
		}

		return struct{}{}, "", nil
	})

	return err
}

// GetCluster returns cluster id with its usage.
func (r *Registry) GetCluster(ctx context.Context, id string) (ClusterUsage, error) {
	db := r.db.WithContext(ctx)

	c, err := loadCluster(db, id)
	if err != nil {
		return ClusterUsage{}, err
	}

	usage, err := clusterUsage(db)
	if err != nil {
		return ClusterUsage{}, err
	}

	u := usage[id]

	return ClusterUsage{Cluster: c, Tailnets: u.Tailnets, Devices: u.Devices}, nil
}

// ListClusters returns every cluster with its usage, by id.
func (r *Registry) ListClusters(ctx context.Context) ([]ClusterUsage, error) {
	db := r.db.WithContext(ctx)

	var cs []Cluster

	err := db.Order("id").Find(&cs).Error
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}

	usage, err := clusterUsage(db)
	if err != nil {
		return nil, err
	}

	out := make([]ClusterUsage, len(cs))
	for i, c := range cs {
		u := usage[c.ID]
		out[i] = ClusterUsage{Cluster: c, Tailnets: u.Tailnets, Devices: u.Devices}
	}

	return out, nil
}

// PlaceTailnet returns the cluster a new tailnet of class would get now:
// an ACTIVE cluster of the class with room and a process holding a live
// cluster lease (P12, owner decision Q2), the least loaded first
// (devices / max_devices), then the fewest live tailnets (P12, Q1: new,
// empty tailnets spread), then by id. A dedicated cluster has room only
// while it owns no live tailnet. CreateTailnet makes the same choice under
// lock, in its own transaction.
func (r *Registry) PlaceTailnet(ctx context.Context, class Class) (Cluster, error) {
	if !class.Valid() {
		return Cluster{}, fmt.Errorf("%w: class %q", ErrInvalid, class)
	}

	return place(r.db.WithContext(ctx), class, false)
}

// place chooses a cluster on db. With lock (CreateTailnet), the candidate
// rows are locked FOR UPDATE on PostgreSQL in id order, so concurrent
// placements serialise and each sees the tailnets the previous one added
// (SQLite serialises every write transaction anyway).
func place(db *gorm.DB, class Class, lock bool) (Cluster, error) {
	var (
		cs  []Cluster
		err error
	)

	if lock {
		cs, err = lockClusters(db, "UPDATE", "class = ? AND state = ?", class, ClusterActive)
	} else {
		err = db.Where("class = ? AND state = ?", class, ClusterActive).Order("id").Find(&cs).Error
	}

	if err != nil {
		return Cluster{}, fmt.Errorf("listing clusters: %w", err)
	}

	usage, err := clusterUsage(db)
	if err != nil {
		return Cluster{}, err
	}

	live, err := liveLeases(db)
	if err != nil {
		return Cluster{}, err
	}

	cands := make([]ClusterUsage, 0, len(cs))

	for _, c := range cs {
		// No process serves the cluster (never started, stopped, lost
		// its lease): a tailnet placed there would not be served.
		if !live[c.ID] {
			continue
		}

		u := usage[c.ID]
		if class == ClassDedicated && u.Tailnets > 0 {
			continue
		}

		if u.Devices >= c.MaxDevices {
			continue
		}

		cands = append(cands, ClusterUsage{Cluster: c, Tailnets: u.Tailnets, Devices: u.Devices})
	}

	if len(cands) == 0 {
		return Cluster{}, fmt.Errorf("%w: class %s", ErrNoCapacity, class)
	}

	// Least loaded: compare devices_a/max_a with devices_b/max_b without
	// floating point.
	best := slices.MinFunc(cands, func(a, b ClusterUsage) int {
		l, rr := a.Devices*b.MaxDevices, b.Devices*a.MaxDevices
		switch {
		case l < rr:
			return -1
		case l > rr:
			return 1
		case a.Tailnets < b.Tailnets:
			return -1
		case a.Tailnets > b.Tailnets:
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}

		return 0
	})

	return best.Cluster, nil
}

// lockClusters selects clusters matching where, ordered by id, with FOR
// <strength> on PostgreSQL. No row is ErrNotFound only for an id lookup.
func lockClusters(db *gorm.DB, strength string, where string, args ...any) ([]Cluster, error) {
	q := db.Where(where, args...).Order("id")
	if isPostgres(db) {
		q = q.Clauses(clause.Locking{Strength: strength})
	}

	var cs []Cluster

	err := q.Find(&cs).Error
	if err != nil {
		return nil, fmt.Errorf("locking clusters: %w", err)
	}

	if where == "id = ?" && len(cs) == 0 {
		return nil, fmt.Errorf("%w: cluster %v", ErrNotFound, args[0])
	}

	return cs, nil
}

func loadCluster(db *gorm.DB, id string) (Cluster, error) {
	var c Cluster

	err := db.Where("id = ?", id).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Cluster{}, fmt.Errorf("%w: cluster %q", ErrNotFound, id)
	}

	if err != nil {
		return Cluster{}, fmt.Errorf("loading cluster: %w", err)
	}

	return c, nil
}

type usageRow struct {
	ClusterID string
	Tailnets  int64
	Devices   int64
}

// liveLeases returns the clusters whose cluster_leases row has not expired
// by the database clock (P11b): a process of the cluster holds its lease.
// A read only; placement never takes or changes a lease.
func liveLeases(db *gorm.DB) (map[string]bool, error) {
	now := lease.SQLiteNow
	if isPostgres(db) {
		now = lease.PostgresNow
	}

	var ids []string

	err := db.Raw(`SELECT cluster_id FROM cluster_leases WHERE expires_at > ` + now).Scan(&ids).Error
	if err != nil {
		return nil, fmt.Errorf("reading cluster leases: %w", err)
	}

	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}

	return out, nil
}

// clusterUsage counts, per owning cluster, live tailnets and their live
// devices. Raw on purpose: nodes is a tenant table and this is the one
// cross-tenant read placement makes; it returns counts only.
func clusterUsage(db *gorm.DB) (map[string]usageRow, error) {
	var rows []usageRow

	err := db.Raw(`SELECT t.owner_cluster_id AS cluster_id,
		count(DISTINCT t.id) AS tailnets, count(n.id) AS devices
		FROM tailnets t LEFT JOIN nodes n ON n.tailnet_id = t.id AND n.deleted_at IS NULL
		WHERE t.deleted_at IS NULL
		GROUP BY t.owner_cluster_id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("counting devices: %w", err)
	}

	out := make(map[string]usageRow, len(rows))
	for _, u := range rows {
		out[u.ClusterID] = u
	}

	return out, nil
}
