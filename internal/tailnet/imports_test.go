package tailnet

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/zhaduchanhzz/mesh-controlplane"

// allowedImports keeps the security-boundary package tiny and free of any
// transport: a scope must never be built from an HTTP header (I01, S11).
var allowedImports = map[string]bool{
	"context": true,
	"errors":  true,
	"fmt":     true,
}

func TestTailnetPackageImportsAreMinimal(t *testing.T) {
	for _, imp := range nonTestImports(t, ".") {
		if !allowedImports[imp] {
			t.Errorf("internal/tailnet imports %q; only %v are allowed (no net/http, no hscontrol)", imp, keys(allowedImports))
		}
	}
}

// The OAuth scope package of the v2 API shares the word "scope" but is a
// different concept; keep the two packages independent so review cannot
// confuse them.
func TestOAuthScopePackageDoesNotUseTailnet(t *testing.T) {
	for _, imp := range nonTestImports(t, filepath.Join("..", "..", "hscontrol", "scope")) {
		if strings.HasPrefix(imp, modulePath+"/internal/tailnet") {
			t.Errorf("hscontrol/scope imports %q", imp)
		}
	}
}

func nonTestImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
	}
	if len(out) == 0 && dir == "." {
		t.Fatal("no non-test Go files found; the package is missing")
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
