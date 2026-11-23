package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// lookupSQL is the one statement bootstrap runs (reviewed raw SQL on the
// tenant table credentials; tenantscope does not parse Raw). It selects the
// row of the presented key only, and of tenant data returns only its
// tailnet's slug, status and owner cluster's endpoint: nothing of another
// tenant, and the client gets the endpoint only.
const lookupSQL = `SELECT c.tailnet_id AS tailnet_id, c.hash AS hash, c.revoked AS revoked,
	c.expiration AS expiration, c.used AS used, c.reusable AS reusable,
	t.slug AS slug, t.status AS status, t.deleted_at AS deleted_at,
	t.moving_to_cluster_id AS moving_to, cl.endpoint_url AS endpoint, cl.state AS cluster_state
	FROM credentials c
	JOIN tailnets t ON t.id = c.tailnet_id
	LEFT JOIN clusters cl ON cl.id = t.owner_cluster_id
	WHERE c.kind = ? AND c.identifier = ?`

type lookupRow struct {
	TailnetID    string     `gorm:"column:tailnet_id"`
	Hash         []byte     `gorm:"column:hash"`
	Revoked      *time.Time `gorm:"column:revoked"`
	Expiration   *time.Time `gorm:"column:expiration"`
	Used         *bool      `gorm:"column:used"`
	Reusable     *bool      `gorm:"column:reusable"`
	Slug         string     `gorm:"column:slug"`
	Status       string     `gorm:"column:status"`
	DeletedAt    *time.Time `gorm:"column:deleted_at"`
	MovingTo     *string    `gorm:"column:moving_to"`
	Endpoint     *string    `gorm:"column:endpoint"`
	ClusterState *string    `gorm:"column:cluster_state"`
}

var errAmbiguousKey = errors.New("pre-auth key identifier matches more than one credential")

// GormStore reads the shared database through a handle opened with
// hscontrol/db.OpenReadOnly.
type GormStore struct {
	db *gorm.DB
}

// NewGormStore returns a Store on db.
func NewGormStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

// LookupAuthKey implements Store. On PostgreSQL the read runs in a READ ONLY
// transaction (valid with PgBouncer in transaction mode); the SQLite handle
// is query_only.
func (s *GormStore) LookupAuthKey(ctx context.Context, keyID string) (Record, bool, error) {
	var rows []lookupRow

	read := func(tx *gorm.DB) error {
		return tx.Raw(lookupSQL, types.CredentialPreAuthKey, keyID).Scan(&rows).Error
	}

	var err error
	if s.db.Name() == "postgres" {
		err = s.db.WithContext(ctx).Transaction(read, &sql.TxOptions{ReadOnly: true})
	} else {
		err = read(s.db.WithContext(ctx))
	}

	if err != nil {
		return Record{}, false, fmt.Errorf("looking up key: %w", err)
	}

	switch len(rows) {
	case 0:
		return Record{}, false, nil
	case 1:
	default:
		return Record{}, false, errAmbiguousKey
	}

	r := rows[0]

	return Record{
		TailnetID:       r.TailnetID,
		Hash:            r.Hash,
		Revoked:         r.Revoked != nil,
		Expiration:      r.Expiration,
		Used:            r.Used != nil && *r.Used,
		Reusable:        r.Reusable != nil && *r.Reusable,
		TailnetSlug:     r.Slug,
		TailnetState:    r.Status,
		Deleted:         r.DeletedAt != nil,
		MovingTo:        r.MovingTo,
		ClusterEndpoint: r.Endpoint,
		ClusterState:    r.ClusterState,
	}, true, nil
}
