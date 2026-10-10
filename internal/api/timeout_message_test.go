package api

import (
	"strings"
	"testing"
	"time"
)

// TIMEOUT-004: the 504 names the bound that actually fired, and only that one.
//
// Two knobs can end a stalled turn: the wall-clock request budget
// (MIMO2API_REQUEST_TIMEOUT_MS) and the turn's silence window
// (MIMO2API_LEASE_TTL_S). The shipped default disables the wall clock
// (REQUEST_TIMEOUT_MS=0, ADR 0036), so a message that names both sends an
// operator to an environment variable that was never in play — and an operator
// who raises the wrong knob is still stalled, now with a longer timeout.
//
// The caller already knows which one bit (budgetError picks the shorter), so the
// message must say it: the integration cases (TIMEOUT-001/002) assert each knob is
// present when it fires; this case pins the other direction — that the knob which
// did *not* fire is never named.
func TestTimeoutErrorNamesTheBoundThatFired(t *testing.T) {
	lease := (&requestTimeoutError{Budget: 2 * time.Minute, Lease: true}).Error()
	if !strings.Contains(lease, "MIMO2API_LEASE_TTL_S") {
		t.Errorf("silence-window 504 does not name the knob that fired: %q", lease)
	}
	if strings.Contains(lease, "MIMO2API_REQUEST_TIMEOUT_MS") {
		t.Errorf("silence-window 504 names the disabled wall clock: %q", lease)
	}

	wall := (&requestTimeoutError{Budget: 90 * time.Second}).Error()
	if !strings.Contains(wall, "MIMO2API_REQUEST_TIMEOUT_MS") {
		t.Errorf("wall-clock 504 does not name the knob that fired: %q", wall)
	}
	if strings.Contains(wall, "MIMO2API_LEASE_TTL_S") {
		t.Errorf("wall-clock 504 names the silence window it did not use: %q", wall)
	}

	// The operator has to be told what the gateway was waiting for; "waiting for
	// the upstream" says nothing about where the time went.
	if !strings.Contains(wall, "waiting for its first byte") {
		t.Errorf("504 does not say where it waited: %q", wall)
	}
	if !strings.Contains(wall, "1m30s") {
		t.Errorf("504 does not report the budget that expired: %q", wall)
	}
}

// A caller that names its own place keeps it: the default is a fallback, not an
// override.
func TestTimeoutErrorMessageKeepsTheCallersWhere(t *testing.T) {
	msg := (&requestTimeoutError{Budget: time.Minute, Where: "streaming the answer"}).Error()
	if !strings.Contains(msg, "streaming the answer") {
		t.Errorf("504 replaced the caller's position with the default: %q", msg)
	}
}
