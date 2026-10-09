// Package session identity: resolving a streaming request to the session it
// continues (SDD §4.3.1, spec docs/specs/session-identity.md).
//
// The ladder is deliberately deterministic and stateless: given the same
// request and credential it always yields the same session key, so a restart
// costs a replay rather than a mis-attribution (ADR 0009 decision 6). Nothing
// here stores or logs message content (INV-3).
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/yg/mimo2api/internal/adapter"
)

// Source records how a session id was resolved (REQ-1). It appears verbatim in
// the per-decision debug line (REQ-15).
type Source string

const (
	// SourceExplicit — the caller named the session (header or body field).
	SourceExplicit Source = "explicit"
	// SourceDerived — the ladder hashed the request's own shape.
	SourceDerived Source = "derived"
	// SourceFresh — nothing identified the session, so a new one was minted.
	SourceFresh Source = "fresh"
)

// PublicPrefix marks ids this gateway minted, so a pinned id is recognisable
// without consulting any state. The scope is part of the id: a pinned derived
// id and a pinned fresh token must never be confused for one another, or a
// client's pin would silently resolve to a different session.
const (
	PublicPrefix  = "ms-"
	derivedPrefix = PublicPrefix + "d-"
	freshPrefix   = PublicPrefix + "f-"
)

// Identity is the resolution result.
type Identity struct {
	// Key is the internal, credential-scoped session key. It is never logged
	// and never returned to a client (INV-3).
	Key string
	// PublicID is what the client sees in X-Mimo-Session-Id and may pin on a
	// later turn (REQ-16).
	PublicID string
	// Source is how this identity was reached.
	Source Source
}

// ResolveInput carries everything the ladder may look at. Credential is the
// calling credential's identity (API key); empty means the deployment runs
// without auth and therefore has a single profile (REQ-3/INV-2).
type ResolveInput struct {
	Credential string
	HeaderID   string // X-Mimo-Session-Id
	BodyID     string // session_id extension
	Model      string
	System     string // system preamble, canonicalised
	Tools      string // tool / attachment descriptor, canonicalised
	FirstMsg   string // first non-system message content
	Mint       func() string
}

// Resolve applies the identity ladder: explicit header → body field → derived
// key → fresh id (REQ-1/REQ-2/REQ-4).
//
// A pinned id is recognised two ways: it may be one this gateway minted for the
// same derived key (recognised by recomputation, so it survives a restart), or
// it may simply be the caller's own opaque value, which is hashed into a
// credential-scoped key and echoed back unchanged.
func Resolve(in ResolveInput) Identity {
	derivedKey, derivedPublic := derivedIdentity(in)

	for _, raw := range []string{in.HeaderID, in.BodyID} {
		if raw == "" {
			continue
		}
		if raw == derivedPublic {
			// The caller pinned the id we handed out for this very history.
			return Identity{Key: derivedKey, PublicID: derivedPublic, Source: SourceExplicit}
		}
		if token, ok := strings.CutPrefix(raw, freshPrefix); ok {
			// An id this gateway minted for a fresh session. The prefix carries
			// its own scope, so recognition needs no stored state and survives a
			// restart; a token from another credential still hashes elsewhere.
			return Identity{Key: hashKey(in.Credential, "fresh", token), PublicID: raw, Source: SourceExplicit}
		}
		return Identity{Key: hashKey(in.Credential, "explicit", raw), PublicID: raw, Source: SourceExplicit}
	}

	if in.FirstMsg != "" {
		return Identity{Key: derivedKey, PublicID: derivedPublic, Source: SourceDerived}
	}

	mint := in.Mint
	if mint == nil {
		mint = adapter.RandomHex32
	}
	token := mint()
	return Identity{Key: hashKey(in.Credential, "fresh", token), PublicID: freshPrefix + token, Source: SourceFresh}
}

// derivedIdentity computes the credential-scoped derived key and the public id
// that stands for it.
//
// The preimage is bounded and canonical (REQ-4): the model, the system
// preamble, the tool/attachment descriptor and the first non-system message.
// Nothing is truncated — a truncated window is exactly what made the previous
// fingerprint lossy — and the whole first message is hashed, so two callers
// sharing a boilerplate opening still differ as soon as their real question
// does.
func derivedIdentity(in ResolveInput) (key string, public string) {
	key = hashKey(in.Credential, "derived", in.Model, in.System, in.Tools, in.FirstMsg)
	// The public id is derived from the key rather than from the credential, so
	// it leaks nothing about the credential but is still recomputable by a
	// later request that presents the same inputs.
	return derivedPrefix + hashKey("public", key), key
}

// hashKey builds a length-prefixed SHA-256 digest (first 16 bytes, lowercase
// hex) over the given parts. Length prefixes make the digest unambiguous:
// ("ab","c") and ("a","bc") cannot collide.
func hashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
		h.Write([]byte{'|'})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// String renders an identity for a debug line without exposing the key.
func (i Identity) String() string {
	return fmt.Sprintf("%s(source=%s)", i.PublicID, i.Source)
}
