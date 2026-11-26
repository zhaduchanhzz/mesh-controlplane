package apiv2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/rs/zerolog/log"
	"github.com/tailscale/hujson"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/scope"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/state"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
)

func init() {
	registrations = append(registrations, registerACL)
}

const (
	contentTypeJSON   = "application/json"
	contentTypeHuJSON = "application/hujson"
)

// defaultPolicy is served by GET when no policy is set, and is what an If-Match
// of "ts-default" matches against. Allow-all, matching the cluster's behaviour
// with no configured policy.
const defaultPolicy = `{
	// Headscale default policy. Allows all communication.
	"acls": [
		{"action": "accept", "src": ["*"], "dst": ["*:*"]},
	],
}
`

// defaultTenantPolicy is defaultPolicy for a tailnet where a missing or
// empty policy denies everything (a tenant tailnet of a cluster, AGENTS
// rule 8): what a tenant reads must be what is enforced, and writing back
// what it read must never open the tailnet.
const defaultTenantPolicy = `{
	// No policy is stored: this tailnet allows nothing.
	"grants": [],
}
`

// defaultPolicyOf is the policy GET serves when none is stored.
func defaultPolicyOf(b Backend) string {
	if b.Cfg != nil && b.Cfg.PolicyEmptyDenies {
		return defaultTenantPolicy
	}

	return defaultPolicy
}

type getACLInput struct {
	Tailnet string `path:"tailnet"`
	Accept  string `header:"Accept"`
	Details bool   `doc:"Accepted for compatibility; ignored." query:"details"`
}

type validateACLInput struct {
	Tailnet string `path:"tailnet"`
	RawBody []byte `contentType:"application/json"`
}

// validateACLOutput is the verdict on a policy. Tailscale answers a refused
// policy with 200, its reason as message and, for failed tests, one data
// entry per failing test.
type validateACLOutput struct {
	Body struct {
		Message string         `json:"message,omitempty"`
		Data    []apiErrorData `json:"data,omitempty"`
	}
}

type setACLInput struct {
	Tailnet string `path:"tailnet"`
	IfMatch string `header:"If-Match"`
	Accept  string `header:"Accept"`
	// RawBody captures the raw HuJSON or JSON policy bytes; huma feeds them here
	// regardless of Content-Type. The declared type only shapes the OpenAPI schema.
	RawBody []byte `contentType:"application/json"`
}

func registerACL(api huma.API, b Backend) {
	aclTags := []string{"Policy", tagTailscaleCompat}

	huma.Register(api, requireScope(huma.Operation{
		OperationID: "getACL",
		Method:      http.MethodGet,
		Path:        "/api/v2/tailnet/{tailnet}/acl",
		Summary:     "Get the policy file",
		Tags:        aclTags,
		Security:    security,
		Errors:      []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}, scope.PolicyFileRead), func(ctx context.Context, in *getACLInput) (*huma.StreamResponse, error) {
		err := requireTailnet(b, in.Tailnet)
		if err != nil {
			return nil, err
		}

		data, err := currentPolicy(b)
		if err != nil {
			return nil, err
		}

		return streamPolicy(data, aclContentType(in.Accept)), nil
	})

	huma.Register(api, policyWriteOp(b, requireScope(huma.Operation{
		OperationID:   "setACL",
		Method:        http.MethodPost,
		Path:          "/api/v2/tailnet/{tailnet}/acl",
		Summary:       "Set the policy file",
		Tags:          aclTags,
		Security:      security,
		DefaultStatus: http.StatusOK,
		// The body is an opaque HuJSON document captured raw; skip huma's
		// schema validation (which would expect a base64 string).
		SkipValidateBody: true,
		// huma's own Body defaults, which it never applies to RawBody; also
		// what v1 setPolicy accepts.
		MaxBodyBytes:    1 << 20,
		BodyReadTimeout: 5 * time.Second,
		Errors: []int{
			http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusPreconditionFailed, http.StatusInternalServerError,
		},
	}, scope.PolicyFile)), func(ctx context.Context, in *setACLInput) (*huma.StreamResponse, error) {
		// Cluster mode (P12d): {tailnet}, the size limit and the tailnet's
		// write bucket were checked before the body was read
		// (tenantWriteMiddleware); checked again here as everywhere.
		err := requireTailnet(b, in.Tailnet)
		if err != nil {
			return nil, err
		}

		if b.Cfg.Policy.Mode != types.PolicyModeDB {
			return nil, huma.Error400BadRequest(
				types.ErrPolicyUpdateIsDisabled.Error(), types.ErrPolicyUpdateIsDisabled,
			)
		}

		def := defaultPolicyOf(b)

		// Checked under the policy lock, so two writers holding the same ETag
		// cannot both pass.
		var match func(string) bool
		if in.IfMatch != "" {
			match = func(stored string) bool {
				if stored == "" {
					stored = def
				}

				return etagMatches(in.IfMatch, []byte(stored), def)
			}
		}

		// Validated (tests included) before anything is stored; a refused
		// policy leaves the stored and enforced one as it was. In cluster
		// mode the policy and its policy_revision are stored in one fenced
		// transaction.
		updated, rev, cs, err := b.State.UpdateTenantPolicyIf(in.RawBody, match)
		b.Change(cs...)

		if errors.Is(err, state.ErrPolicyPrecondition) {
			return nil, huma.Error412PreconditionFailed("precondition failed, invalid old hash")
		}

		// Stored, but not installed (P12d review 2): never a bare failure.
		// The write is committed: audited, answered with its ETag and a
		// message saying so; the previous policy stays enforced until the
		// stored one is installed (the cluster's reconcile loop retries).
		if errors.Is(err, state.ErrPolicyNotInForce) && updated != nil {
			if b.Tailnet != "" {
				auditTenantWrite(ctx, b, TenantWrite{Op: "setACL", Revision: rev, NotInForce: true}, []byte(updated.Data))
			}

			log.Error().Err(err).Str("tailnet", b.Tailnet).Uint64("revision", rev).
				Msg("policy stored but not installed; the previous policy stays enforced")

			return nil, huma.ErrorWithHeaders(huma.Error500InternalServerError(errPolicyNotInForce),
				http.Header{"ETag": {policyETag([]byte(updated.Data))}})
		}

		if err != nil {
			return nil, mapTenantError("setting policy", err)
		}

		if b.Tailnet != "" {
			auditTenantWrite(ctx, b, TenantWrite{Op: "setACL", Revision: rev}, []byte(updated.Data))
		}

		return streamPolicy([]byte(updated.Data), aclWriteContentType(in.Accept)), nil
	})

	registerValidateACL(api, b, aclTags)
}

func registerValidateACL(api huma.API, b Backend, aclTags []string) {
	// Cluster mode (P12d): validating compiles the policy and runs its
	// tests, so it is gated as a write (tenantWriteMiddleware).
	huma.Register(api, policyWriteOp(b, requireScope(huma.Operation{
		OperationID:      "validateACL",
		Method:           http.MethodPost,
		Path:             "/api/v2/tailnet/{tailnet}/acl/validate",
		Summary:          "Validate a policy file",
		Description:      "Checks a policy, its tests and its SSH rules without storing it. A refused policy answers 200 with the reason as message.",
		Tags:             aclTags,
		Security:         security,
		DefaultStatus:    http.StatusOK,
		SkipValidateBody: true,
		// As setACL: huma applies no body limit to RawBody.
		MaxBodyBytes:    1 << 20,
		BodyReadTimeout: 5 * time.Second,
		Errors: []int{
			http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusInternalServerError,
		},
	}, scope.PolicyFileRead)), func(ctx context.Context, in *validateACLInput) (*validateACLOutput, error) {
		err := requireTailnet(b, in.Tailnet)
		if err != nil {
			return nil, err
		}

		// As on Tailscale SaaS, only a body with no policy at all is a 400.
		if len(bytes.TrimSpace(in.RawBody)) == 0 {
			return nil, huma.Error400BadRequest("unexpected end of JSON input")
		}

		out := &validateACLOutput{}

		err = b.State.ValidatePolicy(in.RawBody)
		if errors.Is(err, state.ErrInvalidPolicy) {
			out.Body.Message, out.Body.Data = policyRefusal(err)

			return out, nil
		}

		if err != nil {
			return nil, internalError("validating policy", err)
		}

		return out, nil
	})

	// huma requires a RawBody; the handler answers an empty one as Tailscale
	// does.
	api.OpenAPI().Paths["/api/v2/tailnet/{tailnet}/acl/validate"].Post.RequestBody.Required = false
}

// policyWriteOp is op (setACL, validateACL) in cluster mode marked as a
// tenant configuration write whose body is at most
// tuning.tenant_api.max_policy_bytes (P12d review 5): refused from its
// headers when declared larger, and never read beyond the limit.
func policyWriteOp(b Backend, op huma.Operation) huma.Operation {
	if b.Tailnet == "" {
		return op
	}

	return tenantConfigWrite(op, int64(tenantPolicyLimit(b)))
}

// currentPolicy returns the stored policy bytes, or the default when none is
// set (allow-all, or deny-all where an empty policy denies). File mode reads
// the configured path; DB mode reads the database.
func currentPolicy(b Backend) ([]byte, error) {
	def := defaultPolicyOf(b)

	switch b.Cfg.Policy.Mode {
	case types.PolicyModeDB:
		p, err := b.State.GetPolicy()
		if err != nil {
			if errors.Is(err, types.ErrPolicyNotFound) {
				return []byte(def), nil
			}

			return nil, internalError("loading policy", err)
		}

		if p.Data == "" {
			return []byte(def), nil
		}

		return []byte(p.Data), nil

	case types.PolicyModeFile:
		path := b.Cfg.AbsPath(b.Cfg.Policy.Path)
		if path == "" {
			return []byte(def), nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, internalError("reading policy file", err)
		}

		return data, nil
	}

	return nil, huma.Error500InternalServerError("unsupported policy mode", nil)
}

// streamPolicy writes the policy with the chosen content type and a
// content-addressed ETag, bypassing huma's JSON marshaler so HuJSON survives. As
// JSON, the policy loses its comments and trailing commas: a JSON reader cannot
// parse them. The ETag stays over the stored bytes, so it holds for either read.
func streamPolicy(data []byte, contentType string) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(ctx huma.Context) {
		body := data

		if contentType == contentTypeJSON {
			standard, err := hujson.Standardize(slices.Clone(data))
			if err == nil {
				body = standard
			}
		}

		ctx.SetHeader("Content-Type", contentType)
		ctx.SetHeader("ETag", policyETag(data))
		ctx.SetStatus(http.StatusOK)
		_, _ = ctx.BodyWriter().Write(body)
	}}
}

// aclWriteContentType answers a policy write as HuJSON, as SaaS does, unless the
// caller asks for JSON.
func aclWriteContentType(accept string) string {
	if strings.Contains(accept, contentTypeJSON) {
		return contentTypeJSON
	}

	return contentTypeHuJSON
}

// aclContentType serves HuJSON only when explicitly asked; everything else
// (including an empty Accept) gets application/json.
func aclContentType(accept string) string {
	if strings.Contains(accept, contentTypeHuJSON) {
		return contentTypeHuJSON
	}

	return contentTypeJSON
}

// policyETag is the quoted hex SHA-256 of the policy bytes: stable across reads,
// changes iff the policy changes.
func policyETag(data []byte) string {
	sum := sha256.Sum256(data)

	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// etagMatches reports whether an If-Match header satisfies the current policy.
// "*" always matches; "ts-default" matches only when no policy is set, def
// being what is served then (an approximation of Tailscale's
// untouched-default semantics).
func etagMatches(ifMatch string, current []byte, def string) bool {
	ifMatch = strings.TrimSpace(ifMatch)

	switch ifMatch {
	case "*":
		return true
	case `"ts-default"`, "ts-default":
		return string(current) == def
	default:
		return ifMatch == policyETag(current)
	}
}
