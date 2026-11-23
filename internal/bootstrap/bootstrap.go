// Package bootstrap answers "which cluster serves my tenant" for a client
// that is about to enroll (P11, ADR 0005 Bổ sung 2 §1). The client proves
// its tenant with a pre-auth key it holds, without sending the key: the
// key's public identifier and the SHA-256 of its secret (owner decision
// 2026-11-23). Bootstrap returns the URL of the cluster that owns the
// tenant now; the cluster re-checks ownership itself and never trusts
// bootstrap (F07).
//
// Every refusal a caller without a valid credential can provoke has one
// response: an unknown key, a wrong proof, a revoked, expired or used key,
// a tenant that is not ACTIVE or is deleted, and a tenant name that does
// not match the key all look the same, so the service reveals neither
// which tenants exist nor where they live (I15).
package bootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"regexp"
	"time"
)

// CredentialKindAuthKeyProof is the only credential kind of the MVP.
const CredentialKindAuthKeyProof = "authkey_proof"

// hashPrefixSHA256 is the storage prefix of a SHA-256 credential hash
// (hscontrol/db/secret.go).
const hashPrefixSHA256 = "$sha256$"

var (
	keyIDPattern  = regexp.MustCompile(`^[0-9a-f]{12}$`)
	proofPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tenantPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

	// dummyHash is compared against when no credential row exists, so an
	// unknown key costs the same work as a wrong proof.
	dummyHash = []byte(hashPrefixSHA256 + hex.EncodeToString(make([]byte, sha256.Size)))
)

// Credential identifies the caller's tenant.
type Credential struct {
	Kind  string `json:"kind"`
	KeyID string `json:"key_id"`
	Proof string `json:"proof"`
}

// Request is the body of POST /v1/bootstrap.
type Request struct {
	Credential Credential `json:"credential"`
	// Tenant is optional; when set it must be the slug of the key's tailnet.
	Tenant string `json:"tenant,omitempty"`
}

// Record is what a Store returns for one pre-auth key identifier: the key's
// validity, its tailnet and the tailnet's owner cluster.
type Record struct {
	TailnetID    string
	Hash         []byte
	Revoked      bool
	Expiration   *time.Time
	Used         bool
	Reusable     bool
	TailnetSlug  string
	TailnetState string
	Deleted      bool
	MovingTo     *string
	// ClusterEndpoint and ClusterState are nil when the owner cluster is
	// not registered.
	ClusterEndpoint *string
	ClusterState    *string
}

// Store looks up a pre-auth key by its public identifier.
type Store interface {
	// LookupAuthKey returns the record of keyID, found=false when there is
	// none. It only reads.
	LookupAuthKey(ctx context.Context, keyID string) (rec Record, found bool, err error)
}

// Outcome is the class of a bootstrap answer.
type Outcome int

const (
	// OutcomeRefused: the uniform refusal (404).
	OutcomeRefused Outcome = iota
	// OutcomeOK: the control URL of the owner cluster (200).
	OutcomeOK
	// OutcomeInvalid: a malformed request, decided without the store (400).
	OutcomeInvalid
	// OutcomeMoving: valid key, the tenant is being moved (503).
	OutcomeMoving
	// OutcomeUnavailable: valid key, the owner cluster cannot take clients
	// now, or the store failed (503).
	OutcomeUnavailable
)

// String names the outcome for logs and metrics.
func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeInvalid:
		return "invalid"
	case OutcomeMoving:
		return "moving"
	case OutcomeUnavailable:
		return "unavailable"
	default:
		return "refused"
	}
}

// Result is the answer to one request. TailnetID is set only after the
// credential was verified (for the per-tenant rate limit); it is never sent
// to the client.
type Result struct {
	Outcome    Outcome
	ControlURL string
	TailnetID  string
}

// ErrStore wraps a store failure in logs.
var ErrStore = errors.New("bootstrap store failed")

// Service resolves bootstrap requests against a Store.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService returns a Service on store.
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// Valid reports whether req is well formed; a malformed request never
// reaches the store.
func (r Request) Valid() bool {
	c := r.Credential
	if c.Kind != CredentialKindAuthKeyProof || !keyIDPattern.MatchString(c.KeyID) || !proofPattern.MatchString(c.Proof) {
		return false
	}

	return r.Tenant == "" || tenantPattern.MatchString(r.Tenant)
}

// Resolve answers req. The error is for logs only; the Outcome decides the
// response.
func (s *Service) Resolve(ctx context.Context, req Request) (Result, error) {
	if !req.Valid() {
		return Result{Outcome: OutcomeInvalid}, nil
	}

	rec, found, err := s.store.LookupAuthKey(ctx, req.Credential.KeyID)
	if err != nil {
		return Result{Outcome: OutcomeUnavailable}, errors.Join(ErrStore, err)
	}

	stored := dummyHash
	if found {
		stored = rec.Hash
	}

	if !proofMatches(stored, req.Credential.Proof) || !found {
		return Result{Outcome: OutcomeRefused}, nil
	}

	now := s.now()

	switch {
	case rec.Revoked,
		rec.Expiration != nil && rec.Expiration.Before(now),
		rec.Used && !rec.Reusable,
		req.Tenant != "" && req.Tenant != rec.TailnetSlug,
		rec.Deleted,
		rec.TailnetState != "ACTIVE":
		return Result{Outcome: OutcomeRefused}, nil
	}

	res := Result{TailnetID: rec.TailnetID}

	switch {
	case rec.MovingTo != nil:
		res.Outcome = OutcomeMoving
	case rec.ClusterEndpoint == nil || *rec.ClusterEndpoint == "" || rec.ClusterState == nil ||
		(*rec.ClusterState != "ACTIVE" && *rec.ClusterState != "DRAINING"):
		res.Outcome = OutcomeUnavailable
	default:
		res.Outcome = OutcomeOK
		res.ControlURL = *rec.ClusterEndpoint
	}

	return res, nil
}

// proofMatches compares proof with a stored "$sha256$<hex>" hash in
// constant time. Other hash formats (legacy bcrypt) never match: such a key
// is upgraded the first time a cluster authenticates it.
func proofMatches(stored []byte, proof string) bool {
	want := []byte(hashPrefixSHA256 + proof)
	if len(stored) != len(want) {
		// Still do the comparison work on a same-length value.
		_ = subtle.ConstantTimeCompare(dummyHash, want)

		return false
	}

	return subtle.ConstantTimeCompare(stored, want) == 1
}

// ProofOf returns the proof of a pre-auth key secret (for clients and tests).
func ProofOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(sum[:])
}
