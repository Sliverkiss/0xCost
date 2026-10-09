package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Msg is the minimal (role, content) projection of a chat message used for
// fingerprinting and turn planning.
type Msg struct {
	Role    string
	Content string
}

const (
	fingerprintWindow = 5   // last N non-system messages counted (SDD §4.2)
	fingerprintCut    = 200 // content truncated to this many chars (SDD §4.2)
)

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

// Fingerprint computes the conversation fingerprint: SHA-256 over the last
// ≤5 non-system messages' (role, content) pairs, content truncated to 200
// chars, first 16 bytes hex. System messages are ignored. Deterministic and
// independent of any system preamble (TDD STICKY-001).
func Fingerprint(msgs []Msg) string {
	var recent []Msg
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		c := m.Content
		if len(c) > fingerprintCut {
			c = truncateUTF8(c, fingerprintCut)
		}
		recent = append(recent, Msg{Role: m.Role, Content: c})
	}
	if len(recent) == 0 {
		return ""
	}
	if len(recent) > fingerprintWindow {
		recent = recent[len(recent)-fingerprintWindow:]
	}
	b, err := json.Marshal(recent)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
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

// Continuation reports whether the current message list extends a stored
// frontier: some prefix of the current list (dropping 1..5 tail messages)
// fingerprints to lastFp (SDD §4.2, convstore.go:124-149 analogue).
func Continuation(msgs []Msg, lastFp string) bool {
	if lastFp == "" {
		return false
	}
	var non []Msg
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		non = append(non, m)
	}
	if len(non) == 0 {
		return false
	}
	for drop := 1; drop <= fingerprintWindow && drop <= len(non); drop++ {
		if Fingerprint(non[:len(non)-drop]) == lastFp {
			return true
		}
	}
	return false
}

// truncateUTF8 cuts s to at most n runes without splitting a multi-byte
// character.
func truncateUTF8(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
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
