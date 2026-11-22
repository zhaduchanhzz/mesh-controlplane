package hscontrol

import (
	"errors"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util/zlog/zf"

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
	case ns.headscale == nil:
		ns.headscale = h

		return nil
	case ns.headscale != h:
		return ErrSessionTenantSwitch
	default:
		return nil
	}
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

	return nil
}

// refuseRoute answers a request whose tenant could not be resolved or that
// tried to switch tenants. A switch closes the whole session (I01). The
// response never says which tenant exists.
func (ns *noiseServer) refuseRoute(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrSessionTenantSwitch) {
		http.Error(w, "forbidden", http.StatusForbidden)

		if ns.conn != nil {
			_ = ns.conn.Close()
		}

		return
	}

	if errors.Is(err, errSessionUnbound) || errors.Is(err, db.ErrUnknownTenant) ||
		errors.Is(err, db.ErrTailnetNotActive) || errors.Is(err, db.ErrTailnetNotOwned) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	// A lookup that failed (database unreachable) is retried by the client.
	log.Error().Err(err).Str(zf.MachineKey, ns.machineKey.ShortString()).Msg("resolving tailnet of noise request")
	w.Header().Set("Retry-After", "10")
	http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
}
