// Command gen-openapi emits a Headscale OpenAPI document from the authoritative
// Huma definitions in hscontrol/api/v1 and hscontrol/api/v2. The server also
// serves each spec live (at /openapi.yaml and /api/v2/openapi); this tool emits
// them on demand, and with -downgrade the 3.0.3 form used to generate the typed
// client. The output is not committed.
//
// Usage:
//
//	go run ./cmd/gen-openapi                       # write the v1 3.1 spec to its default path
//	go run ./cmd/gen-openapi -api v2               # write the v2 3.1 spec to its default path
//	go run ./cmd/gen-openapi -api placement        # the mesh-placement operator API
//	go run ./cmd/gen-openapi -downgrade <path>     # write the v1 3.0.3 downgrade (for client gen)
//	go run ./cmd/gen-openapi -api v2 -downgrade <path>  # the v2 3.0.3 downgrade
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	apiv1 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v1"
	apiv2 "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/api/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement/placementapi"
)

// spec bundles a version's full (3.1) and downgraded (3.0.3) generators with the
// committed output path. outPath is relative to the repository root.
type spec struct {
	full    func() ([]byte, error)
	down    func() ([]byte, error)
	outPath string
}

// specs maps the -api value to its generators.
var specs = map[string]spec{
	"v1": {apiv1.Spec, apiv1.Spec30, "openapi/v1/headscale.yaml"},
	"v2": {apiv2.Spec, apiv2.Spec30, "openapi/v2/headscale.yaml"},
	// The placement operator API (cmd/mesh-placement, P10).
	"placement": {placementapi.Spec, placementapi.Spec30, "openapi/placement/mesh-placement.yaml"},
}

func main() {
	api := flag.String("api", "v1", "which API spec to emit: v1, v2 or placement")
	downgrade := flag.String("downgrade", "", "write the OpenAPI 3.0.3 downgrade to this path instead of the committed 3.1 spec")
	flag.Parse()

	s, ok := specs[*api]
	if !ok {
		log.Fatalf("unknown -api %q (want v1, v2 or placement)", *api)
	}

	if *downgrade != "" {
		writeSpec(*downgrade, s.down)

		return
	}

	writeSpec(s.outPath, s.full)
}

func writeSpec(path string, gen func() ([]byte, error)) {
	spec, err := gen()
	if err != nil {
		log.Fatalf("generating OpenAPI spec: %v", err)
	}

	err = os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		log.Fatalf("creating output directory: %v", err)
	}

	err = os.WriteFile(path, spec, 0o600)
	if err != nil {
		log.Fatalf("writing %s: %v", path, err)
	}

	log.Printf("wrote %s", path)
}
