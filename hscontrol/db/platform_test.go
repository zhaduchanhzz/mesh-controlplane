package db

import (
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"tailscale.com/types/key"
)

func openPlatform(t *testing.T, cfg *types.Config) *Platform {
	t.Helper()

	p, err := OpenPlatform(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	return p
}

// platformTailnet inserts a tailnet row through the platform handle, as the
// future CreateTailnet API will.
func platformTailnet(t *testing.T, p *Platform, cluster string, status types.TailnetStatus, deleted bool) tailnet.ID {
	t.Helper()

	id := uuid.NewV4().String()
	now := time.Now().UTC()

	tn := &types.Tailnet{
		ID: id, Slug: "tn-" + id[:8], Status: status,
		OwnerClusterID: cluster, OwnerEpoch: 1, PolicyRevision: 1, ConfigRevision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if deleted {
		tn.DeletedAt = &now
	}

	require.NoError(t, p.db.Create(tn).Error)

	return tailnet.MustParseID(id)
}

func tenantDB(t *testing.T, p *Platform, id tailnet.ID) *HSDatabase {
	t.Helper()

	hsdb, err := p.TenantDB(id)
	require.NoError(t, err)

	return hsdb
}

func addNode(t *testing.T, hsdb *HSDatabase, mk key.MachinePublic, nk key.NodePublic) error {
	t.Helper()

	u, err := hsdb.CreateUser(types.User{Name: "u-" + uuid.NewV4().String()[:8]})
	require.NoError(t, err)

	return hsdb.DB.Create(&types.Node{
		MachineKey: mk, NodeKey: nk, Hostname: "laptop",
		UserID: &u.ID, RegisterMethod: util.RegisterMethodAuthKey,
	}).Error
}

func TestPlatformTailnetForAuthKey(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)

			keys := map[tailnet.ID]string{}

			for _, id := range []tailnet.ID{idA, idB} {
				hsdb := tenantDB(t, p, id)
				u, err := hsdb.CreateUser(types.User{Name: "alice"})
				require.NoError(t, err)

				uid := types.UserID(u.ID)
				pak, err := hsdb.CreatePreAuthKey(&uid, true, false, nil, nil)
				require.NoError(t, err)

				keys[id] = pak.Key
			}

			for id, k := range keys {
				got, err := p.TailnetForAuthKey(k)
				require.NoError(t, err)
				assert.Equal(t, id, got)
			}

			// Right identifier, wrong secret: unknown, like an unknown
			// identifier, so the caller cannot tell which half was wrong.
			k := keys[idA]
			last := k[len(k)-1]

			flip := byte('0')
			if last == '0' {
				flip = '1'
			}

			wrongSecret := k[:len(k)-1] + string(flip)
			unknownID := authKeyPrefix + strings.Repeat("a", keyIdentifierLength) + "-" + strings.Repeat("b", keySecretLength)

			for name, bad := range map[string]string{
				"wrong secret":       wrongSecret,
				"unknown identifier": unknownID,
				"empty":              "",
				"garbage":            "hskey-auth-xyz",
				"legacy unknown":     "0123456789abcdef",
			} {
				_, err := p.TailnetForAuthKey(bad)
				require.ErrorIs(t, err, ErrUnknownTenant, name)
			}
		})
	}
}

func TestPlatformTailnetForNode(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)

			// One device with a profile in each tailnet (S05, I03).
			mk := key.NewMachine().Public()
			nkA, nkB := key.NewNode().Public(), key.NewNode().Public()
			require.NoError(t, addNode(t, tenantDB(t, p, idA), mk, nkA))
			require.NoError(t, addNode(t, tenantDB(t, p, idB), mk, nkB))

			got, err := p.TailnetForNode(mk, nkA)
			require.NoError(t, err)
			assert.Equal(t, idA, got)

			got, err = p.TailnetForNode(mk, nkB)
			require.NoError(t, err)
			assert.Equal(t, idB, got)

			// The node key alone is not enough: the Noise machine key must match.
			_, err = p.TailnetForNode(key.NewMachine().Public(), nkA)
			require.ErrorIs(t, err, ErrUnknownTenant)

			_, err = p.TailnetForNode(mk, key.NewNode().Public())
			require.ErrorIs(t, err, ErrUnknownTenant)

			_, err = p.TailnetForNode(key.MachinePublic{}, key.NodePublic{})
			require.ErrorIs(t, err, ErrUnknownTenant)
		})
	}
}

func TestPlatformOwnedTailnets(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))

			owned, err := p.OwnedTailnets("local")
			require.NoError(t, err)
			assert.Empty(t, owned, "the platform never invents a default tailnet")

			a1 := platformTailnet(t, p, "local", types.TailnetActive, false)
			a2 := platformTailnet(t, p, "local", types.TailnetActive, false)
			platformTailnet(t, p, "local", types.TailnetPending, false)
			platformTailnet(t, p, "local", types.TailnetSuspended, false)
			platformTailnet(t, p, "local", types.TailnetActive, true)
			other := platformTailnet(t, p, "cluster-2", types.TailnetActive, false)

			owned, err = p.OwnedTailnets("local")
			require.NoError(t, err)

			ids := make([]string, 0, len(owned))
			for _, tn := range owned {
				ids = append(ids, tn.ID)
			}

			assert.ElementsMatch(t, []string{a1.String(), a2.String()}, ids)

			owned, err = p.OwnedTailnets("cluster-2")
			require.NoError(t, err)
			require.Len(t, owned, 1)
			assert.Equal(t, other.String(), owned[0].ID)

			_, err = p.OwnedTailnets("")
			require.Error(t, err, "an empty cluster id must not match anything")
		})
	}
}

func TestPlatformTenantDB(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))
			idA := platformTailnet(t, p, "local", types.TailnetActive, false)
			idB := platformTailnet(t, p, "local", types.TailnetActive, false)

			a := tenantDB(t, p, idA)
			bdb := tenantDB(t, p, idB)
			assert.Equal(t, idA.String(), a.Tailnet().ID)

			// One pool for every tenant (ADR 0007 §1).
			poolP, err := p.db.DB()
			require.NoError(t, err)
			poolA, err := a.DB.DB()
			require.NoError(t, err)
			poolB, err := bdb.DB.DB()
			require.NoError(t, err)
			assert.Same(t, poolP, poolA)
			assert.Same(t, poolP, poolB)

			_, err = a.CreateUser(types.User{Name: "alice"})
			require.NoError(t, err)
			users, err := bdb.ListUsers(nil)
			require.NoError(t, err)
			assert.Empty(t, users, "B does not see A's user")

			// Closing a tenant handle must not close the shared pool.
			require.NoError(t, a.Close())
			require.NoError(t, bdb.PingDB(t.Context()))
			require.NoError(t, p.Ping(t.Context()))
			_, err = p.OwnedTailnets("local")
			require.NoError(t, err)

			pending := platformTailnet(t, p, "local", types.TailnetPending, false)
			suspended := platformTailnet(t, p, "local", types.TailnetSuspended, false)
			deleted := platformTailnet(t, p, "local", types.TailnetActive, true)
			foreign := platformTailnet(t, p, "cluster-2", types.TailnetActive, false)

			_, err = p.TenantDB(pending)
			require.ErrorIs(t, err, ErrTailnetNotActive)
			_, err = p.TenantDB(suspended)
			require.ErrorIs(t, err, ErrTailnetNotActive)
			_, err = p.TenantDB(deleted)
			require.ErrorIs(t, err, ErrUnknownTenant)
			_, err = p.TenantDB(foreign)
			require.ErrorIs(t, err, ErrTailnetNotOwned)
			_, err = p.TenantDB(tailnet.MustParseID(uuid.NewV4().String()))
			require.ErrorIs(t, err, ErrUnknownTenant)
			_, err = p.TenantDB(tailnet.ID{})
			require.ErrorIs(t, err, ErrUnknownTenant)
		})
	}
}

// TestPlatformSchemaMatchesSingleTenant: the platform and the single-tenant
// path open the same database; NewHeadscaleDatabase still creates the
// default tailnet the platform then lists.
func TestPlatformSeesSingleTenantDatabase(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			hsdb := open(t, b.cfg(t, "", ""))
			p := openPlatform(t, b.cfg(t, "", ""))

			owned, err := p.OwnedTailnets("local")
			require.NoError(t, err)
			require.Len(t, owned, 1)
			assert.Equal(t, hsdb.Tailnet().ID, owned[0].ID)
		})
	}
}

// TestPlatformPostgresSimpleProtocol runs the platform and a tenant over the
// simple query protocol, the mode PgBouncer needs without prepared statement
// support (ADR 0006).
func TestPlatformPostgresSimpleProtocol(t *testing.T) {
	cfg := postgresConfig(t)
	cfg.Database.Postgres.QueryExecMode = types.PostgresQueryExecSimpleProtocol

	p := openPlatform(t, cfg)
	id := platformTailnet(t, p, "local", types.TailnetActive, false)
	hsdb := tenantDB(t, p, id)

	u, err := hsdb.CreateUser(types.User{Name: "alice"})
	require.NoError(t, err)

	uid := types.UserID(u.ID)
	pak, err := hsdb.CreatePreAuthKey(&uid, false, false, nil, nil)
	require.NoError(t, err)

	got, err := p.TailnetForAuthKey(pak.Key)
	require.NoError(t, err)
	assert.Equal(t, id, got)

	mk, nk := key.NewMachine().Public(), key.NewNode().Public()
	require.NoError(t, addNode(t, hsdb, mk, nk))
	got, err = p.TailnetForNode(mk, nk)
	require.NoError(t, err)
	assert.Equal(t, id, got)
}

// TestPlatformDeletingIsTerminal (S18): a tailnet in DELETING cannot be set
// back to ACTIVE, PENDING or SUSPENDED; setting DELETING again succeeds; a
// tailnet of another cluster is still "not owned".
func TestPlatformDeletingIsTerminal(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			p := openPlatform(t, b.cfg(t, "", ""))

			id := platformTailnet(t, p, "local", types.TailnetActive, false)
			require.NoError(t, p.SetTailnetStatus(id, types.TailnetSuspended))
			require.NoError(t, p.SetTailnetStatus(id, types.TailnetActive), "control: ACTIVE <-> SUSPENDED")
			require.NoError(t, p.SetTailnetStatus(id, types.TailnetDeleting))
			require.NoError(t, p.SetTailnetStatus(id, types.TailnetDeleting), "DELETING again is a no-op")

			for _, st := range []types.TailnetStatus{types.TailnetActive, types.TailnetPending, types.TailnetSuspended} {
				err := p.SetTailnetStatus(id, st)
				require.ErrorIs(t, err, ErrTailnetDeleting, "DELETING -> %s must be refused", st)
			}

			var got types.Tailnet
			require.NoError(t, p.db.Where("id = ?", id.String()).Take(&got).Error)
			assert.Equal(t, types.TailnetDeleting, got.Status)

			owned, err := p.OwnedTailnets("local")
			require.NoError(t, err)
			assert.Empty(t, owned, "a DELETING tailnet is not served")

			other := platformTailnet(t, p, "cluster-2", types.TailnetActive, false)
			require.ErrorIs(t, p.SetTailnetStatus(other, types.TailnetActive), ErrTailnetNotOwned)
		})
	}
}
