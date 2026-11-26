package placementapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	errNoOperators = errors.New("no operator identity allowed")
	errNoClientCA  = errors.New("client CA pool is required")
)

// Authorizer admits platform operators only (PLAN §4 item 6, I08/S10):
//
//   - the client must present a certificate that verifies against the pinned
//     operator CA (the TLS layer, see TLSConfig: a certificate from another CA
//     fails the handshake);
//   - one of its URI or DNS SANs must be in the allowed identity list (exact
//     match; the subject CN is ignored);
//   - a request carrying any bearer, API key, basic or cookie credential is
//     refused with 403 even with a valid certificate: the operator API never
//     consumes tenant credentials, so their presence is a misdirected or
//     hostile call.
//
// The matched identity is the actor recorded in the audit.
type Authorizer struct {
	allowed map[string]bool

	// revoked, when set (P13f L5), refuses chains with a revoked
	// certificate, and everything while its file is unavailable.
	revoked *Revocations
}

// WithRevocations makes a check every request against r (P13f L5).
func (a *Authorizer) WithRevocations(r *Revocations) *Authorizer {
	a.revoked = r

	return a
}

// Reasons an operator request is refused (mesh_placement_operator_refused_total).
const (
	refusedCredentials = "credentials"
	refusedNoCert      = "no_certificate"
	refusedIdentity    = "identity"
	refusedRevoked     = "revoked"
	refusedRevocation  = "revocation_unavailable"
)

var operatorRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "mesh_placement",
	Name:      "operator_refused_total",
	Help: "Operator API requests refused, by reason: credentials (a bearer/API key/cookie), no_certificate, " +
		"identity (not an allowed operator), revoked, revocation_unavailable (fail closed).",
}, []string{"reason"})

func init() {
	for _, r := range []string{refusedCredentials, refusedNoCert, refusedIdentity, refusedRevoked, refusedRevocation} {
		operatorRefused.WithLabelValues(r)
	}
}

// NewAuthorizer returns an Authorizer for the given operator identities
// (e.g. "spiffe://mesh/operator/alice"). An empty list is refused: the API
// would admit no one, and a misconfiguration must not look like a running
// service.
func NewAuthorizer(identities []string) (*Authorizer, error) {
	a := &Authorizer{allowed: map[string]bool{}}

	for _, id := range identities {
		id = strings.TrimSpace(id)
		if id != "" {
			a.allowed[id] = true
		}
	}

	if len(a.allowed) == 0 {
		return nil, errNoOperators
	}

	return a, nil
}

// credentialHeaders are headers through which a tenant credential could
// arrive (Headscale API keys travel as Authorization: Bearer).
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Headscale-Api-Key", "Cookie"}

type actorKey struct{}

func actorFrom(ctx context.Context) (string, bool) {
	a, ok := ctx.Value(actorKey{}).(string)

	return a, ok && a != ""
}

// Wrap returns next behind the operator check.
func (a *Authorizer) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range credentialHeaders {
			if r.Header.Get(h) != "" {
				operatorRefused.WithLabelValues(refusedCredentials).Inc()
				http.Error(w, "forbidden: tenant or bearer credentials are not accepted by the operator API", http.StatusForbidden)

				return
			}
		}

		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
			operatorRefused.WithLabelValues(refusedNoCert).Inc()
			http.Error(w, "unauthorized: operator client certificate required", http.StatusUnauthorized)

			return
		}

		actor, ok := a.identity(r.TLS.VerifiedChains[0][0])
		if !ok {
			operatorRefused.WithLabelValues(refusedIdentity).Inc()
			http.Error(w, "forbidden: certificate identity is not an allowed operator", http.StatusForbidden)

			return
		}

		if a.revoked != nil {
			var err error

			// Every chain the handshake verified: a revoked intermediate in
			// any of them refuses the request.
			for _, chain := range r.TLS.VerifiedChains {
				if err = a.revoked.Check(chain); err != nil {
					break
				}
			}

			switch {
			case errors.Is(err, ErrRevoked):
				operatorRefused.WithLabelValues(refusedRevoked).Inc()
				http.Error(w, "forbidden: operator certificate is revoked", http.StatusForbidden)

				return
			case err != nil:
				operatorRefused.WithLabelValues(refusedRevocation).Inc()
				http.Error(w, "operator certificate revocation list unavailable", http.StatusServiceUnavailable)

				return
			}
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
	})
}

func (a *Authorizer) identity(leaf *x509.Certificate) (string, bool) {
	for _, u := range leaf.URIs {
		if a.allowed[u.String()] {
			return u.String(), true
		}
	}

	for _, d := range leaf.DNSNames {
		if a.allowed[d] {
			return d, true
		}
	}

	return "", false
}

// TLSConfig is the server TLS configuration: TLS 1.3, the server
// certificate, and client certificates verified against clientCAs (the
// pinned operator CA, never the system roots). A client may connect without
// a certificate so that Authorizer answers 401/403 instead of a bare
// handshake failure; a certificate that does not verify fails the handshake.
func TLSConfig(cert tls.Certificate, clientCAs *x509.CertPool) (*tls.Config, error) {
	if clientCAs == nil {
		return nil, errNoClientCA
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.VerifyClientCertIfGiven,
	}, nil
}
