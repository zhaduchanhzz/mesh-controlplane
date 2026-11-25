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

var (
	errKeyCount       = errors.New("node key count")
	errKeyLine        = errors.New("unparsable")
	errUnexpectedAuth = errors.New("register: unexpected auth URL")
	errMapStatus      = errors.New("map: HTTP")
)

type result struct {
	Index      int
	Phase      string
	Register   time.Duration // TryLogin start -> return (re-login for pre-registered nodes)
	FirstMap   time.Duration // map request sent -> first frame read
	Total      time.Duration // start -> first frame read
	Peers      int           // peers in the first map, -1 if not decoded
	Err        string
	frames     atomic.Int64
	bytes      atomic.Int64
	enrolledAt time.Time

	hostname string
	authKey  string             // register with this key, or
	machine  key.MachinePrivate // use these pre-registered keys
	node     key.NodePrivate
}

type config struct {
	server      string
	concurrency int
	timeout     time.Duration
	hold        time.Duration
	deadline    time.Duration
	decodeEvery int
	out         string
}

func main() {
	var (
		cfg                    config
		keysPath, nodeKeysPath string
		nodes, register        int
		perUser                int
	)

	flag.StringVar(&cfg.server, "server", "", "Headscale server URL, e.g. http://192.168.1.242:8080")
	flag.StringVar(&keysPath, "keys", "", "file with one reusable pre-auth key per user")
	flag.StringVar(&nodeKeysPath, "node-keys", "", "capseed nodes.keys: connect these pre-registered nodes instead of enrolling")
	flag.IntVar(&nodes, "nodes", 1000, "nodes to enroll, or to connect with -node-keys")
	flag.IntVar(&register, "register", 0, "after the first phase, register this many new nodes, one per user in turn")
	flag.IntVar(&perUser, "per-user", 10, "nodes per user (key file line) when enrolling")
	flag.IntVar(&cfg.concurrency, "concurrency", 32, "concurrent registrations/connections")
	flag.DurationVar(&cfg.timeout, "timeout", 120*time.Second, "per-node register+first-map timeout")
	flag.DurationVar(&cfg.hold, "hold", 60*time.Second, "steady-state hold after the last phase")
	flag.DurationVar(&cfg.deadline, "deadline", 40*time.Minute, "deadline per phase")
	flag.IntVar(&cfg.decodeEvery, "decode-every", 100, "decode the first map of every n-th client (0: none)")
	flag.StringVar(&cfg.out, "out", "capload-out", "output directory")
	flag.Parse()

	if cfg.server == "" || keysPath == "" {
		log.Fatal("capload: -server and -keys are required")
	}

	keys, err := readLines(keysPath)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	first, err := firstPhase(nodeKeysPath, keys, nodes, perUser)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	phases := []phaseSpec{first}

	if register > 0 {
		rs := make([]*result, register)
		for i := range rs {
			rs[i] = &result{
				Index: nodes + i, Phase: "register", Peers: -1,
				hostname: fmt.Sprintf("cap-new-%05d", i), authKey: keys[i%len(keys)],
			}
		}

		phases = append(phases, phaseSpec{"register", rs})
	}

	err = os.MkdirAll(cfg.out, 0o750)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	err = run(cfg, phases)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}
}

type phaseSpec struct {
	name    string
	results []*result
}

// firstPhase connects the pre-registered nodes of nodeKeysPath or, without
// it, enrolls new nodes perUser to a key.
func firstPhase(nodeKeysPath string, keys []string, nodes, perUser int) (phaseSpec, error) {
	rs := make([]*result, nodes)

	if nodeKeysPath == "" {
		if need := (nodes + perUser - 1) / perUser; len(keys) < need {
			return phaseSpec{}, fmt.Errorf("%w: %d nodes at %d per user need %d keys, file has %d",
				errKeyCount, nodes, perUser, need, len(keys))
		}

		for i := range rs {
			rs[i] = &result{
				Index: i, Phase: "enroll", Peers: -1,
				hostname: fmt.Sprintf("cap-node-%05d", i), authKey: keys[i/perUser],
			}
		}

		return phaseSpec{"enroll", rs}, nil
	}

	lines, err := readLines(nodeKeysPath)
	if err != nil {
		return phaseSpec{}, err
	}

	if len(lines) < nodes {
		return phaseSpec{}, fmt.Errorf("%w: -nodes %d but node-keys has %d", errKeyCount, nodes, len(lines))
	}

	for i := range rs {
		f := strings.Fields(lines[i])
		r := &result{Index: i, Phase: "connect", Peers: -1, hostname: fmt.Sprintf("cap-node-%05d", i)}

		if len(f) != 2 || r.machine.UnmarshalText([]byte(f[0])) != nil || r.node.UnmarshalText([]byte(f[1])) != nil {
			return phaseSpec{}, fmt.Errorf("node-keys line %d %w", i, errKeyLine)
		}

		rs[i] = r
	}

	return phaseSpec{"connect", rs}, nil
}

func readLines(path string) ([]string, error) {
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

func run(cfg config, phases []phaseSpec) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		all     []*result
		streams sync.WaitGroup
		out     = summary{Concurrency: cfg.concurrency}
	)

	for _, p := range phases {
		all = append(all, p.results...)
	}

	stopProgress := make(chan struct{})
	start := time.Now()

	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()

		for {
			select {
			case <-stopProgress:
				return
			case <-t.C:
				fmt.Printf("progress t=%.0fs frames=%d\n", time.Since(start).Seconds(), totalFrames(all))
			}
		}
	}()

	for _, p := range phases {
		framesBefore, bytesBefore := totalFrames(all), totalBytes(all)

		phase(p.name + "_start")
		wall := runPhase(ctx, cfg, p.results, &streams)
		phase(p.name + "_end")

		ps := summarize(p.name, p.results, wall)
		ps.Frames, ps.Bytes = totalFrames(all)-framesBefore, totalBytes(all)-bytesBefore
		out.Phases = append(out.Phases, ps)

		fmt.Printf("%s wall=%.1fs ok=%d failed=%d frames=%d bytes=%d\n",
			p.name, wall.Seconds(), ps.OK, ps.Failed, ps.Frames, ps.Bytes)
	}

	framesBefore, bytesBefore := totalFrames(all), totalBytes(all)

	phase("hold_start")

	select {
	case <-time.After(cfg.hold):
	case <-ctx.Done():
	}

	phase("hold_end")
	close(stopProgress)

	out.HoldSec = cfg.hold.Seconds()
	out.HoldFrames, out.HoldBytes = totalFrames(all)-framesBefore, totalBytes(all)-bytesBefore

	cancel()

	err := writeResults(cfg.out, all, out)
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(b))

	return nil
}

func runPhase(ctx context.Context, cfg config, rs []*result, streams *sync.WaitGroup) time.Duration {
	phaseCtx, phaseCancel := context.WithTimeout(ctx, cfg.deadline)
	defer phaseCancel()

	jobs := make(chan *result)

	var workers sync.WaitGroup

	start := time.Now()

	for range cfg.concurrency {
		workers.Go(func() {
			for r := range jobs {
				err := enrollOne(ctx, phaseCtx, cfg, r, streams)
				if err != nil {
					r.Err = err.Error()
				}
			}
		})
	}

feed:
	for _, r := range rs {
		select {
		case jobs <- r:
		case <-phaseCtx.Done():
			break feed
		}
	}

	close(jobs)
	workers.Wait()

	return time.Since(start)
}

func enrollOne(ctx, phaseCtx context.Context, cfg config, r *result, streams *sync.WaitGroup) error {
	nodeCtx, nodeCancel := context.WithTimeout(phaseCtx, cfg.timeout)
	defer nodeCancel()

	bus := eventbus.New()
	tracker := health.NewTracker(bus)
	dialer := tsdial.NewDialer(netmon.NewStatic())
	dialer.SetBus(bus)

	preRegistered := r.authKey == ""

	machineKey := r.machine

	var p persist.Persist

	if preRegistered {
		p.PrivateNodeKey = r.node
	} else {
		machineKey = key.NewMachine()
	}

	disco := key.NewDisco().Public()
	hi := &tailcfg.Hostinfo{
		BackendLogID: "capload-" + r.hostname,
		Hostname:     r.hostname,
	}

	direct, err := controlclient.NewDirect(controlclient.Options{
		Persist:              p,
		GetMachinePrivateKey: func() (key.MachinePrivate, error) { return machineKey, nil },
		ServerURL:            cfg.server,
		AuthKey:              r.authKey,
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

	// A pre-registered node logs in with its existing node key, as
	// tailscaled does on restart: the server finds the node and authorises
	// it without a key. That also fetches the server's Noise key.
	{
		url, err := direct.TryLogin(nodeCtx, controlclient.LoginDefault)
		if err != nil {
			direct.Close()
			return fmt.Errorf("register: %w", err)
		}

		if url != "" {
			direct.Close()
			return errUnexpectedAuth
		}

		r.Register = time.Since(start)
	}

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

	// The stream outlives its phase: it ends when ctx (the run) ends.
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

			first <- firstFrame{err: fmt.Errorf("%w %d", errMapStatus, resp.StatusCode)}

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

type phaseSummary struct {
	Name   string         `json:"name"`
	Nodes  int            `json:"nodes"`
	OK     int            `json:"ok"`
	Failed int            `json:"failed"`
	Errors map[string]int `json:"errors"`
	// ErrorSample is the first message of each class.
	ErrorSample map[string]string `json:"error_sample"`
	WallSec     float64           `json:"wall_s"`
	Register    dist              `json:"register"`
	FirstMap    dist              `json:"first_map"`
	Total       dist              `json:"start_to_first_map"`
	Frames      int64             `json:"frames_all_clients"`
	Bytes       int64             `json:"bytes_all_clients"`
	Peers       map[int]int       `json:"decoded_first_map_peers"`
}

type summary struct {
	Concurrency int            `json:"concurrency"`
	Phases      []phaseSummary `json:"phases"`
	HoldSec     float64        `json:"hold_s"`
	HoldFrames  int64          `json:"hold_frames"`
	HoldBytes   int64          `json:"hold_bytes"`
}

func summarize(name string, rs []*result, wall time.Duration) phaseSummary {
	var reg, fm, tot []time.Duration

	s := phaseSummary{
		Name: name, Nodes: len(rs), Errors: map[string]int{}, ErrorSample: map[string]string{}, Peers: map[int]int{},
		WallSec: wall.Seconds(),
	}

	for _, r := range rs {
		switch {
		case r.Err != "":
			s.Failed++
			s.Errors[errClass(r.Err)]++

			if _, ok := s.ErrorSample[errClass(r.Err)]; !ok {
				s.ErrorSample[errClass(r.Err)] = r.Err[:min(len(r.Err), 200)]
			}
		case r.Total > 0:
			s.OK++

			reg, fm, tot = append(reg, r.Register), append(fm, r.FirstMap), append(tot, r.Total)

			if r.Peers >= 0 {
				s.Peers[r.Index] = r.Peers
			}
		default:
			s.Failed++
			s.Errors["not started: phase deadline"]++
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
	_ = w.Write([]string{"index", "phase", "register_ms", "first_map_ms", "total_ms", "peers", "frames", "bytes", "enrolled_unix", "err"})

	ms := func(d time.Duration) string { return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 3, 64) }

	for _, r := range rs {
		at := ""
		if !r.enrolledAt.IsZero() {
			at = strconv.FormatFloat(float64(r.enrolledAt.UnixNano())/1e9, 'f', 3, 64)
		}

		_ = w.Write([]string{
			strconv.Itoa(r.Index), r.Phase, ms(r.Register), ms(r.FirstMap), ms(r.Total), strconv.Itoa(r.Peers),
			strconv.FormatInt(r.frames.Load(), 10), strconv.FormatInt(r.bytes.Load(), 10), at, errClass(r.Err),
		})
	}

	w.Flush()

	err = w.Error()
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o600)
}
