package apiv2

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/scope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

// Tailscale's DNS endpoints (P12d): nameservers, search paths, split DNS
// and preferences, with Tailscale's paths and JSON shapes (the Go SDK's
// DNSResource), over the DNS settings a tailnet may set (ADR 0008 §2).
//
// In cluster mode they read and write the tailnet's own settings through
// its runtime: every write is checked against ADR 0008 §3 (split domains
// outside the cluster's MagicDNS domain, nameservers IP addresses or https
// URLs, the tuning.tenant_dns limits), stored with a config_revision bump
// in one fenced transaction, installed for the tailnet's clients only, rate
// limited and audited. A refused write changes nothing. Outside cluster
// mode the DNS settings come from the configuration file: reads report
// them, writes are 501.

func init() {
	registrations = append(registrations, registerDNS)
}

const errDNSFromConfig = "DNS settings are read from the configuration file"

type (
	dnsTailnetInput struct {
		Tailnet string `path:"tailnet"`
	}

	nameserversRequest struct {
		_ struct{} `additionalProperties:"true"`

		DNS []string `json:"dns"`
	}
	setNameserversInput struct {
		Tailnet string `path:"tailnet"`
		Body    nameserversRequest
	}
	nameserversOutput struct {
		Body struct {
			DNS []string `json:"dns" nullable:"false"`
		}
	}
	setNameserversOutput struct {
		Body struct {
			DNS      []string `json:"dns"      nullable:"false"`
			MagicDNS bool     `json:"magicDNS"`
		}
	}

	searchPathsRequest struct {
		_ struct{} `additionalProperties:"true"`

		SearchPaths []string `json:"searchPaths"`
	}
	setSearchPathsInput struct {
		Tailnet string `path:"tailnet"`
		Body    searchPathsRequest
	}
	searchPathsOutput struct {
		Body struct {
			SearchPaths []string `json:"searchPaths" nullable:"false"`
		}
	}

	// splitDNSInput takes the body raw: a domain mapped to null is a
	// removal in PATCH, which a validated []string would refuse.
	splitDNSInput struct {
		Tailnet string `path:"tailnet"`
		RawBody []byte `contentType:"application/json"`
	}
	splitDNSOutput struct {
		Body map[string][]string
	}

	// DNSPreferences is Tailscale's DNS preferences object. OverrideLocalDNS
	// is not in Tailscale's object: it is ADR 0008's override_local_dns
	// (Tailscale's console toggle "Override DNS servers"); omitted, it is
	// left as it is.
	DNSPreferences struct {
		_ struct{} `additionalProperties:"true"`

		MagicDNS         bool  `json:"magicDNS"`
		OverrideLocalDNS *bool `json:"overrideLocalDNS,omitempty"`
	}
	setPreferencesInput struct {
		Tailnet string `path:"tailnet"`
		Body    DNSPreferences
	}
	preferencesOutput struct {
		Body DNSPreferences
	}
)

func registerDNS(api huma.API, b Backend) {
	dnsTags := []string{"DNS", tagTailscaleCompat}

	readOp := func(id, path, summary string) huma.Operation {
		return requireScope(huma.Operation{
			OperationID: id,
			Method:      http.MethodGet,
			Path:        "/api/v2/tailnet/{tailnet}/dns/" + path,
			Summary:     summary,
			Tags:        dnsTags,
			Security:    security,
			Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
		}, scope.DNSRead)
	}

	writeOp := func(id, method, path, summary string) huma.Operation {
		return requireScope(huma.Operation{
			OperationID:     id,
			Method:          method,
			Path:            "/api/v2/tailnet/{tailnet}/dns/" + path,
			Summary:         summary,
			Tags:            dnsTags,
			Security:        security,
			DefaultStatus:   http.StatusOK,
			MaxBodyBytes:    maxBodyBytes,
			BodyReadTimeout: 5 * time.Second,
			Errors: []int{
				http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
				http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusNotImplemented,
				http.StatusServiceUnavailable,
			},
		}, scope.DNS)
	}

	huma.Register(api, readOp("getDNSNameservers", "nameservers", "List the DNS nameservers"),
		func(ctx context.Context, in *dnsTailnetInput) (*nameserversOutput, error) {
			d, err := readDNS(b, in.Tailnet)
			if err != nil {
				return nil, err
			}

			out := &nameserversOutput{}
			out.Body.DNS = emptyIfNil(d.Nameservers.Global)

			return out, nil
		})

	huma.Register(api, writeOp("setDNSNameservers", http.MethodPost, "nameservers", "Set the DNS nameservers"),
		func(ctx context.Context, in *setNameserversInput) (*setNameserversOutput, error) {
			d, err := writeDNS(ctx, b, in.Tailnet, "setDNSNameservers", func(d *types.TenantDNS) error {
				d.Nameservers.Global = in.Body.DNS

				return nil
			})
			if err != nil {
				return nil, err
			}

			out := &setNameserversOutput{}
			out.Body.DNS = emptyIfNil(d.Nameservers.Global)
			out.Body.MagicDNS = magicDNSOn(d)

			return out, nil
		})

	huma.Register(api, readOp("getDNSSearchPaths", "searchpaths", "List the DNS search paths"),
		func(ctx context.Context, in *dnsTailnetInput) (*searchPathsOutput, error) {
			d, err := readDNS(b, in.Tailnet)
			if err != nil {
				return nil, err
			}

			out := &searchPathsOutput{}
			out.Body.SearchPaths = emptyIfNil(d.SearchDomains)

			return out, nil
		})

	huma.Register(api, writeOp("setDNSSearchPaths", http.MethodPost, "searchpaths", "Set the DNS search paths"),
		func(ctx context.Context, in *setSearchPathsInput) (*searchPathsOutput, error) {
			d, err := writeDNS(ctx, b, in.Tailnet, "setDNSSearchPaths", func(d *types.TenantDNS) error {
				d.SearchDomains = in.Body.SearchPaths

				return nil
			})
			if err != nil {
				return nil, err
			}

			out := &searchPathsOutput{}
			out.Body.SearchPaths = emptyIfNil(d.SearchDomains)

			return out, nil
		})

	registerSplitDNS(api, b, readOp, writeOp)

	huma.Register(api, readOp("getDNSPreferences", "preferences", "Get the DNS preferences"),
		func(ctx context.Context, in *dnsTailnetInput) (*preferencesOutput, error) {
			d, err := readDNS(b, in.Tailnet)
			if err != nil {
				return nil, err
			}

			return &preferencesOutput{Body: preferencesOf(d)}, nil
		})

	huma.Register(api, writeOp("setDNSPreferences", http.MethodPost, "preferences", "Set the DNS preferences"),
		func(ctx context.Context, in *setPreferencesInput) (*preferencesOutput, error) {
			d, err := writeDNS(ctx, b, in.Tailnet, "setDNSPreferences", func(d *types.TenantDNS) error {
				on := in.Body.MagicDNS
				d.MagicDNS = &on

				if in.Body.OverrideLocalDNS != nil {
					d.OverrideLocalDNS = *in.Body.OverrideLocalDNS
				}

				return nil
			})
			if err != nil {
				return nil, err
			}

			return &preferencesOutput{Body: preferencesOf(d)}, nil
		})
}

func registerSplitDNS(
	api huma.API, b Backend,
	readOp func(id, path, summary string) huma.Operation,
	writeOp func(id, method, path, summary string) huma.Operation,
) {
	huma.Register(api, readOp("getDNSSplitDNS", "split-dns", "Get the split DNS settings"),
		func(ctx context.Context, in *dnsTailnetInput) (*splitDNSOutput, error) {
			d, err := readDNS(b, in.Tailnet)
			if err != nil {
				return nil, err
			}

			return &splitDNSOutput{Body: splitOf(d)}, nil
		})

	for _, w := range []struct {
		id, method, summary string
		replace             bool
	}{
		{"updateDNSSplitDNS", http.MethodPatch, "Update the split DNS settings (a domain mapped to null is removed)", false},
		{"setDNSSplitDNS", http.MethodPut, "Replace the split DNS settings ({} removes them all)", true},
	} {
		op := writeOp(w.id, w.method, "split-dns", w.summary)
		op.SkipValidateBody = true

		huma.Register(api, op, func(ctx context.Context, in *splitDNSInput) (*splitDNSOutput, error) {
			err := requireTailnet(b, in.Tailnet)
			if err != nil {
				return nil, err
			}

			req, err := parseSplitDNS(in.RawBody)
			if err != nil {
				return nil, err
			}

			d, err := writeDNS(ctx, b, in.Tailnet, w.id, func(d *types.TenantDNS) error {
				split := map[string][]string{}
				if !w.replace {
					maps.Copy(split, d.Nameservers.Split)
				}

				for domain, ns := range req {
					// Stored as the client gets it (lowercase, no trailing dot), so
					// a PATCH of "Corp.Example." replaces "corp.example".
					key := strings.ToLower(strings.TrimSuffix(domain, "."))
					if ns == nil {
						delete(split, key)

						continue
					}

					split[key] = ns
				}

				d.Nameservers.Split = split

				return nil
			})
			if err != nil {
				return nil, err
			}

			return &splitDNSOutput{Body: splitOf(d)}, nil
		})
	}
}

// parseSplitDNS decodes a split DNS body: one JSON object of domain to a
// list of nameservers or null.
func parseSplitDNS(raw []byte) (map[string][]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, huma.Error400BadRequest("the body must be an object of domain to nameservers")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))

	var req map[string][]string

	err := dec.Decode(&req)
	if err != nil || dec.More() || req == nil {
		return nil, huma.Error400BadRequest("the body must be an object of domain to nameservers")
	}

	return req, nil
}

// readDNS returns the tailnet's DNS settings: in cluster mode the stored
// ones (the defaults when none are stored), otherwise those of the
// configuration file.
func readDNS(b Backend, tn string) (*types.TenantDNS, error) {
	err := requireTailnet(b, tn)
	if err != nil {
		return nil, err
	}

	if b.Tailnet == "" {
		dc := b.Cfg.DNSConfig
		magic := dc.MagicDNS

		return &types.TenantDNS{
			MagicDNS:         &magic,
			OverrideLocalDNS: dc.OverrideLocalDNS,
			Nameservers:      types.TenantNameservers{Global: dc.Nameservers.Global, Split: dc.Nameservers.Split},
			SearchDomains:    dc.SearchDomains,
		}, nil
	}

	d, err := b.State.TenantDNS()
	if err != nil {
		return nil, huma.Error500InternalServerError("loading DNS settings", err)
	}

	if d == nil {
		d = &types.TenantDNS{}
	}

	return d, nil
}

// writeDNS applies edit to a copy of the tailnet's stored DNS settings,
// validates the result (ADR 0008 §3) and stores and installs it (cluster
// mode only). It returns what was stored.
func writeDNS(ctx context.Context, b Backend, tn, op string, edit func(*types.TenantDNS) error) (*types.TenantDNS, error) {
	err := requireTailnet(b, tn)
	if err != nil {
		return nil, err
	}

	if b.Tailnet == "" {
		return nil, huma.Error501NotImplemented(errDNSFromConfig)
	}

	err = tenantWriteGate(b, 0, 0)
	if err != nil {
		return nil, err
	}

	lim := b.Cfg.Tuning.TenantDNS

	stored, rev, c, err := b.State.UpdateTenantDNS(func(cur *types.TenantDNS) (*types.TenantDNS, error) {
		next := cloneTenantDNS(cur)

		err := edit(next)
		if err != nil {
			return nil, err
		}

		err = next.Validate(b.Tenant.Slug, b.Tenant.ClusterBaseDomain, lim)
		if err != nil {
			return nil, err
		}

		return next, nil
	})
	if err != nil {
		return nil, mapError("setting DNS", err)
	}

	b.Change(c)

	raw, err := json.Marshal(stored)
	if err != nil {
		return nil, huma.Error500InternalServerError("encoding DNS settings", err)
	}

	auditTenantWrite(ctx, b, op, rev, raw)

	return stored, nil
}

// cloneTenantDNS is a deep copy of d (the defaults for nil) that an edit
// can change without touching d.
func cloneTenantDNS(d *types.TenantDNS) *types.TenantDNS {
	if d == nil {
		return &types.TenantDNS{}
	}

	out := *d
	if d.MagicDNS != nil {
		on := *d.MagicDNS
		out.MagicDNS = &on
	}

	out.Nameservers.Global = slices.Clone(d.Nameservers.Global)
	out.Nameservers.Split = maps.Clone(d.Nameservers.Split)
	out.SearchDomains = slices.Clone(d.SearchDomains)
	out.ExtraRecords = slices.Clone(d.ExtraRecords)

	return &out
}

func magicDNSOn(d *types.TenantDNS) bool {
	return d.MagicDNS == nil || *d.MagicDNS
}

func preferencesOf(d *types.TenantDNS) DNSPreferences {
	override := d.OverrideLocalDNS

	return DNSPreferences{MagicDNS: magicDNSOn(d), OverrideLocalDNS: &override}
}

func splitOf(d *types.TenantDNS) map[string][]string {
	out := make(map[string][]string, len(d.Nameservers.Split))
	for k, v := range d.Nameservers.Split {
		out[k] = emptyIfNil(v)
	}

	return out
}
