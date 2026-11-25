package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

const (
	idA = "0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5f"
	idB = "7e9d1a3b-5c2f-4e8a-b6d4-1f0c3a5b7d9e"
)

func TestParseIDRejectsInvalid(t *testing.T) {
	for _, in := range []string{
		"",
		"00000000-0000-0000-0000-000000000000",  // nil UUID is not a tailnet
		"0B6F4C2E-8A1D-4F3B-9C7E-2D5A1B3C4E5F",  // non-canonical case would alias an ID
		"0b6f4c2e8a1d4f3b9c7e2d5a1b3c4e5f",      // missing hyphens
		"0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5",   // too short
		"0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5fa", // too long
		"0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5g",  // not hex
		" 0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5f",
		"-",         // the single-tailnet API placeholder is not an ID
		"tailnet-a", // hostnames/slugs are not IDs
		"../" + idA, // path tricks
	} {
		id, err := ParseID(in)
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("ParseID(%q) err = %v, want ErrInvalidID", in, err)
		}

		if !id.IsZero() {
			t.Errorf("ParseID(%q) returned non-zero ID %q on error", in, id)
		}
	}
}

func TestParseIDAcceptsCanonical(t *testing.T) {
	id, err := ParseID(idA)
	if err != nil {
		t.Fatalf("ParseID(%q): %v", idA, err)
	}

	if id.IsZero() || id.String() != idA {
		t.Fatalf("ParseID(%q) = %q", idA, id)
	}

	other := MustParseID(idB)
	if id == other {
		t.Fatal("distinct IDs compare equal")
	}

	if id != MustParseID(idA) {
		t.Fatal("equal IDs compare unequal")
	}
}

func TestZeroIDIsInvalid(t *testing.T) {
	var id ID
	if !id.IsZero() || id.String() != "" {
		t.Fatalf("zero ID = %q, IsZero=%v", id, id.IsZero())
	}

	_, err := id.MarshalText()
	if !errors.Is(err, ErrInvalidID) {
		t.Fatalf("MarshalText of zero ID err = %v, want ErrInvalidID", err)
	}
}

func TestIDTextRoundTripValidates(t *testing.T) {
	type wrap struct {
		ID ID `json:"id"`
	}

	b, err := json.Marshal(wrap{MustParseID(idA)})
	if err != nil {
		t.Fatal(err)
	}

	var got wrap

	err = json.Unmarshal(b, &got)
	if err != nil || got.ID.String() != idA {
		t.Fatalf("round trip = %q, %v", got.ID, err)
	}

	err = json.Unmarshal([]byte(`{"id":"not-a-tailnet"}`), &got)
	if !errors.Is(err, ErrInvalidID) {
		t.Fatalf("Unmarshal invalid err = %v, want ErrInvalidID", err)
	}
}

func TestFromContextFailsClosed(t *testing.T) {
	s, err := FromContext(context.Background())
	if !errors.Is(err, ErrNoScope) {
		t.Fatalf("FromContext(empty) err = %v, want ErrNoScope", err)
	}

	if s.Valid() || !s.ID().IsZero() {
		t.Fatalf("FromContext(empty) returned a usable scope: %+v", s)
	}
	//nolint:staticcheck // deliberately testing a nil context
	_, err = FromContext(nil)
	if !errors.Is(err, ErrNoScope) {
		t.Fatalf("FromContext(nil) err = %v, want ErrNoScope", err)
	}
}

func TestNewScopeValidates(t *testing.T) {
	cases := []struct {
		name      string
		id        ID
		src       Source
		principal string
	}{
		{"zero id", ID{}, SourceClientSession, "node:1"},
		{"unknown source", MustParseID(idA), SourceUnknown, "node:1"},
		{"out of range source", MustParseID(idA), Source(99), "node:1"},
		{"empty principal", MustParseID(idA), SourceInternalService, ""},
	}
	for _, c := range cases {
		_, err := NewScope(c.id, c.src, c.principal, 1)
		if !errors.Is(err, ErrInvalidScope) {
			t.Errorf("%s: err = %v, want ErrInvalidScope", c.name, err)
		}
	}

	var zero Scope
	if zero.Valid() {
		t.Fatal("zero Scope is Valid")
	}
}

func TestWithScopeRejectsInvalid(t *testing.T) {
	_, err := WithScope(context.Background(), Scope{})
	if !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("WithScope(zero) err = %v, want ErrInvalidScope", err)
	}
}

func TestWithScopeCannotSwitchTailnet(t *testing.T) {
	a := mustScope(t, idA)
	b := mustScope(t, idB)

	ctx, err := WithScope(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	// I01: one session/request carries exactly one tailnet.
	_, err = WithScope(ctx, b)
	if !errors.Is(err, ErrScopeConflict) {
		t.Fatalf("re-binding A->B err = %v, want ErrScopeConflict", err)
	}
	// Re-asserting the same scope is harmless.
	_, err = WithScope(ctx, a)
	if err != nil {
		t.Fatalf("re-binding A->A: %v", err)
	}

	got, err := FromContext(ctx)
	if err != nil || got.ID() != a.ID() {
		t.Fatalf("FromContext = %v, %v; want %v", got.ID(), err, a.ID())
	}
}

func TestRequireChecksTailnet(t *testing.T) {
	ctx, err := WithScope(context.Background(), mustScope(t, idA))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Require(ctx, MustParseID(idA))
	if err != nil {
		t.Fatalf("Require(A) in A: %v", err)
	}

	_, err = Require(ctx, MustParseID(idB))
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("Require(B) in A err = %v, want ErrScopeMismatch", err)
	}

	_, err = Require(context.Background(), MustParseID(idA))
	if !errors.Is(err, ErrNoScope) {
		t.Fatalf("Require without scope err = %v, want ErrNoScope", err)
	}

	_, err = Require(ctx, ID{})
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("Require(zero) err = %v, want ErrScopeMismatch", err)
	}
}

func mustScope(t *testing.T, id string) Scope {
	t.Helper()

	s, err := NewScope(MustParseID(id), SourceClientSession, "node:test", 1)
	if err != nil {
		t.Fatal(err)
	}

	return s
}
