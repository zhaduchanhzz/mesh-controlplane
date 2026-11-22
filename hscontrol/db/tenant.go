package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db/tenantscope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

var (
	// ErrNotV01Database reports a database that was not created by
	// mesh-controlplane v0.1, e.g. a Headscale database.
	ErrNotV01Database = errors.New("database was not created by mesh-controlplane v0.1; Headscale databases cannot be upgraded")
	// ErrTailnetNotOwned reports a tailnet owned by another cluster.
	ErrTailnetNotOwned = errors.New("tailnet is owned by another cluster")
	// ErrAmbiguousTailnet reports a database with several tailnets and no
	// tailnet chosen for the runtime.
	ErrAmbiguousTailnet = errors.New("database holds several tailnets; the runtime must name one")
)

const (
	defaultClusterID   = "local"
	defaultTailnetSlug = "default"
	// gormigrate records this ID when InitSchema creates a fresh database.
	schemaInitMigration = "SCHEMA_INIT"
)

// preCreatedTables exist before createSchema runs.
var preCreatedTables = []string{"migrations", "database_versions"}

// createSchema runs the v0.1 schema file of the connection's dialect.
// Each file is executed as one script so trigger bodies stay intact.
func createSchema(tx *gorm.DB) error {
	// A previous start may have committed the schema and then died before
	// gormigrate recorded it. The schema is created in one transaction, so a
	// present tailnets table means it is complete; it is validated afterwards.
	if tx.Migrator().HasTable("tailnets") {
		return nil
	}

	script := dbSchema
	if tx.Name() == "postgres" {
		script = dbSchemaPostgres
	}

	// gormigrate and the version check create these tables before the schema
	// runs; the schema files still declare them (schema.sql is the squibble
	// source of truth), so their CREATE TABLE is skipped when present.
	for _, table := range preCreatedTables {
		if tx.Migrator().HasTable(table) {
			script = regexp.MustCompile(`(?s)CREATE TABLE `+table+`\(.*?\);`).ReplaceAllString(script, "")
		}
	}

	err := tx.Exec(script).Error
	if err != nil {
		return fmt.Errorf("creating v0.1 schema: %w", err)
	}

	return nil
}

// checkV01Database refuses any non-fresh database that is not a v0.1
// database: one without the tailnets table, or with migration IDs this
// binary does not know (Headscale's chain or a newer release).
func checkV01Database(db *gorm.DB) error {
	m := db.Migrator()

	if !m.HasTable("migrations") {
		if m.HasTable("users") || m.HasTable("nodes") {
			return fmt.Errorf("%w: tables exist but no migration record", ErrNotV01Database)
		}

		return nil // fresh database
	}

	if !m.HasTable("tailnets") {
		return fmt.Errorf("%w: no tailnets table", ErrNotV01Database)
	}

	var ids []string

	err := db.Table("migrations").Pluck("id", &ids).Error
	if err != nil {
		return fmt.Errorf("reading migrations: %w", err)
	}

	known := map[string]bool{schemaInitMigration: true, v01InitMigration: true}
	for _, id := range ids {
		if !known[id] {
			return fmt.Errorf("%w: unknown migration %q", ErrNotV01Database, id)
		}
	}

	return nil
}

// bindRuntimeTailnet registers the tenantscope plugin on raw and returns a
// handle whose statements are confined to the runtime's tailnet, together
// with that tailnet. The tailnet must be owned by this process's cluster.
func bindRuntimeTailnet(raw *gorm.DB, cfg *types.Config) (*gorm.DB, types.Tailnet, error) {
	err := tenantscope.Register(raw)
	if err != nil {
		return nil, types.Tailnet{}, fmt.Errorf("registering tenantscope: %w", err)
	}

	cluster := cfg.ClusterID
	if cluster == "" {
		cluster = defaultClusterID
	}

	tn, err := resolveRuntimeTailnet(raw, cfg.TailnetID, cluster)
	if err != nil {
		return nil, types.Tailnet{}, err
	}

	id, err := tailnet.ParseID(tn.ID)
	if err != nil {
		return nil, types.Tailnet{}, fmt.Errorf("tailnet %q: %w", tn.ID, err)
	}

	scope, err := tailnet.NewScope(id, tailnet.SourceInternalService, "runtime@"+cluster, tailnet.Epoch(tn.OwnerEpoch))
	if err != nil {
		return nil, types.Tailnet{}, err
	}

	ctx, err := tailnet.WithScope(context.Background(), scope)
	if err != nil {
		return nil, types.Tailnet{}, err
	}

	return raw.WithContext(ctx), tn, nil
}

func resolveRuntimeTailnet(raw *gorm.DB, want, cluster string) (types.Tailnet, error) {
	var tns []types.Tailnet

	err := raw.Where("deleted_at IS NULL").Order("created_at").Find(&tns).Error
	if err != nil {
		return types.Tailnet{}, fmt.Errorf("listing tailnets: %w", err)
	}

	var tn types.Tailnet

	switch {
	case want != "":
		found := false

		for _, t := range tns {
			if t.ID == want {
				tn, found = t, true

				break
			}
		}

		if !found {
			return types.Tailnet{}, fmt.Errorf("tailnet %s does not exist", want)
		}
	case len(tns) == 0:
		return createDefaultTailnet(raw, cluster)
	case len(tns) == 1:
		tn = tns[0]
	default:
		ids := make([]string, 0, len(tns))
		for _, t := range tns {
			ids = append(ids, t.ID)
		}

		return types.Tailnet{}, fmt.Errorf("%w: %s", ErrAmbiguousTailnet, strings.Join(ids, ", "))
	}

	if tn.OwnerClusterID != cluster {
		return types.Tailnet{}, fmt.Errorf("%w: tailnet %s owner %q, this cluster %q",
			ErrTailnetNotOwned, tn.ID, tn.OwnerClusterID, cluster)
	}

	return tn, nil
}

// createDefaultTailnet gives a fresh single-tenant database its one tailnet.
func createDefaultTailnet(raw *gorm.DB, cluster string) (types.Tailnet, error) {
	now := time.Now().UTC()
	tn := types.Tailnet{
		ID:             uuid.NewV4().String(),
		Slug:           defaultTailnetSlug,
		Status:         types.TailnetActive,
		OwnerClusterID: cluster,
		OwnerEpoch:     1,
		PolicyRevision: 1,
		ConfigRevision: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	err := raw.Create(&tn).Error
	if err != nil {
		return types.Tailnet{}, fmt.Errorf("creating default tailnet: %w", err)
	}

	return tn, nil
}

// ScopedContext returns parent bound to this handle's tailnet scope. Use it
// when a statement needs another context (deadline, cancellation):
// db.WithContext(ctx) with a scope-less ctx fails with tailnet.ErrNoScope.
func (hsdb *HSDatabase) ScopedContext(parent context.Context) context.Context {
	s, err := tailnet.FromContext(hsdb.DB.Statement.Context)
	if err != nil {
		return parent
	}

	ctx, err := tailnet.WithScope(parent, s)
	if err != nil {
		return parent
	}

	return ctx
}
