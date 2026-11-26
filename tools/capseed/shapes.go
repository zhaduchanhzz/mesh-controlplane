package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
)

// Policy shapes of the P14 benchmark plan (evidence/P14-benchmark-plan.md
// §3). Every benchmark tailnet gets an explicit policy: a tenant policy
// without rules denies everything (AGENTS rule 8).
const (
	shapeAllowAll        = "allow_all"
	shapeIsolated        = "isolated"
	shapePersonalDevices = "personal_devices"
	shapeTagHeavy        = "tag_heavy"
	shapeMix             = "mix"
)

// tag_heavy dimensions (§3): 100 role tags, 20 environment tags, every
// tagged node has one of each; 70 % of the nodes are tagged; each role
// reaches 3 other roles on tcp:22,443 (300 grants); 50 groups of 2 users,
// each group reaching 5 role tags.
const (
	tagRoles        = 100
	tagEnvs         = 20
	tagFanout       = 3
	tagGroups       = 50
	tagGroupMembers = 2
	tagGroupTags    = 5
	tagShare        = 0.7
)

var errShape = errors.New("unknown policy shape")

func validShape(s string, mixOK bool) error {
	switch s {
	case shapeAllowAll, shapeIsolated, shapePersonalDevices, shapeTagHeavy:
		return nil
	case shapeMix:
		if mixOK {
			return nil
		}
	}

	return fmt.Errorf("%w %q", errShape, s)
}

// mixShape picks the shape of one tailnet of a mixed cell: 70 %
// personal_devices, 20 % tag_heavy, 10 % allow_all, in seed order.
func mixShape(r *rand.Rand) string {
	switch x := r.Float64(); {
	case x < 0.7:
		return shapePersonalDevices
	case x < 0.9:
		return shapeTagHeavy
	default:
		return shapeAllowAll
	}
}

// layout is how the devices of one tailnet split into user-owned and tagged
// nodes.
type layout struct {
	userNodes, taggedNodes, users int
}

func layoutFor(shape string, devices, perUser int) layout {
	var l layout

	if shape == shapeTagHeavy {
		l.taggedNodes = int(float64(devices)*tagShare + 0.5)
	}

	l.userNodes = devices - l.taggedNodes
	l.users = (l.userNodes + perUser - 1) / perUser

	// tag_heavy needs users for its groups and tag owners even when every
	// node is tagged.
	if shape == shapeTagHeavy && l.users == 0 {
		l.users = 1
	}

	return l
}

func userName(u int) string { return fmt.Sprintf("u%04d", u) }

func roleTag(r int) string { return fmt.Sprintf("tag:r%03d", r) }

func envTag(e int) string { return fmt.Sprintf("tag:e%02d", e) }

// taggedKey is the tag set of tagged node t: role t%100, environment
// (t/100)%20.
func taggedKey(t int) (int, int) { return t % tagRoles, (t / tagRoles) % tagEnvs }

type grant struct {
	Src []string `json:"src"`
	Dst []string `json:"dst"`
	IP  []string `json:"ip"`
}

type policyDoc struct {
	Groups    map[string][]string `json:"groups,omitempty"`
	TagOwners map[string][]string `json:"tagOwners,omitempty"`
	Grants    []grant             `json:"grants,omitempty"`
	ACLs      *[]struct{}         `json:"acls,omitempty"`
}

// policyFor renders the policy of one tailnet. users is the number of users
// the tailnet has (names u0000…); r drives the tag_heavy role graph.
func policyFor(shape string, users int, r *rand.Rand) ([]byte, error) {
	var doc policyDoc

	switch shape {
	case shapeAllowAll:
		doc.Grants = []grant{{Src: []string{"*"}, Dst: []string{"*"}, IP: []string{"tcp:443"}}}
	case shapeIsolated:
		// {"acls": []}: no rule at all (control shape: same-user visibility
		// only where the server grants it).
		doc.ACLs = &[]struct{}{}
	case shapePersonalDevices:
		doc.Grants = []grant{{Src: []string{"autogroup:member"}, Dst: []string{"autogroup:self"}, IP: []string{"tcp:443"}}}
	case shapeTagHeavy:
		doc = tagHeavyPolicy(users, r)
	default:
		return nil, fmt.Errorf("%w %q", errShape, shape)
	}

	return json.Marshal(doc)
}

func tagHeavyPolicy(users int, r *rand.Rand) policyDoc {
	doc := policyDoc{
		Groups:    map[string][]string{"group:admin": {userName(0) + "@"}},
		TagOwners: map[string][]string{},
	}

	for i := range tagRoles {
		doc.TagOwners[roleTag(i)] = []string{"group:admin"}
	}

	for i := range tagEnvs {
		doc.TagOwners[envTag(i)] = []string{"group:admin"}
	}

	ports := []string{"tcp:22", "tcp:443"}

	for i := range tagRoles {
		for _, j := range pickDistinct(r, tagRoles, tagFanout, i) {
			doc.Grants = append(doc.Grants, grant{Src: []string{roleTag(i)}, Dst: []string{roleTag(j)}, IP: ports})
		}
	}

	for g := range tagGroups {
		name := fmt.Sprintf("group:g%02d", g)

		var members []string

		for m := range tagGroupMembers {
			u := userName((g*tagGroupMembers + m) % users)
			if !slices.Contains(members, u+"@") {
				members = append(members, u+"@")
			}
		}

		doc.Groups[name] = members

		dst := make([]string, 0, tagGroupTags)
		for _, j := range pickDistinct(r, tagRoles, tagGroupTags, -1) {
			dst = append(dst, roleTag(j))
		}

		doc.Grants = append(doc.Grants, grant{Src: []string{name}, Dst: dst, IP: []string{"tcp:443"}})
	}

	return doc
}

// pickDistinct returns k distinct values in [0, n), none equal to not.
func pickDistinct(r *rand.Rand, n, k, not int) []int {
	out := make([]int, 0, k)
	for len(out) < k {
		j := r.IntN(n)
		if j != not && !slices.Contains(out, j) {
			out = append(out, j)
		}
	}

	slices.Sort(out)

	return out
}

// probeGrant is the grant the policy probe (capseed -mode policy-flip) adds
// and removes: valid in every shape, and it changes the packet filter of
// every user-owned node.
var probeGrant = grant{Src: []string{"autogroup:member"}, Dst: []string{"autogroup:self"}, IP: []string{"tcp:9999"}}
