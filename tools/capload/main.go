// Command capload is the client half of the P00c server-only capacity
// harness: it enrolls N real Tailscale control clients
// ([controlclient.Direct]) against a Headscale server running in another
// process or on another host, opens one streaming map session per node,
// and records time-to-register and time-to-first-map per node.
//
// It keeps clients light so the load generator does not dominate the run:
// map frames are counted and discarded rather than decoded into netmaps,
// except for every -decode-every'th client, whose first map is decoded to
// record its peer count.
//
// Pre-auth keys come from -keys, one per line; node i uses line
// i/-per-user, so each line is one user's reusable key. The file holds
// short-lived lab test credentials: it is read, never logged.
//
// See tools/capload/README.md for the full harness.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/control/controlclient"
	"tailscale.com/health"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsdial"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/persist"
	"tailscale.com/util/eventbus"
	"tailscale.com/util/httpbody"
	"tailscale.com/util/zstdframe"
)

type result struct {
	Index      int
	Register   time.Duration // TryLogin start → return
	FirstMap   time.Duration // map request sent → first frame read
	Total      time.Duration // TryLogin start → first frame read
	Peers      int           // peers in the first map, -1 if not decoded
	Err        string
	frames     atomic.Int64
	bytes      atomic.Int64
	enrolledAt time.Time
}

type config struct {
	server      string
	keys        []string
	nodes       int
	perUser     int
	concurrency int
	timeout     time.Duration
	hold        time.Duration
	deadline    time.Duration
	decodeEvery int
	out         string
}

func main() {
	var (
		cfg      config
		keysPath string
	)

	flag.StringVar(&cfg.server, "server", "", "Headscale server URL, e.g. http://192.168.1.242:8080")
	flag.StringVar(&keysPath, "keys", "", "file with one reusable pre-auth key per user")
	flag.IntVar(&cfg.nodes, "nodes", 1000, "nodes to enroll")
	flag.IntVar(&cfg.perUser, "per-user", 10, "nodes per user (key file line)")
	flag.IntVar(&cfg.concurrency, "concurrency", 32, "concurrent registrations")
	flag.DurationVar(&cfg.timeout, "timeout", 120*time.Second, "per-node register+first-map timeout")
	flag.DurationVar(&cfg.hold, "hold", 60*time.Second, "steady-state hold after enrollment")
	flag.DurationVar(&cfg.deadline, "deadline", 40*time.Minute, "enrollment phase deadline")
	flag.IntVar(&cfg.decodeEvery, "decode-every", 100, "decode the first map of every n-th client (0: none)")
	flag.StringVar(&cfg.out, "out", "capload-out", "output directory")
	flag.Parse()

	if cfg.server == "" || keysPath == "" {
		log.Fatal("capload: -server and -keys are required")
	}

	keys, err := readKeys(keysPath)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	cfg.keys = keys

	need := (cfg.nodes + cfg.perUser - 1) / cfg.perUser
	if len(keys) < need {
		log.Fatalf("capload: %d nodes at %d per user need %d keys, file has %d", cfg.nodes, cfg.perUser, need, len(keys))
	}

	err = os.MkdirAll(cfg.out, 0o750)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	err = run(cfg)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}
}

func readKeys(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var keys []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k := strings.TrimSpace(sc.Text()); k != "" {
			keys = append(keys, k)
		}
	}

	return keys, sc.Err()
}

func phase(name string) {
	fmt.Printf("PHASE %s %.3f\n", name, float64(time.Now().UnixNano())/1e9)
}

func run(cfg config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make([]*result, cfg.nodes)
	for i := range results {
		results[i] = &result{Index: i, Peers: -1}
	}

	var (
		enrolled, failed atomic.Int64
		streams          sync.WaitGroup
	)

	enrollCtx, enrollCancel := context.WithTimeout(ctx, cfg.deadline)
	defer enrollCancel()

	jobs := make(chan int)

	var workers sync.WaitGroup

	phase("enroll_start")

	start := time.Now()

	for range cfg.concurrency {
		workers.Go(func() {
			for i := range jobs {
				r := results[i]

				err := enrollOne(ctx, enrollCtx, cfg, r, &streams)
				if err != nil {
					r.Err = err.Error()

					failed.Add(1)

					continue
				}

				enrolled.Add(1)
			}
		})
	}

	stopProgress := make(chan struct{})

	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()

		for {
			select {
			case <-stopProgress:
				return
			case <-t.C:
				fmt.Printf("progress t=%.0fs enrolled=%d failed=%d frames=%d\n",
					time.Since(start).Seconds(), enrolled.Load(), failed.Load(), totalFrames(results))
			}
		}
	}()

feed:
	for i := range cfg.nodes {
		select {
		case jobs <- i:
		case <-enrollCtx.Done():
			break feed
		}
	}

	close(jobs)
	workers.Wait()

	enrollWall := time.Since(start)
	enrollFrames, enrollBytes := totalFrames(results), totalBytes(results)

	phase("enroll_end")
	fmt.Printf("enroll wall=%.1fs enrolled=%d failed=%d frames=%d bytes=%d\n",
		enrollWall.Seconds(), enrolled.Load(), failed.Load(), enrollFrames, enrollBytes)

	phase("hold_start")
	time.Sleep(cfg.hold)
	phase("hold_end")

	close(stopProgress)

	holdFrames, holdBytes := totalFrames(results)-enrollFrames, totalBytes(results)-enrollBytes

	cancel()

	summary := summarize(cfg, results, enrollWall, enrollFrames, enrollBytes, holdFrames, holdBytes)

	err := writeResults(cfg.out, results, summary)
	if err != nil {
		return err
	}

	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))

	return nil
}

func enrollOne(ctx, enrollCtx context.Context, cfg config, r *result, streams *sync.WaitGroup) error {
	nodeCtx, nodeCancel := context.WithTimeout(enrollCtx, cfg.timeout)
	defer nodeCancel()

	bus := eventbus.New()
	tracker := health.NewTracker(bus)
	dialer := tsdial.NewDialer(netmon.NewStatic())
	dialer.SetBus(bus)

	machineKey := key.NewMachine()
	disco := key.NewDisco().Public()
	hi := &tailcfg.Hostinfo{
		BackendLogID: fmt.Sprintf("capload-%d", r.Index),
		Hostname:     fmt.Sprintf("cap-node-%05d", r.Index),
	}

	direct, err := controlclient.NewDirect(controlclient.Options{
		Persist:              persist.Persist{},
		GetMachinePrivateKey: func() (key.MachinePrivate, error) { return machineKey, nil },
		ServerURL:            cfg.server,
		AuthKey:              cfg.keys[r.Index/cfg.perUser],
		Hostinfo:             hi,
		DiscoPublicKey:       disco,
		Logf:                 logger.Discard,
		HealthTracker:        tracker,
		Dialer:               dialer,
		Bus:                  bus,
	})
	if err != nil {
		return fmt.Errorf("NewDirect: %w", err)
	}

	start := time.Now()

	url, err := direct.TryLogin(nodeCtx, controlclient.LoginDefault)
	if err != nil {
		direct.Close()
		return fmt.Errorf("register: %w", err)
	}

	if url != "" {
		direct.Close()
		return errors.New("register: unexpected auth URL")
	}

	r.Register = time.Since(start)

	body, err := json.Marshal(&tailcfg.MapRequest{
		Version:   tailcfg.CurrentCapabilityVersion,
		KeepAlive: true,
		NodeKey:   direct.GetPersist().PrivateNodeKey().Public(),
		DiscoKey:  disco,
		Stream:    true,
		Hostinfo:  hi,
		Compress:  "zstd",
	})
	if err != nil {
		direct.Close()
		return err
	}

	// The stream outlives enrollment: it ends when ctx (the run) ends.
	streamCtx := httpbody.WithMaxSize(ctx, 0)
	mapURL := strings.Replace(cfg.server, "http:", "https:", 1) + "/machine/map"

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, mapURL, bytes.NewReader(body))
	if err != nil {
		direct.Close()
		return err
	}

	sent := time.Now()

	type firstFrame struct {
		resp *http.Response
		msg  []byte
		err  error
	}

	first := make(chan firstFrame, 1)

	go func() {
		resp, err := direct.DoNoiseRequest(req)
		if err != nil {
			first <- firstFrame{err: err}
			return
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			first <- firstFrame{err: fmt.Errorf("map: HTTP %d", resp.StatusCode)}

			return
		}

		decode := cfg.decodeEvery > 0 && r.Index%cfg.decodeEvery == 0

		msg, n, err := readFrame(resp.Body, decode)
		if err != nil {
			resp.Body.Close()
			first <- firstFrame{err: fmt.Errorf("map: first frame: %w", err)}

			return
		}

		r.frames.Add(1)
		r.bytes.Add(int64(n))
		first <- firstFrame{resp: resp, msg: msg}
	}()

	var ff firstFrame

	select {
	case ff = <-first:
	case <-nodeCtx.Done():
		direct.Close()
		return fmt.Errorf("map: no first frame: %w", nodeCtx.Err())
	}

	if ff.err != nil {
		direct.Close()
		return ff.err
	}

	r.FirstMap = time.Since(sent)
	r.Total = time.Since(start)
	r.enrolledAt = time.Now()

	if ff.msg != nil {
		r.Peers = countPeers(ff.msg)
	}

	streams.Go(func() {
		defer direct.Close()
		defer ff.resp.Body.Close()

		for {
			_, n, err := readFrame(ff.resp.Body, false)
			if err != nil {
				return
			}

			r.frames.Add(1)
			r.bytes.Add(int64(n))
		}
	})

	return nil
}

// readFrame reads one size-prefixed map frame. Unless keep is set the body
// is discarded without buffering, so idle clients hold no map bytes.
func readFrame(r io.Reader, keep bool) ([]byte, int, error) {
	var siz [4]byte

	_, err := io.ReadFull(r, siz[:])
	if err != nil {
		return nil, 0, err
	}

	size := int64(binary.LittleEndian.Uint32(siz[:]))

	if !keep {
		_, err = io.CopyN(io.Discard, r, size)
		return nil, int(size) + 4, err
	}

	msg := make([]byte, size)
	_, err = io.ReadFull(r, msg)

	return msg, int(size) + 4, err
}

func countPeers(compressed []byte) int {
	b, err := zstdframe.AppendDecode(nil, compressed, zstdframe.MaxDecodedSize(1<<30))
	if err != nil {
		return -2
	}

	var mr tailcfg.MapResponse

	err = json.Unmarshal(b, &mr)
	if err != nil {
		return -3
	}

	return len(mr.Peers)
}

func totalFrames(rs []*result) int64 {
	var t int64
	for _, r := range rs {
		t += r.frames.Load()
	}

	return t
}

func totalBytes(rs []*result) int64 {
	var t int64
	for _, r := range rs {
		t += r.bytes.Load()
	}

	return t
}

type dist struct {
	N       int     `json:"n"`
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	P99     float64 `json:"p99_ms"`
	Max     float64 `json:"max_ms"`
	Over10s int     `json:"over_10s"`
}

func distOf(ds []time.Duration) dist {
	if len(ds) == 0 {
		return dist{}
	}

	slices.Sort(ds)

	q := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(ds)))) - 1
		return float64(ds[max(0, i)].Microseconds()) / 1000
	}

	over := 0

	for _, d := range ds {
		if d > 10*time.Second {
			over++
		}
	}

	return dist{N: len(ds), P50: q(0.50), P95: q(0.95), P99: q(0.99), Max: q(1), Over10s: over}
}

type summary struct {
	Nodes         int            `json:"nodes"`
	Enrolled      int            `json:"enrolled"`
	Failed        int            `json:"failed"`
	Errors        map[string]int `json:"errors"`
	EnrollWallSec float64        `json:"enroll_wall_s"`
	Register      dist           `json:"register"`
	FirstMap      dist           `json:"first_map"`
	Total         dist           `json:"register_to_first_map"`
	EnrollFrames  int64          `json:"enroll_frames"`
	EnrollBytes   int64          `json:"enroll_bytes"`
	HoldSec       float64        `json:"hold_s"`
	HoldFrames    int64          `json:"hold_frames"`
	HoldBytes     int64          `json:"hold_bytes"`
	DecodedPeers  map[int]int    `json:"decoded_first_map_peers"`
	Concurrency   int            `json:"concurrency"`
}

func summarize(cfg config, rs []*result, wall time.Duration, ef, eb, hf, hb int64) summary {
	var reg, fm, tot []time.Duration

	s := summary{
		Nodes: cfg.nodes, Errors: map[string]int{}, DecodedPeers: map[int]int{},
		EnrollWallSec: wall.Seconds(), EnrollFrames: ef, EnrollBytes: eb,
		HoldSec: cfg.hold.Seconds(), HoldFrames: hf, HoldBytes: hb, Concurrency: cfg.concurrency,
	}

	for _, r := range rs {
		switch {
		case r.Err != "":
			s.Failed++
			s.Errors[errClass(r.Err)]++
		case r.Total > 0:
			s.Enrolled++
			reg, fm, tot = append(reg, r.Register), append(fm, r.FirstMap), append(tot, r.Total)

			if r.Peers >= 0 {
				s.DecodedPeers[r.Index] = r.Peers
			}
		}
	}

	s.Register, s.FirstMap, s.Total = distOf(reg), distOf(fm), distOf(tot)

	return s
}

func errClass(e string) string {
	if e == "" {
		return ""
	}

	if i := strings.Index(e, ":"); i > 0 {
		rest := e[i+1:]
		if strings.Contains(rest, "deadline exceeded") {
			return e[:i] + ": deadline exceeded"
		}

		if strings.Contains(rest, "context canceled") {
			return e[:i] + ": canceled"
		}

		return e[:i] + ": other"
	}

	return "other"
}

func writeResults(dir string, rs []*result, s summary) error {
	f, err := os.Create(filepath.Join(dir, "nodes.csv"))
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	_ = w.Write([]string{"index", "register_ms", "first_map_ms", "total_ms", "peers", "frames", "bytes", "enrolled_unix", "err"})

	ms := func(d time.Duration) string { return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 3, 64) }

	for _, r := range rs {
		at := ""
		if !r.enrolledAt.IsZero() {
			at = strconv.FormatFloat(float64(r.enrolledAt.UnixNano())/1e9, 'f', 3, 64)
		}

		_ = w.Write([]string{
			strconv.Itoa(r.Index), ms(r.Register), ms(r.FirstMap), ms(r.Total), strconv.Itoa(r.Peers),
			strconv.FormatInt(r.frames.Load(), 10), strconv.FormatInt(r.bytes.Load(), 10), at, errClass(r.Err),
		})
	}

	w.Flush()

	if err := w.Error(); err != nil {
		return err
	}

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o600)
}
