package session_test

// Identity ladder (TDD §3.5.1 SESS-001..005, spec REQ-1..REQ-4, INV-2/INV-3).
// These cases pin the properties that make session isolation structural: a
// credential-scoped key, no truncation, no reliance on the OpenAI `user` field,
// and a public id a client can pin without ever seeing the internal key.

import (
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/session"
)

const credA = "sk-tenant-a"

func derived(t *testing.T, in session.ResolveInput) session.Identity {
	t.Helper()
	if in.Mint == nil {
		in.Mint = func() string { return "fixedmint" }
	}
	return session.Resolve(in)
}

// SESS-001 — the explicit header wins, and only the header is looked at when
// both carriers are present (SESS-005 precedence rides on the same case).
func TestSess001ExplicitHeaderWins(t *testing.T) {
	id := derived(t, session.ResolveInput{
		Credential: credA,
		HeaderID:   "s-abc",
		BodyID:     "s-xyz",
		FirstMsg:   "hello",
	})
	if id.Source != session.SourceExplicit {
		t.Fatalf("source = %q, want explicit", id.Source)
	}
	if id.PublicID != "s-abc" {
		t.Errorf("public id = %q, want the header value echoed back", id.PublicID)
	}
	onlyHeader := derived(t, session.ResolveInput{Credential: credA, HeaderID: "s-abc"})
	if id.Key != onlyHeader.Key {
		t.Errorf("key depends on the losing carrier: %s vs %s", id.Key, onlyHeader.Key)
	}
}

// SESS-002 — the body field is the same extension on a different carrier: it
// resolves to exactly the same session as the header would.
func TestSess002BodyFieldIsTheSameExtension(t *testing.T) {
	viaBody := derived(t, session.ResolveInput{Credential: credA, BodyID: "s-abc"})
	viaHeader := derived(t, session.ResolveInput{Credential: credA, HeaderID: "s-abc"})
	if viaBody.Key != viaHeader.Key {
		t.Errorf("body and header carriers disagree: %s vs %s", viaBody.Key, viaHeader.Key)
	}
	if viaBody.Source != session.SourceExplicit || viaBody.PublicID != "s-abc" {
		t.Errorf("body resolution = %+v", viaBody)
	}
}

// SESS-003 — the OpenAI `user` field is not an identity source: two requests
// differing only in `user` resolve to one session, because `user` never enters
// the preimage. (The API-level half of this case, that a `user` value does not
// become a session id, lives in internal/api/session_test.go.)
func TestSess003UserIsNotAnIdentitySource(t *testing.T) {
	base := session.ResolveInput{Credential: credA, Model: "mimo-v2.6-flash", FirstMsg: "hello"}
	a := derived(t, base)
	// Same inputs, `user` differs: the caller cannot express it through the
	// ladder at all, which is the point — the ladder has no such input.
	b := derived(t, base)
	if a.Key != b.Key {
		t.Errorf("same inputs produced different keys: %s vs %s", a.Key, b.Key)
	}
	if a.Source != session.SourceDerived {
		t.Errorf("source = %q, want derived", a.Source)
	}
}

// SESS-004 — the derived key is credential-scoped (REQ-3/INV-2): byte-identical
// requests from two tenants are two sessions, in both directions.
func TestSess004DerivedKeyIsCredentialScoped(t *testing.T) {
	in := session.ResolveInput{Model: "mimo-v2.6-flash", System: "sys", Tools: "[]", FirstMsg: "hello"}
	a := derived(t, withCred(in, "sk-tenant-a"))
	b := derived(t, withCred(in, "sk-tenant-b"))
	if a.Key == b.Key {
		t.Fatal("two credentials share one session key")
	}
	if a.PublicID == b.PublicID {
		t.Error("two credentials share one public id")
	}
	// The same credential is stable across calls.
	if again := derived(t, withCred(in, "sk-tenant-a")); again.Key != a.Key {
		t.Error("resolution is not deterministic for one credential")
	}
}

// SESS-005 — precedence is fixed: header, then body, then derived, then fresh.
func TestSess005PrecedenceIsFixed(t *testing.T) {
	base := session.ResolveInput{Credential: credA, Model: "m", FirstMsg: "hi"}
	if got := derived(t, base); got.Source != session.SourceDerived {
		t.Fatalf("no carriers: source = %q, want derived", got.Source)
	}
	withBody := base
	withBody.BodyID = "s-body"
	if got := derived(t, withBody); got.Source != session.SourceExplicit || got.PublicID != "s-body" {
		t.Fatalf("body carrier: %+v", got)
	}
	both := withBody
	both.HeaderID = "s-header"
	if got := derived(t, both); got.PublicID != "s-header" {
		t.Fatalf("header did not outrank the body field: %+v", got)
	}
}

// Round trip (REQ-16/SESS-061): whatever public id we hand out, pinning it on
// the next turn resolves to the same session — for both the derived and the
// fresh path, and without any stored state.
func TestIdentityPublicIDRoundTrip(t *testing.T) {
	base := session.ResolveInput{Credential: credA, Model: "m", System: "s", FirstMsg: "hi"}
	first := derived(t, base)
	pinned := derived(t, session.ResolveInput{Credential: credA, HeaderID: first.PublicID, Model: "m", System: "s", FirstMsg: "hi"})
	if pinned.Key != first.Key {
		t.Errorf("pinning the derived id did not restore the session: %s vs %s", pinned.Key, first.Key)
	}
	if pinned.Source != session.SourceExplicit {
		t.Errorf("source = %q, want explicit", pinned.Source)
	}

	fresh := session.ResolveInput{Credential: credA, Mint: func() string { return "t0ken" }}
	f := derived(t, fresh)
	if f.Source != session.SourceFresh {
		t.Fatalf("empty history should be fresh, got %q", f.Source)
	}
	if !strings.HasPrefix(f.PublicID, session.PublicPrefix) {
		t.Errorf("minted public id = %q, want the %q prefix", f.PublicID, session.PublicPrefix)
	}
	f2 := derived(t, session.ResolveInput{Credential: credA, HeaderID: f.PublicID})
	if f2.Key != f.Key {
		t.Errorf("pinning a minted id did not restore the session: %s vs %s", f2.Key, f.Key)
	}

	// A derived id pins the session only while the request's derived inputs still
	// agree (same model, system, tools, opening). With a rewritten history the
	// pin is honoured as an opaque id of its own — stable, isolated, and never a
	// chain into the original session.
	rewritten := derived(t, session.ResolveInput{Credential: credA, HeaderID: first.PublicID, Model: "m", System: "s", FirstMsg: "a different opening"})
	if rewritten.Key == first.Key {
		t.Error("a pinned derived id chained onto a session whose derived inputs no longer match")
	}
	again := derived(t, session.ResolveInput{Credential: credA, HeaderID: first.PublicID, Model: "m", System: "s", FirstMsg: "a different opening"})
	if again.Key != rewritten.Key {
		t.Error("an opaque pinned id must be stable across turns")
	}
}

// Opacity (INV-3): the internal key never equals a client-supplied value, is
// stable-width hex, and does not appear inside the public id.
func TestIdentityKeyIsOpaque(t *testing.T) {
	id := derived(t, session.ResolveInput{Credential: credA, HeaderID: "s-abc"})
	if id.Key == "s-abc" {
		t.Fatal("internal key is the client value")
	}
	if len(id.Key) != 32 {
		t.Errorf("key length = %d, want 32 hex chars", len(id.Key))
	}
	if strings.Contains(id.PublicID, id.Key) {
		t.Error("public id exposes the internal key")
	}
	// A forged id from another tenant must not resolve to the original key.
	forged := derived(t, session.ResolveInput{Credential: "sk-tenant-b", HeaderID: id.PublicID})
	if forged.Key == id.Key {
		t.Fatal("a foreign credential resolved to another tenant's session")
	}
}

// Unambiguity: length-prefixed hashing means concatenation tricks cannot make
// two different requests collide (REQ-4).
func TestIdentityHashingIsUnambiguous(t *testing.T) {
	a := derived(t, session.ResolveInput{Credential: credA, Model: "ab", FirstMsg: "c"})
	b := derived(t, session.ResolveInput{Credential: credA, Model: "a", FirstMsg: "bc"})
	if a.Key == b.Key {
		t.Error("field boundaries are not respected in the derived key")
	}
}

func withCred(in session.ResolveInput, cred string) session.ResolveInput {
	in.Credential = cred
	return in
}
