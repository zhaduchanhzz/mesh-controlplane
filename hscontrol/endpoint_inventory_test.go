package hscontrol

// P08a endpoint inventory (I09, I11, S14, S15).
//
// TestEndpointInventory builds every HTTP surface the server can expose and
// compares the exact route set against testdata/p08a_endpoint_allowlist.txt.
// A new, removed or re-authenticated route fails CI until both the allowlist
// and evidence/P08a-endpoint-inventory.md are updated. Set
// MESH_PRINT_ROUTES=1 to print the generated list.
//
// How each surface is enumerated:
//   - public:   chi.Walk over createRouter with DERP and OIDC routes on; the
//               /api/v1/* and /api/v2/* mounts are expanded by walking the
//               Huma muxes (they register full /api/vN/... paths).
//   - noise:    the router is built inside NoiseUpgradeHandler for each
//               hijacked connection, so it cannot be walked without changing
//               non-test code. It is read from noise.go's AST instead (static,
//               but still fails on any new r.Get/r.Post/... registration).
//   - debug:    http.ServeMux has no enumeration API; the tsweb /debug/ index
//               page lists every Handle()d endpoint, plus the explicit
//               debugMux registrations (statsviz, /metrics).
//   - unix:     the socket mux sends /api/v2/ to v2 and everything else to v1,
//               both with local trust: the route set is the Huma route set.
// Huma operations are tagged "auth" when they declare security; on the unix
// socket every operation is "local-trust".

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	apiv1 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v1"
	apiv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v2"
)

const (
	endpointAllowlistPath = "testdata/p08a_endpoint_allowlist.txt"
	endpointInventoryDoc  = "../evidence/P08a-endpoint-inventory.md"
)

// p08aApp is createTestApp whose state is closed at cleanup, so the SQLite
// file in t.TempDir can be removed on Windows.
func p08aApp(t *testing.T) *Headscale {
	t.Helper()

	app := createTestApp(t)
	t.Cleanup(func() { _ = app.state.Close() })

	return app
}

// endpointRoute is one line of the allowlist: "<server> <METHOD> <path> <auth>".
type endpointRoute struct {
	Server, Method, Path, Auth string
}

func (r endpointRoute) String() string {
	return strings.Join([]string{r.Server, r.Method, r.Path, r.Auth}, " ")
}

// humaAuth maps each "METHOD path" of a Huma API to "auth" or "none".
func humaAuth(api huma.API) map[string]string {
	out := map[string]string{}

	for path, item := range api.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put,
			http.MethodPatch: item.Patch, http.MethodDelete: item.Delete, http.MethodHead: item.Head,
		} {
			if op == nil {
				continue
			}

			auth := "none"
			if len(op.Security) > 0 {
				auth = "auth"
			}

			out[method+" "+path] = auth
		}
	}

	return out
}

func walkChi(t *testing.T, r chi.Routes, fn func(method, route string)) {
	t.Helper()

	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		fn(method, route)
		return nil
	})
	require.NoError(t, err)
}

// humaRoutes walks a Huma mux. Routes Huma serves itself (docs, OpenAPI
// documents, schemas) carry no operation and are reported as "none".
func humaRoutes(t *testing.T, server string, mux *chi.Mux, api huma.API, localTrust bool) []endpointRoute {
	t.Helper()

	auth := humaAuth(api)

	var out []endpointRoute

	walkChi(t, mux, func(method, route string) {
		a, ok := auth[method+" "+route]
		if !ok {
			a = "none"
		}

		if localTrust {
			a = "local-trust"
		}

		out = append(out, endpointRoute{server, method, route, a})
	})

	return out
}

// publicRoutes walks the public router with every optional route group on.
func publicRoutes(t *testing.T) []endpointRoute {
	t.Helper()

	app := p08aApp(t)
	app.cfg.DERP.Server.Enabled = true
	// Only method values are taken from the provider; nothing is called.
	app.authProvider = &AuthProviderOIDC{}

	v1Mux, v1API := apiv1.Handler(apiv1.Backend{State: app.state, Change: app.Change, Cfg: app.cfg})
	v2Mux, v2API := apiv2.Handler(apiv2.Backend{State: app.state, Change: app.Change, Cfg: app.cfg, Connected: app.isConnected})

	var out []endpointRoute

	walkChi(t, app.createRouter(v1Mux, v2Mux), func(method, route string) {
		switch route {
		case "/api/v1/*", "/api/v2/*":
			return // expanded below
		}

		out = append(out, endpointRoute{"public", method, route, publicAuth(route)})
	})

	out = append(out, humaRoutes(t, "public", v1Mux, v1API, false)...)
	out = append(out, humaRoutes(t, "public", v2Mux, v2API, false)...)

	return out
}

// publicAuth records what authenticates each non-API public route today.
func publicAuth(route string) string {
	switch route {
	case "/ts2021":
		return "noise-handshake"
	case "/register/{auth_id}", "/auth/{auth_id}", "/register/confirm/{auth_id}":
		return "auth-id-secret"
	case "/oidc/callback":
		return "oidc-state"
	case "/machine/ping-response":
		return "ping-id-secret"
	case "/verify":
		return "verify-secret"
	default:
		return "none"
	}
}

const noiseRouterFunc = "serveNoiseSession"

// noiseRoutes reads the routes the Noise session router registers from
// noise.go. Since P05 the router is built in serveNoiseSession (called by
// both Headscale.NoiseUpgradeHandler and the cluster), not in
// NoiseUpgradeHandler itself.
func noiseRoutes(t *testing.T) []endpointRoute {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "noise.go", nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl

	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == noiseRouterFunc {
			fn = f
		}
	}

	require.NotNil(t, fn, noiseRouterFunc+" not found in noise.go")

	methods := map[string]string{
		"Get": http.MethodGet, "Post": http.MethodPost, "Put": http.MethodPut, "Patch": http.MethodPatch,
		"Delete": http.MethodDelete, "Head": http.MethodHead, "Options": http.MethodOptions,
		"HandleFunc": "ANY", "Handle": "ANY", "Connect": http.MethodConnect, "Trace": http.MethodTrace,
	}

	var (
		out  []endpointRoute
		walk func(n ast.Node, prefix string)
	)

	walk = func(n ast.Node, prefix string) {
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}

			// Only registrations on the chi router variable r.
			if recv, ok := sel.X.(*ast.Ident); !ok || recv.Name != "r" {
				return true
			}

			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}

			path, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)

			switch {
			case sel.Sel.Name == "Route" || sel.Sel.Name == "Mount":
				if len(call.Args) == 2 { //nolint:mnd
					walk(call.Args[1], prefix+path)
				}

				return false
			case methods[sel.Sel.Name] != "":
				out = append(out, endpointRoute{"noise", methods[sel.Sel.Name], prefix + path, "noise-session"})
			}

			return true
		})
	}

	walk(fn.Body, "")
	require.NotEmpty(t, out)

	return out
}

var debugIndexHref = regexp.MustCompile(`href="(/[^"]*)"`)

// debugRoutes lists the metrics/debug server from its /debug/ index page.
func debugRoutes(t *testing.T) []endpointRoute {
	t.Helper()

	app := p08aApp(t)
	handler := app.debugHTTPServer().Handler

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/", nil)
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Not linked from the index: the explicit debugMux registrations, and the
	// pprof handlers tsweb registers with HandleSilent (probing them would
	// start a CPU profile or trace, so they are listed, not probed).
	paths := []string{
		"/debug/", "/debug/statsviz/", "/debug/statsviz/ws", "/metrics",
		"/debug/pprof/cmdline", "/debug/pprof/profile", "/debug/pprof/symbol", "/debug/pprof/trace",
	}

	for _, m := range debugIndexHref.FindAllStringSubmatch(rec.Body.String(), -1) {
		p, _, _ := strings.Cut(m[1], "?")
		paths = append(paths, p)
	}

	var out []endpointRoute

	for _, p := range paths {
		// P08: the whole listener, /metrics and statsviz included, is behind
		// operatorOnly (loopback or metrics_operator_cidrs).
		auth := "operator-only"

		out = append(out, endpointRoute{"debug", "ANY", p, auth})
	}

	return out
}

// clusterRoutes walks the public listener of a cluster process (P05/P10,
// headscale cluster serve). Every route is behind hostGuard (Host/SNI must be
// the server_url host, else 421); the label is what authenticates it beyond
// that. The listener has no admin API, debug or metrics (AGENTS rule 9).
func clusterRoutes(t *testing.T) []endpointRoute {
	t.Helper()

	// Only method values are taken; nothing is called.
	c := &Cluster{host: "c1.lab.invalid"}

	var out []endpointRoute

	r, ok := c.HTTPHandler().(chi.Routes)
	require.True(t, ok, "cluster HTTPHandler must be a chi router")

	walkChi(t, r, func(method, route string) {
		a := publicAuth(route)
		if route == "/register/{auth_id}" || route == "/auth/{auth_id}" {
			a = "disabled-404" // clusterRegistrationDisabled
		}

		if route == "/api/v2/*" {
			a = "api-key-tailnet" // P08b: dispatched by the key's tailnet
		}

		out = append(out, endpointRoute{"cluster", method, route, a})
	})

	require.NotEmpty(t, out)

	// P08b: what /api/v2/* reaches, the v2 API of the key's runtime in
	// cluster mode (operations refused there are listed in
	// evidence/P08a-endpoint-inventory.md).
	v2Mux, v2API := apiv2.Handler(apiv2.Backend{Tailnet: "00000000-0000-4000-8000-000000000001"})
	out = append(out, humaRoutes(t, "cluster-v2", v2Mux, v2API, false)...)

	// P08b: the live admin API on the cluster's unix socket.
	ar, ok := c.AdminHandler().(chi.Routes)
	require.True(t, ok, "cluster AdminHandler must be a chi router")

	walkChi(t, ar, func(method, route string) {
		out = append(out, endpointRoute{"cluster-unix", method, route, "local-trust"})
	})

	return out
}

// unixRoutes is what the unix socket serves: both Huma APIs, local trust.
func unixRoutes(t *testing.T) []endpointRoute {
	t.Helper()

	v1Mux, v1API := apiv1.Handler(apiv1.Backend{})
	v2Mux, v2API := apiv2.Handler(apiv2.Backend{})

	return slices.Concat(
		humaRoutes(t, "unix", v1Mux, v1API, true),
		humaRoutes(t, "unix", v2Mux, v2API, true),
	)
}

// collapseAnyMethod reports a route chi registered for every method (Handle,
// HandleFunc) once, as ANY.
func collapseAnyMethod(routes []endpointRoute) []endpointRoute {
	methods := map[string]map[string]bool{}

	for _, r := range routes {
		k := r.Server + " " + r.Path
		if methods[k] == nil {
			methods[k] = map[string]bool{}
		}

		methods[k][r.Method] = true
	}

	var out []endpointRoute

	for _, r := range routes {
		m := methods[r.Server+" "+r.Path]
		if m[http.MethodConnect] && m[http.MethodTrace] && m[http.MethodGet] && m[http.MethodPost] {
			r.Method = "ANY"
		}

		out = append(out, r)
	}

	return out
}

func allEndpointRoutes(t *testing.T) []string {
	t.Helper()

	var lines []string

	routes := collapseAnyMethod(slices.Concat(publicRoutes(t), clusterRoutes(t), noiseRoutes(t), debugRoutes(t), unixRoutes(t)))
	for _, r := range routes {
		lines = append(lines, r.String())
	}

	slices.Sort(lines)

	return slices.Compact(lines)
}

func readEndpointAllowlist(t *testing.T) []string {
	t.Helper()

	f, err := os.Open(endpointAllowlistPath)
	require.NoError(t, err)

	defer f.Close()

	var lines []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}

	require.NoError(t, sc.Err())
	slices.Sort(lines)

	return lines
}

// TestEndpointInventory (P08a, S14/I09): the exact set of routes on the
// public, Noise, debug and unix-socket servers equals the checked-in
// allowlist, and every route path appears in the Vietnamese inventory.
func TestEndpointInventory(t *testing.T) {
	got := allEndpointRoutes(t)

	if os.Getenv("MESH_PRINT_ROUTES") != "" {
		for _, l := range got {
			fmt.Println(l)
		}
	}

	want := readEndpointAllowlist(t)

	var added, removed []string

	for _, l := range got {
		if !slices.Contains(want, l) {
			added = append(added, l)
		}
	}

	for _, l := range want {
		if !slices.Contains(got, l) {
			removed = append(removed, l)
		}
	}

	if len(added)+len(removed) > 0 {
		t.Errorf("endpoint set differs from %s; review the route, then update the allowlist AND %s.\n"+
			"new or changed (not in allowlist):\n  %s\nmissing (in allowlist, not served):\n  %s",
			endpointAllowlistPath, endpointInventoryDoc,
			strings.Join(added, "\n  "), strings.Join(removed, "\n  "))
	}

	doc, err := os.ReadFile(endpointInventoryDoc)
	require.NoError(t, err, "the inventory document must exist")

	counts := map[string]int{}

	for _, l := range got {
		f := strings.Fields(l)
		counts[f[0]]++

		if !strings.Contains(string(doc), "`"+f[2]+"`") {
			t.Errorf("route %q is not documented in %s", l, endpointInventoryDoc)
		}
	}

	t.Logf("routes per server: %v", counts)
}
