package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/lease"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
	"tailscale.com/types/key"
)

var (
	// ErrUnknownTenant reports input that resolves to no tailnet this
	// platform may serve: an unknown or wrong credential, an unknown node,
	// or a tailnet that does not exist or is deleted. The cases are not
	// told apart, so the error reveals nothing about other tenants.
	ErrUnknownTenant = errors.New("unknown tenant")
	// ErrTailnetNotActive reports a tailnet of this cluster that is not
	// ACTIVE (PENDING, SUSPENDED or DELETING): no runtime may serve it.
	ErrTailnetNotActive = errors.New("tailnet is not active")

	// ErrNoClusterLease reports that this cluster process does not hold
	// the cluster lease (P11b): it is standby, and gets no tenant handle.
	ErrNoClusterLease = errors.New("cluster process does not hold the cluster lease")

	errEmptyClusterID = errors.New("empty cluster id")
)

// v01InitMigration marks a database created from the v0.1 schema files. The
// upstream Headscale migration chain is not carried: v0.1 does not upgrade
// Headscale databases (tickets/P03.md).
const v01InitMigration = "202611220000-v0.1-init"

// v01Migrations is the v0.1 migration chain. New migrations are appended,
// each in tx.Transaction with explicit DDL for SQLite and PostgreSQL
// (hscontrol/db/AGENTS.md). A fresh database gets every entry recorded by
// InitSchema, so the schema files must already contain their effect.
func v01Migrations() []*gormigrate.Migration {
	return []*gormigrate.Migration{
		{
			// The schema files create everything; this ID only marks a
			// v0.1 database.
			ID:       v01InitMigration,
			Migrate:  func(*gorm.DB) error { return nil },
			Rollback: func(*gorm.DB) error { return nil },
		},
		nodeKeyPairMigration(),
		registryMigration(),
		movingMigration(),
		nodeIPMigration(),
		tailnetSettingsMigration(),
		clusterLeasesMigration(),
		tailnetMovesMigration(),
	}
}

// Platform is the unscoped, cluster-level database handle (ADR 0007 §1). It
// owns the one connection pool of the process, reads the platform table
// tailnets, resolves the tenant of a Noise session, and hands out
// [HSDatabase] handles scoped to one tailnet on that same pool.
//
// Its lookups on tenant tables use raw SQL, which tenantscope does not
// filter; every such statement is in this file and selects only tailnet_id
// (and the credential hash to verify it), never tenant data.
type Platform struct {
	db      *gorm.DB // tenantscope registered: tenant tables need a scope
	cfg     *types.Config
	cluster string

	// P11b: a cluster process (RequireLease) gets tenant handles only while
	// it holds the cluster lease, and every such handle's fence claims that
	// lease. Operator processes (CLI) never set one.
	leaseMu      sync.RWMutex
	requireLease bool
	leaseHolder  string
	leaseEpoch   uint64
}

// RequireLease makes TenantDB fail with ErrNoClusterLease while no lease is
// set: the cluster serve process calls it before anything else.
func (p *Platform) RequireLease() {
	p.leaseMu.Lock()
	p.requireLease = true
	p.leaseMu.Unlock()
}

// SetLease records the cluster lease this process now holds; tenant handles
// opened from now on claim it.
func (p *Platform) SetLease(holder string, epoch uint64) {
	p.leaseMu.Lock()
	p.leaseHolder, p.leaseEpoch = holder, epoch
	p.leaseMu.Unlock()
}

// ClearLease forgets the lease: no new tenant handle until SetLease. Handles
// already open keep claiming the old lease, so their writes roll back.
func (p *Platform) ClearLease() {
	p.SetLease("", 0)
}

func (p *Platform) currentLease() (string, uint64, error) {
	p.leaseMu.RLock()
	defer p.leaseMu.RUnlock()

	if p.leaseHolder == "" && p.requireLease {
		return "", 0, ErrNoClusterLease
	}

	return p.leaseHolder, p.leaseEpoch, nil
}

// LeaseStore is the cluster_leases table on this pool.
func (p *Platform) LeaseStore() *lease.Store { return lease.NewStore(p.db) }

// OpenPlatform opens the database, creates the v0.1 schema on a fresh one,
// refuses databases from Headscale or unknown versions, runs the v0.1
// migrations, validates the schema (SQLite) and registers tenantscope on the
// pool. It never creates a tailnet.
func OpenPlatform(cfg *types.Config) (_ *Platform, retErr error) {
	dbConn, err := openDB(cfg.Database, cfg.SQLitePath())
	if err != nil {
		return nil, err
	}

	// Release the connection on every failure path (e.g. a refused Headscale
	// database); otherwise it leaks and, on Windows, keeps the file locked.
	defer func() {
		if retErr != nil {
			sqlDB, err := dbConn.DB()
			if err == nil {
				_ = sqlDB.Close()
			}
		}
	}()

	err = checkV01Database(dbConn)
	if err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}

	err = checkVersionUpgradePath(dbConn)
	if err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}

	migrations := gormigrate.New(dbConn, gormigrate.DefaultOptions, v01Migrations())

	migrations.InitSchema(func(tx *gorm.DB) error {
		return tx.Transaction(createSchema)
	})

	err = runMigrations(cfg.Database, dbConn, migrations)
	if err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	// Store the current version in the database after migrations succeed.
	// Dev builds skip this to preserve the stored version for the next
	// real versioned binary.
	currentVersion := types.GetVersionInfo().Version
	if !isDev(currentVersion) {
		err = setDatabaseVersion(dbConn, currentVersion)
		if err != nil {
			return nil, fmt.Errorf("storing database version: %w", err)
		}
	}

	// Validate that the schema ends up in the expected state (SQLite only;
	// squibble does not support PostgreSQL). Done in a helper so its
	// dedicated connection is released before the pool is used: SQLite
	// allows a single open connection.
	if cfg.Database.IsSQLite() {
		err = validateSQLiteSchema(dbConn)
		if err != nil {
			return nil, err
		}
	}

	err = tenantscope.Register(dbConn)
	if err != nil {
		return nil, fmt.Errorf("registering tenantscope: %w", err)
	}

	return &Platform{db: dbConn, cfg: cfg, cluster: clusterIDOf(cfg)}, nil
}

// ClusterID returns the cluster this platform serves for.
func (p *Platform) ClusterID() string { return p.cluster }

// Ping checks that the shared pool reaches the database (cluster /health).
func (p *Platform) Ping(ctx context.Context) error {
	sqlDB, err := p.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.PingContext(ctx)
}

// Close closes the shared pool; every handle from TenantDB stops working.
func (p *Platform) Close() error {
	sqlDB, err := p.db.DB()
	if err != nil {
		return err
	}

	return sqlDB.Close()
}

func (p *Platform) lookupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), contextTimeout)
}

// TailnetForAuthKey returns the tailnet of the pre-auth key key. The
// credential is found by its identifier (unique per kind across tailnets)
// and its secret verified exactly as registration does; an unparsable key,
// an unknown identifier and a wrong secret all return ErrUnknownTenant.
// Expiry, revocation and single use are left to the tenant's runtime, which
// reports them as upstream does once the key holder proved the secret.
func (p *Platform) TailnetForAuthKey(authKey string) (tailnet.ID, error) {
	identifier, secret, err := parseAuthKey(authKey)
	if err != nil {
		return tailnet.ID{}, ErrUnknownTenant
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var (
		tn   string
		hash []byte
	)

	err = p.db.WithContext(ctx).Raw(
		`SELECT tailnet_id, hash FROM credentials WHERE kind = ? AND identifier = ?`,
		types.CredentialPreAuthKey, identifier,
	).Row().Scan(&tn, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return tailnet.ID{}, ErrUnknownTenant
	}

	if err != nil {
		return tailnet.ID{}, fmt.Errorf("looking up auth key: %w", err)
	}

	// A revoked key may have no hash; it never verifies.
	if len(hash) == 0 {
		return tailnet.ID{}, ErrUnknownTenant
	}

	// needsRehash is ignored: the runtime upgrades a legacy hash when it
	// authenticates the key itself.
	_, err = verifySecret(hash, secret)
	if err != nil {
		return tailnet.ID{}, ErrUnknownTenant
	}

	return parseStoredTailnet(tn)
}

// TailnetForNode returns the tailnet of the node with machine key mk (from
// the Noise handshake) and node key nk. Several rows of one tailnet may carry
// the pair (upstream re-login as another user keeps the old node), but the
// schema refuses the pair in a second tailnet (trg_nodes_key_pair_tailnet*);
// a pair seen in more than one tailnet is still refused here. Zero keys
// never resolve.
func (p *Platform) TailnetForNode(mk key.MachinePublic, nk key.NodePublic) (tailnet.ID, error) {
	if mk.IsZero() || nk.IsZero() {
		return tailnet.ID{}, ErrUnknownTenant
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var tns []string

	// LIMIT 2: a second tailnet would be ambiguous; the trigger makes it
	// impossible, and it is still refused.
	err := p.db.WithContext(ctx).Raw(
		`SELECT DISTINCT tailnet_id FROM nodes WHERE machine_key = ? AND node_key = ? LIMIT 2`,
		mk.String(), nk.String(),
	).Scan(&tns).Error
	if err != nil {
		return tailnet.ID{}, fmt.Errorf("looking up node: %w", err)
	}

	if len(tns) != 1 {
		return tailnet.ID{}, ErrUnknownTenant
	}

	return parseStoredTailnet(tns[0])
}

// OwnedTailnets returns the ACTIVE, not deleted tailnets owned by clusterID
// with no move requested (ADR 0010), oldest first.
func (p *Platform) OwnedTailnets(clusterID string) ([]types.Tailnet, error) {
	if clusterID == "" {
		return nil, errEmptyClusterID
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var tns []types.Tailnet

	err := p.db.WithContext(ctx).
		// A tailnet with a move requested is not loaded: its owner hands it
		// over once it stopped serving it (ADR 0010).
		Where("owner_cluster_id = ? AND status = ? AND moving_to_cluster_id IS NULL AND deleted_at IS NULL",
			clusterID, types.TailnetActive).
		Order("created_at, id").
		Find(&tns).Error
	if err != nil {
		return nil, fmt.Errorf("listing tailnets of cluster %s: %w", clusterID, err)
	}

	return tns, nil
}

// TenantDB returns a handle scoped to tailnet id on the platform's pool (no
// new connection). The tailnet must exist, not be deleted, be owned by this
// platform's cluster and be ACTIVE; the scope carries its owner_epoch. The
// handle's Close leaves the pool open.
func (p *Platform) TenantDB(id tailnet.ID) (*HSDatabase, error) {
	if id.IsZero() {
		return nil, ErrUnknownTenant
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var tns []types.Tailnet

	err := p.db.WithContext(ctx).Where("id = ?", id.String()).Limit(1).Find(&tns).Error
	if err != nil {
		return nil, fmt.Errorf("loading tailnet %s: %w", id, err)
	}

	if len(tns) == 0 || tns[0].DeletedAt != nil {
		return nil, ErrUnknownTenant
	}

	tn := tns[0]

	if tn.OwnerClusterID != p.cluster {
		return nil, fmt.Errorf("%w: tailnet %s", ErrTailnetNotOwned, tn.ID)
	}

	if tn.Status != types.TailnetActive {
		return nil, fmt.Errorf("%w: tailnet %s is %s", ErrTailnetNotActive, tn.ID, tn.Status)
	}

	holder, leaseEpoch, err := p.currentLease()
	if err != nil {
		return nil, err
	}

	scoped, err := scopeTo(p.db, tn, p.cluster, holder, leaseEpoch)
	if err != nil {
		return nil, err
	}

	return &HSDatabase{
		DB:         scoped,
		raw:        p.db,
		tailnet:    tn,
		cfg:        p.cfg,
		sharedPool: true,
	}, nil
}

// parseStoredTailnet validates a tailnet_id read from a row; a malformed
// value fails closed.
func parseStoredTailnet(s string) (tailnet.ID, error) {
	id, err := tailnet.ParseID(s)
	if err != nil {
		return tailnet.ID{}, fmt.Errorf("stored tailnet id: %w", err)
	}

	return id, nil
}
