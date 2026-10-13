package api

// Risk-control and evidence helpers (SDD §4.5.2, spec §5).
//
// The cooldown ladder these helpers used to feed is gone (owner ruling,
// 2026-10: no cooldown/backoff, failures pass through). What remains is the
// 451/461 detection that disables an account fail-closed, and the body digest
// recorded with the evidence — no credential material ever enters either.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/yg/mimo2api/internal/mimo"
)

// isRiskControl reports a 451/461 — the class whose form we have not verified
// (SDD §4.5.2). Kept separate from isAccountDead so the risk-control path can
// record evidence and stay recoverable.
func isRiskControl(err error) bool {
	var ue *mimo.UpstreamError
	return errors.As(err, &ue) && (ue.StatusCode == 451 || ue.StatusCode == 461)
}

// upstreamStatus returns the upstream's HTTP status, or 0 when there was none.
func upstreamStatus(err error) int {
	var ue *mimo.UpstreamError
	if errors.As(err, &ue) {
		return ue.StatusCode
	}
	return 0
}

// bodyDigest is the SHA-256 of the upstream's response body, hex-encoded. The
// body itself is never stored: it can carry account-identifying material, and a
// digest is enough to tell two events apart when reading the evidence later
// (INV-4).
func bodyDigest(err error) string {
	var ue *mimo.UpstreamError
	if !errors.As(err, &ue) || ue.Body == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ue.Body))
	return hex.EncodeToString(sum[:8])
}
