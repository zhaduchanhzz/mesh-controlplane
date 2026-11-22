// Package tenantscope is a GORM plugin that confines every statement on a
// tenant table (one with a TailnetID / tailnet_id column) to the tailnet of
// the verified scope carried by the statement's context
// (internal/tailnet.WithScope). It is the application-layer tenant isolation
// chosen in ADR 0005: there is no database-level backstop, so this plugin
// fails closed.
//
//   - Query, Count, Row, Update and Delete get "AND <table>.tailnet_id = ?".
//   - Create stamps tailnet_id on every row and rejects rows already carrying
//     another tailnet.
//   - Upserts (ON CONFLICT ... DO UPDATE, used by Save when an UPDATE hits no
//     row) only update rows of the scope's tailnet, so a Save carrying another
//     tenant's primary key can never rewrite or adopt that tenant's row.
//   - A statement on a tenant table without a scope fails with
//     tailnet.ErrNoScope before reaching the database.
//   - Every INSERT/UPDATE/DELETE on a tenant table is fenced (I12): inside
//     the statement's own transaction the tailnets row is read (FOR SHARE on
//     PostgreSQL, so an ownership move waits for writers in flight) and its
//     owner_cluster_id/owner_epoch must equal the tailnet.Fence of the
//     context. A different owner or epoch fails with ErrFenced; a mutation
//     without a fence fails with ErrNoFence.
//
// Tables without the column are untouched. Raw SQL (db.Raw / db.Exec) is not
// parsed and must be reviewed by hand; see evidence/source-inventory-P03b.md.
package tenantscope

import (
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

const (
	fieldName = "TailnetID"
	column    = "tailnet_id"
	name      = "tenantscope"
)

var (
	// ErrForeignTenant reports a write whose row names a different tailnet
	// than the statement's scope.
	ErrForeignTenant = errors.New("row belongs to another tailnet")
	// ErrFenced reports a write by a process that no longer owns the tailnet
	// (another cluster, or a newer owner_epoch). The write is rolled back.
	ErrFenced = errors.New("tailnet ownership fence failed: owned by another cluster or epoch")
	// ErrNoFence reports a write to tenant data without an ownership fence.
	ErrNoFence = errors.New("tenant write without an ownership fence")
)

// Register installs the plugin's callbacks on db.
func Register(db *gorm.DB) error {
	cb := db.Callback()

	steps := []error{
		cb.Query().Before("gorm:query").Register(name+":query", scopeWhere),
		cb.Row().Before("gorm:row").Register(name+":row", scopeWhere),
		cb.Update().Before("gorm:update").Register(name+":update", scopeMutation),
		cb.Delete().Before("gorm:delete").Register(name+":delete", scopeMutation),
		cb.Create().Before("gorm:create").Register(name+":create", stampCreate),
		// After the scope callbacks, inside the default transaction.
		cb.Create().After(name+":create").After("gorm:begin_transaction").Before("gorm:create").
			Register(name+":fence_create", checkFence),
		cb.Update().After(name+":update").After("gorm:begin_transaction").Before("gorm:update").
			Register(name+":fence_update", checkFence),
		cb.Delete().After(name+":delete").After("gorm:begin_transaction").Before("gorm:delete").
			Register(name+":fence_delete", checkFence),
	}

	return errors.Join(steps...)
}

func tenantField(db *gorm.DB) *schema.Field {
	if db.Statement == nil || db.Statement.Schema == nil {
		return nil
	}

	if f := db.Statement.Schema.LookUpField(fieldName); f != nil {
		return f
	}

	return db.Statement.Schema.LookUpField(column)
}

func scopeOf(db *gorm.DB) (tailnet.Scope, bool) {
	s, err := tailnet.FromContext(db.Statement.Context)
	if err != nil {
		_ = db.AddError(err)

		return tailnet.Scope{}, false
	}

	return s, true
}

func whereTailnet(db *gorm.DB, f *schema.Field, id tailnet.ID) clause.Expression {
	return clause.Eq{
		Column: clause.Column{Table: db.Statement.Table, Name: f.DBName},
		Value:  id.String(),
	}
}

func scopeWhere(db *gorm.DB) {
	if db.Error != nil {
		return
	}

	f := tenantField(db)
	if f == nil {
		return
	}

	s, ok := scopeOf(db)
	if !ok {
		return
	}

	db.Statement.AddClause(clause.Where{Exprs: []clause.Expression{whereTailnet(db, f, s.ID())}})
}

// scopeMutation scopes UPDATE/DELETE. Adding a WHERE would otherwise disarm
// GORM's guard against accidental table-wide writes, so the guard is
// re-applied here: without a caller condition, a primary key, or
// AllowGlobalUpdate, the statement is refused.
func scopeMutation(db *gorm.DB) {
	if db.Error != nil {
		return
	}

	f := tenantField(db)
	if f == nil {
		return
	}

	if _, hasWhere := db.Statement.Clauses["WHERE"]; !hasWhere && !db.AllowGlobalUpdate && !hasPrimaryKey(db) {
		_ = db.AddError(gorm.ErrMissingWhereClause)

		return
	}

	scopeWhere(db)
}

func hasPrimaryKey(db *gorm.DB) bool {
	sch := db.Statement.Schema
	if sch == nil || len(sch.PrimaryFields) == 0 {
		return false
	}

	rv := reflect.Indirect(db.Statement.ReflectValue)
	switch rv.Kind() {
	case reflect.Struct:
		for _, pf := range sch.PrimaryFields {
			if _, zero := pf.ValueOf(db.Statement.Context, rv); zero {
				return false
			}
		}

		return true
	case reflect.Slice, reflect.Array:
		return rv.Len() > 0
	default:
		return false
	}
}

func stampCreate(db *gorm.DB) {
	if db.Error != nil {
		return
	}

	f := tenantField(db)
	if f == nil {
		return
	}

	s, ok := scopeOf(db)
	if !ok {
		return
	}

	id := s.ID().String()
	rv := reflect.Indirect(db.Statement.ReflectValue)

	switch rv.Kind() {
	case reflect.Struct:
		stampOne(db, f, rv, id)
	case reflect.Slice, reflect.Array:
		for i := range rv.Len() {
			stampOne(db, f, reflect.Indirect(rv.Index(i)), id)
		}
	case reflect.Map:
		stampMap(db, f, rv, id)
	default:
		_ = db.AddError(fmt.Errorf("%s: unsupported create value %s", name, rv.Kind()))
	}

	if db.Error != nil {
		return
	}

	confineUpsert(db, f, s.ID())
}

func stampOne(db *gorm.DB, f *schema.Field, rv reflect.Value, id string) {
	if cur, zero := f.ValueOf(db.Statement.Context, rv); !zero {
		if v, ok := cur.(string); !ok || v != id {
			_ = db.AddError(fmt.Errorf("%w: %v", ErrForeignTenant, cur))

			return
		}
	}

	if err := f.Set(db.Statement.Context, rv, id); err != nil {
		_ = db.AddError(err)
	}
}

func stampMap(db *gorm.DB, f *schema.Field, rv reflect.Value, id string) {
	for _, k := range []string{f.Name, f.DBName} {
		if v := rv.MapIndex(reflect.ValueOf(k)); v.IsValid() {
			if s, ok := v.Interface().(string); !ok || s != id {
				_ = db.AddError(fmt.Errorf("%w: %v", ErrForeignTenant, v.Interface()))

				return
			}
		}
	}

	rv.SetMapIndex(reflect.ValueOf(f.DBName), reflect.ValueOf(id))
}

// confineUpsert restricts ON CONFLICT ... DO UPDATE to rows of the scope's
// tailnet: a conflicting row of another tenant is left untouched.
func confineUpsert(db *gorm.DB, f *schema.Field, id tailnet.ID) {
	c, ok := db.Statement.Clauses["ON CONFLICT"]
	if !ok {
		return
	}

	oc, ok := c.Expression.(clause.OnConflict)
	if !ok || oc.DoNothing {
		return
	}

	oc.Where.Exprs = append(oc.Where.Exprs, whereTailnet(db, f, id))
	c.Expression = oc
	db.Statement.Clauses["ON CONFLICT"] = c
}

// checkFence verifies, in the statement's transaction, that the fence of the
// context still owns the scope's tailnet.
func checkFence(db *gorm.DB) {
	if db.Error != nil || tenantField(db) == nil {
		return
	}

	s, ok := scopeOf(db)
	if !ok {
		return
	}

	f, ok := tailnet.FenceFromContext(db.Statement.Context)
	if !ok {
		_ = db.AddError(ErrNoFence)

		return
	}

	q := `SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id = ?`
	if db.Dialector.Name() == "postgres" {
		q = `SELECT owner_cluster_id, owner_epoch FROM tailnets WHERE id = $1 FOR SHARE`
	}

	var (
		owner string
		epoch int64
	)

	err := db.Statement.ConnPool.QueryRowContext(db.Statement.Context, q, s.ID().String()).Scan(&owner, &epoch)
	if err != nil {
		_ = db.AddError(fmt.Errorf("%w: reading owner of %s: %w", ErrFenced, s.ID(), err))

		return
	}

	if owner != f.Cluster() || epoch < 0 || tailnet.Epoch(epoch) != f.Epoch() {
		_ = db.AddError(fmt.Errorf("%w: tailnet %s is owned by %q at epoch %d, writer holds %q at epoch %d",
			ErrFenced, s.ID(), owner, epoch, f.Cluster(), f.Epoch()))
	}
}
