// Package adapter message projections: the message roles/contents and media
// parts the turn planner walks. The session-identity fingerprint machinery
// (SystemPreamble / FrontierHash / CountNonSystem) was removed with the session
// store — requests are one-shot full replays (ADR 0017).
package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Msg is the minimal (role, content) projection of a chat message used for
// fingerprinting and turn planning.
type Msg struct {
	Role    string
	Content string
	// Media are the client's image parts for this message, in order (OpenAI
	// image_url parts), each with the detail level the client asked for. They are
	// inputs, not upstream handles: the pipeline converts them — fetching bytes,
	// applying the level, uploading — with the turn they belong to (IMG-*,
	// docs/attachments-reverse.md). Detail deliberately does NOT enter any hash:
	// the same image at another level is the same turn, not another session.
	Media []MediaRef
}

// MediaRef is one image part: the client's URL and OpenAI's image_url.detail
// level ("" when the client did not send one, which is OpenAI's "auto").
type MediaRef struct {
	URL    string
	Detail string
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
