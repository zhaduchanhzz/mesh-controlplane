package tenantscope

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// TenantTables are the tables of the v0.1 schema whose rows belong to one
// tailnet (tailnet_id NOT NULL). A statement may only name them through a
// model that carries TailnetID, so the plugin can confine it (P12c A2).
// tailnet_moves also has a tailnet_id column, but it is a platform table
// read and written with reviewed SQL only (hscontrol/db/tailnet_moves.go).
func TenantTables() []string {
	return []string{"users", "nodes", "credentials", "policies", "tailnet_settings"}
}

// plugin holds the tenant tables a statement must not name unconfined.
type plugin struct {
	tables map[string]bool
}

func newPlugin(extra []string) *plugin {
	p := &plugin{tables: make(map[string]bool)}
	for _, t := range append(TenantTables(), extra...) {
		p.tables[strings.ToLower(t)] = true
	}

	return p
}

// checkTables refuses a statement that names a tenant table where the
// plugin cannot confine it (P12c A2): GORM statements built from Table(),
// a table expression, a string join, a FROM clause or a destination without
// TailnetID, and on a tenant handle a condition, select, order or group
// string, would otherwise run unscoped and unfenced. hasField reports
// whether the statement's schema carries TailnetID (then its own table is
// confined by the predicate).
//
// Raw SQL (Raw/Exec: Statement.SQL already set) is not parsed here; every
// such statement is reviewed and checked by the raw SQL guard
// (hscontrol/db/raw_sql_guard_test.go).
func (p *plugin) checkTables(db *gorm.DB, hasField bool) (err error) {
	stmt := db.Statement
	if stmt.SQL.Len() > 0 {
		return nil
	}

	// Building arbitrary clauses with the text builder must never take the
	// process down; a clause it cannot render is refused.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: rendering the statement: %v", ErrUnscopable, r)
		}
	}()

	main := strings.ToLower(stmt.Table)

	if te := stmt.TableExpr; te != nil {
		if base, ok := simpleTable(te); ok {
			main = base
		} else if t, found := p.refersTo(te.SQL); found {
			return fmt.Errorf("%w: table expression %q names tenant table %s", ErrUnscopable, te.SQL, t)
		}
	}

	if p.tables[main] && !hasField {
		return fmt.Errorf("%w: tenant table %s without a %s field to confine it", ErrUnscopable, main, fieldName)
	}

	for _, j := range stmt.Joins {
		if rels := relations(stmt.Schema, j.Name); rels != nil {
			if hasField {
				// An association of a confined model: the composite foreign
				// keys (tailnet_id, …) keep the joined rows in its tailnet.
				continue
			}

			for _, rel := range rels {
				if rel.FieldSchema != nil && (p.tables[strings.ToLower(rel.FieldSchema.Table)] || hasTenantField(rel.FieldSchema)) {
					return fmt.Errorf("%w: join %q reaches tenant table %s from an unconfined model",
						ErrUnscopable, j.Name, rel.FieldSchema.Table)
				}
			}

			continue
		}

		if t, found := p.refersTo(j.Name); found {
			return fmt.Errorf("%w: join %q names tenant table %s", ErrUnscopable, j.Name, t)
		}
	}

	if c, ok := stmt.Clauses["FROM"]; ok {
		if from, ok := c.Expression.(clause.From); ok {
			if t, found := p.fromRefersTo(from); found {
				return fmt.Errorf("%w: FROM clause names tenant table %s", ErrUnscopable, t)
			}
		}
	}

	// Strings in conditions, select, order and group may hold a subquery
	// on a tenant table that the predicate does not reach. They are checked
	// on tenant handles (a scope in the context). The platform handle
	// (operator and placement paths, no scope) is not a tenant's view: its
	// reviewed lookups correlate such subqueries with the tailnets row
	// (e.g. "EXISTS (SELECT 1 FROM policies p WHERE p.tailnet_id =
	// tailnets.id)").
	if _, err := tailnet.FromContext(stmt.Context); err != nil { //nolint:noinlineerr
		return nil
	}

	for _, s := range stmt.Selects {
		if t, found := p.refersTo(s); found {
			return fmt.Errorf("%w: select %q names tenant table %s", ErrUnscopable, s, t)
		}
	}

	for cname, c := range stmt.Clauses {
		if cname == "FROM" {
			continue
		}

		var b textBuilder

		for _, e := range []clause.Expression{c.BeforeExpression, c.AfterNameExpression, c.Expression, c.AfterExpression} {
			if e != nil {
				e.Build(&b)
				_ = b.WriteByte(' ')
			}
		}

		if t, found := p.refersTo(b.String()); found {
			return fmt.Errorf("%w: %s clause names tenant table %s", ErrUnscopable, cname, t)
		}
	}

	return nil
}

// fromRefersTo checks a FROM clause the caller set with Clauses().
func (p *plugin) fromRefersTo(from clause.From) (string, bool) {
	for _, t := range from.Tables {
		if t.Name == clause.CurrentTable {
			continue
		}

		if t.Raw {
			if name, found := p.refersTo(t.Name); found {
				return name, true
			}
		} else if p.tables[strings.ToLower(t.Name)] {
			return t.Name, true
		}
	}

	for _, j := range from.Joins {
		if j.Table.Name != clause.CurrentTable && p.tables[strings.ToLower(j.Table.Name)] {
			return j.Table.Name, true
		}

		var b textBuilder

		for _, e := range j.ON.Exprs {
			e.Build(&b)
			_ = b.WriteByte(' ')
		}

		if j.Expression != nil {
			j.Expression.Build(&b)
		}

		if name, found := p.refersTo(b.String()); found {
			return name, true
		}
	}

	return "", false
}

func hasTenantField(s *schema.Schema) bool {
	return s.LookUpField(fieldName) != nil || s.LookUpField(column) != nil
}

// relations resolves a join name the way GORM does: an association of the
// schema, or a dotted chain of associations. nil means GORM will use the
// name as raw SQL.
func relations(s *schema.Schema, joinName string) []*schema.Relationship {
	if s == nil {
		return nil
	}

	if rel, ok := s.Relationships.Relations[joinName]; ok {
		return []*schema.Relationship{rel}
	}

	parts := strings.Split(joinName, ".")
	if len(parts) < 2 {
		return nil
	}

	var (
		out []*schema.Relationship
		cur = s.Relationships.Relations
	)

	for _, part := range parts {
		rel, ok := cur[part]
		if !ok {
			return nil
		}

		out = append(out, rel)
		cur = rel.FieldSchema.Relationships.Relations
	}

	return out
}

// simpleTableRE matches what Table() turns a plain table name into: an
// optionally quoted, optionally schema-qualified name with an optional alias.
var simpleTableRE = regexp.MustCompile("(?i)^\\s*[`\"]?(\\w+)[`\"]?(?:\\s*\\.\\s*[`\"]?(\\w+)[`\"]?)?(?:\\s+(?:as\\s+)?[`\"]?\\w+[`\"]?)?\\s*$")

// simpleTable returns the table a plain table expression names.
func simpleTable(te *clause.Expr) (string, bool) {
	if len(te.Vars) > 0 {
		return "", false
	}

	m := simpleTableRE.FindStringSubmatch(te.SQL)
	if m == nil {
		return "", false
	}

	if m[2] != "" {
		return strings.ToLower(m[2]), true
	}

	return strings.ToLower(m[1]), true
}

// refersTo reports the first tenant table sql uses as a table: an
// identifier (bare or quoted, possibly schema-qualified) equal to a tenant
// table that is not a column qualifier ("nodes.id"). String literals and
// comments are skipped. It errs on the side of refusing: an alias or a
// column named like a tenant table counts as a reference.
func (p *plugin) refersTo(sql string) (string, bool) {
	for i := 0; i < len(sql); {
		c := sql[i]

		switch {
		case c == '\'':
			i = skipQuoted(sql, i, '\'')
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				// An unterminated comment is a syntax error in the database:
				// nothing after it runs.
				return "", false
			}

			i += end + 4
		case c == '"' || c == '`' || c == '[':
			closing := c
			if c == '[' {
				closing = ']'
			}

			end := strings.IndexByte(sql[i+1:], closing)
			if end < 0 {
				// Not an identifier after all (e.g. an array subscript):
				// keep scanning what follows.
				i++

				continue
			}

			ident := sql[i+1 : i+1+end]
			i += end + 2

			if p.isTableUse(sql, ident, i) {
				return ident, true
			}
		case isIdentStart(c):
			j := i + 1
			for j < len(sql) && isIdentPart(sql[j]) {
				j++
			}

			ident := sql[i:j]
			i = j

			if p.isTableUse(sql, ident, i) {
				return ident, true
			}
		default:
			i++
		}
	}

	return "", false
}

// isTableUse: ident, ending before sql[next], is a tenant table and is not
// followed by "." (which would make it the qualifier of a column).
func (p *plugin) isTableUse(sql, ident string, next int) bool {
	if !p.tables[strings.ToLower(ident)] {
		return false
	}

	for next < len(sql) && (sql[next] == ' ' || sql[next] == '\t' || sql[next] == '\n' || sql[next] == '\r') {
		next++
	}

	return next >= len(sql) || sql[next] != '.'
}

// skipQuoted returns the index after the literal starting at sql[i]; a
// doubled quote is an escaped quote.
func skipQuoted(sql string, i int, q byte) int {
	for i++; i < len(sql); i++ {
		if sql[i] != q {
			continue
		}

		if i+1 < len(sql) && sql[i+1] == q {
			i++

			continue
		}

		return i + 1
	}

	return len(sql)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

// textBuilder renders clauses as text for refersTo: identifiers quoted,
// values as "?". A subquery built from a GORM statement is checked by its
// own callbacks when it is built for real; a raw one (db.Raw) is inlined
// here so its text is checked too.
type textBuilder struct {
	strings.Builder
}

func (b *textBuilder) WriteQuoted(field any) {
	switch v := field.(type) {
	case clause.Table:
		if v.Name == clause.CurrentTable {
			_, _ = b.WriteString("@table")

			return
		}

		b.ident(v.Name, v.Raw)
	case clause.Column:
		if v.Table != "" {
			if v.Table == clause.CurrentTable {
				_, _ = b.WriteString("@table")
			} else {
				b.ident(v.Table, false)
			}

			_ = b.WriteByte('.')
		}

		if v.Name == clause.PrimaryKey {
			_, _ = b.WriteString("@pk")
		} else {
			b.ident(v.Name, v.Raw)
		}
	case string:
		b.ident(v, false)
	default:
		_, _ = fmt.Fprint(b, v)
	}
}

func (b *textBuilder) ident(s string, raw bool) {
	if raw {
		_, _ = b.WriteString(s)

		return
	}

	_ = b.WriteByte('"')
	_, _ = b.WriteString(s)
	_ = b.WriteByte('"')
}

func (b *textBuilder) AddVar(w clause.Writer, vars ...any) {
	for i, v := range vars {
		if i > 0 {
			_ = w.WriteByte(',')
		}

		switch v := v.(type) {
		case *gorm.DB:
			if v != nil && v.Statement != nil && v.Statement.SQL.Len() > 0 {
				_ = w.WriteByte('(')
				_, _ = w.WriteString(v.Statement.SQL.String())
				_ = w.WriteByte(')')
			} else {
				_, _ = w.WriteString("?")
			}
		case clause.Column, clause.Table:
			b.WriteQuoted(v)
		case clause.Expression:
			v.Build(b)
		case []any:
			_ = w.WriteByte('(')
			b.AddVar(w, v...)
			_ = w.WriteByte(')')
		default:
			_, _ = w.WriteString("?")
		}
	}
}

func (b *textBuilder) AddError(err error) error { return err }
