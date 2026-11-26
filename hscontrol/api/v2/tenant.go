package apiv2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// Tenant is what cluster mode adds to [Backend] for the writes a tenant
// makes to its own tailnet's policy and DNS settings (P12d, owner decision
// 2026-11-26, replacing P08b Q3 for those). Nil outside cluster mode.
type Tenant struct {
	// Slug and ClusterBaseDomain place the tailnet's DNS names: its own
	// base domain is <Slug>.<ClusterBaseDomain> (ADR 0008 §1).
	Slug              string
	ClusterBaseDomain string

	// AllowConfigWrite takes a token of the tailnet's config-write bucket
	// (tuning.tenant_api.config_write_rate); false is a 429.
	AllowConfigWrite func() bool

	// Audit records every stored write.
	Audit func(TenantWrite)
}

// TenantWrite is the audit record of one stored policy or DNS write: never
// the content, only its SHA-256.
type TenantWrite struct {
	// Op is the operation ID (setACL, setDNSNameservers, ...).
	Op string
	// KeyID is the database ID of the API key that made the write.
	KeyID uint64
	// Revision is the tailnet's policy_revision (setACL) or
	// config_revision (DNS) after the write.
	Revision uint64
	// SHA256 is the hex SHA-256 of what was stored: the policy bytes, or
	// the DNS settings as stored JSON.
	SHA256 string
	// NotInForce is set for a policy that was stored but could not be put
	// in force yet: the previous policy is still enforced until the
	// cluster installs the stored one.
	NotInForce bool
	// DeviceID and KeyExpiryDisabled describe a device key write
	// (setDeviceKey): the device and the value set.
	DeviceID          uint64
	KeyExpiryDisabled *bool
}

// errNoTenant answers a tenant write in cluster mode when the cluster did
// not wire Backend.Tenant: fail closed.
const errNoTenant = "tenant configuration writes are not available"

// tenantWriteGate is the order every tenant configuration write follows in
// cluster mode, after {tailnet} was checked (so another tailnet's ID is a
// 404 and spends nothing): the body size, then the tailnet's config-write
// bucket. maxBytes 0 skips the size check.
func tenantWriteGate(b Backend, size, maxBytes int) error {
	if b.Tenant == nil || b.Tenant.AllowConfigWrite == nil {
		return huma.Error503ServiceUnavailable(errNoTenant)
	}

	if maxBytes > 0 && size > maxBytes {
		return huma.Error413RequestEntityTooLarge(
			fmt.Sprintf("the policy is %d bytes, over the limit of %d", size, maxBytes))
	}

	if !b.Tenant.AllowConfigWrite() {
		return huma.ErrorWithHeaders(
			huma.Error429TooManyRequests("too many policy or DNS writes for this tailnet; retry later"),
			http.Header{"Retry-After": {"1"}},
		)
	}

	return nil
}

// tenantPolicyLimit is tuning.tenant_api.max_policy_bytes (cluster mode).
func tenantPolicyLimit(b Backend) int {
	if n := b.Cfg.Tuning.TenantAPI.MaxPolicyBytes; n > 0 {
		return n
	}

	return types.MaxTenantPolicyBytes
}

// auditTenantWrite records a stored write: the operation, the key, the
// revision and the SHA-256 of what was stored, never the content.
func auditTenantWrite(ctx context.Context, b Backend, op string, rev uint64, stored []byte) {
	if b.Tenant == nil || b.Tenant.Audit == nil {
		return
	}

	sum := sha256.Sum256(stored)
	b.Tenant.Audit(TenantWrite{Op: op, KeyID: apiKeyID(ctx), Revision: rev, SHA256: hex.EncodeToString(sum[:])})
}
