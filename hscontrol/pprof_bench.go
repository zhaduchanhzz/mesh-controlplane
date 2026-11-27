//go:build benchpprof

package hscontrol

import (
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util/zlog/zf"
)

// Benchmark builds only (P14 Q7, owner decision 2026-11-26): with the
// benchpprof build tag the cluster metrics listener also serves
// /debug/pprof/, to TCP peers on loopback only. Release builds have no build
// tags (.goreleaser.yml), so this file is never in them; without the tag
// pprof_nobench.go provides no-op versions of these functions.
//
// Profiles show the state of every tailnet of the process (goroutine stacks,
// heap contents), so they are stricter than /metrics: metrics_operator_cidrs
// does not admit them (AGENTS rule 9). Profiling a lab server is done over
// ssh on the server host (curl 127.0.0.1:<metrics port>/debug/pprof/...).
//
// Mutex and block profiling are off by default (their sampling costs CPU);
// HEADSCALE_BENCH_MUTEX_FRACTION and HEADSCALE_BENCH_BLOCK_RATE turn them
// on (runtime.SetMutexProfileFraction / runtime.SetBlockProfileRate).

const benchPprofEnabled = true

const benchPprofPrefix = "/debug/pprof/"

func init() {
	if n, ok := benchEnvInt("HEADSCALE_BENCH_MUTEX_FRACTION"); ok {
		runtime.SetMutexProfileFraction(n)
	}

	if n, ok := benchEnvInt("HEADSCALE_BENCH_BLOCK_RATE"); ok {
		runtime.SetBlockProfileRate(n)
	}
}

func benchEnvInt(name string) (int, bool) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return 0, false
	}

	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Warn().Msgf("benchpprof: ignoring invalid %s=%q", name, v)

		return 0, false
	}

	return n, true
}

// withBenchPprof serves /debug/pprof/ to loopback peers in front of next
// (the metrics handler). It runs before operatorOnly, which rewrites the
// peer of an admitted operator to a loopback address: the check here sees
// the real TCP peer.
func withBenchPprof(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+benchPprofPrefix, pprof.Index)
	mux.HandleFunc("GET "+benchPprofPrefix+"cmdline", pprof.Cmdline)
	mux.HandleFunc("GET "+benchPprofPrefix+"profile", pprof.Profile)
	mux.HandleFunc("GET "+benchPprofPrefix+"symbol", pprof.Symbol)
	mux.HandleFunc("GET "+benchPprofPrefix+"trace", pprof.Trace)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, benchPprofPrefix) {
			next.ServeHTTP(w, r)

			return
		}

		ip, ok := peerIP(r.RemoteAddr)
		if !ok || !ip.IsLoopback() {
			log.Debug().Str(zf.ClientAddress, r.RemoteAddr).Str(zf.Path, r.URL.Path).Msg("benchpprof: refused: not loopback")
			http.Error(w, "forbidden", http.StatusForbidden)

			return
		}

		mux.ServeHTTP(w, r)
	})
}

func logBenchPprof(addr string) {
	log.Warn().Str(zf.Address, addr).Msg("benchpprof build: /debug/pprof/ is served on the metrics listener to loopback peers; not a release build")
}
