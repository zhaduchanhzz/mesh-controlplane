package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// GenerateAPIKeySecret returns a new API key (hskey-api-<identifier>-<secret>),
// its public identifier and the storage hash of its secret. The platform
// registry mints tenant API keys with it (P08b); the key itself is shown once
// and never stored.
func GenerateAPIKeySecret() (string, string, []byte) {
	return generateSecret(apiKeyPrefix)
}

// TailnetForAPIKey returns the tailnet an API key belongs to, and the key's
// public identifier (for per-key rate limits), before any runtime is
// involved (P08b, owner decision Q3: the key picks the tailnet of a public
// /api/v2 request). Only current-format keys (hskey-api-) are accepted; a
// malformed, unknown, revoked or expired key, or a wrong secret, is
// ErrUnknownTenant, with no hint which. The runtime authenticates the key
// again through its scoped handle.
func (p *Platform) TailnetForAPIKey(apiKey string) (tailnet.ID, string, error) {
	rest, ok := strings.CutPrefix(apiKey, apiKeyPrefix)
	if !ok {
		return tailnet.ID{}, "", ErrUnknownTenant
	}

	identifier, secret, err := parsePrefixedKey(rest, ErrAPIKeyFailedToParse)
	if err != nil {
		return tailnet.ID{}, "", ErrUnknownTenant
	}

	ctx, cancel := p.lookupContext()
	defer cancel()

	var (
		tn         string
		hash       []byte
		expiration sql.NullTime
	)

	err = p.db.WithContext(ctx).Raw(
		`SELECT tailnet_id, hash, expiration FROM credentials WHERE kind = ? AND identifier = ? AND revoked IS NULL`,
		types.CredentialAPIKey, identifier,
	).Row().Scan(&tn, &hash, &expiration)
	if errors.Is(err, sql.ErrNoRows) {
		return tailnet.ID{}, "", ErrUnknownTenant
	}

	if err != nil {
		return tailnet.ID{}, "", fmt.Errorf("looking up API key: %w", err)
	}

	if len(hash) == 0 || (expiration.Valid && !expiration.Time.After(time.Now())) {
		return tailnet.ID{}, "", ErrUnknownTenant
	}

	_, err = verifySecret(hash, secret)
	if err != nil {
		return tailnet.ID{}, "", ErrUnknownTenant
	}

	id, err := parseStoredTailnet(tn)
	if err != nil {
		return tailnet.ID{}, "", err
	}

	return id, identifier, nil
}
