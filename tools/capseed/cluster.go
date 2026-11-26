package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/config"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/policy"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// Cluster mode (P14a.0): provision T tailnets on a running cluster process
// through the operator paths of `headscale cluster tailnet …` (placement
// registry create on a cluster holding its lease, ACTIVE, users and pre-auth
// keys through TenantDB, a policy validated with tenant semantics then
// stored with a policy revision bump). Nodes are NOT written here: capload
// enrolls them over Noise with these keys (the real registration path, with
// the device quota and enroll gate). A running runtime loads users only at
// load time, so restart the cluster after capseed (server.sh cluster-restart)
// before enrolling.
//
// Output in -out (mode 0700):
//   - plan.tsv (0600, lab test credentials): one line per planned device,
//     "slug<TAB>tailnet-id<TAB>phase<TAB>hostname<TAB>pre-auth-key", phase
//     enroll or register; capload -plan reads it.
//   - tailnets.json: slug, id, shape, devices, users, tagged, keys, sha256
//     of the policy; policies/<slug>.json. No secrets.

var (
	errClusterFlags = errors.New("cluster mode needs -config, -cluster-id and -out")
	errNoPolicy     = errors.New("tailnet has no stored policy")
	errNoUsers      = errors.New("register-phase devices need a tailnet with users (devices > 0)")
)

type clusterOpts struct {
	cfgPath, clusterID, out, shape, prefix, bigShape, slug string
	tailnets, devices, perUser, extra, big, conc           int
	seed                                                   uint64
	keyTTL                                                 time.Duration
}

type tailnetPlan struct {
	Slug         string  `json:"slug"`
	ID           string  `json:"id"`
	Shape        string  `json:"shape"`
	Devices      int     `json:"devices"`
	Extra        int     `json:"extra"`
	Users        int     `json:"users"`
	Tagged       int     `json:"tagged"`
	Keys         int     `json:"keys"`
	PolicySHA256 string  `json:"policy_sha256"`
	ProvisionSec float64 `json:"provision_s"`

	lines []planLine
}

type planLine struct{ phase, hostname, key string }

func clusterConfig(path, clusterID string) (*types.Config, error) {
	loaded, err := config.Load(path, true, os.Environ())
	if err != nil {
		return nil, err
	}

	cfg := loaded.Config
	cfg.CLI.APIKey = ""
	cfg.ClusterID = clusterID
	cfg.TailnetID = ""
	cfg.PolicyEmptyDenies = true

	return cfg, nil
}

func runCluster(o clusterOpts) error {
	if o.cfgPath == "" || o.clusterID == "" || o.out == "" {
		return errClusterFlags
	}

	err := validShape(o.shape, true)
	if err != nil {
		return err
	}

	if o.big > 0 {
		err = validShape(o.bigShape, false)
		if err != nil {
			return err
		}
	}

	cfg, err := clusterConfig(o.cfgPath, o.clusterID)
	if err != nil {
		return err
	}

	p, err := db.OpenPlatform(cfg)
	if err != nil {
		return err
	}
	defer p.Close()

	reg, err := placement.New(p.RegistryDB(), placement.Config{})
	if err != nil {
		return err
	}

	err = os.MkdirAll(filepath.Join(o.out, "policies"), 0o700)
	if err != nil {
		return err
	}

	rng := rand.New(rand.NewPCG(o.seed, 0x9e3779b97f4a7c15)) //nolint:gosec // benchmark layout, not security

	var plans []*tailnetPlan

	if o.big > 0 {
		plans = append(plans, &tailnetPlan{Slug: o.prefix + "-big", Shape: o.bigShape, Devices: o.big})
	}

	for i := range o.tailnets {
		shape := o.shape
		if shape == shapeMix {
			shape = mixShape(rng)
		}

		plans = append(plans, &tailnetPlan{Slug: fmt.Sprintf("%s-%04d", o.prefix, i), Shape: shape, Devices: o.devices})
	}

	// Register-phase devices, round-robin over the tailnets.
	for i := range o.extra {
		plans[i%len(plans)].Extra++
	}

	t0 := time.Now()

	err = forEach(plans, o.conc, func(i int, tp *tailnetPlan) error {
		// Each tailnet its own generator: the layout does not depend on
		// the order the workers run in.
		r := rand.New(rand.NewPCG(o.seed, uint64(i)+1)) //nolint:gosec // benchmark layout

		return provision(p, reg, o, tp, r)
	})
	if err != nil {
		return err
	}

	err = writePlan(o.out, plans)
	if err != nil {
		return err
	}

	total, extra := 0, 0
	for _, tp := range plans {
		total += tp.Devices
		extra += tp.Extra
	}

	fmt.Printf("provisioned tailnets=%d devices=%d extra=%d provision_s=%.1f\n", //nolint:forbidigo // CLI output
		len(plans), total, extra, time.Since(t0).Seconds())

	return nil
}

// forEach runs fn on every plan, conc at a time; the first error wins.
func forEach(plans []*tailnetPlan, conc int, fn func(int, *tailnetPlan) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)

	jobs := make(chan int)

	for range max(conc, 1) {
		wg.Go(func() {
			for i := range jobs {
				err := fn(i, plans[i])
				if err != nil {
					mu.Lock()
					first = errors.Join(first, fmt.Errorf("tailnet %s: %w", plans[i].Slug, err))
					mu.Unlock()
				}
			}
		})
	}

	for i := range plans {
		jobs <- i
	}

	close(jobs)
	wg.Wait()

	return first
}

func provision(p *db.Platform, reg *placement.Registry, o clusterOpts, tp *tailnetPlan, r *rand.Rand) error {
	t0 := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tn, err := reg.CreateTailnetOn(ctx, placement.Op{Actor: "capseed:" + o.clusterID}, tp.Slug, o.clusterID)
	if err != nil {
		return err
	}

	tp.ID = tn.ID

	id, err := tailnet.ParseID(tn.ID)
	if err != nil {
		return err
	}

	err = p.SetTailnetStatus(id, types.TailnetActive) //nolint:contextcheck // Platform bounds its own calls
	if err != nil {
		return err
	}

	hs, err := p.TenantDB(id) //nolint:contextcheck // Platform bounds its own calls
	if err != nil {
		return err
	}

	l := layoutFor(tp.Shape, tp.Devices, o.perUser)
	tp.Users, tp.Tagged = l.users, l.taggedNodes
	exp := time.Now().Add(o.keyTTL)

	userKeys := make([]string, l.users)

	for u := range l.users {
		user, err := hs.CreateUser(types.User{Name: userName(u)})
		if err != nil {
			return fmt.Errorf("user %d: %w", u, err)
		}

		uid := types.UserID(user.ID)

		k, err := hs.CreatePreAuthKey(&uid, true, false, &exp, nil)
		if err != nil {
			return fmt.Errorf("key of user %d: %w", u, err)
		}

		userKeys[u] = k.Key
	}

	tagKeys := map[[2]int]string{}

	for n := range l.userNodes {
		tp.lines = append(tp.lines, planLine{"enroll", fmt.Sprintf("%s-u%05d", tp.Slug, n), userKeys[n/o.perUser]})
	}

	for t := range l.taggedNodes {
		role, env := taggedKey(t)

		k, ok := tagKeys[[2]int{role, env}]
		if !ok {
			pk, err := hs.CreatePreAuthKey(nil, true, false, &exp, []string{roleTag(role), envTag(env)})
			if err != nil {
				return fmt.Errorf("tagged key %d/%d: %w", role, env, err)
			}

			k = pk.Key
			tagKeys[[2]int{role, env}] = k
		}

		tp.lines = append(tp.lines, planLine{"enroll", fmt.Sprintf("%s-t%05d", tp.Slug, t), k})
	}

	if tp.Extra > 0 && len(userKeys) == 0 {
		return errNoUsers
	}

	for e := range tp.Extra {
		tp.lines = append(tp.lines, planLine{"register", fmt.Sprintf("%s-x%05d", tp.Slug, e), userKeys[e%len(userKeys)]})
	}

	tp.Keys = len(userKeys) + len(tagKeys)

	pol, err := policyFor(tp.Shape, l.users, r)
	if err != nil {
		return err
	}

	err = storePolicy(p, id, hs, pol)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(pol)
	tp.PolicySHA256 = hex.EncodeToString(sum[:])
	tp.ProvisionSec = time.Since(t0).Seconds()

	return os.WriteFile(filepath.Join(o.out, "policies", tp.Slug+".json"), pol, 0o600)
}

// storePolicy is `headscale cluster tailnet policy --file`: validated with
// tenant semantics against the tailnet's users and nodes, stored, revision
// bumped so a running cluster reloads it.
func storePolicy(p *db.Platform, id tailnet.ID, hs *db.HSDatabase, pol []byte) error {
	users, err := hs.ListUsers(nil)
	if err != nil {
		return err
	}

	nodes, err := hs.ListNodes()
	if err != nil {
		return err
	}

	err = policy.ValidatePolicy(pol, users, nodes.ViewSlice(), policy.WithEmptyPolicyDenies(true))
	if err != nil {
		return fmt.Errorf("policy rejected: %w", err)
	}

	_, err = hs.SetPolicy(string(pol))
	if err != nil {
		return err
	}

	return p.BumpPolicyRevision(id)
}

func writePlan(out string, plans []*tailnetPlan) error {
	err := writeLines(filepath.Join(out, "plan.tsv"), func(w *bufio.Writer) {
		for _, tp := range plans {
			for _, l := range tp.lines {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", tp.Slug, tp.ID, l.phase, l.hostname, l.key)
			}
		}
	})
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(plans, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(out, "tailnets.json"), b, 0o600)
}

// runPolicyFlip adds the probe grant to the stored policy of tailnet slug,
// or removes it when present (P14a policy-change probe; capload -policy-cmd
// runs it). It goes through the same validation and revision bump as
// `tailnet policy --file`, so the measured latency includes the cluster's
// reconcile poll, as an operator edit would.
func runPolicyFlip(cfgPath, clusterID, slug string) error {
	cfg, err := clusterConfig(cfgPath, clusterID)
	if err != nil {
		return err
	}

	p, err := db.OpenPlatform(cfg)
	if err != nil {
		return err
	}
	defer p.Close()

	tn, err := p.TailnetBySlug(slug)
	if err != nil {
		return err
	}

	id, err := tailnet.ParseID(tn.ID)
	if err != nil {
		return err
	}

	hs, err := p.TenantDB(id)
	if err != nil {
		return err
	}

	cur, err := hs.GetPolicy()
	if err != nil {
		return fmt.Errorf("%w: %w", errNoPolicy, err)
	}

	var doc map[string]any

	err = json.Unmarshal([]byte(cur.Data), &doc)
	if err != nil {
		return err
	}

	grants, _ := doc["grants"].([]any)
	isProbe := func(g any) bool {
		m, ok := g.(map[string]any)
		if !ok {
			return false
		}

		ip, _ := m["ip"].([]any)

		return len(ip) == 1 && ip[0] == probeGrant.IP[0]
	}

	added := !slices.ContainsFunc(grants, isProbe)
	if added {
		grants = append(grants, probeGrant)
	} else {
		grants = slices.DeleteFunc(grants, isProbe)
	}

	doc["grants"] = grants

	pol, err := json.Marshal(doc)
	if err != nil {
		return err
	}

	err = storePolicy(p, id, hs, pol)
	if err != nil {
		return err
	}

	fmt.Printf("policy-flip slug=%s probe_grant_added=%t\n", slug, added) //nolint:forbidigo // CLI output

	return nil
}
