package hscontrol

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"tailscale.com/tailcfg"
)

// VerifyHandler answers an external DERP server of this cluster (ADR 0009):
// the request must carry the cluster's derp.verify_secret_path secret as
// ?secret=, otherwise every caller gets the same 403 (no node-key oracle,
// P08a F2). A node key is admitted iff it belongs to a live, unexpired node
// of a tailnet this cluster owns and serves now: a process without the
// cluster lease (standby) admits nobody, and a tailnet being handed over or
// owned by another cluster is not served here. The DERP server reaches the
// cluster through its public URL, so the lease holder answers.
func (c *Cluster) VerifyHandler(w http.ResponseWriter, req *http.Request) {
	if !c.front.verifyCallerAuthenticated(req) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, verifyBodyLimit)

	body, err := io.ReadAll(req.Body)
	if err != nil {
		httpError(w, NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large",
			fmt.Errorf("reading request body: %w", err)))

		return
	}

	var admit tailcfg.DERPAdmitClientRequest

	err = json.Unmarshal(body, &admit)
	if err != nil {
		httpError(w, NewHTTPError(http.StatusBadRequest, "Bad Request: invalid JSON",
			fmt.Errorf("parsing DERP client request: %w", err)))

		return
	}

	w.Header().Set("Content-Type", "application/json")

	err = json.NewEncoder(w).Encode(&tailcfg.DERPAdmitClientResponse{Allow: c.admitsNodeKey(admit)})
	if err != nil {
		log.Debug().Err(err).Msg("DERP verify: writing response")
	}
}

// admitsNodeKey reports whether a runtime this process serves under the
// cluster lease holds a live, unexpired node with the request's node key.
func (c *Cluster) admitsNodeKey(admit tailcfg.DERPAdmitClientRequest) bool {
	if !c.Ready() {
		return false
	}

	for _, h := range c.runtimesSnapshot() {
		if !h.servable() {
			continue
		}

		nv, ok := h.state.GetNodeByNodeKey(admit.NodePublic)
		if ok && nv.Valid() && !nv.IsExpired() {
			return true
		}
	}

	return false
}
