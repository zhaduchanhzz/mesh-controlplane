package tenantscope

import (
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// confineAssignments refuses an UPDATE that would set tailnet_id to
// anything but the scope's tailnet (P12c A3): the WHERE keeps the statement
// on the scope's rows, but "SET tailnet_id = B" would move them into B, and
// a key, node or policy moved that way authenticates or applies in B. It
// covers Update/UpdateColumn (map destination), Updates with a map or a
// struct, Save and Select("*").Updates (struct destination, every column
// written) and an explicit clause.Set. A zero TailnetID that would be
// written (Save of a row built without one) is stamped with the scope, as
// on Create. The database trigger of migration 202611261200 backs this up.
func confineAssignments(db *gorm.DB, f *schema.Field, id string) {
	stmt := db.Statement

	if c, ok := stmt.Clauses["SET"]; ok {
		if set, ok := c.Expression.(clause.Set); ok {
			for _, a := range set {
				if isTenantColumn(f, a.Column.Name) && !isScopeValue(a.Value, id) {
					_ = db.AddError(fmt.Errorf("%w: SET %s = %v", ErrForeignTenant, a.Column.Name, a.Value))

					return
				}
			}
		}
	}

	if stmt.Dest == nil {
		return
	}

	rv := reflect.ValueOf(stmt.Dest)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}

		rv = rv.Elem()
	}

	switch rv.Kind() { //nolint:exhaustive // the kinds gorm updates from; others assign nothing
	case reflect.Map:
		confineMapAssignment(db, f, rv, id)
	case reflect.Struct:
		confineStructAssignment(db, f, rv, id)
	case reflect.Slice, reflect.Array:
		for i := range rv.Len() {
			elem := reflect.Indirect(rv.Index(i))
			if elem.Kind() == reflect.Struct {
				confineStructAssignment(db, f, elem, id)
			}

			if db.Error != nil {
				return
			}
		}
	}
}

func confineMapAssignment(db *gorm.DB, f *schema.Field, rv reflect.Value, id string) {
	if rv.Type().Key().Kind() != reflect.String {
		return
	}

	iter := rv.MapRange()
	for iter.Next() {
		k := iter.Key().String()
		if !isTenantColumn(f, k) {
			continue
		}

		v := iter.Value().Interface()
		if !isScopeValue(v, id) {
			_ = db.AddError(fmt.Errorf("%w: update sets %s = %v", ErrForeignTenant, k, v))

			return
		}
	}
}

func confineStructAssignment(db *gorm.DB, f *schema.Field, rv reflect.Value, id string) {
	stmt := db.Statement

	// The schema GORM writes the struct with: the statement's, or that of
	// a destination of another type (as callbacks.ConvertToAssignments).
	sch := stmt.Schema
	if rv.Type() != sch.ModelType {
		parsed := &gorm.Statement{DB: db}

		err := parsed.Parse(rv.Interface())
		if err != nil {
			_ = db.AddError(fmt.Errorf("%s: %w: parsing update destination: %w", name, ErrUnscopable, err))

			return
		}

		sch = parsed.Schema
	}

	field := sch.LookUpField(f.DBName)
	if field == nil {
		return
	}

	value, zero := field.ValueOf(stmt.Context, rv)
	selected, restricted := stmt.SelectAndOmitColumns(false, true)
	sel, listed := selected[f.DBName]
	written := (listed && sel) || (!listed && !restricted && !zero)

	if !written {
		return
	}

	if !zero {
		if !isScopeValue(value, id) {
			_ = db.AddError(fmt.Errorf("%w: update sets %s = %v", ErrForeignTenant, f.DBName, value))
		}

		return
	}

	// Written while zero (Save, Select("*")): stamp the scope, or refuse
	// when the value cannot be set.
	if !rv.CanAddr() {
		_ = db.AddError(fmt.Errorf("%w: update would set %s to an empty value", ErrForeignTenant, f.DBName))

		return
	}

	err := field.Set(stmt.Context, rv, id)
	if err != nil {
		_ = db.AddError(err)
	}
}

// isTenantColumn reports whether an assignment key names the tenant
// column: the field name or column name, in any case, quoted or qualified.
func isTenantColumn(f *schema.Field, key string) bool {
	k := strings.Trim(key, "`\" ")
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = strings.Trim(k[i+1:], "`\" ")
	}

	return strings.EqualFold(k, f.DBName) || strings.EqualFold(k, f.Name)
}

// isScopeValue accepts only the scope's own tailnet ID as a plain string
// (or a non-nil pointer to one); expressions, subqueries and anything else
// are refused.
func isScopeValue(v any, id string) bool {
	switch s := v.(type) {
	case string:
		return s == id
	case *string:
		return s != nil && *s == id
	default:
		return false
	}
}
