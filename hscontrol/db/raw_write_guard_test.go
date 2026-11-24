package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Tenant data is written only through GORM, where the tenantscope plugin
// scopes every statement and fences it on tailnet ownership (I06, I12). Raw
// SQL (Exec/Raw) skips both. This test fails on any raw statement in
// non-test code that writes a tenant table, except the reviewed allowlist.

var tenantWriteRE = regexp.MustCompile(
	`(?is)\b(insert\s+into|update|delete\s+from)\s+"?(users|nodes|credentials|policies|tailnet_settings)\b`)

// rawTenantWriteAllowlist holds file:reason for reviewed raw tenant writes.
var rawTenantWriteAllowlist = map[string]string{
	// Platform operator path: inserts the deny-all policy of a tailnet that
	// is being created, in the same transaction as the tailnets row
	// (AGENTS rule 8). The tailnet has no runtime and no fence yet.
	"hscontrol/db/platform_admin.go": "CreateTailnet deny-all policy",
	// Placement CreateTailnet (operator API): same deny-all policy insert in
	// the transaction that creates the PENDING tailnet (AGENTS rule 8).
	"internal/placement/tailnets.go": "placement CreateTailnet deny-all policy",
	// Benchmark B3 (build tag benchdb, never in the server binary): measures
	// the cost of rewriting every row of a tenant on a move, on purpose.
	"cmd/bench-db/b3.go": "benchmark B3 bulk rewrite",
	// Platform operator path (P06, ADR 0008): the tailnet's DNS settings,
	// validated, under the ownership lock with the config_revision bump.
	"hscontrol/db/tailnet_settings.go": "SetTailnetDNS under LockOwnership",
}

func TestNoRawTenantWritesOutsideFence(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			switch d.Name() {
			case ".git", "gen", "node_modules", "artifacts":
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// Test harnesses seed data on purpose.
		if strings.HasPrefix(rel, "hscontrol/servertest/") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Exec" && sel.Sel.Name != "Raw" && sel.Sel.Name != "ExecContext") {
				return true
			}

			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}

				sql, err := strconv.Unquote(lit.Value)
				if err != nil || !tenantWriteRE.MatchString(sql) {
					continue
				}

				if _, allowed := rawTenantWriteAllowlist[rel]; !allowed {
					found = append(found, fset.Position(lit.Pos()).String())
				}
			}

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range found {
		t.Errorf("raw SQL writes a tenant table outside the ownership fence: %s", f)
	}
}
