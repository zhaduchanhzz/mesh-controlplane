// Package tailnet holds the types that mark the tenant security boundary of
// mesh-controlplane: which tailnet a request, Noise session or map stream
// belongs to.
//
// Rules this package enforces (AGENTS.md rule 6, 03-isolation-threat-model.md
// I01/I06):
//
//   - An ID is a canonical lowercase UUID. The zero ID is invalid, and the
//     single-tailnet API placeholder "-" or a hostname is never an ID.
//   - A Scope can only be attached to a context once per tailnet: re-binding a
//     context from tailnet A to tailnet B fails (one session, one tailnet).
//   - Reading a scope from a context without one fails closed: there is no
//     default tailnet.
//
// The package deliberately imports nothing that carries transport data (no
// net/http): code must authenticate a caller first (Noise session for a
// verified host binding, or mTLS plus signed service claims) and only then
// call NewScope. It is unrelated to hscontrol/scope, which models OAuth
// scopes of the v2 API.
package tailnet

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrInvalidID reports a value that is not a canonical tailnet ID.
	ErrInvalidID = errors.New("invalid tailnet id")
	// ErrInvalidScope reports a scope that is missing a tailnet, source or principal.
	ErrInvalidScope = errors.New("invalid tailnet scope")
	// ErrNoScope reports a context that carries no verified tailnet scope.
	ErrNoScope = errors.New("no verified tailnet scope")
	// ErrScopeConflict reports an attempt to bind a context to a second tailnet.
	ErrScopeConflict = errors.New("context already bound to a different tailnet")
	// ErrScopeMismatch reports a verified scope for another tailnet than required.
	ErrScopeMismatch = errors.New("verified tailnet scope does not match")
)

// ID identifies a tailnet. It wraps a canonical lowercase UUID so that an
// arbitrary string cannot be converted into an ID without ParseID.
type ID struct {
	s string
}

// ParseID validates s as a canonical lowercase, non-nil UUID
// (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx). Non-canonical spellings are rejected
// rather than normalised, so one tailnet never has two IDs.
func ParseID(s string) (ID, error) {
	if !isCanonicalUUID(s) || s == "00000000-0000-0000-0000-000000000000" {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}

	return ID{s: s}, nil
}

// MustParseID is ParseID for constants and tests; it panics on invalid input.
func MustParseID(s string) ID {
	id, err := ParseID(s)
	if err != nil {
		panic(err)
	}

	return id
}

// String returns the canonical form, or "" for the zero ID.
func (id ID) String() string { return id.s }

// IsZero reports whether id is the (invalid) zero value.
func (id ID) IsZero() bool { return id.s == "" }

// MarshalText implements encoding.TextMarshaler; the zero ID is not encodable.
func (id ID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: zero value", ErrInvalidID)
	}

	return []byte(id.s), nil
}

// UnmarshalText implements encoding.TextUnmarshaler with full validation.
func (id *ID) UnmarshalText(b []byte) error {
	parsed, err := ParseID(string(b))
	if err != nil {
		return err
	}

	*id = parsed

	return nil
}

func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}

	for i := range len(s) {
		c := s[i]

		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}

	return true
}

// Epoch is the placement ownership epoch of a tailnet on a shard. It only
// grows; a stale epoch must be rejected (I12).
type Epoch uint64

// Source records how the caller of a scope was authenticated.
type Source uint8

const (
	// SourceUnknown is the zero Source and never valid.
	SourceUnknown Source = iota
	// SourceClientSession: a Tailscale client whose Noise handshake completed
	// with the key of the tailnet its verified host binding selected.
	SourceClientSession
	// SourceInternalService: an internal caller authenticated with mTLS and
	// signed claims naming this tailnet and epoch.
	SourceInternalService

	sourceEnd
)

func (s Source) String() string {
	switch s { //nolint:exhaustive // SourceUnknown and out-of-range values print as unknown(n)
	case SourceClientSession:
		return "client-session"
	case SourceInternalService:
		return "internal-service"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}

// Scope is a verified binding of a caller to exactly one tailnet. Its fields
// are unexported so a Scope can only come from NewScope (or a context that
// received one through WithScope).
type Scope struct {
	id        ID
	source    Source
	principal string
	epoch     Epoch
}

// NewScope builds a Scope. Call it only after the caller has been
// authenticated by the mechanism that src names; never from client-supplied
// headers or payload fields.
func NewScope(id ID, src Source, principal string, epoch Epoch) (Scope, error) {
	switch {
	case id.IsZero():
		return Scope{}, fmt.Errorf("%w: missing tailnet id", ErrInvalidScope)
	case src == SourceUnknown || src >= sourceEnd:
		return Scope{}, fmt.Errorf("%w: unknown source %s", ErrInvalidScope, src)
	case principal == "":
		return Scope{}, fmt.Errorf("%w: missing principal", ErrInvalidScope)
	}

	return Scope{id: id, source: src, principal: principal, epoch: epoch}, nil
}

// ID returns the tailnet the scope is bound to.
func (s Scope) ID() ID { return s.id }

// Source returns how the caller was authenticated.
func (s Scope) Source() Source { return s.source }

// Principal identifies the authenticated caller (e.g. node or service).
func (s Scope) Principal() string { return s.principal }

// Epoch returns the placement epoch the scope was verified against.
func (s Scope) Epoch() Epoch { return s.epoch }

// Valid reports whether s was built by NewScope.
func (s Scope) Valid() bool {
	return !s.id.IsZero() && s.source != SourceUnknown && s.source < sourceEnd && s.principal != ""
}

type ctxKey struct{}

// WithScope returns a context bound to s. It fails if s is invalid, or if ctx
// is already bound to a different tailnet: one request, Noise session or map
// stream carries exactly one tailnet (I01).
func WithScope(ctx context.Context, s Scope) (context.Context, error) {
	if !s.Valid() {
		return ctx, fmt.Errorf("%w: refusing to bind", ErrInvalidScope)
	}

	prev, err := FromContext(ctx)
	if err == nil {
		if prev.id != s.id {
			return ctx, fmt.Errorf("%w: bound to %s, refusing %s", ErrScopeConflict, prev.id, s.id)
		}

		return ctx, nil
	}

	return context.WithValue(ctx, ctxKey{}, s), nil
}

// FromContext returns the verified scope of ctx, or ErrNoScope. There is no
// default tailnet.
func FromContext(ctx context.Context) (Scope, error) {
	if ctx == nil {
		return Scope{}, ErrNoScope
	}

	s, ok := ctx.Value(ctxKey{}).(Scope)
	if !ok || !s.Valid() {
		return Scope{}, ErrNoScope
	}

	return s, nil
}

// Require returns the scope of ctx if it is bound to want, and fails closed
// otherwise. Use it where a request names a tailnet (e.g. an API path) to make
// sure the authenticated caller belongs to that tailnet.
func Require(ctx context.Context, want ID) (Scope, error) {
	s, err := FromContext(ctx)
	if err != nil {
		return Scope{}, err
	}

	if want.IsZero() || s.id != want {
		return Scope{}, fmt.Errorf("%w: scope %s, required %s", ErrScopeMismatch, s.id, want)
	}

	return s, nil
}
