package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	hsdns "github.com/zhaduchanhzz/mesh-controlplane/hscontrol/dns"
	"tailscale.com/tailcfg"
)

// Per-tailnet DNS in cluster mode (P06, ADR 0008). A tailnet's base domain
// is set by the system, <slug>.<cluster base domain>, so MagicDNS and
// certificate names never collide between tailnets; what a tailnet may set
// is [TenantDNS], stored in the database and validated by
// [TenantDNS.Validate] before it is stored and again when it is loaded.

// TenantDNS is what a tailnet may set of its DNS (ADR 0008 §2): the same
// keys and types as the `dns:` section of the config file, minus what only
// the cluster sets (base domain, certificates, ACME challenge, records
// file).
type TenantDNS struct {
	// MagicDNS defaults to on when unset.
	MagicDNS         *bool               `json:"magic_dns,omitempty"`
	OverrideLocalDNS bool                `json:"override_local_dns,omitempty"`
	Nameservers      TenantNameservers   `json:"nameservers,omitzero"`
	SearchDomains    []string            `json:"search_domains,omitempty"`
	ExtraRecords     []tailcfg.DNSRecord `json:"extra_records,omitempty"`
}

// TenantNameservers are a tailnet's global and split nameservers.
type TenantNameservers struct {
	Global []string            `json:"global,omitempty"`
	Split  map[string][]string `json:"split,omitempty"`
}

// TenantDNSLimits bound what one tailnet may store (ADR 0008 §3, against
// one tenant's DNS costing every client and the shared cluster). Operator
// settings, `tuning.tenant_dns`.
type TenantDNSLimits struct {
	// MaxExtraRecords is the most extra DNS records a tailnet may store.
	MaxExtraRecords int `example:"2000" koanf:"max_extra_records"`

	// MaxNameservers is the most nameservers a tailnet may store, global
	// and split together.
	MaxNameservers int `example:"16" koanf:"max_nameservers"`

	// MaxSearchDomains is the most search domains a tailnet may store.
	MaxSearchDomains int `example:"16" koanf:"max_search_domains"`

	// MaxSplitDomains is the most split DNS domains a tailnet may store,
	// counting a domain mapped to an empty list of nameservers.
	MaxSplitDomains int `example:"16" koanf:"max_split_domains"`

	// MaxConfigBytes is the largest a tailnet's DNS settings may be, as
	// stored JSON.
	MaxConfigBytes int `example:"524288" koanf:"max_config_bytes"`
}

// DefaultTenantDNSLimits are the limits without configuration.
var DefaultTenantDNSLimits = TenantDNSLimits{
	MaxExtraRecords: 1000, MaxNameservers: 64, MaxSearchDomains: 64, MaxSplitDomains: 64, MaxConfigBytes: 256 << 10,
}

var (
	ErrInvalidSlug      = errors.New("tailnet slug must be a DNS label: a-z, 0-9 and '-', 1-63 characters, no leading or trailing '-'")
	ErrInvalidTenantDNS = errors.New("invalid tailnet DNS settings")
)

const (
	maxDNSLabel = 63
	maxDNSName  = 253
)

func validLabel(l string) bool {
	if l == "" || len(l) > maxDNSLabel || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}

	for _, r := range l {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}

	return true
}

// ValidSlug reports whether slug can be the first label of the tailnet's
// base domain (ADR 0008 §1).
func ValidSlug(slug string) error {
	if !validLabel(slug) {
		return fmt.Errorf("%w: %q", ErrInvalidSlug, slug)
	}

	return nil
}

// TailnetBaseDomain is the base domain of tailnet slug under the cluster's.
func TailnetBaseDomain(slug, clusterBase string) string {
	return slug + "." + strings.TrimSuffix(clusterBase, ".")
}

// normDomain lowercases name and drops a trailing dot; ok reports whether
// it is a DNS name of valid labels.
func normDomain(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	if n == "" || len(n) > maxDNSName {
		return n, false
	}

	for l := range strings.SplitSeq(n, ".") {
		if !validLabel(l) {
			return n, false
		}
	}

	return n, true
}

func atOrBelow(name, domain string) bool {
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// tenantNameserverProblem is why ns cannot be a tenant's nameserver, ""
// when it can. A nameserver is an IP address or an https DNS-over-HTTPS URL;
// the config file's looser check (any URL) is not enough for input a tenant
// controls. With strict (every write, P12d review 7), the address, or a
// DoH URL's IP host, must also be one a client can ask: not loopback,
// link-local, interface-local, unspecified or zoned (a client would ask
// itself or its own link, never a resolver of the tailnet). Host names in
// DoH URLs are not resolved.
func tenantNameserverProblem(ns string, strict bool) string {
	ip, err := netip.ParseAddr(ns)
	if err == nil {
		if strict {
			return unusableResolverIP(ip)
		}

		return ""
	}

	u, err := url.Parse(ns)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "is not an IP address or an https URL"
	}

	if strict {
		ip, err := netip.ParseAddr(u.Hostname())
		if err == nil {
			return unusableResolverIP(ip)
		}
	}

	return ""
}

// unusableResolverIP is why ip cannot be a tailnet's resolver, "" when it
// can.
func unusableResolverIP(ip netip.Addr) string {
	if ip.Zone() != "" {
		return "has a zone"
	}

	ip = ip.Unmap()

	switch {
	case ip.IsUnspecified():
		return "is an unspecified address"
	case ip.IsLoopback():
		return "is a loopback address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return "is a link-local address"
	}

	return ""
}

// unlimitedTenantDNS bounds nothing: [TenantDNS.ValidateStored] checks
// form only.
var unlimitedTenantDNS = TenantDNSLimits{
	MaxExtraRecords: math.MaxInt, MaxNameservers: math.MaxInt, MaxSearchDomains: math.MaxInt,
	MaxSplitDomains: math.MaxInt, MaxConfigBytes: math.MaxInt,
}

func (l TenantDNSLimits) orDefaults() TenantDNSLimits {
	d := DefaultTenantDNSLimits
	if l.MaxExtraRecords > 0 {
		d.MaxExtraRecords = l.MaxExtraRecords
	}

	if l.MaxNameservers > 0 {
		d.MaxNameservers = l.MaxNameservers
	}

	if l.MaxSearchDomains > 0 {
		d.MaxSearchDomains = l.MaxSearchDomains
	}

	if l.MaxSplitDomains > 0 {
		d.MaxSplitDomains = l.MaxSplitDomains
	}

	if l.MaxConfigBytes > 0 {
		d.MaxConfigBytes = l.MaxConfigBytes
	}

	return d
}

// Validate checks d for tailnet slug under clusterBase against lim (ADR
// 0008 §3), as every write must pass (tenant API and operator). Records
// must be names below the tailnet's own base domain or outside the
// cluster's entirely; split domains and search domains must be DNS names;
// split domains outside the cluster's base and not above it; search
// domains outside the cluster's base or at or below the tailnet's own;
// nameservers IP addresses or https URLs, never loopback, link-local or
// unspecified; split domains count toward max_split_domains even mapped to
// no nameserver. Every violation is listed.
func (d *TenantDNS) Validate(slug, clusterBase string, lim TenantDNSLimits) error {
	return d.validate(slug, clusterBase, lim, true)
}

// ValidateStored checks settings stored earlier, when a runtime loads them:
// the rules of form every write has had to pass since P06, without limits.
// The rules added since (P12d review 7: search domains in the cluster's
// domain, split domains above it, unusable resolver addresses) and the
// limits are [TenantDNS.Validate]'s: settings stored before them still
// load (the caller warns), and the next write must pass them, as for a
// lowered limit (ADR 0008 §3).
func (d *TenantDNS) ValidateStored(slug, clusterBase string) error {
	return d.validate(slug, clusterBase, unlimitedTenantDNS, false)
}

//nolint:gocyclo // one check per field; every violation is collected
func (d *TenantDNS) validate(slug, clusterBase string, lim TenantDNSLimits, strict bool) error {
	if d == nil {
		return nil
	}

	lim = lim.orDefaults()
	cluster := strings.ToLower(strings.TrimSuffix(clusterBase, "."))
	own := strings.ToLower(TailnetBaseDomain(slug, cluster))

	var errs []string

	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTenantDNS, err)
	}

	if len(raw) > lim.MaxConfigBytes {
		bad("settings are %d bytes, over tuning.tenant_dns.max_config_bytes (%d)", len(raw), lim.MaxConfigBytes)
	}

	if n := len(d.ExtraRecords); n > lim.MaxExtraRecords {
		bad("%d extra records, over tuning.tenant_dns.max_extra_records (%d)", n, lim.MaxExtraRecords)
	}

	nameservers := len(d.Nameservers.Global)
	for _, ns := range d.Nameservers.Split {
		nameservers += len(ns)
	}

	if nameservers > lim.MaxNameservers {
		bad("%d nameservers, over tuning.tenant_dns.max_nameservers (%d)", nameservers, lim.MaxNameservers)
	}

	if n := len(d.SearchDomains); n > lim.MaxSearchDomains {
		bad("%d search domains, over tuning.tenant_dns.max_search_domains (%d)", n, lim.MaxSearchDomains)
	}

	// Every split domain counts, also one mapped to no nameserver.
	if n := len(d.Nameservers.Split); n > lim.MaxSplitDomains {
		bad("%d split DNS domains, over tuning.tenant_dns.max_split_domains (%d)", n, lim.MaxSplitDomains)
	}

	for _, ns := range d.Nameservers.Global {
		if why := tenantNameserverProblem(ns, strict); why != "" {
			bad("nameservers.global: %q %s", ns, why)
		}
	}

	for _, domain := range slices.Sorted(maps.Keys(d.Nameservers.Split)) {
		n, ok := normDomain(domain)
		switch {
		case !ok:
			bad("nameservers.split: %q is not a DNS name", domain)
		case cluster != "" && atOrBelow(n, cluster):
			bad("nameservers.split: %q is inside the cluster's MagicDNS domain", domain)
		case strict && cluster != "" && atOrBelow(cluster, n):
			bad("nameservers.split: %q contains the cluster's MagicDNS domain", domain)
		}

		for _, ns := range d.Nameservers.Split[domain] {
			if why := tenantNameserverProblem(ns, strict); why != "" {
				bad("nameservers.split.%s: %q %s", domain, ns, why)
			}
		}
	}

	for _, s := range d.SearchDomains {
		n, ok := normDomain(s)
		switch {
		case !ok:
			bad("search_domains: %q is not a DNS name", s)
		case strict && cluster != "" && atOrBelow(n, cluster) && !atOrBelow(n, own):
			bad("search_domains: %q is in the cluster's MagicDNS domain but not at or below this tailnet's (%s)", s, own)
		}
	}

	for _, r := range d.ExtraRecords {
		n, ok := normDomain(r.Name)
		switch {
		case !ok:
			bad("extra_records: %q is not a DNS name", r.Name)
		case cluster != "" && atOrBelow(n, cluster) && !strings.HasSuffix(n, "."+own):
			bad("extra_records: %q is in the cluster's MagicDNS domain but not below this tailnet's (%s)", r.Name, own)
		}

		ip, err := netip.ParseAddr(r.Value)

		switch r.Type {
		case "A":
			if err != nil || !ip.Is4() {
				bad("extra_records: %q type A needs an IPv4 value", r.Name)
			}
		case "AAAA":
			if err != nil || !ip.Is6() || ip.Is4In6() {
				bad("extra_records: %q type AAAA needs an IPv6 value", r.Name)
			}
		default:
			bad("extra_records: %q has type %q; only A and AAAA", r.Name, r.Type)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidTenantDNS, strings.Join(errs, "; "))
	}

	return nil
}

// ParseTenantDNS decodes stored or operator-given settings strictly: one
// JSON object, no unknown keys (a tenant cannot set the base domain or a
// cluster-only key by naming it).
func ParseTenantDNS(raw []byte) (*TenantDNS, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var d TenantDNS

	err := dec.Decode(&d)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTenantDNS, err)
	}

	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after the settings object", ErrInvalidTenantDNS)
	}

	return &d, nil
}

// applyTenantDNS sets the tenant part of dc from d; nil means the defaults
// (MagicDNS on, nothing else).
func applyTenantDNS(dc *DNSConfig, d *TenantDNS) {
	if d == nil {
		d = &TenantDNS{}
	}

	dc.MagicDNS = d.MagicDNS == nil || *d.MagicDNS
	dc.OverrideLocalDNS = d.OverrideLocalDNS
	dc.Nameservers = Nameservers{Global: slices.Clone(d.Nameservers.Global)}

	if len(d.Nameservers.Split) > 0 {
		dc.Nameservers.Split = make(map[string][]string, len(d.Nameservers.Split))
		for k, v := range d.Nameservers.Split {
			dc.Nameservers.Split[strings.ToLower(strings.TrimSuffix(k, "."))] = slices.Clone(v)
		}
	}

	dc.SearchDomains = slices.Clone(d.SearchDomains)
	dc.ExtraRecords = slices.Clone(d.ExtraRecords)
	dc.ExtraRecordsPath = ""
}

// ForTailnet returns the config of the runtime of tailnet slug: a copy of
// c whose DNS is the tailnet's (base domain <slug>.<c's>, d or the
// defaults) with a live DNS of its own, so loading one runtime never
// rebuilds the DNS another one serves. Everything else is c's and read
// only. c itself is not changed.
func (c *Config) ForTailnet(slug string, d *TenantDNS) *Config {
	rc := *c

	dc := c.DNSConfig
	dc.BaseDomain = TailnetBaseDomain(slug, c.DNSConfig.BaseDomain)
	applyTenantDNS(&dc, d)
	rc.DNSConfig = dc

	rc.dns = &liveDNS{dns: rc.TailcfgDNSConfig(), provider: c.dnsProvider()}

	return &rc
}

// ReplaceTenantDNS swaps the DNS c's clients get for d (nil: defaults),
// keeping the base domain and everything only the cluster sets; for a
// runtime config from [Config.ForTailnet] whose tailnet's settings changed.
// Readers see the old or the new DNS, never a mix.
func (c *Config) ReplaceTenantDNS(_ string, d *TenantDNS) {
	tmp := *c
	dc := c.DNSConfig
	applyTenantDNS(&dc, d)
	tmp.DNSConfig = dc
	next := tmp.TailcfgDNSConfig()

	if c.dns == nil {
		return
	}

	c.dns.mu.Lock()
	defer c.dns.mu.Unlock()

	c.dns.dns = next
}

// dnsProvider is the provider [Config.StartDNS] was given, if any.
func (c *Config) dnsProvider() hsdns.Provider {
	if c.dns == nil {
		return nil
	}

	return c.dns.provider
}
