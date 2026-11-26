// Command capload is the client half of the server-only capacity harness
// (P00c; cluster mode P14a.0): it enrolls N real Tailscale control clients
// ([controlclient.Direct]) against a Headscale server running in another
// process or on another host, opens one streaming map session per node,
// and records time-to-register and time-to-first-map per node.
//
// It keeps clients light so the load generator does not dominate the run:
// map frames are counted and discarded, except on the -observe streams,
// whose every frame is decoded, timestamped and checked for isolation, and
// the first map of every -decode-every'th client.
//
// Nodes come from one of:
//   - -plan (capseed -mode cluster plan.tsv): per device its tailnet, phase
//     (enroll or register), hostname and pre-auth key; many tailnets;
//   - -keys (P00c): one reusable pre-auth key per user, node i uses line
//     i/-per-user, one tailnet;
//   - -node-keys: pre-registered nodes ("machine-private node-private
//     [tailnet]" per line, capseed serve mode or a previous -save-node-keys)
//     connect instead of enrolling.
//
// Phases, in order: enroll|connect, steady (-steady, endpoint churn at
// -churn-pct-per-min), probe (-probes endpoint/Hostinfo changes), register
// (-register), policy (-policy-changes runs of -policy-cmd), hold. Each
// prints "PHASE <name>_start|_end <unix>" for the server-side windows.
//
// Key files hold short-lived lab test credentials: they are read, never
// logged. Exit 3 means a decoded frame held a peer of another tailnet
// (isolation failure: stop the benchmark, BLOCK MERGE).
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
	"math/rand/v2"
	"net/http"
	"net/netip"
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
)

var (
	errKeyCount       = errors.New("node key count")
	errKeyLine        = errors.New("unparsable")
	errUnexpectedAuth = errors.New("register: unexpected auth URL")
	errMapStatus      = errors.New("map: HTTP")
	errNoSource       = errors.New("give -plan, -keys or -node-keys")
)

// exitIsolation is the exit code when any decoded frame held a peer of
// another tailnet.
const exitIsolation = 3

type result struct {
	Index    int
	Phase    string
	Tailnet  string        // tailnet slug ("" for one-tailnet runs)
	Register time.Duration // TryLogin start -> return (re-login for pre-registered nodes)
	FirstMap time.Duration // map request sent -> first frame read
	Total    time.Duration // start -> first frame read
	Peers    int           // peers in the first map, -1 if not decoded
	Err      string
	frames   atomic.Int64
	bytes    atomic.Int64

	enrolledAt time.Time

	hostname string
	authKey  string // register with this key (empty: pre-registered)
	machine  key.MachinePrivate
	node     key.NodePrivate
	disco    key.DiscoPublic
	obs      *observer
	alive    atomic.Bool

	mu        sync.Mutex // guards the live session state below
	direct    *controlclient.Direct
	hi        *tailcfg.Hostinfo
	endpoints []netip.AddrPort
}

type config struct {
	server      string
	concurrency int
	timeout     time.Duration
	hold        time.Duration
	deadline    time.Duration
	decodeEvery int
	out         string

	observe        int
	steady         time.Duration
	churnPerMin    float64
	probes         int
	probeInterval  time.Duration
	probeTimeout   time.Duration
	policyCmd      string
	policyTailnet  string
	policyChanges  int
	policyInterval time.Duration
	seed           uint64
	saveNodeKeys   string
	commit         string
	label          string
}

// mapURL is the Noise request URL of /machine/map (the Noise client wants
// https; the transport is the Noise connection).
func (c config) mapURL() string {
	return strings.Replace(c.server, "http:", "https:", 1) + "/machine/map"
}

func main() {
	var (
		cfg                              config
		keysPath, nodeKeysPath, planPath string
		resolve                          string
		nodes, register                  int
		perUser                          int
		churnPct                         float64
	)

	flag.StringVar(&cfg.server, "server", "", "server URL, e.g. http://bench.lab.internal:8080 (cluster mode needs its server_url hostname)")
	flag.StringVar(&resolve, "resolve", "", "host=ipv4[,…]: resolve these hostnames in-process (no /etc/hosts change)")
	flag.StringVar(&planPath, "plan", "", "capseed -mode cluster plan.tsv: tailnet, phase, hostname and pre-auth key per device")
	flag.StringVar(&keysPath, "keys", "", "P00c: file with one reusable pre-auth key per user (one tailnet)")
	flag.StringVar(&nodeKeysPath, "node-keys", "", "connect these pre-registered nodes instead of enrolling")
	flag.StringVar(&cfg.saveNodeKeys, "save-node-keys", "", "write the keys of every node that got a first map here (0600; for -node-keys)")
	flag.IntVar(&nodes, "nodes", 0, "first-phase nodes (0: all of -plan / -node-keys; required with -keys)")
	flag.IntVar(&register, "register", 0, "after the first phase (and steady/probes), register this many new nodes")
	flag.IntVar(&perUser, "per-user", 10, "-keys: nodes per user (key file line)")
	flag.IntVar(&cfg.concurrency, "concurrency", 32, "concurrent registrations/connections")
	flag.DurationVar(&cfg.timeout, "timeout", 120*time.Second, "per-node register+first-map timeout")
	flag.DurationVar(&cfg.hold, "hold", 60*time.Second, "hold after the last phase")
	flag.DurationVar(&cfg.deadline, "deadline", 40*time.Minute, "deadline per enroll/register phase")
	flag.IntVar(&cfg.decodeEvery, "decode-every", 100, "decode (and isolation-check) the first map of every n-th client (0: none)")
	flag.IntVar(&cfg.observe, "observe", 200, "streams whose every frame is decoded, timestamped and isolation-checked")
	flag.DurationVar(&cfg.steady, "steady", 0, "steady phase after the first phase, with endpoint churn")
	flag.Float64Var(&churnPct, "churn-pct-per-min", 1, "steady: percent of the nodes that change endpoint per minute")
	flag.IntVar(&cfg.probes, "probes", 0, "client changes to time (alternating endpoint and Hostinfo)")
	flag.DurationVar(&cfg.probeInterval, "probe-interval", 10*time.Second, "time between probes")
	flag.DurationVar(&cfg.probeTimeout, "probe-timeout", 30*time.Second, "an observer that has not seen a change by then missed it")
	flag.StringVar(&cfg.policyCmd, "policy-cmd", "", "sh -c command that changes one tailnet's policy (env CAPLOAD_TAILNET, CAPLOAD_STEP)")
	flag.StringVar(&cfg.policyTailnet, "policy-tailnet", "", "tailnet the policy changes go to (default: the first observer's)")
	flag.IntVar(&cfg.policyChanges, "policy-changes", 3, "policy changes when -policy-cmd is set")
	flag.DurationVar(&cfg.policyInterval, "policy-interval", 30*time.Second, "time between policy changes")
	flag.Uint64Var(&cfg.seed, "seed", 1, "seed of observer/probe/churn choices")
	flag.StringVar(&cfg.commit, "commit", "", "git commit of the binaries under test (manifest)")
	flag.StringVar(&cfg.label, "label", "", "cell label (manifest)")
	flag.StringVar(&cfg.out, "out", "capload-out", "output directory")
	flag.Parse()

	if cfg.server == "" {
		log.Fatal("capload: -server is required")
	}

	if resolve != "" {
		m, err := parseResolve(resolve)
		if err != nil {
			log.Fatalf("capload: %v", err)
		}

		installResolver(m)
	}

	first, reg, err := loadNodes(planPath, keysPath, nodeKeysPath, nodes, register, perUser)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	cfg.churnPerMin = churnPct / 100 * float64(len(first.results))

	err = os.MkdirAll(cfg.out, 0o750)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	code, err := run(cfg, first, reg)
	if err != nil {
		log.Fatalf("capload: %v", err)
	}

	os.Exit(code)
}

type phaseSpec struct {
	name    string
	results []*result
}

// loadNodes builds the first phase (enroll or connect) and the register
// phase from the node sources.
func loadNodes(planPath, keysPath, nodeKeysPath string, nodes, register, perUser int) (phaseSpec, phaseSpec, error) {
	var (
		first, reg phaseSpec
		err        error
	)

	switch {
	case planPath != "":
		first, reg, err = fromPlan(planPath, nodes, register)
	case keysPath != "":
		first, reg, err = fromKeys(keysPath, nodes, register, perUser)
	case nodeKeysPath == "":
		err = errNoSource
	}

	if err != nil {
		return first, reg, err
	}

	if nodeKeysPath != "" {
		first, err = fromNodeKeys(nodeKeysPath, nodes)
		if err != nil {
			return first, reg, err
		}
	}

	for i, r := range slices.Concat(first.results, reg.results) {
		r.Index = i
	}

	return first, reg, nil
}

func newNode(phase, tailnet, hostname, authKey string) *result {
	return &result{
		Phase: phase, Tailnet: tailnet, Peers: -1, hostname: hostname, authKey: authKey,
		machine: key.NewMachine(), node: key.NewNode(),
	}
}

// fromPlan reads capseed's plan.tsv: slug, tailnet ID, phase, hostname, key.
func fromPlan(path string, nodes, register int) (phaseSpec, phaseSpec, error) {
	lines, err := readLines(path)
	if err != nil {
		return phaseSpec{}, phaseSpec{}, err
	}

	first, reg := phaseSpec{name: "enroll"}, phaseSpec{name: "register"}

	for i, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 5 {
			return first, reg, fmt.Errorf("plan line %d %w", i+1, errKeyLine)
		}

		switch f[2] {
		case "enroll":
			if nodes == 0 || len(first.results) < nodes {
				first.results = append(first.results, newNode("enroll", f[0], f[3], f[4]))
			}
		case "register":
			if len(reg.results) < register {
				reg.results = append(reg.results, newNode("register", f[0], f[3], f[4]))
			}
		default:
			return first, reg, fmt.Errorf("plan line %d phase %w", i+1, errKeyLine)
		}
	}

	if nodes > 0 && len(first.results) < nodes || len(reg.results) < register {
		return first, reg, fmt.Errorf("%w: plan has %d enroll and %d register lines, want %d and %d",
			errKeyCount, len(first.results), len(reg.results), nodes, register)
	}

	return first, reg, nil
}

// fromKeys is the P00c source: one reusable key per user.
func fromKeys(path string, nodes, register, perUser int) (phaseSpec, phaseSpec, error) {
	keys, err := readLines(path)
	if err != nil {
		return phaseSpec{}, phaseSpec{}, err
	}

	if need := (nodes + perUser - 1) / perUser; len(keys) < need || len(keys) == 0 {
		return phaseSpec{}, phaseSpec{}, fmt.Errorf("%w: %d nodes at %d per user need %d keys, file has %d",
			errKeyCount, nodes, perUser, need, len(keys))
	}

	first, reg := phaseSpec{name: "enroll"}, phaseSpec{name: "register"}

	for i := range nodes {
		first.results = append(first.results, newNode("enroll", "", fmt.Sprintf("cap-node-%05d", i), keys[i/perUser]))
	}

	for i := range register {
		reg.results = append(reg.results, newNode("register", "", fmt.Sprintf("cap-new-%05d", i), keys[i%len(keys)]))
	}

	return first, reg, nil
}

// fromNodeKeys reads pre-registered nodes: "machine node [tailnet]".
func fromNodeKeys(path string, nodes int) (phaseSpec, error) {
	lines, err := readLines(path)
	if err != nil {
		return phaseSpec{}, err
	}

	if nodes == 0 {
		nodes = len(lines)
	}

	if len(lines) < nodes {
		return phaseSpec{}, fmt.Errorf("%w: -nodes %d but node-keys has %d", errKeyCount, nodes, len(lines))
	}

	p := phaseSpec{name: "connect"}

	for i := range nodes {
		f := strings.Fields(lines[i])
		r := &result{Phase: "connect", Peers: -1, hostname: fmt.Sprintf("cap-node-%05d", i)}

		if len(f) < 2 || len(f) > 3 || r.machine.UnmarshalText([]byte(f[0])) != nil || r.node.UnmarshalText([]byte(f[1])) != nil {
			return phaseSpec{}, fmt.Errorf("node-keys line %d %w", i, errKeyLine)
		}

		if len(f) == 3 {
			r.Tailnet = f[2]
			r.hostname = fmt.Sprintf("%s-c%05d", f[2], i)
		}

		p.results = append(p.results, r)
	}

	return p, nil
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

// phaseClock records the PHASE stamps, printed and kept for summary.json.
type phaseClock struct {
	mu    sync.Mutex
	times map[string]float64
}

func (pc *phaseClock) mark(name string) {
	t := float64(time.Now().UnixNano()) / 1e9

	pc.mu.Lock()
	pc.times[name] = t
	pc.mu.Unlock()

	fmt.Printf("PHASE %s %.3f\n", name, t) //nolint:forbidigo // harness output
}

func run(cfg config, first, reg phaseSpec) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		all     = slices.Concat(first.results, reg.results)
		streams sync.WaitGroup
		out     = summary{Concurrency: cfg.concurrency}
		clock   = &phaseClock{times: map[string]float64{}}
		rng     = rand.New(rand.NewPCG(cfg.seed, 0x5eed)) //nolint:gosec // load pattern, not security
		recs    []probeRec
	)

	iso := &isolation{keyTailnet: make(map[key.NodePublic]string, len(all))}
	byKey := make(map[key.NodePublic]*result, len(all))

	for _, r := range all {
		iso.keyTailnet[r.node.Public()] = r.Tailnet
		byKey[r.node.Public()] = r
	}

	pickObservers(first.results, cfg.observe)

	var observers []*observer

	for _, r := range first.results {
		if r.obs != nil {
			observers = append(observers, r.obs)
		}
	}

	out.Observers = len(observers)

	man := newManifest(cfg, os.Args)

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
				fmt.Printf("progress t=%.0fs frames=%d\n", time.Since(start).Seconds(), totalFrames(all)) //nolint:forbidigo // harness output
			}
		}
	}()

	enrollPhase := func(p phaseSpec) {
		framesBefore, bytesBefore := totalFrames(all), totalBytes(all)

		clock.mark(p.name + "_start")
		wall := runPhase(ctx, cfg, iso, p.results, &streams)
		clock.mark(p.name + "_end")

		ps := summarize(p.name, p.results, wall)
		ps.Frames, ps.Bytes = totalFrames(all)-framesBefore, totalBytes(all)-bytesBefore
		out.Phases = append(out.Phases, ps)

		fmt.Printf("%s wall=%.1fs ok=%d failed=%d frames=%d bytes=%d\n", //nolint:forbidigo // harness output
			p.name, wall.Seconds(), ps.OK, ps.Failed, ps.Frames, ps.Bytes)
	}

	window := func(name string, d time.Duration, during func(context.Context)) phaseWindow {
		framesBefore, bytesBefore := totalFrames(all), totalBytes(all)

		clock.mark(name + "_start")

		wctx, wcancel := context.WithTimeout(ctx, d)
		if during != nil {
			during(wctx)
		}

		<-wctx.Done()
		wcancel()
		clock.mark(name + "_end")

		return phaseWindow{Seconds: d.Seconds(), Frames: totalFrames(all) - framesBefore, Bytes: totalBytes(all) - bytesBefore}
	}

	enrollPhase(first)

	if cfg.steady > 0 {
		var cs churnSummary

		w := window("steady", cfg.steady, func(wctx context.Context) {
			cs = churn(wctx, cfg, first.results, cfg.churnPerMin, rng)
		})
		out.Steady, out.Churn = &w, &cs

		fmt.Printf("steady %.0fs churn sent=%d ok=%d failed=%d frames=%d\n", //nolint:forbidigo // harness output
			w.Seconds, cs.Sent, cs.OK, cs.Failed, w.Frames)
	}

	if cfg.probes > 0 && len(observers) > 0 {
		clock.mark("probe_start")
		out.Probes = probeRun(ctx, cfg, byKey, observers, rng, &recs)
		clock.mark("probe_end")

		for _, s := range out.Probes {
			fmt.Printf("probe %s n=%d observations=%d missed=%d p50=%.0fms p99=%.0fms\n", //nolint:forbidigo // harness output
				s.Kind, s.Probes, s.Observations, s.Missed, s.Latency.P50, s.Latency.P99)
		}
	}

	if len(reg.results) > 0 {
		enrollPhase(reg)
	}

	if cfg.policyCmd != "" && len(observers) > 0 {
		tn := cfg.policyTailnet
		if tn == "" {
			tn = observers[0].r.Tailnet
		}

		clock.mark("policy_start")
		ps := policyRun(ctx, cfg, tn, observers, &recs)
		clock.mark("policy_end")
		out.Policy = &ps

		fmt.Printf("policy n=%d observations=%d missed=%d p50=%.0fms p99=%.0fms other_tailnet_filter_frames=%d\n", //nolint:forbidigo // harness output
			ps.Probes, ps.Observations, ps.Missed, ps.Latency.P50, ps.Latency.P99, ps.OtherTailnetFrames)
	}

	hold := window("hold", cfg.hold, nil)
	close(stopProgress)

	out.HoldSec, out.HoldFrames, out.HoldBytes = hold.Seconds, hold.Frames, hold.Bytes
	out.Isolation = iso.summary()
	out.PhaseTimes = clock.times
	out.Tailnets = perTailnet(all)

	cancel()

	err := writeResults(cfg.out, all, observers, recs, out)
	if err != nil {
		return 1, err
	}

	if cfg.saveNodeKeys != "" {
		err = saveNodeKeys(cfg.saveNodeKeys, all)
		if err != nil {
			return 1, err
		}
	}

	err = man.finish(cfg.out)
	if err != nil {
		return 1, err
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 1, err
	}

	fmt.Println(string(b)) //nolint:forbidigo // harness output

	if out.Isolation.Violations > 0 {
		fmt.Printf("ISOLATION FAILURE: %d peers of another tailnet in decoded frames\n", out.Isolation.Violations) //nolint:forbidigo // harness output

		return exitIsolation, nil
	}

	return 0, nil
}

func runPhase(ctx context.Context, cfg config, iso *isolation, rs []*result, streams *sync.WaitGroup) time.Duration {
	phaseCtx, phaseCancel := context.WithTimeout(ctx, cfg.deadline)
	defer phaseCancel()

	jobs := make(chan *result)

	var workers sync.WaitGroup

	start := time.Now()

	for range cfg.concurrency {
		workers.Go(func() {
			for r := range jobs {
				err := enrollOne(ctx, phaseCtx, cfg, iso, r, streams)
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

func enrollOne(ctx, phaseCtx context.Context, cfg config, iso *isolation, r *result, streams *sync.WaitGroup) error {
	nodeCtx, nodeCancel := context.WithTimeout(phaseCtx, cfg.timeout)
	defer nodeCancel()

	bus := eventbus.New()
	tracker := health.NewTracker(bus)
	dialer := tsdial.NewDialer(netmon.NewStatic())
	dialer.SetBus(bus)

	// The node key is generated up front (newNode) so every key capload
	// drives is known before any frame arrives (isolation check). With an
	// auth key the server registers it as a new node; without, the node
	// logs in again with its existing key, as tailscaled does on restart.
	p := persist.Persist{PrivateNodeKey: r.node}
	r.disco = key.NewDisco().Public()
	hi := &tailcfg.Hostinfo{
		BackendLogID: "capload-" + r.hostname,
		Hostname:     r.hostname,
	}

	direct, err := controlclient.NewDirect(controlclient.Options{
		Persist:              p,
		GetMachinePrivateKey: func() (key.MachinePrivate, error) { return r.machine, nil },
		ServerURL:            cfg.server,
		AuthKey:              r.authKey,
		Hostinfo:             hi,
		DiscoPublicKey:       r.disco,
		Logf:                 logger.Discard,
		HealthTracker:        tracker,
		Dialer:               dialer,
		Bus:                  bus,
	})
	if err != nil {
		return fmt.Errorf("NewDirect: %w", err)
	}

	start := time.Now()

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
		NodeKey:   r.node.Public(),
		DiscoKey:  r.disco,
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

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, cfg.mapURL(), bytes.NewReader(body))
	if err != nil {
		direct.Close()
		return err
	}

	sent := time.Now()

	type firstFrame struct {
		resp *http.Response
		msg  []byte
		at   time.Time
		n    int
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

		decode := r.obs != nil || cfg.decodeEvery > 0 && r.Index%cfg.decodeEvery == 0

		msg, n, err := readFrame(resp.Body, decode)
		if err != nil {
			resp.Body.Close()

			first <- firstFrame{err: fmt.Errorf("map: first frame: %w", err)}

			return
		}

		r.frames.Add(1)
		r.bytes.Add(int64(n))

		first <- firstFrame{resp: resp, msg: msg, at: time.Now(), n: n}
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

	r.FirstMap = ff.at.Sub(sent)
	r.Total = ff.at.Sub(start)
	r.enrolledAt = ff.at

	r.mu.Lock()
	r.direct, r.hi = direct, hi
	r.mu.Unlock()
	r.alive.Store(true)

	if ff.msg != nil {
		if r.obs != nil {
			r.obs.frame(iso, ff.msg, ff.n, ff.at)
			r.Peers = len(r.obs.peerList())
		} else if mr, err := decodeMap(ff.msg); err == nil { //nolint:noinlineerr
			iso.check(r, mr)
			r.Peers = len(mr.Peers)
		} else {
			iso.decodeErrors.Add(1)
		}
	}

	streams.Go(func() {
		defer direct.Close()
		defer ff.resp.Body.Close()
		defer r.alive.Store(false)

		for {
			msg, n, err := readFrame(ff.resp.Body, r.obs != nil)
			if err != nil {
				return
			}

			r.frames.Add(1)
			r.bytes.Add(int64(n))

			if r.obs != nil {
				r.obs.frame(iso, msg, n, time.Now())
			}
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

	ds = slices.Clone(ds)
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

type phaseWindow struct {
	Seconds float64 `json:"s"`
	Frames  int64   `json:"frames_all_clients"`
	Bytes   int64   `json:"bytes_all_clients"`
}

type tailnetCount struct {
	Nodes     int `json:"nodes"`
	Connected int `json:"connected"`
}

type summary struct {
	Concurrency int                     `json:"concurrency"`
	Observers   int                     `json:"observers"`
	Phases      []phaseSummary          `json:"phases"`
	Steady      *phaseWindow            `json:"steady,omitempty"`
	Churn       *churnSummary           `json:"churn,omitempty"`
	Probes      []probeSummary          `json:"probes,omitempty"`
	Policy      *probeSummary           `json:"policy,omitempty"`
	HoldSec     float64                 `json:"hold_s"`
	HoldFrames  int64                   `json:"hold_frames"`
	HoldBytes   int64                   `json:"hold_bytes"`
	Isolation   isolationSummary        `json:"isolation"`
	Tailnets    map[string]tailnetCount `json:"tailnets"`
	PhaseTimes  map[string]float64      `json:"phase_times_unix"`
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

			if r.Peers >= 0 && len(s.Peers) < 200 {
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

func perTailnet(rs []*result) map[string]tailnetCount {
	m := map[string]tailnetCount{}

	for _, r := range rs {
		c := m[r.Tailnet]
		c.Nodes++

		if r.Total > 0 && r.Err == "" {
			c.Connected++
		}

		m[r.Tailnet] = c
	}

	return m
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

func unix(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 3, 64)
}

func writeCSV(path string, header []string, rows func(w *csv.Writer)) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	_ = w.Write(header)

	rows(w)
	w.Flush()

	return w.Error()
}

func writeResults(dir string, rs []*result, observers []*observer, recs []probeRec, s summary) error {
	err := writeCSV(filepath.Join(dir, "nodes.csv"),
		[]string{"index", "phase", "tailnet", "register_ms", "first_map_ms", "total_ms", "peers", "frames", "bytes", "enrolled_unix", "observer", "err"},
		func(w *csv.Writer) {
			for _, r := range rs {
				_ = w.Write([]string{
					strconv.Itoa(r.Index), r.Phase, r.Tailnet, ms(r.Register), ms(r.FirstMap), ms(r.Total), strconv.Itoa(r.Peers),
					strconv.FormatInt(r.frames.Load(), 10), strconv.FormatInt(r.bytes.Load(), 10), unix(r.enrolledAt),
					strconv.FormatBool(r.obs != nil), errClass(r.Err),
				})
			}
		})
	if err != nil {
		return err
	}

	err = writeCSV(filepath.Join(dir, "frames.csv"),
		[]string{"observer", "tailnet", "t_unix", "bytes", "peers", "peers_changed", "patches", "removed", "keepalive", "filter"},
		func(w *csv.Writer) {
			for _, o := range observers {
				o.mu.Lock()

				for _, f := range o.recs {
					_ = w.Write([]string{
						strconv.Itoa(o.r.Index), o.r.Tailnet, unix(f.at), strconv.Itoa(f.bytes), strconv.Itoa(f.peers),
						strconv.Itoa(f.changed), strconv.Itoa(f.patches), strconv.Itoa(f.removed),
						strconv.FormatBool(f.keepAlive), strconv.FormatBool(f.filter),
					})
				}

				o.mu.Unlock()
			}
		})
	if err != nil {
		return err
	}

	err = writeCSV(filepath.Join(dir, "probes.csv"),
		[]string{"probe", "kind", "source", "tailnet", "sent_unix", "observer", "latency_ms"},
		func(w *csv.Writer) {
			for _, p := range recs {
				lat := "-1"
				if p.latency >= 0 {
					lat = ms(p.latency)
				}

				_ = w.Write([]string{
					strconv.Itoa(p.probe), p.kind, strconv.Itoa(p.source), p.tailnet, unix(p.sent), strconv.Itoa(p.observer), lat,
				})
			}
		})
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o600)
}

// saveNodeKeys writes the keys of every node that got a first map, for a
// later -node-keys run on a snapshot of this database (0600: lab test
// credentials of the clients).
func saveNodeKeys(path string, rs []*result) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(f)

	for _, r := range rs {
		if r.Total <= 0 || r.Err != "" {
			continue
		}

		m, _ := r.machine.MarshalText()
		n, _ := r.node.MarshalText()

		if r.Tailnet != "" {
			fmt.Fprintf(w, "%s %s %s\n", m, n, r.Tailnet)
		} else {
			fmt.Fprintf(w, "%s %s\n", m, n)
		}
	}

	err = w.Flush()
	if err != nil {
		f.Close()

		return err
	}

	return f.Close()
}
