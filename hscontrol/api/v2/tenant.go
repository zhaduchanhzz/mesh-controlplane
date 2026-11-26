package apiv2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"

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

// errConfigWriteRate answers a write over the tailnet's config-write bucket.
const errConfigWriteRate = "too many policy, DNS or device key writes for this tailnet; retry later"

// tenantWriteMetaKey marks, in huma.Operation.Metadata, a tenant
// configuration write (see [tenantConfigWrite]); the value is its body
// limit in bytes (int64).
const tenantWriteMetaKey = "headscale.tenantConfigWrite"

// tenantConfigWrite marks op, in cluster mode, as a tenant configuration
// write of at most maxBytes of body (P12d review 5): [tenantWriteMiddleware]
// checks it before the body is read, and huma reads no more than maxBytes.
func tenantConfigWrite(op huma.Operation, maxBytes int64) huma.Operation {
	if op.Metadata == nil {
		op.Metadata = map[string]any{}
	}

	op.Metadata[tenantWriteMetaKey] = maxBytes
	op.MaxBodyBytes = maxBytes

	return op
}

// tenantWriteMiddleware gates the operations marked by [tenantConfigWrite]
// in cluster mode, after authentication and before huma reads the body, in
// this order: {tailnet} ("-" or the key's; another tailnet's ID is a 404
// and spends nothing), the declared body size (413: an oversize body is
// refused from its headers, never buffered, and spends nothing), then the
// tailnet's config-write bucket (429). A refused request's body is not
// read and its connection is not reused.
func tenantWriteMiddleware(api huma.API, b Backend) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		maxBytes, ok := ctx.Operation().Metadata[tenantWriteMetaKey].(int64)
		if !ok || b.Tailnet == "" {
			next(ctx)

			return
		}

		refuse := func(status int, msg string) {
			ctx.SetHeader("Connection", "close")

			_ = huma.WriteErr(api, ctx, status, msg)
		}

		if requireTailnet(b, ctx.Param("tailnet")) != nil {
			refuse(http.StatusNotFound, "tailnet not found")

			return
		}

		if b.Tenant == nil || b.Tenant.AllowConfigWrite == nil {
			refuse(http.StatusServiceUnavailable, errNoTenant)

			return
		}

		if cl := ctx.Header("Content-Length"); cl != "" {
			n, err := strconv.ParseInt(cl, 10, 64)
			if err == nil && n > maxBytes {
				refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is %d bytes, over the limit of %d", n, maxBytes))

				return
			}
		}

		if !b.Tenant.AllowConfigWrite() {
			ctx.SetHeader("Retry-After", "1")
			refuse(http.StatusTooManyRequests, errConfigWriteRate)

			return
		}

		next(ctx)
	}
}

// spendConfigWrite takes a token of the tailnet's config-write bucket for a
// write that is not gated by [tenantWriteMiddleware] (a device key write,
// once the device is known to be the tailnet's own).
func spendConfigWrite(b Backend) error {
	if b.Tenant == nil || b.Tenant.AllowConfigWrite == nil {
		return huma.Error503ServiceUnavailable(errNoTenant)
	}

	if !b.Tenant.AllowConfigWrite() {
		return huma.ErrorWithHeaders(huma.Error429TooManyRequests(errConfigWriteRate), http.Header{"Retry-After": {"1"}})
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

// errPolicyNotInForce answers a policy write that was stored but could not
// be put in force at once (P12d review 2).
const errPolicyNotInForce = "the policy was stored but is not in force yet: " +
	"the previous policy stays enforced until the stored one is installed; GET returns the stored policy"

// auditTenantWrite records a stored write w (Op, Revision and the write's
// own fields set by the caller): the key and the SHA-256 of what was
// stored are added, never the content.
func auditTenantWrite(ctx context.Context, b Backend, w TenantWrite, stored []byte) {
	if b.Tenant == nil || b.Tenant.Audit == nil {
		return
	}

	sum := sha256.Sum256(stored)
	w.KeyID = apiKeyID(ctx)
	w.SHA256 = hex.EncodeToString(sum[:])
	b.Tenant.Audit(w)
}
