package main

import (
	"encoding/json"
	"math/rand/v2"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/policy"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// TestShapesValidateAsTenantPolicies: every generated policy passes the
// validation `cluster tailnet policy --file` applies (tenant semantics), at
// the tailnet sizes of the benchmark matrix, and the policy probe grant
// keeps it valid.
func TestShapesValidateAsTenantPolicies(t *testing.T) {
	for _, shape := range []string{shapeAllowAll, shapeIsolated, shapePersonalDevices, shapeTagHeavy, shapeSharedService} {
		for _, devices := range []int{1, 10, 50, 200, 5000} {
			l := layoutFor(shape, devices, 10)

			users := make([]types.User, l.users)
			for u := range users {
				users[u] = types.User{ID: uint(u + 1), Name: userName(u)} //nolint:gosec // small
			}

			pol, err := policyFor(shape, l.users, rand.New(rand.NewPCG(1, 2))) //nolint:gosec // test layout
			if err != nil {
				t.Fatal(err)
			}

			err = policy.ValidatePolicy(pol, users, types.Nodes{}.ViewSlice(), policy.WithEmptyPolicyDenies(true))
			if err != nil {
				t.Fatalf("%s/%d: %v\n%s", shape, devices, err, pol)
			}

			var doc map[string]any
			if err := json.Unmarshal(pol, &doc); err != nil { //nolint:noinlineerr
				t.Fatal(err)
			}

			grants, _ := doc["grants"].([]any)
			doc["grants"] = append(grants, probeGrant)

			flipped, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}

			err = policy.ValidatePolicy(flipped, users, types.Nodes{}.ViewSlice(), policy.WithEmptyPolicyDenies(true))
			if err != nil {
				t.Fatalf("%s/%d with the probe grant: %v", shape, devices, err)
			}

			if l.userNodes+l.taggedNodes != devices {
				t.Fatalf("%s/%d: layout %+v", shape, devices, l)
			}
		}
	}
}

// TestTagHeavyDimensions pins the tag_heavy shape of the benchmark plan §3.
func TestTagHeavyDimensions(t *testing.T) {
	doc := tagHeavyPolicy(300, rand.New(rand.NewPCG(1, 2))) //nolint:gosec // test layout

	if got, want := len(doc.TagOwners), tagRoles+tagEnvs; got != want {
		t.Errorf("tagOwners %d, want %d", got, want)
	}

	if got, want := len(doc.Grants), tagRoles*tagFanout+tagGroups; got != want {
		t.Errorf("grants %d, want %d", got, want)
	}

	if got, want := len(doc.Groups), tagGroups+1; got != want {
		t.Errorf("groups %d, want %d (with group:admin)", got, want)
	}

	if l := layoutFor(shapeTagHeavy, 1000, 10); l.taggedNodes != 700 || l.users != 30 {
		t.Errorf("layout %+v, want 700 tagged and 30 users", l)
	}
}
