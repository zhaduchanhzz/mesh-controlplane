// Command capseed builds a seeded tailnet for the P00c server-only harness:
// users, one reusable pre-auth key per user and N registered nodes, written
// through the same State paths the server uses (State.CreateUser,
// State.CreatePreAuthKey, State.HandleNodeFromPreAuthKey), so the schema,
// constraints, IP allocation and NodeStore checks all apply. No SQL.
//
// It writes, into -out (mode 0700):
//   - nodes.keys: per node "machine-private node-private", the client-side
//     key fixtures capload -connect uses to open map sessions for the
//     pre-registered nodes without registering again;
//   - users.keys: one reusable pre-auth key per user, for registration
//     samples on top of the seeded tailnet.
//
// Both are lab test fixtures: they stay in the bench working directory and
// are never committed or printed. The database is the server's own file
// from the config; copy it after capseed exits to make the snapshot.
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

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

var errRegistrations = errors.New("registrations failed")

func main() {
	var (
		cfgPath     string
		out         string
		nodes       int
		perUser     int
		concurrency int
	)

	flag.StringVar(&cfgPath, "config", "", "headscale config (database, prefixes, policy)")
	flag.StringVar(&out, "out", "", "directory for nodes.keys and users.keys")
	flag.IntVar(&nodes, "nodes", 5000, "nodes to register")
	flag.IntVar(&perUser, "per-user", 10, "nodes per user")
	flag.IntVar(&concurrency, "concurrency", 256, "concurrent registrations")
	flag.Parse()

	if cfgPath == "" || out == "" {
		log.Fatal("capseed: -config and -out are required")
	}

	err := run(cfgPath, out, nodes, perUser, concurrency)
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
