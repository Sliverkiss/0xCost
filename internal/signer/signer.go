// Package signer implements the chatglm.cn request signature (X-Timestamp /
// X-Nonce / X-Sign) and the small amount of JWT reading the gateway needs.
//
// Ground truth: docs/reverse/SIGNING.md, module 86129 of the SPA bundle. The
// signature covers only (timestamp, nonce, secret) — not the path, body or
// method — so one call can sign any request within the server's clock window.
package signer

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Secret is the hardcoded signing key shipped in the SPA bundle. It is also
// present verbatim in the independent HelloGML and glm2api projects.
const Secret = "8a1317a7468aa3ad86e997d08f3f31cb"

// Signature is a complete set of signing headers.
type Signature struct {
	Timestamp string // checksum-adjusted ms epoch, 13 digits
	Nonce     string // 32 hex chars (uuid4 without dashes)
	Sign      string // lowercase hex MD5
}

// Now returns a signature for the given wall-clock instant. The timestamp keeps
// the current millisecond value except for its tens-of-ms digit, which is
// replaced by (digit-sum - penultimate-digit) mod 10 — the SPA's checksum.
func Now(at time.Time) Signature {
	ms := at.UnixMilli()
	return fromMillis(ms, RandomHex32())
}

// NowAt is Now with an explicit time; equivalent to Now but named for callers
// that pass a clock through for testability.
func NowAt(at time.Time) Signature { return Now(at) }

// Timestamp adjusts a raw millisecond epoch the way the SPA does.
func Timestamp(ms int64) string {
	s := fmt.Sprintf("%d", ms)
	if len(s) < 3 {
		return s
	}
	digits := make([]int, len(s))
	sum := 0
	for i := 0; i < len(s); i++ {
		digits[i] = int(s[i] - '0')
		sum += digits[i]
	}
	// The SPA computes (sum(digits) - digits[len-2]) % 10. For a 13-digit ms
	// epoch that is the tens-of-ms digit position.
	checksum := ((sum-digits[len(s)-2])%10 + 10) % 10
	return s[:len(s)-2] + fmt.Sprintf("%d", checksum) + s[len(s)-1:]
}

func fromMillis(ms int64, nonce string) Signature {
	ts := Timestamp(ms)
	return Signature{
		Timestamp: ts,
		Nonce:     nonce,
		Sign:      Sign(ts, nonce),
	}
}

// Sign computes the MD5 signature for an explicit timestamp/nonce pair. It is
// exported for fixture tests that assert against vectors captured from the SPA.
func Sign(timestamp, nonce string) string {
	sum := md5.Sum([]byte(timestamp + "-" + nonce + "-" + Secret))
	return hex.EncodeToString(sum[:])
}

// RandomHex32 returns 16 cryptographically random bytes as 32 lowercase hex
// characters — the shape of X-Nonce and X-Request-Id.
func RandomHex32() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal for signing; fall back to a time-derived
		// value so the process degrades rather than panicking.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Claims holds the JWT claims the gateway reads. Guest access and refresh
// tokens are HS256 JWTs whose payload is plain base64url JSON.
type Claims struct {
	Subject   string `json:"sub"`
	UID       string `json:"uid"`
	DeviceID  string `json:"device_id"`
	IsGuest   bool   `json:"is_guest"`
	Mode      int    `json:"mode"`
	Type      string `json:"type"` // "access" | "refresh"
	ExpiresAt int64  `json:"exp"`
	NotBefore int64  `json:"nbf"`
	IssuedAt  int64  `json:"iat"`
}

// ParseClaims decodes the payload of a JWT without verifying its signature. The
// gateway only needs uid/device_id/expiry from tokens the server issued.
func ParseClaims(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("jwt: expected 3 parts, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders leave padding in place; tolerate it.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("jwt: decode payload: %w", err)
		}
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("jwt: parse payload: %w", err)
	}
	return &c, nil
}

// Expiry returns the token expiry as a time.Time.
func (c *Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0) }
