package hscontrol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P13f L10 (review P03b): createDefaultTailnet inserts a tailnet ACTIVE
// without the PENDING + {"grants":[]} transaction of AGENTS rule 8. It is
// reached only through the single-tenant opener (db.NewHeadscaleDatabase,
// via state.NewState). The cluster code must never call either: its
// runtimes are built on Platform.TenantDB handles (state.NewStateWithDB) of
// tailnets created PENDING with a deny-all policy by the platform.
func TestClusterNeverCreatesDefaultTailnet(t *testing.T) {
	files, err := filepath.Glob("cluster*.go")
	if err != nil {
		t.Fatal(err)
	}

	forbidden := map[string]bool{"NewHeadscaleDatabase": true, "NewState": true, "createDefaultTailnet": true}
	checked := 0

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}

		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatal(err)
		}

		checked++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			var name string

			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}

			if forbidden[name] {
				t.Errorf("%s calls %s: a cluster process must not open the single-tenant path that creates a default tailnet", f, name)
			}

			return true
		})
	}

	if checked < 3 {
		t.Fatalf("checked %d cluster files; the glob no longer finds the cluster code", checked)
	}
}
