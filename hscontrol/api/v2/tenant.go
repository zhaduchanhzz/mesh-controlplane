package apiv2

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

	// PolicyStored and ConfigStored record the tailnet's policy_revision
	// and config_revision a write stored and the runtime already applied,
	// so the reconcile loop does not apply it a second time.
	PolicyStored func(rev uint64)
	ConfigStored func(rev uint64)

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
}
