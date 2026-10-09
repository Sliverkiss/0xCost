// Package adapter message projections: the canonical hashes and counters that
// session identity and turn planning are built on. Content-derived fingerprint
// lookups were retired in favour of a keyed store with whole-frontier
// verification (ADR 0009).
package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Msg is the minimal (role, content) projection of a chat message used for
// fingerprinting and turn planning.
type Msg struct {
	Role    string
	Content string
	// Media are the client's image URLs for this message, in order (OpenAI
	// image_url parts). They are inputs, not upstream handles: the pipeline
	// uploads them with the turn they belong to (IMG-*, docs/attachments-reverse.md).
	Media []string
}

// SystemPreamble returns the canonical system preamble: every system message's
// content, length-prefixed and in order. It is a derived-key input (REQ-4): two
// clients with different instructions are different sessions, and the
// canonical form keeps the key stable when a client merely reorders nothing.
func SystemPreamble(msgs []Msg) string {
	var parts []string
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		parts = append(parts, m.Content)
	}
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
		b.WriteByte('|')
	}
	return b.String()
}

// FrontierHash is the canonical hash of a message frontier: every non-system
// message up to and including the last consumed turn, each length-prefixed.
// Unlike Fingerprint it truncates nothing and covers the whole frontier, so
// verification cannot be fooled by an edited early turn or a shared 200-rune
// opening (SDD §4.3.1, INV-1). An empty history hashes to the empty string.
func FrontierHash(msgs []Msg) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		parts = append(parts, m.Role, m.Content)
	}
	if len(parts) == 0 {
		return ""
	}
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
		h.Write([]byte{'|'})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// CountNonSystem returns the number of non-system messages — the replay-guard
// counter stored alongside fingerprints (SDD §4.2, TDD STICKY-003).
func CountNonSystem(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		if m.Role != "system" {
			n++
		}
	}
	return n
}

// RandomHex32 returns a fresh 32-char lowercase hex string (uuid4 with dashes
// stripped, SDD §2.4). Used for msgId / conversationId minting.
func RandomHex32() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand effectively never fails on supported platforms; keep a
		// hash-derived fallback so IDs remain unique per call.
		sum := sha256.Sum256([]byte(fmt.Sprintf("mimo2api-fallback:%d", time.Now().UnixNano())))
		copy(b, sum[:16])
	}
	return hex.EncodeToString(b)
}
