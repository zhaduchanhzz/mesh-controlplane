//go:build p05cluster

package servertest

import (
	"context"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// P09, S04: a tailnet created through the registry and activated while
// clients race to enroll and the operator writes its first policy.

// CreateTailnetPending creates slug through the registry on this cluster
// (PENDING, owner epoch 1, deny-all policy in the same transaction) and
// returns its ID. It serves nothing until ActivateTailnet.
func (tc *TestCluster) CreateTailnetPending(tb testing.TB, slug string) tailnet.ID {
	tb.Helper()

	tn, err := tc.Registry(tb).CreateTailnetOn(context.Background(), TestOperator, slug, tc.ClusterID)
	if err != nil {
		tb.Fatalf("servertest: create %q: %v", slug, err)
	}

	id := tailnet.MustParseID(tn.ID)
	tc.tailnets[slug] = &SeededTailnet{ID: id, Slug: slug}

	return id
}

// ActivateTailnet makes slug ACTIVE through the registry at epoch 1.
func (tc *TestCluster) ActivateTailnet(tb testing.TB, slug string) {
	tb.Helper()

	_, err := tc.Registry(tb).ActivateTailnet(context.Background(), TestOperator, tc.Tailnet(tb, slug).ID.String(), 1)
	if err != nil {
		tb.Fatalf("servertest: activate %q: %v", slug, err)
	}
}

// TenantHandleErr is the error of opening slug's tenant handle on the
// shared database, as anything minting a credential must (nil when allowed).
func (tc *TestCluster) TenantHandleErr(tb testing.TB, slug string) error {
	tb.Helper()

	hs, err := tc.Platform(tb).TenantDB(tc.Tailnet(tb, slug).ID)
	if err == nil {
		_ = hs.Close()
	}

	return err
}

// MintTenantKey creates user in slug through its tenant handle (no runtime
// needed) and returns a reusable pre-auth key of that user.
func (tc *TestCluster) MintTenantKey(tb testing.TB, slug, user string) string {
	tb.Helper()

	hs, err := tc.Platform(tb).TenantDB(tc.Tailnet(tb, slug).ID)
	if err != nil {
		tb.Fatalf("servertest: tenant handle of %q: %v", slug, err)
	}
	defer hs.Close()

	u, err := hs.CreateUser(types.User{Name: user})
	if err != nil {
		tb.Fatalf("servertest: user %q in %q: %v", user, slug, err)
	}

	uid := types.UserID(u.ID)

	k, err := hs.CreatePreAuthKey(&uid, true, false, nil, nil)
	if err != nil {
		tb.Fatalf("servertest: key in %q: %v", slug, err)
	}

	return k.Key
}
