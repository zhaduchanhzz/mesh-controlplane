package servertest

// Test-only capacity instrumentation for P00b. Disabled unless
// HEADSCALE_CAPACITY_OUT names a directory; then, for the whole test
// process (server and in-process clients together):
//   - samples.csv: once per second, live heap objects, total Go-managed
//     memory, and goroutines (runtime/metrics, no stop-the-world);
//   - heap-peak.prof: an in-use heap profile rewritten whenever live heap
//     grows 15% past the last one written, so the largest observed heap
//     can be split by allocation site (server vs client packages) even
//     when the run later times out or is killed;
//   - metrics.txt: every headscale_* Prometheus metric at exit.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/metrics"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

func TestMain(m *testing.M) {
	dir := os.Getenv("HEADSCALE_CAPACITY_OUT")
	if dir == "" {
		os.Exit(m.Run())
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "capacity:", err)
		os.Exit(2)
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	go capacitySample(dir, stop, done)

	code := m.Run()

	close(stop)
	<-done

	capacityWriteMetrics(filepath.Join(dir, "metrics.txt"))
	os.Exit(code)
}

func capacitySample(dir string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	f, err := os.Create(filepath.Join(dir, "samples.csv"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "capacity:", err)

		return
	}
	defer f.Close()

	fmt.Fprintln(f, "elapsed_s,heap_objects_bytes,go_total_bytes,goroutines")

	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/sched/goroutines:goroutines"},
	}
	started := time.Now()
	tick := time.NewTicker(time.Second)

	defer tick.Stop()

	var (
		profiled    uint64
		lastProfile time.Time
	)

	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}

		metrics.Read(samples)
		heap := samples[0].Value.Uint64()
		fmt.Fprintf(f, "%.0f,%d,%d,%d\n", time.Since(started).Seconds(),
			heap, samples[1].Value.Uint64(), samples[2].Value.Uint64())

		if heap > 256<<20 && heap > profiled+profiled*15/100 &&
			time.Since(lastProfile) > 10*time.Second {
			if capacityWriteHeap(filepath.Join(dir, "heap-peak.prof")) == nil {
				profiled, lastProfile = heap, time.Now()
			}
		}
	}
}

func capacityWriteHeap(path string) error {
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	err = pprof.Lookup("heap").WriteTo(f, 0)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func capacityWriteMetrics(path string) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		fmt.Fprintln(os.Stderr, "capacity:", err)
	}

	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capacity:", err)

		return
	}
	defer f.Close()

	for _, mf := range families {
		if strings.HasPrefix(mf.GetName(), "headscale_") {
			_, _ = expfmt.MetricFamilyToText(f, mf)
		}
	}
}
