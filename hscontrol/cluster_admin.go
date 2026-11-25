package hscontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/db"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/types"
	"github.com/zhaduchanhzz/mesh-controlplane/hscontrol/util"
	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
	"tailscale.com/net/tsaddr"
)

// Live operator administration of a running cluster (P08b, owner decision
// Q2, direction A). Node and user state lives in the memory of the runtime
// serving the tailnet (NodeStore, PolicyManager), so a change written to the
// database by the offline CLI would not reach connected clients. These
// operations run in the runtime itself, through the same State calls and
// change path as the single-tailnet admin API: peers of the tailnet see a
// deleted or expired node within a map update, and no other tailnet gets a
// frame (P07).
//
// The endpoint is served only on the unix socket (unix_socket, mode
// unix_socket_permission): socket access is operator trust, as for the
// single-tailnet socket. It is never mounted on the public listener nor on
// the metrics listener (AGENTS 9). Policy and DNS stay on the offline CLI,
// which writes the database and bumps the revision the runtime reloads.
//
// A tailnet is named by its ID only (never the slug); a tailnet this process
// does not serve is 404, a node or user of another tailnet is 404 like one
// that does not exist, and a standby process answers 503.

var errAdminSocketPath = errors.New("cluster admin socket")

const (
	clusterAdminPrefix = "/cluster/v1/tailnet/{tailnet}"
	adminMaxBody       = 64 << 10
)

// adminNode is a node in the admin listing.
type adminNode struct {
	ID        uint64     `json:"id"`
	Name      string     `json:"name"`
	Hostname  string     `json:"hostname"`
	User      string     `json:"user,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	IPs       []string   `json:"ips"`
	Online    bool       `json:"online"`
	Expiry    *time.Time `json:"expiry,omitempty"`
	Expired   bool       `json:"expired"`
	Announced []string   `json:"announced_routes,omitempty"`
	Approved  []string   `json:"approved_routes,omitempty"`
}

type adminRoutesBody struct {
	Routes []string `json:"routes"`
}

type adminHandlerFunc func(w http.ResponseWriter, r *http.Request, id tailnet.ID, h *Headscale) (string, error)

// adminError is an error with the HTTP status the admin endpoint answers.
type adminError struct {
	status int
	msg    string
}

func (e *adminError) Error() string { return e.msg }

func adminErr(status int, format string, a ...any) error {
	return &adminError{status: status, msg: fmt.Sprintf(format, a...)}
}

// AdminHandler is the live operator API of the cluster, for the unix socket
// only.
func (c *Cluster) AdminHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Route(clusterAdminPrefix, func(r chi.Router) {
		r.Get("/nodes", c.admin("list nodes", adminListNodes))
		r.Delete("/node/{node}", c.admin("delete node", adminDeleteNode))
		r.Post("/node/{node}/expire", c.admin("expire node", adminExpireNode))
		r.Post("/node/{node}/routes", c.admin("approve routes", adminApproveRoutes))
		r.Delete("/user/{user}", c.admin("delete user", adminDeleteUser))
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeAdminError(w, http.StatusNotFound, "not found")
	})

	return r
}

// admin resolves the tailnet to the runtime serving it and runs fn, with an
// audit line for every call.
func (c *Cluster) admin(op string, fn adminHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := chi.URLParam(r, "tailnet")

		id, err := tailnet.ParseID(raw)
		if err != nil {
			writeAdminError(w, http.StatusNotFound, "tailnet not found")

			return
		}

		if !c.ready.Load() || !c.leaseHeld() {
			writeAdminError(w, http.StatusServiceUnavailable,
				"standby: this process does not hold the cluster lease; run the command on the active process")

			return
		}

		h, ok := c.Runtime(id)
		if !ok {
			writeAdminError(w, http.StatusNotFound, "tailnet not found")

			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)

		target, err := fn(w, r, id, h)

		ev := log.Info()
		if err != nil {
			ev = log.Warn().Err(err)
		}

		ev.Str("op", op).Str("tailnet", id.String()).Str("target", target).
			Msg("cluster admin (unix socket)")

		if err != nil {
			if ae, ok := errors.AsType[*adminError](err); ok {
				writeAdminError(w, ae.status, ae.msg)

				return
			}

			writeAdminError(w, http.StatusInternalServerError, op+": "+err.Error())
		}
	}
}

func writeAdminError(w http.ResponseWriter, status int, msg string) {
	writeAdminJSON(w, status, map[string]string{"error": msg})
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// adminNodeOf returns the node {node} of the runtime's tailnet; a node of
// another tailnet is not in this runtime's NodeStore (node IDs are global,
// ADR 0002), so it is 404 like an unknown ID.
func adminNodeOf(r *http.Request, h *Headscale) (types.NodeView, string, error) {
	raw := chi.URLParam(r, "node")

	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		return types.NodeView{}, raw, adminErr(http.StatusNotFound, "node not found")
	}

	node, ok := h.state.GetNodeByID(types.NodeID(n))
	if !ok {
		return types.NodeView{}, raw, adminErr(http.StatusNotFound, "node not found")
	}

	return node, raw, nil
}

func adminNodeView(n types.NodeView) adminNode {
	out := adminNode{
		ID:       uint64(n.ID()),
		Name:     n.GivenName(),
		Hostname: n.Hostname(),
		IPs:      n.IPsAsString(),
		Expired:  n.IsExpired(),
		Tags:     n.Tags().AsSlice(),
	}

	if n.User().Valid() {
		out.User = n.User().Name()
	}

	if on, ok := n.IsOnline().GetOk(); ok {
		out.Online = on
	}

	if e, ok := n.Expiry().GetOk(); ok {
		out.Expiry = &e
	}

	for _, p := range n.AnnouncedRoutes() {
		out.Announced = append(out.Announced, p.String())
	}

	for _, p := range n.ApprovedRoutes().All() {
		out.Approved = append(out.Approved, p.String())
	}

	return out
}

func adminListNodes(w http.ResponseWriter, _ *http.Request, _ tailnet.ID, h *Headscale) (string, error) {
	nodes := h.state.ListNodes()

	out := make([]adminNode, 0, nodes.Len())
	for _, n := range nodes.All() {
		out = append(out, adminNodeView(n))
	}

	slices.SortFunc(out, func(a, b adminNode) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}

		return 0
	})

	writeAdminJSON(w, http.StatusOK, map[string]any{"nodes": out})

	return "", nil
}

func adminDeleteNode(w http.ResponseWriter, r *http.Request, _ tailnet.ID, h *Headscale) (string, error) {
	node, raw, err := adminNodeOf(r, h)
	if err != nil {
		return raw, err
	}

	changes, err := h.state.DeleteNode(node)
	h.Change(changes...)

	if err != nil {
		return raw, err
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{"deleted": uint64(node.ID())})

	return raw, nil
}

func adminExpireNode(w http.ResponseWriter, r *http.Request, _ tailnet.ID, h *Headscale) (string, error) {
	node, raw, err := adminNodeOf(r, h)
	if err != nil {
		return raw, err
	}

	updated, ch, err := h.state.ExpireNodeAt(node.ID(), time.Now())
	h.Change(ch)

	if err != nil {
		return raw, err
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{"node": adminNodeView(updated)})

	return raw, nil
}

func adminApproveRoutes(w http.ResponseWriter, r *http.Request, _ tailnet.ID, h *Headscale) (string, error) {
	node, raw, err := adminNodeOf(r, h)
	if err != nil {
		return raw, err
	}

	var body adminRoutesBody

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&body); err != nil { //nolint:noinlineerr
		return raw, adminErr(http.StatusBadRequest, "body: %v", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) { //nolint:noinlineerr
		return raw, adminErr(http.StatusBadRequest, "body: trailing data")
	}

	routes, err := parseAdminRoutes(body.Routes)
	if err != nil {
		return raw, err
	}

	updated, ch, err := h.state.SetApprovedRoutes(node.ID(), routes)
	h.Change(ch)

	if err != nil {
		return raw, err
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{"node": adminNodeView(updated)})

	return raw, nil
}

// parseAdminRoutes parses approved routes like the v2 API: masked prefixes,
// an exit route approves both families, sorted without duplicates.
func parseAdminRoutes(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))

	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, adminErr(http.StatusBadRequest, "route %q: %v", s, err)
		}

		if err := util.CheckRouteMasked(p); err != nil { //nolint:noinlineerr
			return nil, adminErr(http.StatusBadRequest, "route %q: %v", s, err)
		}

		if tsaddr.IsExitRoute(p) {
			out = append(out, tsaddr.AllIPv4(), tsaddr.AllIPv6())

			continue
		}

		out = append(out, p)
	}

	slices.SortFunc(out, netip.Prefix.Compare)

	return slices.Compact(out), nil
}

func adminDeleteUser(w http.ResponseWriter, r *http.Request, _ tailnet.ID, h *Headscale) (string, error) {
	name := chi.URLParam(r, "user")

	user, err := h.state.GetUserByName(name)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			return name, adminErr(http.StatusNotFound, "user not found")
		}

		return name, err
	}

	ch, err := h.state.DeleteUser(types.UserID(user.ID))
	if err != nil {
		if errors.Is(err, db.ErrUserStillHasNodes) {
			return name, adminErr(http.StatusConflict, "%v", err)
		}

		return name, err
	}

	h.Change(ch)

	writeAdminJSON(w, http.StatusOK, map[string]any{"deleted": name})

	return name, nil
}

// listenAdminSocket opens the unix socket of the admin API (unix_socket),
// replacing a stale socket file, with mode unix_socket_permission.
func listenAdminSocket(cfg *types.Config) (net.Listener, error) {
	path := cfg.UnixSocket

	// Only a stale socket is removed: anything else at the path is an
	// operator mistake, not ours to delete.
	if fi, err := os.Lstat(path); err == nil { //nolint:noinlineerr
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%w: %s exists and is not a socket", errAdminSocketPath, path)
		}

		if err := os.Remove(path); err != nil { //nolint:noinlineerr
			return nil, fmt.Errorf("removing stale admin socket: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:noinlineerr,mnd
		return nil, fmt.Errorf("creating admin socket directory: %w", err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, &types.ListenerBindError{Listener: "cluster admin socket", YAMLKey: "unix_socket", Addr: path, Err: err}
	}

	if err := os.Chmod(path, fs.FileMode(cfg.UnixSocketPermission)); err != nil { //nolint:noinlineerr
		_ = ln.Close()

		return nil, fmt.Errorf("changing admin socket permission: %w", err)
	}

	return ln, nil
}
