package placement

import (
	"context"
	"fmt"
	"time"

	hsdb "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"gorm.io/gorm"
)

// Tenant API keys (P08b, owner decision Q3). An API key is bound to one
// tailnet (credentials.tailnet_id) and is the only thing that picks the
// tailnet of a public /api/v2 request. Only the platform issues them: the
// operator CLI and the operator API (mTLS), both through these calls, each
// audited; a tenant cannot mint a key, for its own tailnet or another.
//
// The key is returned once, by the call that creates it. It is never stored
// (only the hash of its secret), so an idempotent replay of the create
// returns the key's metadata without the key.

// MaxAPIKeyLifetime bounds the lifetime of a tenant API key.
const MaxAPIKeyLifetime = 365 * 24 * time.Hour

// APIKeyInfo is a tenant API key without its secret.
type APIKeyInfo struct {
	ID         uint64     `json:"id"`
	TailnetID  string     `json:"tailnet_id"`
	Prefix     string     `json:"prefix"`
	User       string     `json:"user,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	Expiration *time.Time `json:"expiration,omitempty"`
	Revoked    *time.Time `json:"revoked,omitempty"`
}

// CreateAPIKey mints an API key for tailnet id, valid for lifetime
// (at most [MaxAPIKeyLifetime]). When user is set, the key acts as that user
// of the tailnet (it must exist there): pre-auth keys it creates without tags
// belong to that user. key is empty when op replays an earlier create: the
// key was shown then and cannot be shown again.
func (r *Registry) CreateAPIKey(ctx context.Context, op Op, id string, lifetime time.Duration, user string) (APIKeyInfo, string, error) {
	if lifetime <= 0 || lifetime > MaxAPIKeyLifetime {
		return APIKeyInfo{}, "", fmt.Errorf("%w: API key lifetime must be within (0, %s]", ErrInvalid, MaxAPIKeyLifetime)
	}

	var key string

	detail := map[string]any{"lifetime": lifetime.String(), "user": user}

	info, err := mutate(ctx, r, op, "CreateAPIKey", id, detail, func(tx *gorm.DB) (APIKeyInfo, string, error) {
		tn, err := loadTailnet(tx, id)
		if err != nil {
			return APIKeyInfo{}, "", err
		}

		// The user is looked up in this tailnet only; the credentials
		// triggers refuse a user of another tailnet as well.
		var userID *uint64

		if user != "" {
			var ids []uint64

			err = tx.Raw(`SELECT id FROM users WHERE tailnet_id = ? AND name = ? AND deleted_at IS NULL`,
				tn.ID, user).Scan(&ids).Error
			if err != nil {
				return APIKeyInfo{}, "", fmt.Errorf("looking up user: %w", err)
			}

			if len(ids) != 1 {
				return APIKeyInfo{}, "", fmt.Errorf("%w: user %q in tailnet %s", ErrNotFound, user, tn.ID)
			}

			userID = &ids[0]
		}

		k, identifier, hash := hsdb.GenerateAPIKeySecret()
		now := r.now().UTC()
		exp := now.Add(lifetime)

		var keyID uint64

		// Raw on purpose: credentials is a tenant table and the statement
		// names its tailnet explicitly (an operator write, no tenant scope).
		err = tx.Raw(
			`INSERT INTO credentials (tailnet_id, kind, identifier, hash, user_id, created_at, expiration)
			 VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			tn.ID, types.CredentialAPIKey, identifier, hash, userID, now, exp,
		).Scan(&keyID).Error
		if err != nil {
			return APIKeyInfo{}, "", fmt.Errorf("creating API key: %w", err)
		}

		key = k

		return APIKeyInfo{ID: keyID, TailnetID: tn.ID, Prefix: identifier, User: user, CreatedAt: now, Expiration: &exp},
			fmt.Sprintf("%s/apikey/%d", tn.ID, keyID), nil
	})
	if err != nil {
		return APIKeyInfo{}, "", err
	}

	return info, key, nil
}

// ListAPIKeys lists the API keys of tailnet id, revoked ones included.
func (r *Registry) ListAPIKeys(ctx context.Context, id string) ([]APIKeyInfo, error) {
	db := r.db.WithContext(ctx)

	tn, err := loadTailnet(db, id)
	if err != nil {
		return nil, err
	}

	var rows []struct {
		ID         uint64
		Identifier string
		UserName   *string
		CreatedAt  time.Time
		Expiration *time.Time
		Revoked    *time.Time
	}

	err = db.Raw(
		`SELECT c.id, c.identifier, u.name AS user_name, c.created_at, c.expiration, c.revoked
		 FROM credentials c LEFT JOIN users u ON u.id = c.user_id AND u.tailnet_id = c.tailnet_id
		 WHERE c.tailnet_id = ? AND c.kind = ? ORDER BY c.id`,
		tn.ID, types.CredentialAPIKey,
	).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("listing API keys: %w", err)
	}

	out := make([]APIKeyInfo, 0, len(rows))
	for _, k := range rows {
		info := APIKeyInfo{
			ID: k.ID, TailnetID: tn.ID, Prefix: k.Identifier, CreatedAt: k.CreatedAt,
			Expiration: k.Expiration, Revoked: k.Revoked,
		}
		if k.UserName != nil {
			info.User = *k.UserName
		}

		out = append(out, info)
	}

	return out, nil
}

// RevokeAPIKey revokes API key keyID of tailnet id: its hash is dropped, so
// it never verifies again. A key of another tailnet is ErrNotFound.
func (r *Registry) RevokeAPIKey(ctx context.Context, op Op, id string, keyID uint64) (APIKeyInfo, error) {
	target := fmt.Sprintf("%s/apikey/%d", id, keyID)

	return mutate(ctx, r, op, "RevokeAPIKey", target, nil, func(tx *gorm.DB) (APIKeyInfo, string, error) {
		tn, err := loadTailnet(tx, id)
		if err != nil {
			return APIKeyInfo{}, "", err
		}

		now := r.now().UTC()

		res := tx.Exec(
			`UPDATE credentials SET revoked = ?, hash = NULL
			 WHERE tailnet_id = ? AND kind = ? AND id = ? AND revoked IS NULL`,
			now, tn.ID, types.CredentialAPIKey, keyID,
		)
		if res.Error != nil {
			return APIKeyInfo{}, "", fmt.Errorf("revoking API key: %w", res.Error)
		}

		if res.RowsAffected != 1 {
			return APIKeyInfo{}, "", fmt.Errorf("%w: API key %d of tailnet %s", ErrNotFound, keyID, tn.ID)
		}

		var k struct {
			Identifier string
			CreatedAt  time.Time
			Expiration *time.Time
		}

		err = tx.Raw(`SELECT identifier, created_at, expiration FROM credentials WHERE id = ?`, keyID).Scan(&k).Error
		if err != nil {
			return APIKeyInfo{}, "", fmt.Errorf("reading revoked API key: %w", err)
		}

		return APIKeyInfo{
			ID: keyID, TailnetID: tn.ID, Prefix: k.Identifier, CreatedAt: k.CreatedAt,
			Expiration: k.Expiration, Revoked: &now,
		}, "", nil
	})
}
