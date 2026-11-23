package hscontrol

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util/zlog/zf"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/placement"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

var (
	// ErrSessionTenantSwitch reports a request that resolves to another tenant
	// than the one its Noise session is bound to (I01). The session is closed.
	ErrSessionTenantSwitch = errors.New("noise session is bound to another tailnet")
	// errSessionUnbound reports a request that needs a tenant on a session that
	// has not resolved one yet (cluster process only).
	errSessionUnbound = errors.New("noise session is not bound to a tailnet")
	// errRuntimeRetired reports a session bound to a tailnet runtime that this
	// cluster stopped serving (ownership moved, P10). The session is closed
	// so the client reconnects and is resolved again.
	errRuntimeRetired = errors.New("tailnet is no longer served by this cluster process")
)

// sessionRouter resolves the tailnet runtime for requests on a shared Noise
// key (ADR 0007). Implementations fail closed: no default tenant.
type sessionRouter interface {
	// runtimeForRegister resolves a registration: by its auth key, else by an
	// existing node (machine key + node key).
	runtimeForRegister(req tailcfg.RegisterRequest, mk key.MachinePublic) (*Headscale, error)
	// runtimeForNode resolves an existing node by machine key (authenticated
	// by Noise) and node key.
	runtimeForNode(mk key.MachinePublic, nk key.NodePublic) (*Headscale, error)
}

// bind attaches the session to h, or refuses a different tenant. Requests on
// one session run concurrently (HTTP/2), so binding is locked. ns.headscale is
// written once, from nil, and every handler passes bind or requireBound before
// reading it, which orders those reads after the write.
func (ns *noiseServer) bind(h *Headscale) error {
	ns.bindMu.Lock()
	defer ns.bindMu.Unlock()

	switch {
	case h == nil:
		return errSessionUnbound
	case ns.headscale != nil && ns.headscale.retired.Load():
		return errRuntimeRetired
	case h.retired.Load():
		return errRuntimeRetired
	case ns.headscale == nil:
		ns.headscale = h
		h.sessions.Store(ns, struct{}{})

		// Retired between the check and the registration: closeSessions may
		// have missed this session, so refuse it here.
		if h.retired.Load() {
			h.sessions.Delete(ns)

			return errRuntimeRetired
		}

		return nil
	case ns.headscale != h:
		return ErrSessionTenantSwitch
	default:
		return nil
	}
}

// unbind forgets the session when it ends.
func (ns *noiseServer) unbind() {
	ns.bindMu.Lock()
	defer ns.bindMu.Unlock()

	if ns.router != nil && ns.headscale != nil {
		ns.headscale.sessions.Delete(ns)
	}
}

// closeSessions closes every Noise session bound to h (cluster runtimes).
func (h *Headscale) closeSessions() {
	h.sessions.Range(func(k, _ any) bool {
		if ns, ok := k.(*noiseServer); ok && ns.conn != nil {
			_ = ns.conn.Close()
		}

		h.sessions.Delete(k)

		return true
	})
}

// bindForRegister binds the session for a registration request.
func (ns *noiseServer) bindForRegister(req tailcfg.RegisterRequest) error {
	if ns.router == nil {
		return nil // single-tailnet process: bound at upgrade
	}

	h, err := ns.router.runtimeForRegister(req, ns.machineKey)
	if err != nil {
		return err
	}

	return ns.bind(h)
}

// bindForNode binds the session for a request carrying a node key.
func (ns *noiseServer) bindForNode(nk key.NodePublic) error {
	if ns.router == nil {
		return nil
	}

	h, err := ns.router.runtimeForNode(ns.machineKey, nk)
	if err != nil {
		return err
	}

	return ns.bind(h)
}

// requireBound refuses requests that need a tenant before one is resolved.
func (ns *noiseServer) requireBound() error {
	ns.bindMu.Lock()
	defer ns.bindMu.Unlock()

	if ns.headscale == nil {
		return errSessionUnbound
	}

	if ns.headscale.retired.Load() {
		return errRuntimeRetired
	}

	return nil
}

// refuseRoute answers a request whose tenant could not be resolved or that
// tried to switch tenants. A switch closes the whole session (I01). The
// response never says which tenant exists.
func (ns *noiseServer) refuseRoute(w http.ResponseWriter, err error) {
	if errors.Is(err, errRuntimeRetired) {
		// The client reconnects and is resolved again, now to the new owner
		// (or refused if this cluster no longer serves the tailnet).
		w.Header().Set("Retry-After", "1")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)

		if ns.conn != nil {
			_ = ns.conn.Close()
		}

		return
	}

	if errors.Is(err, ErrSessionTenantSwitch) {
		http.Error(w, "forbidden", http.StatusForbidden)

		if ns.conn != nil {
			_ = ns.conn.Close()
		}

		return
	}

	if isTenantRefusal(err) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	// A lookup that failed (database unreachable) is retried by the client.
	log.Error().Err(err).Str(zf.MachineKey, ns.machineKey.ShortString()).Msg("resolving tailnet of noise request")
	w.Header().Set("Retry-After", "10")
	http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
}

// registerRefusal maps a tenant-resolution failure of a registration to the
// RegisterResponse.Error the client shows, for permanent refusals only. The
// message is the same for an unknown key and for a key of a foreign or
// inactive tailnet, so it reveals nothing about other tenants.
func registerRefusal(req tailcfg.RegisterRequest, err error) (string, bool) {
	if errors.Is(err, ErrSessionTenantSwitch) || errors.Is(err, errRuntimeRetired) {
		return "", false // closes the session instead
	}

	if errors.Is(err, placement.ErrQuotaExceeded) {
		// Only reached with a valid key of this tailnet: the holder may know.
		return "this tailnet has reached its device limit", true
	}

	if !isTenantRefusal(err) {
		return "", false // lookup failure: 503, the client retries
	}

	if req.Auth != nil && req.Auth.AuthKey != "" {
		return "invalid pre auth key", true
	}

	return "this server only registers nodes with an auth key", true
}

// isTenantRefusal reports a permanent refusal by tenant resolution or
// placement: unknown, foreign or inactive tailnet, or an owner cluster whose
// quota is unknown. Other errors are lookup failures (retried).
func isTenantRefusal(err error) bool {
	for _, e := range []error{
		errSessionUnbound, db.ErrUnknownTenant, db.ErrTailnetNotActive, db.ErrTailnetNotOwned,
		placement.ErrNotFound, placement.ErrTailnetNotActive, placement.ErrUnknownCluster,
	} {
		if errors.Is(err, e) {
			return true
		}
	}

	return false
}

func writeRegisterResponse(w http.ResponseWriter, status int, resp *tailcfg.RegisterResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	err := json.NewEncoder(w).Encode(resp)
	if err != nil {
		log.Error().Caller().Err(err).Msg("writing RegisterResponse")
	}
}
