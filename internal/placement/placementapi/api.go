// Package placementapi is the operator HTTP API of the placement service
// (cmd/mesh-placement, P10): Huma v2 operations over internal/placement,
// served only to platform operators authenticated by mTLS (see auth.go).
// Tenant credentials are never accepted. Mutations require an
// Idempotency-Key header and are audited.
package placementapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

// Config returns the Huma configuration: OpenAPI document at /v1/openapi
// (behind the same mTLS check as everything else), no docs UI, no $schema.
func Config() huma.Config {
	config := huma.DefaultConfig("mesh-placement operator API", "v1")
	config.Info.Description = "Platform operator API of the placement registry (P10). " +
		"mTLS with an allowed operator certificate is required; tenant credentials are refused. " +
		"Mutations require an Idempotency-Key header and are audited."
	config.OpenAPIPath = "/v1/openapi"
	config.DocsPath = ""
	config.SchemasPath = ""
	config.CreateHooks = nil
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"operatorMTLS": {Type: "mutualTLS", Description: "client certificate issued by the pinned operator CA, with an allowed SAN"},
	}
	config.Security = []map[string][]string{{"operatorMTLS": {}}}

	return config
}

// NewAPI registers the operations on router. reg may be nil only to emit the
// OpenAPI document.
func NewAPI(router chi.Router, reg *placement.Registry) huma.API {
	api := humachi.New(router, Config())
	register(api, &handlers{reg: reg})

	return api
}

// Handler returns the complete operator handler: the operator check of auth
// wrapping the API.
func Handler(reg *placement.Registry, auth *Authorizer) http.Handler {
	mux := chi.NewMux()
	NewAPI(mux, reg)

	return auth.Wrap(mux)
}

// Spec emits the OpenAPI 3.1 document.
func Spec() ([]byte, error) { return NewAPI(chi.NewMux(), nil).OpenAPI().YAML() }

// Spec30 emits the OpenAPI 3.0.3 downgrade.
func Spec30() ([]byte, error) { return NewAPI(chi.NewMux(), nil).OpenAPI().DowngradeYAML() }

type handlers struct{ reg *placement.Registry }

// TailnetView is a tailnet as the API returns it.
type TailnetView struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Status         string `json:"status"`
	OwnerClusterID string `json:"owner_cluster_id"`
	OwnerEpoch     uint64 `json:"owner_epoch"`
	PolicyRevision uint64 `json:"policy_revision"`
	ConfigRevision uint64 `json:"config_revision"`
	Devices        *int64 `json:"devices,omitempty"`
	// MovingToClusterID is the target of a pending move (ADR 0010).
	MovingToClusterID *string   `json:"moving_to_cluster_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func view(tn types.Tailnet) TailnetView {
	return TailnetView{
		ID: tn.ID, Slug: tn.Slug, Status: string(tn.Status), OwnerClusterID: tn.OwnerClusterID,
		OwnerEpoch: tn.OwnerEpoch, PolicyRevision: tn.PolicyRevision, ConfigRevision: tn.ConfigRevision,
		CreatedAt: tn.CreatedAt, UpdatedAt: tn.UpdatedAt, MovingToClusterID: tn.MovingToClusterID,
	}
}

type idPath struct {
	ID string `maxLength:"128" path:"id"`
}

type (
	clusterBody struct {
		ID             string `json:"id"                         maxLength:"63"`
		EndpointURL    string `json:"endpoint_url"               maxLength:"512"`
		Class          string `enum:"shared,dedicated"           json:"class"`
		State          string `enum:"ACTIVE,DRAINING,DISABLED"   json:"state,omitempty" required:"false"`
		NoisePublicKey string `json:"noise_public_key,omitempty" maxLength:"128"        required:"false"`
		MaxDevices     int64  `json:"max_devices"                minimum:"1"`
	}
	createClusterIn struct {
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           clusterBody
	}
	clusterPatchBody struct {
		EndpointURL    *string `json:"endpoint_url,omitempty"     maxLength:"512" required:"false"`
		NoisePublicKey *string `json:"noise_public_key,omitempty" maxLength:"128" required:"false"`
		MaxDevices     *int64  `json:"max_devices,omitempty"      minimum:"1"     required:"false"`
	}
	updateClusterIn struct {
		ID             string `maxLength:"128"          path:"id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           clusterPatchBody
	}
	clusterStateBody struct {
		State            string `enum:"ACTIVE,DRAINING,DISABLED"                         json:"state"`
		ExpectedRevision uint64 `doc:"compare-and-swap on the cluster revision; 0 = any" json:"expected_revision,omitempty" required:"false"`
	}
	clusterStateIn struct {
		ID             string `maxLength:"128"          path:"id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           clusterStateBody
	}
	deleteClusterIn struct {
		ID             string `maxLength:"128"          path:"id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
	}
	clusterOut  struct{ Body placement.ClusterUsage }
	clustersOut struct {
		Body struct {
			Clusters []placement.ClusterUsage `json:"clusters"`
		}
	}
	placementIn struct {
		Class string `enum:"shared,dedicated" query:"class" required:"true"`
	}
	createTailnetBody struct {
		Slug  string `json:"slug"             maxLength:"63"`
		Class string `enum:"shared,dedicated" json:"class"`
	}
	createTailnetIn struct {
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           createTailnetBody
	}
	epochBody struct {
		ExpectedEpoch uint64 `json:"expected_epoch" minimum:"1"`
	}
	statusIn struct {
		ID             string `maxLength:"128"          path:"id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           epochBody
	}
	moveBody struct {
		FromCluster string `json:"from_cluster" maxLength:"63"`
		FromEpoch   uint64 `json:"from_epoch"   minimum:"1"`
		ToCluster   string `json:"to_cluster"   maxLength:"63"`
	}
	moveIn struct {
		ID             string `maxLength:"128"          path:"id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           moveBody
	}
	tailnetOut struct{ Body TailnetView }
	quotaOut   struct {
		Body struct {
			placement.Quota

			Reason string `json:"reason,omitempty"`
		}
	}
	auditIn struct {
		Limit int `default:"100" maximum:"1000" minimum:"1" query:"limit"`
	}
	createAPIKeyBody struct {
		ExpiresInSeconds int64  `doc:"lifetime of the key, at most 365 days"           json:"expires_in_seconds" maximum:"31536000" minimum:"1"`
		User             string `doc:"user of the tailnet the key acts as; must exist" json:"user,omitempty"     maxLength:"256"    required:"false"`
	}
	createAPIKeyIn struct {
		ID             string `maxLength:"128"          path:"id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
		Body           createAPIKeyBody
	}
	apiKeyCreated struct {
		placement.APIKeyInfo

		Key      string `doc:"the API key; shown only by the call that created it"                                 json:"key,omitempty"`
		Replayed bool   `doc:"an idempotent replay: the key was shown by the first call and cannot be shown again" json:"replayed,omitempty"`
	}
	apiKeyCreatedOut struct{ Body apiKeyCreated }
	apiKeysOut       struct {
		Body struct {
			Keys []placement.APIKeyInfo `json:"keys"`
		}
	}
	revokeAPIKeyIn struct {
		ID             string `maxLength:"128"          path:"id"`
		KeyID          uint64 `minimum:"1"              path:"key_id"`
		IdempotencyKey string `header:"Idempotency-Key" maxLength:"200" minLength:"1" required:"true"`
	}
	apiKeyOut        struct{ Body placement.APIKeyInfo }
	scheduleMoveBody struct {
		ToCluster   string    `json:"to_cluster" minLength:"1" maxLength:"63"`
		ScheduledAt time.Time `json:"scheduled_at" doc:"when mesh-placement requests the move (RFC 3339)"`
		Reason      string    `json:"reason,omitempty" maxLength:"256" required:"false" doc:"shown to the tenant admin and its clients"`
	}
	scheduleMoveIn struct {
		ID             string `path:"id" maxLength:"128"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true" minLength:"1" maxLength:"200"`
		Body           scheduleMoveBody
	}
	movePath struct {
		MoveID string `path:"move_id" maxLength:"64"`
	}
	cancelMoveIn struct {
		MoveID         string `path:"move_id" maxLength:"64"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true" minLength:"1" maxLength:"200"`
	}
	moveOut  struct{ Body placement.Move }
	movesOut struct {
		Body struct {
			Moves []placement.Move `json:"moves"`
		}
	}
	reconcileOut struct{ Body placement.Report }
	auditOut     struct {
		Body struct {
			Entries []placement.AuditEntry `json:"entries"`
		}
	}
)

func register(api huma.API, h *handlers) {
	tag := []string{"placement"}

	huma.Register(api, huma.Operation{
		OperationID: "listClusters", Method: http.MethodGet, Path: "/v1/clusters", Tags: tag,
		Summary: "List clusters with their tailnet and device counts",
	}, h.listClusters)
	huma.Register(api, huma.Operation{
		OperationID: "createCluster", Method: http.MethodPost, Path: "/v1/clusters", Tags: tag,
		Summary: "Register a cluster (DISABLED unless state is given)", DefaultStatus: http.StatusCreated,
	}, h.createCluster)
	huma.Register(api, huma.Operation{
		OperationID: "getCluster", Method: http.MethodGet, Path: "/v1/clusters/{id}", Tags: tag,
		Summary: "Get a cluster",
	}, h.getCluster)
	huma.Register(api, huma.Operation{
		OperationID: "updateCluster", Method: http.MethodPatch, Path: "/v1/clusters/{id}", Tags: tag,
		Summary: "Change endpoint, Noise public key or device budget",
	}, h.updateCluster)
	huma.Register(api, huma.Operation{
		OperationID: "updateClusterState", Method: http.MethodPut, Path: "/v1/clusters/{id}/state", Tags: tag,
		Summary: "Set the cluster state (revision only grows; owner epochs untouched)",
	}, h.updateClusterState)
	huma.Register(api, huma.Operation{
		OperationID: "deleteCluster", Method: http.MethodDelete, Path: "/v1/clusters/{id}", Tags: tag,
		Summary: "Delete a cluster that no tailnet references", DefaultStatus: http.StatusNoContent,
	}, h.deleteCluster)
	huma.Register(api, huma.Operation{
		OperationID: "placeTailnet", Method: http.MethodGet, Path: "/v1/placement", Tags: tag,
		Summary: "Preview the cluster a new tailnet of a class would get",
	}, h.placeTailnet)
	huma.Register(api, huma.Operation{
		OperationID: "createTailnet", Method: http.MethodPost, Path: "/v1/tailnets", Tags: tag,
		Summary: "Place and create a tailnet: PENDING, owner, deny-all policy, one transaction", DefaultStatus: http.StatusCreated,
	}, h.createTailnet)
	huma.Register(api, huma.Operation{
		OperationID: "getTailnet", Method: http.MethodGet, Path: "/v1/tailnets/{id}", Tags: tag,
		Summary: "Get a tailnet with its device count",
	}, h.getTailnet)
	huma.Register(api, huma.Operation{
		OperationID: "activateTailnet", Method: http.MethodPost, Path: "/v1/tailnets/{id}/activate", Tags: tag,
		Summary: "PENDING/SUSPENDED → ACTIVE at the expected owner epoch",
	}, h.activateTailnet)
	huma.Register(api, huma.Operation{
		OperationID: "suspendTailnet", Method: http.MethodPost, Path: "/v1/tailnets/{id}/suspend", Tags: tag,
		Summary: "PENDING/ACTIVE → SUSPENDED at the expected owner epoch",
	}, h.suspendTailnet)
	huma.Register(api, huma.Operation{
		OperationID: "moveTailnetOwner", Method: http.MethodPost, Path: "/v1/tailnets/{id}/owner", Tags: tag,
		Summary: "Request a move to another cluster (moving_to; the owner hands it over, owner_epoch + 1, ADR 0010), or bump the epoch on the same cluster",
	}, h.moveOwner)
	huma.Register(api, huma.Operation{
		OperationID: "getTailnetQuota", Method: http.MethodGet, Path: "/v1/tailnets/{id}/quota", Tags: tag,
		Summary: "Enrollment quota decision of a tailnet",
	}, h.quota)
	huma.Register(api, huma.Operation{
		OperationID: "createTailnetAPIKey", Method: http.MethodPost, Path: "/v1/tailnets/{id}/apikeys", Tags: tag,
		Summary: "Mint an API key bound to the tailnet (tenant /api/v2); the key is shown once", DefaultStatus: http.StatusCreated,
	}, h.createAPIKey)
	huma.Register(api, huma.Operation{
		OperationID: "listTailnetAPIKeys", Method: http.MethodGet, Path: "/v1/tailnets/{id}/apikeys", Tags: tag,
		Summary: "List the API keys of a tailnet (no secrets)",
	}, h.listAPIKeys)
	huma.Register(api, huma.Operation{
		OperationID: "revokeTailnetAPIKey", Method: http.MethodPost, Path: "/v1/tailnets/{id}/apikeys/{key_id}/revoke", Tags: tag,
		Summary: "Revoke an API key of the tailnet",
	}, h.revokeAPIKey)
	huma.Register(api, huma.Operation{
		OperationID: "reconcileReport", Method: http.MethodGet, Path: "/v1/reconcile", Tags: tag,
		Summary: "Cross-cluster anomalies R1-R9 by id and count (read only; nothing is moved)",
	}, h.reconcile)
	huma.Register(api, huma.Operation{
		OperationID: "scheduleTailnetMove", Method: http.MethodPost, Path: "/v1/tailnets/{id}/moves", Tags: tag,
		Summary:       "Schedule a planned move of the tailnet to another cluster (P12b); requested at scheduled_at, handed over by the owner (ADR 0010)",
		DefaultStatus: http.StatusCreated,
	}, h.scheduleMove)
	huma.Register(api, huma.Operation{
		OperationID: "listTailnetMoves", Method: http.MethodGet, Path: "/v1/tailnets/{id}/moves", Tags: tag,
		Summary: "Planned moves of the tailnet, newest first",
	}, h.listMoves)
	huma.Register(api, huma.Operation{
		OperationID: "getMove", Method: http.MethodGet, Path: "/v1/moves/{move_id}", Tags: tag,
		Summary: "Get a planned move",
	}, h.getMove)
	huma.Register(api, huma.Operation{
		OperationID: "cancelMove", Method: http.MethodPost, Path: "/v1/moves/{move_id}/cancel", Tags: tag,
		Summary: "Cancel a planned move before its hand-over",
	}, h.cancelMove)
	huma.Register(api, huma.Operation{
		OperationID: "listAudit", Method: http.MethodGet, Path: "/v1/audit", Tags: tag,
		Summary: "Newest operator audit entries",
	}, h.audit)
}

// op builds the placement.Op of a mutation: the verified operator identity
// and a fingerprint of the operation and its whole request.
func op(ctx context.Context, operation, key string, request ...any) (placement.Op, error) {
	actor, ok := actorFrom(ctx)
	if !ok {
		return placement.Op{}, huma.Error401Unauthorized("operator certificate required")
	}

	b, err := json.Marshal(append([]any{operation}, request...))
	if err != nil {
		return placement.Op{}, huma.Error500InternalServerError("encoding request")
	}

	sum := sha256.Sum256(b)

	return placement.Op{Actor: actor, IdempotencyKey: key, RequestHash: hex.EncodeToString(sum[:])}, nil
}

// readGuard refuses reads outside the operator check (defence in depth: the
// handler is always wrapped by Authorizer).
func readGuard(ctx context.Context) error {
	if _, ok := actorFrom(ctx); !ok {
		return huma.Error401Unauthorized("operator certificate required")
	}

	return nil
}

// apiError maps registry errors to HTTP; anything unexpected is a 500 with
// no detail.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, placement.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, placement.ErrInvalid):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, placement.ErrIdempotencyConflict):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, placement.ErrStaleEpoch), errors.Is(err, placement.ErrStaleRevision),
		errors.Is(err, placement.ErrConflict), errors.Is(err, placement.ErrClusterInUse),
		errors.Is(err, placement.ErrInvalidTransition):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, placement.ErrNoCapacity):
		return huma.Error503ServiceUnavailable(err.Error())
	default:
		return huma.Error500InternalServerError("internal error")
	}
}

// reconcile serves the cross-cluster reconcile report (P12, Q4): a read;
// it neither moves a tailnet nor writes the audit log.
func (h *handlers) reconcile(ctx context.Context, _ *struct{}) (*reconcileOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	rep, err := h.reg.Reconcile(ctx)
	if err != nil {
		return nil, apiError(err)
	}

	return &reconcileOut{Body: rep}, nil
}

func (h *handlers) listClusters(ctx context.Context, _ *struct{}) (*clustersOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	cs, err := h.reg.ListClusters(ctx)
	if err != nil {
		return nil, apiError(err)
	}

	out := &clustersOut{}
	out.Body.Clusters = cs

	return out, nil
}

func (h *handlers) createCluster(ctx context.Context, in *createClusterIn) (*clusterOut, error) {
	o, err := op(ctx, "createCluster", in.IdempotencyKey, in.Body)
	if err != nil {
		return nil, err
	}

	c, err := h.reg.CreateCluster(ctx, o, placement.ClusterSpec{
		ID: in.Body.ID, EndpointURL: in.Body.EndpointURL, Class: placement.Class(in.Body.Class),
		State: placement.ClusterState(in.Body.State), NoisePublicKey: in.Body.NoisePublicKey, MaxDevices: in.Body.MaxDevices,
	})
	if err != nil {
		return nil, apiError(err)
	}

	return &clusterOut{Body: placement.ClusterUsage{Cluster: c}}, nil
}

func (h *handlers) getCluster(ctx context.Context, in *idPath) (*clusterOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	c, err := h.reg.GetCluster(ctx, in.ID)
	if err != nil {
		return nil, apiError(err)
	}

	return &clusterOut{Body: c}, nil
}

func (h *handlers) updateCluster(ctx context.Context, in *updateClusterIn) (*clusterOut, error) {
	o, err := op(ctx, "updateCluster", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	c, err := h.reg.UpdateCluster(ctx, o, in.ID, placement.ClusterPatch{
		EndpointURL: in.Body.EndpointURL, NoisePublicKey: in.Body.NoisePublicKey, MaxDevices: in.Body.MaxDevices,
	})
	if err != nil {
		return nil, apiError(err)
	}

	return &clusterOut{Body: placement.ClusterUsage{Cluster: c}}, nil
}

func (h *handlers) updateClusterState(ctx context.Context, in *clusterStateIn) (*clusterOut, error) {
	o, err := op(ctx, "updateClusterState", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	c, err := h.reg.UpdateClusterState(ctx, o, in.ID, placement.ClusterState(in.Body.State), in.Body.ExpectedRevision)
	if err != nil {
		return nil, apiError(err)
	}

	return &clusterOut{Body: placement.ClusterUsage{Cluster: c}}, nil
}

func (h *handlers) deleteCluster(ctx context.Context, in *deleteClusterIn) (*struct{}, error) {
	o, err := op(ctx, "deleteCluster", in.IdempotencyKey, in.ID)
	if err != nil {
		return nil, err
	}

	return nil, apiError(h.reg.DeleteCluster(ctx, o, in.ID))
}

func (h *handlers) placeTailnet(ctx context.Context, in *placementIn) (*clusterOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	c, err := h.reg.PlaceTailnet(ctx, placement.Class(in.Class))
	if err != nil {
		return nil, apiError(err)
	}

	return &clusterOut{Body: placement.ClusterUsage{Cluster: c}}, nil
}

func (h *handlers) createTailnet(ctx context.Context, in *createTailnetIn) (*tailnetOut, error) {
	o, err := op(ctx, "createTailnet", in.IdempotencyKey, in.Body)
	if err != nil {
		return nil, err
	}

	tn, err := h.reg.CreateTailnet(ctx, o, in.Body.Slug, placement.Class(in.Body.Class))
	if err != nil {
		return nil, apiError(err)
	}

	return &tailnetOut{Body: view(tn)}, nil
}

func (h *handlers) getTailnet(ctx context.Context, in *idPath) (*tailnetOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	info, err := h.reg.GetTailnet(ctx, in.ID)
	if err != nil {
		return nil, apiError(err)
	}

	v := view(info.Tailnet)
	v.Devices = &info.Devices

	return &tailnetOut{Body: v}, nil
}

func (h *handlers) activateTailnet(ctx context.Context, in *statusIn) (*tailnetOut, error) {
	o, err := op(ctx, "activateTailnet", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	tn, err := h.reg.ActivateTailnet(ctx, o, in.ID, in.Body.ExpectedEpoch)
	if err != nil {
		return nil, apiError(err)
	}

	return &tailnetOut{Body: view(tn)}, nil
}

func (h *handlers) suspendTailnet(ctx context.Context, in *statusIn) (*tailnetOut, error) {
	o, err := op(ctx, "suspendTailnet", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	tn, err := h.reg.SuspendTailnet(ctx, o, in.ID, in.Body.ExpectedEpoch)
	if err != nil {
		return nil, apiError(err)
	}

	return &tailnetOut{Body: view(tn)}, nil
}

func (h *handlers) moveOwner(ctx context.Context, in *moveIn) (*tailnetOut, error) {
	o, err := op(ctx, "moveTailnetOwner", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	tn, err := h.reg.MoveOwner(ctx, o, in.ID, in.Body.FromCluster, in.Body.FromEpoch, in.Body.ToCluster)
	if err != nil {
		return nil, apiError(err)
	}

	return &tailnetOut{Body: view(tn)}, nil
}

func (h *handlers) quota(ctx context.Context, in *idPath) (*quotaOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	id, err := tailnet.ParseID(in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("tailnet not found")
	}

	q, err := h.reg.EnrollmentAllowed(ctx, id)
	if errors.Is(err, placement.ErrNotFound) {
		return nil, apiError(err)
	}

	out := &quotaOut{}
	out.Body.Quota = q

	if err != nil {
		var known bool

		for _, e := range []error{placement.ErrQuotaExceeded, placement.ErrTailnetNotActive, placement.ErrUnknownCluster} {
			known = known || errors.Is(err, e)
		}

		if !known {
			return nil, apiError(err)
		}

		out.Body.Reason = err.Error()
	}

	return out, nil
}

func (h *handlers) audit(ctx context.Context, in *auditIn) (*auditOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	es, err := h.reg.AuditLog(ctx, in.Limit)
	if err != nil {
		return nil, apiError(err)
	}

	out := &auditOut{}
	out.Body.Entries = es

	return out, nil
}

func (h *handlers) createAPIKey(ctx context.Context, in *createAPIKeyIn) (*apiKeyCreatedOut, error) {
	o, err := op(ctx, "createTailnetAPIKey", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	info, key, err := h.reg.CreateAPIKey(ctx, o, in.ID, time.Duration(in.Body.ExpiresInSeconds)*time.Second, in.Body.User)
	if err != nil {
		return nil, apiError(err)
	}

	return &apiKeyCreatedOut{Body: apiKeyCreated{APIKeyInfo: info, Key: key, Replayed: key == ""}}, nil
}

func (h *handlers) listAPIKeys(ctx context.Context, in *idPath) (*apiKeysOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	keys, err := h.reg.ListAPIKeys(ctx, in.ID)
	if err != nil {
		return nil, apiError(err)
	}

	out := &apiKeysOut{}
	out.Body.Keys = keys

	return out, nil
}

func (h *handlers) revokeAPIKey(ctx context.Context, in *revokeAPIKeyIn) (*apiKeyOut, error) {
	o, err := op(ctx, "revokeTailnetAPIKey", in.IdempotencyKey, in.ID, in.KeyID)
	if err != nil {
		return nil, err
	}

	info, err := h.reg.RevokeAPIKey(ctx, o, in.ID, in.KeyID)
	if err != nil {
		return nil, apiError(err)
	}

	return &apiKeyOut{Body: info}, nil
}

func (h *handlers) scheduleMove(ctx context.Context, in *scheduleMoveIn) (*moveOut, error) {
	o, err := op(ctx, "scheduleTailnetMove", in.IdempotencyKey, in.ID, in.Body)
	if err != nil {
		return nil, err
	}

	m, err := h.reg.ScheduleMove(ctx, o, in.ID, in.Body.ToCluster, in.Body.ScheduledAt, in.Body.Reason)
	if err != nil {
		return nil, apiError(err)
	}

	return &moveOut{Body: m}, nil
}

func (h *handlers) listMoves(ctx context.Context, in *idPath) (*movesOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	ms, err := h.reg.ListMoves(ctx, in.ID)
	if err != nil {
		return nil, apiError(err)
	}

	out := &movesOut{}
	out.Body.Moves = ms

	if out.Body.Moves == nil {
		out.Body.Moves = []placement.Move{}
	}

	return out, nil
}

func (h *handlers) getMove(ctx context.Context, in *movePath) (*moveOut, error) {
	if err := readGuard(ctx); err != nil { //nolint:noinlineerr
		return nil, err
	}

	m, err := h.reg.GetMove(ctx, in.MoveID)
	if err != nil {
		return nil, apiError(err)
	}

	return &moveOut{Body: m}, nil
}

func (h *handlers) cancelMove(ctx context.Context, in *cancelMoveIn) (*moveOut, error) {
	o, err := op(ctx, "cancelMove", in.IdempotencyKey, in.MoveID)
	if err != nil {
		return nil, err
	}

	m, err := h.reg.CancelScheduledMove(ctx, o, in.MoveID)
	if err != nil {
		return nil, apiError(err)
	}

	return &moveOut{Body: m}, nil
}
