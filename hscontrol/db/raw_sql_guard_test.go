package db

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Raw SQL guard (P12c A4, ADR 0005 Bổ sung 2 §3 point 4).
//
// Tenant data is read and written through GORM, where tenantscope confines
// every statement to the scope's tailnet and fences writes on its owner
// (I06, I12). Raw SQL skips both. This test walks the non-test Go code of
// the module and inspects every Raw/Exec/ExecContext/Query/QueryContext/
// QueryRow/QueryRowContext call:
//
//   - the SQL argument is resolved statically: string literals, constants
//     of the package or of another package of the module, "+" of those, and
//     local variables whose every assignment resolves (q := A; if pg { q = B }
//     gives both A and B);
//   - resolved SQL that names a tenant table anywhere (read or write, any
//     syntax: SELECT, INSERT [OR REPLACE], REPLACE, UPDATE [ONLY], DELETE,
//     MERGE, TRUNCATE, schema-qualified or quoted names, DDL) must be in
//     rawSQLAllowlist;
//   - SQL that does not resolve (a parameter, a call such as fmt.Sprintf, a
//     package variable) in a package that touches the database must be in
//     rawSQLAllowlist too, keyed by its function and source expression.
//
// An entry is one statement: the file, a hash of the normalised SQL (or of
// "expr:<function>: <source>" when unresolved) and the reviewed reason. Entries
// that match nothing fail the test as well, so the list cannot go stale.
// To list the statements of a failing run as entries, run the test with
// MESH_RAW_SQL_GUARD_PRINT=1.

// rawSQLTenantTableRE matches a tenant table name as a word (see
// tenantscope.TenantTables); "users_x" or "idx_nodes" are not matches.
var rawSQLTenantTableRE = regexp.MustCompile(`(?i)\b(users|nodes|credentials|policies|tailnet_settings)\b`)

// rawSQLMethods are the methods whose argument is SQL text.
var rawSQLMethods = map[string]bool{
	"Raw": true, "Exec": true, "ExecContext": true,
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
}

// rawSQLSkipDirs are not product code: generated code and the server test
// harness, which seeds tenant data on purpose.
var rawSQLSkipDirs = []string{".git", "gen", "node_modules", "artifacts"}

const (
	rawSQLModule      = "github.com/zhaduchanhzz/mesh-controlplane/"
	rawSQLHarnessDir  = "hscontrol/servertest"
	rawSQLMaxDepth    = 16
	rawSQLHashLen     = 16
	rawSQLExprPrefix  = "expr:"
	rawSQLPrintEnvVar = "MESH_RAW_SQL_GUARD_PRINT"
)

type rawSQLEntry struct {
	file, hash, reason string
}

// rawSQLStatement is one statement found by the guard.
type rawSQLStatement struct {
	file, pos, sql string // sql: normalised SQL, or "expr:<source>"
	hash           string
}

func rawSQLHash(norm string) string {
	sum := sha256.Sum256([]byte(norm))

	return hex.EncodeToString(sum[:])[:rawSQLHashLen]
}

// rawSQLNormalize collapses whitespace so formatting does not change a hash.
func rawSQLNormalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

type rawSQLPackage struct {
	dir       string // module-relative, slash separated
	files     map[string]*ast.File
	consts    map[string]ast.Expr
	touchesDB bool
}

type rawSQLScanner struct {
	fset *token.FileSet
	pkgs map[string]*rawSQLPackage
}

func (s *rawSQLScanner) load(t *testing.T, root string) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if slices.Contains(rawSQLSkipDirs, d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == rawSQLHarnessDir || strings.HasPrefix(dir, rawSQLHarnessDir+"/") {
			return nil
		}

		f, err := parser.ParseFile(s.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		pkg := s.pkgs[dir]
		if pkg == nil {
			pkg = &rawSQLPackage{dir: dir, files: map[string]*ast.File{}, consts: map[string]ast.Expr{}}
			s.pkgs[dir] = pkg
		}

		pkg.files[rel] = f

		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "gorm.io/gorm" || p == "database/sql" || strings.HasPrefix(p, "github.com/jackc/pgx") {
				pkg.touchesDB = true
			}
		}

		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}

			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}

				for i, n := range vs.Names {
					if i < len(vs.Values) {
						pkg.consts[n.Name] = vs.Values[i]
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rawSQLScope holds what a function body (or a closure in it) assigns to
// its local names. A name is looked up from the innermost scope out to the
// scope that defines it, collecting every value assigned on the way: an
// "=" in a closure changes the variable of the enclosing function.
type rawSQLScope struct {
	parent  *rawSQLScope
	defined map[string]bool        // := , var, parameters, range variables
	opaque  map[string]bool        // assigned something the guard cannot follow
	locals  map[string][]ast.Expr // values assigned
}

func newRawSQLScope(parent *rawSQLScope) *rawSQLScope {
	return &rawSQLScope{
		parent: parent, defined: map[string]bool{}, opaque: map[string]bool{}, locals: map[string][]ast.Expr{},
	}
}

func (sc *rawSQLScope) addParams(fields *ast.FieldList) {
	if fields == nil {
		return
	}

	for _, f := range fields.List {
		for _, n := range f.Names {
			sc.defined[n.Name], sc.opaque[n.Name] = true, true
		}
	}
}

// lookup returns the values name can hold, whether one of them cannot be
// followed, and whether name is a local of the function at all.
func (sc *rawSQLScope) lookup(name string) ([]ast.Expr, bool, bool) {
	var (
		vals   []ast.Expr
		opaque bool
	)

	for s := sc; s != nil; s = s.parent {
		vals = append(vals, s.locals[name]...)
		opaque = opaque || s.opaque[name]

		if s.defined[name] {
			return vals, opaque, true
		}
	}

	// Assigned with "=" but defined outside the function: a package
	// variable, which anything may change.
	return nil, len(vals) > 0 || opaque, false
}

// rawSQLScopes builds the scopes of a function: its body and every closure.
func rawSQLScopes(fd *ast.FuncDecl) map[ast.Node]*rawSQLScope {
	root := newRawSQLScope(nil)
	root.addParams(fd.Recv)
	root.addParams(fd.Type.Params)
	root.addParams(fd.Type.Results)

	scopes := map[ast.Node]*rawSQLScope{fd.Body: root}
	stack := []*rawSQLScope{root}

	var nodes []ast.Node

	// assign records a value (nil: one the guard cannot follow) for name,
	// in the current scope for a definition, in every enclosing scope for
	// an assignment.
	assign := func(name string, value ast.Expr, define bool) {
		targets := stack
		if define {
			targets = stack[len(stack)-1:]
			targets[0].defined[name] = true
		}

		for _, s := range targets {
			if value == nil {
				s.opaque[name] = true
			} else {
				s.locals[name] = append(s.locals[name], value)
			}
		}
	}

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			if _, ok := nodes[len(nodes)-1].(*ast.FuncLit); ok {
				stack = stack[:len(stack)-1]
			}

			nodes = nodes[:len(nodes)-1]

			return true
		}

		nodes = append(nodes, n)

		switch n := n.(type) {
		case *ast.FuncLit:
			sc := newRawSQLScope(stack[len(stack)-1])
			sc.addParams(n.Type.Params)
			sc.addParams(n.Type.Results)
			scopes[n] = sc
			stack = append(stack, sc)
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}

				var value ast.Expr
				if len(n.Lhs) == len(n.Rhs) && (n.Tok == token.DEFINE || n.Tok == token.ASSIGN) {
					value = n.Rhs[i]
				}

				assign(id.Name, value, n.Tok == token.DEFINE)
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				var value ast.Expr
				if i < len(n.Values) {
					value = n.Values[i]
				}

				if value != nil || len(n.Values) > 0 {
					assign(id.Name, value, true)
				} else {
					stack[len(stack)-1].defined[id.Name] = true
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{n.Key, n.Value} {
				if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
					assign(id.Name, nil, n.Tok == token.DEFINE)
				}
			}
		}

		return true
	})

	return scopes
}

// rawSQLFunc is the context an expression is resolved in.
type rawSQLFunc struct {
	pkg     *rawSQLPackage
	scope   *rawSQLScope      // nil at package level
	imports map[string]string // local name -> import path
}

func fileImports(file *ast.File) map[string]string {
	out := map[string]string{}

	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)

		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}

		out[name] = p
	}

	return out
}

// resolve returns every SQL text expr can hold, or false if one of them is
// not a constant.
func (s *rawSQLScanner) resolve(fn *rawSQLFunc, expr ast.Expr, depth int) ([]string, bool) {
	if depth > rawSQLMaxDepth {
		return nil, false
	}

	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil, false
		}

		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return nil, false
		}

		return []string{v}, true
	case *ast.ParenExpr:
		return s.resolve(fn, e.X, depth+1)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return nil, false
		}

		left, ok := s.resolve(fn, e.X, depth+1)
		if !ok {
			return nil, false
		}

		right, ok := s.resolve(fn, e.Y, depth+1)
		if !ok {
			return nil, false
		}

		var out []string

		for _, l := range left {
			for _, r := range right {
				out = append(out, l+r)
			}
		}

		return out, true
	case *ast.Ident:
		if fn.scope != nil {
			vals, opaque, local := fn.scope.lookup(e.Name)
			if opaque {
				return nil, false
			}

			if local {
				var out []string

				for _, v := range vals {
					r, ok := s.resolve(fn, v, depth+1)
					if !ok {
						return nil, false
					}

					out = append(out, r...)
				}

				return out, len(out) > 0
			}
		}

		if c, ok := fn.pkg.consts[e.Name]; ok {
			return s.resolve(s.funcFor(fn.pkg, c), c, depth+1)
		}

		return nil, false
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			return nil, false
		}

		path, ok := fn.imports[x.Name]
		if !ok || !strings.HasPrefix(path, rawSQLModule) {
			return nil, false
		}

		pkg := s.pkgs[strings.TrimPrefix(path, rawSQLModule)]
		if pkg == nil {
			return nil, false
		}

		c, ok := pkg.consts[e.Sel.Name]
		if !ok {
			return nil, false
		}

		return s.resolve(s.funcFor(pkg, c), c, depth+1)
	default:
		return nil, false
	}
}

// funcFor is the package-level context of a constant's expression.
func (s *rawSQLScanner) funcFor(pkg *rawSQLPackage, expr ast.Expr) *rawSQLFunc {
	for _, f := range pkg.files {
		if f.Pos() <= expr.Pos() && expr.End() <= f.End() {
			return &rawSQLFunc{pkg: pkg, imports: fileImports(f)}
		}
	}

	return &rawSQLFunc{pkg: pkg, imports: map[string]string{}}
}

// sqlArg picks the SQL argument: the second one of the *Context methods
// and of calls whose first argument is a context, the first otherwise.
func sqlArg(name string, args []ast.Expr) ast.Expr {
	idx := 0
	if strings.HasSuffix(name, "Context") || (len(args) > 1 && looksLikeContext(args[0])) {
		idx = 1
	}

	if idx >= len(args) {
		return nil
	}

	return args[idx]
}

func looksLikeContext(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		n := strings.ToLower(e.Name)

		return n == "ctx" || strings.HasSuffix(n, "ctx")
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "context" {
				return true
			}

			return sel.Sel.Name == "Context"
		}
	}

	return false
}

func (s *rawSQLScanner) statements(t *testing.T) []rawSQLStatement {
	t.Helper()

	var out []rawSQLStatement

	for _, pkg := range s.pkgs {
		for rel, f := range pkg.files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}

				scopes := rawSQLScopes(fd)
				imports := fileImports(f)
				stack := []*rawSQLScope{scopes[fd.Body]}

				var nodes []ast.Node

				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if n == nil {
						if _, ok := nodes[len(nodes)-1].(*ast.FuncLit); ok {
							stack = stack[:len(stack)-1]
						}

						nodes = nodes[:len(nodes)-1]

						return true
					}

					nodes = append(nodes, n)

					if lit, ok := n.(*ast.FuncLit); ok {
						stack = append(stack, scopes[lit])
					}

					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}

					fn := &rawSQLFunc{pkg: pkg, scope: stack[len(stack)-1], imports: imports}

					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !rawSQLMethods[sel.Sel.Name] {
						return true
					}

					arg := sqlArg(sel.Sel.Name, call.Args)
					if arg == nil {
						return true
					}

					pos := s.fset.Position(call.Pos()).String()

					sqls, ok := s.resolve(fn, arg, 0)
					if !ok {
						if !pkg.touchesDB {
							return true
						}

						var b strings.Builder
						_ = printer.Fprint(&b, s.fset, arg)
						norm := rawSQLExprPrefix + fd.Name.Name + ": " + rawSQLNormalize(b.String())
						out = append(out, rawSQLStatement{file: rel, pos: pos, sql: norm, hash: rawSQLHash(norm)})

						return true
					}

					for _, q := range sqls {
						if !rawSQLTenantTableRE.MatchString(q) {
							continue
						}

						norm := rawSQLNormalize(q)
						out = append(out, rawSQLStatement{file: rel, pos: pos, sql: norm, hash: rawSQLHash(norm)})
					}

					return true
				})
			}
		}
	}

	slices.SortFunc(out, func(a, b rawSQLStatement) int { return strings.Compare(a.pos, b.pos) })

	return out
}

func scanRawSQL(t *testing.T) []rawSQLStatement {
	t.Helper()

	s := &rawSQLScanner{fset: token.NewFileSet(), pkgs: map[string]*rawSQLPackage{}}
	s.load(t, filepath.Join("..", ".."))

	return s.statements(t)
}

// checkRawSQL returns the statements not allowlisted and the allowlist
// entries that match no statement.
func checkRawSQL(found []rawSQLStatement, allow []rawSQLEntry) ([]rawSQLStatement, []rawSQLEntry) {
	used := make([]bool, len(allow))

	var missing []rawSQLStatement

	for _, st := range found {
		ok := false

		for i, e := range allow {
			if e.file == st.file && e.hash == st.hash {
				used[i], ok = true, true
			}
		}

		if !ok {
			missing = append(missing, st)
		}
	}

	var stale []rawSQLEntry

	for i, e := range allow {
		if !used[i] {
			stale = append(stale, e)
		}
	}

	return missing, stale
}

func TestRawSQLGuard(t *testing.T) {
	found := scanRawSQL(t)
	missing, stale := checkRawSQL(found, rawSQLAllowlist)

	if os.Getenv(rawSQLPrintEnvVar) != "" {
		for _, st := range found {
			t.Logf("{%q, %q, \"\"}, // %s: %.140s", st.file, st.hash, st.pos, st.sql)
		}
	}

	for _, st := range missing {
		t.Errorf("raw SQL not in the reviewed allowlist (tenant table or unresolvable SQL): %s\n  hash %s\n  sql  %.300s",
			st.pos, st.hash, st.sql)
	}

	for _, e := range stale {
		t.Errorf("allowlist entry matches no statement (remove it): %s %s (%s)", e.file, e.hash, e.reason)
	}

	for _, e := range rawSQLAllowlist {
		if strings.TrimSpace(e.reason) == "" {
			t.Errorf("allowlist entry without a reason: %s %s", e.file, e.hash)
		}
	}
}

// The guard's own checks: every syntax the review listed is caught, a
// statement read through a constant, a concatenation or a local variable is
// resolved, and SQL in a variable it cannot follow is reported.
func TestRawSQLGuardCatches(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM nodes WHERE id = ?",
		"INSERT OR REPLACE INTO nodes (id) VALUES (?)",
		"REPLACE INTO credentials (id) VALUES (?)",
		"UPDATE ONLY users SET name = ?",
		`DELETE FROM public."policies"`,
		"MERGE INTO tailnet_settings t USING x ON true WHEN MATCHED THEN DELETE",
		"TRUNCATE nodes",
		"select 1 from Main.Nodes",
	} {
		if !rawSQLTenantTableRE.MatchString(q) {
			t.Errorf("tenant table not found in %q", q)
		}
	}

	for _, q := range []string{"SELECT 1 FROM tailnets", "DROP INDEX idx_nodes_tailnet_ipv4", "SELECT user_id FROM tailnet_moves"} {
		if rawSQLTenantTableRE.MatchString(q) {
			t.Errorf("false tenant table match in %q", q)
		}
	}

	src := `package x

import "gorm.io/gorm"

const base = "SELECT id FROM "
const q = base + "nodes"

func f(db *gorm.DB, pg bool, dyn string) {
	db.Raw(q)
	s := "SELECT 1 FROM tailnets"
	if pg {
		s = "SELECT 1 FROM credentials"
	}
	db.Raw(s)
	db.Exec(dyn)
	db.Raw("SELECT 1 FROM tailnets")
}

// g: a parameter shadows the package constant q.
func g(db *gorm.DB, q string) {
	db.Raw(q)
}

// h: a closure assigns the variable the outer call uses.
func h(db *gorm.DB) {
	s := "SELECT 1 FROM tailnets"
	set := func() { s = "SELECT 1 FROM policies" }
	set()
	db.Raw(s)
	run := func() {
		u := "SELECT 1 FROM tailnets"
		db.Raw(u)
	}
	run()
}
`
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()

		if err != nil {
			t.Fatal(err)
		}
	}

	must(os.MkdirAll(filepath.Join(dir, "x"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "x", "x.go"), []byte(src), 0o600))

	s := &rawSQLScanner{fset: token.NewFileSet(), pkgs: map[string]*rawSQLPackage{}}
	s.load(t, dir)

	var got []string
	for _, st := range s.statements(t) {
		got = append(got, st.sql)
	}

	want := []string{"SELECT id FROM nodes", "SELECT 1 FROM credentials", "expr:f: dyn", "expr:g: q", "SELECT 1 FROM policies"}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("guard did not report %q; reported %q", w, got)
		}
	}

	if len(got) != len(want) {
		t.Errorf("guard reported %d statements, want %d: %q", len(got), len(want), got)
	}

	missing, stale := checkRawSQL(s.statements(t), []rawSQLEntry{{"x/x.go", rawSQLHash("expr:f: dyn"), "reviewed"}, {"x/x.go", "0000", "gone"}})
	if len(missing) != len(want)-1 || len(stale) != 1 {
		t.Errorf("allowlist check: %d missing (want %d), %d stale (want 1)", len(missing), len(want)-1, len(stale))
	}
}
