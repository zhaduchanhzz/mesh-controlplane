//go:build !benchpprof

package hscontrol

import "net/http"

// Without the benchpprof build tag (every release and CI build) there is no
// pprof route anywhere in cluster mode; see pprof_bench.go.

const benchPprofEnabled = false

func withBenchPprof(next http.Handler) http.Handler { return next }

func logBenchPprof(string) {}
