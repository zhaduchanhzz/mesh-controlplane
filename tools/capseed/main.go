// Command capseed seeds tailnets for the server-only capacity harness.
//
// -mode serve (P00c, default) builds one seeded tailnet for `headscale
// serve` on SQLite: users, one reusable pre-auth key per user and N
// registered nodes, written through the same State paths the server uses
// (State.CreateUser, State.CreatePreAuthKey, State.HandleNodeFromPreAuthKey),
// so the schema, constraints, IP allocation and NodeStore checks all apply.
// No SQL. It writes, into -out (mode 0700):
//   - nodes.keys: per node "machine-private node-private", the client-side
//     key fixtures capload -node-keys uses to open map sessions for the
//     pre-registered nodes without registering again;
//   - users.keys: one reusable pre-auth key per user, for registration
//     samples on top of the seeded tailnet.
//
// -mode cluster (P14a.0) provisions T tailnets with their users, keys and
// policy on a running cluster process (see cluster.go); capload enrolls the
// devices. -mode policy-flip toggles the probe grant in one tailnet's policy.
//
// Keys are lab test fixtures: they stay in the bench working directory and
// are never committed or printed. In serve mode the database is the
// server's own file from the config; copy it after capseed exits to make
// the snapshot.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

var errRegistrations = errors.New("registrations failed")

func main() {
	var (
		mode        string
		cfgPath     string
		out         string
		nodes       int
		perUser     int
		concurrency int
		co          clusterOpts
	)

	flag.StringVar(&mode, "mode", "serve", "serve (P00c, SQLite, one tailnet), cluster (P14a) or policy-flip")
	flag.StringVar(&cfgPath, "config", "", "headscale config (database, prefixes, policy)")
	flag.StringVar(&out, "out", "", "output directory")
	flag.IntVar(&nodes, "nodes", 5000, "serve: nodes to register")
	flag.IntVar(&perUser, "per-user", 10, "nodes per user")
	flag.IntVar(&concurrency, "concurrency", 256, "serve: concurrent registrations; cluster: tailnets provisioned at once")
	flag.StringVar(&co.clusterID, "cluster-id", "", "cluster: ID of the (running) cluster that owns the tailnets")
	flag.IntVar(&co.tailnets, "tailnets", 1, "cluster: number of tailnets")
	flag.IntVar(&co.devices, "devices", 100, "cluster: devices per tailnet")
	flag.StringVar(&co.shape, "policy", shapePersonalDevices, "cluster: allow_all, isolated, personal_devices, shared_service, tag_heavy or mix (70/20/10)")
	flag.IntVar(&co.big, "big", 0, "cluster: also one large tailnet <prefix>-big with this many devices (mixed shared cell)")
	flag.StringVar(&co.bigShape, "big-policy", shapePersonalDevices, "cluster: policy of the large tailnet")
	flag.IntVar(&co.extra, "extra", 0, "cluster: register-phase devices, round-robin over the tailnets (user keys)")
	flag.StringVar(&co.prefix, "prefix", "bench", "cluster: tailnet slug prefix")
	flag.Uint64Var(&co.seed, "seed", 1, "cluster: seed of the layout (mix shapes, tag_heavy role graph)")
	flag.DurationVar(&co.keyTTL, "key-ttl", 72*time.Hour, "cluster: pre-auth key lifetime")
	flag.StringVar(&co.slug, "slug", "", "policy-flip: tailnet slug")
	flag.Parse()

	// The library logs policy parsing at debug level; a seeding tool wants
	// warnings and errors only.
	zerolog.SetGlobalLevel(zerolog.WarnLevel)

	var err error

	switch mode {
	case "serve":
		if cfgPath == "" || out == "" {
			log.Fatal("capseed: -config and -out are required")
		}

		err = run(cfgPath, out, nodes, perUser, concurrency)
	case "cluster":
		co.cfgPath, co.out, co.perUser = cfgPath, out, perUser
		co.conc = min(concurrency, 16)
		err = runCluster(co)
	case "policy-flip":
		err = runPolicyFlip(cfgPath, co.clusterID, co.slug)
	default:
		log.Fatalf("capseed: unknown -mode %q", mode)
	}

	if err != nil {
		log.Fatalf("capseed: %v", err)
	}
}

func run(cfgPath, out string, nodes, perUser, concurrency int) error {
	loaded, err := config.Load(cfgPath, true, os.Environ())
	if err != nil {
		return err
	}

	cfg := loaded.Config

	err = config.Validate(cfg)
	if err != nil {
		return err
	}

	cfg.StartDNS(nil)

	st, err := state.NewState(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	err = os.MkdirAll(out, 0o700)
	if err != nil {
		return err
	}

	users := (nodes + perUser - 1) / perUser
	authKeys := make([]string, users)
	expiry := time.Now().Add(30 * 24 * time.Hour)
	t0 := time.Now()

	for u := range users {
		user, _, err := st.CreateUser(types.User{Name: fmt.Sprintf("cap-user-%d", u)})
		if err != nil {
			return fmt.Errorf("user %d: %w", u, err)
		}

		uid := types.UserID(user.ID)

		pak, err := st.CreatePreAuthKey(&uid, true, false, &expiry, nil)
		if err != nil {
			return fmt.Errorf("key %d: %w", u, err)
		}

		authKeys[u] = pak.Key
	}

	tUsers := time.Since(t0)

	type nodeKeys struct{ machine, node []byte }

	keys := make([]nodeKeys, nodes)
	jobs := make(chan int)

	var (
		wg     sync.WaitGroup
		failed atomic.Int64
		first  atomic.Value
	)

	t1 := time.Now()

	for range concurrency {
		wg.Go(func() {
			for i := range jobs {
				mk, nk := key.NewMachine(), key.NewNode()
				req := tailcfg.RegisterRequest{
					Version: tailcfg.CurrentCapabilityVersion,
					NodeKey: nk.Public(),
					Auth:    &tailcfg.RegisterResponseAuth{AuthKey: authKeys[i/perUser]},
					Hostinfo: &tailcfg.Hostinfo{
						BackendLogID: fmt.Sprintf("capseed-%d", i),
						Hostname:     fmt.Sprintf("cap-node-%05d", i),
					},
				}

				_, _, err := st.HandleNodeFromPreAuthKey(req, mk.Public())
				if err != nil {
					failed.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("node %d: %v", i, err))

					continue
				}

				m, _ := mk.MarshalText()
				n, _ := nk.MarshalText()
				keys[i] = nodeKeys{machine: m, node: n}
			}
		})
	}

	for i := range nodes {
		jobs <- i
	}

	close(jobs)
	wg.Wait()

	if f := failed.Load(); f > 0 {
		return fmt.Errorf("%d %w, first: %v", f, errRegistrations, first.Load())
	}

	tNodes := time.Since(t1)

	err = writeLines(filepath.Join(out, "users.keys"), func(w *bufio.Writer) {
		for _, k := range authKeys {
			fmt.Fprintln(w, k)
		}
	})
	if err != nil {
		return err
	}

	err = writeLines(filepath.Join(out, "nodes.keys"), func(w *bufio.Writer) {
		for _, k := range keys {
			fmt.Fprintf(w, "%s %s\n", k.machine, k.node)
		}
	})
	if err != nil {
		return err
	}

	fmt.Printf("seeded users=%d nodes=%d users_s=%.1f nodes_s=%.1f\n",
		users, nodes, tUsers.Seconds(), tNodes.Seconds())

	return nil
}

func writeLines(path string, fill func(*bufio.Writer)) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(f)
	fill(w)

	err = w.Flush()
	if err != nil {
		f.Close()
		return err
	}

	return f.Close()
}
