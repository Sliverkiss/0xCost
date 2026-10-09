package pool

// ② Risk-control signals (TDD §3.5.2 RISK-*, spec §5, SDD §4.5.2).
//
// 451/461 are handled fail-closed but were opaque: the account was disabled
// (461 permanently) and nothing recorded what had happened, so the open question —
// is the signal account-scoped, egress-scoped or a transient challenge — could
// never be answered from our own data. These cases pin the recording and the
// correlation, and pin that permanence is reserved for the evidence we do have.

import (
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/testutil"
)

// poolWithEgress builds a three-account pool whose accounts carry egress labels,
// on the same fake clock the other pool tests use.
func poolWithEgress(t *testing.T, labels map[string]string) (*Pool, *testutil.FakeClock) {
	t.Helper()
	fc := testutil.NewFakeClock()
	var accts []accounts.Account
	for _, id := range []string{"a1", "a2", "a3"} {
		accts = append(accts, accounts.Account{ID: id, Enabled: true, Egress: labels[id]})
	}
	return New(accts, Options{Now: fc.Now}, nil, nil), fc
}

// RISK-001: a 461 (or 451) is recorded with everything needed to interpret it
// later, and the account is disabled — fail closed, no retry.
func TestRiskControlRecordsEvent(t *testing.T) {
	p, fc := poolWithEgress(t, map[string]string{"a1": "hk1"})
	p.ReportRiskControl("a1", "upstream 461 (account restricted)", RiskEvent{
		Model: "mimo-v2.6-pro", Channel: "chat", Status: 461, BodyDigest: "deadbeef",
	})
	events := p.RiskEvents()
	if len(events) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.AccountID != "a1" || ev.Egress != "hk1" {
		t.Errorf("event account/egress = %s/%s, want a1/hk1", ev.AccountID, ev.Egress)
	}
	if ev.Model != "mimo-v2.6-pro" || ev.Channel != "chat" {
		t.Errorf("event model/channel = %s/%s, want the request's own", ev.Model, ev.Channel)
	}
	if ev.Status != 461 || ev.BodyDigest != "deadbeef" {
		t.Errorf("event status/digest = %d/%s, want 461/deadbeef", ev.Status, ev.BodyDigest)
	}
	if !ev.At.Equal(fc.Now()) {
		t.Errorf("event time = %v, want %v", ev.At, fc.Now())
	}
	if p.StateOf("a1") != StateDisabled {
		t.Errorf("state = %v, want disabled (fail closed)", p.StateOf("a1"))
	}
}

// RISK-002: 461 sets no permanent flag — the account stays recoverable, because
// the form of the signal is unverified (INV-5).
func TestRiskControlIsNotPermanent(t *testing.T) {
	p, _ := poolWithEgress(t, nil)
	p.ReportRiskControl("a1", "upstream 461", RiskEvent{Status: 461})
	if p.PermanentOf("a1") {
		t.Error("461 was recorded as permanent; permanence needs account-scoped evidence we do not have")
	}
}

// RISK-003: a dead passToken is still permanent — that signal is account-scoped by
// construction (the credential itself is dead), unlike 451/461.
func TestDeadPassTokenStaysPermanent(t *testing.T) {
	p, _ := poolWithEgress(t, nil)
	p.ReportDisabled("a1", "passToken rejected", true)
	if !p.PermanentOf("a1") {
		t.Error("a dead passToken must stay permanent")
	}
}

// RISK-004: correlation — two accounts behind one egress, both signalling inside
// the window, count 2; an account behind another egress is not counted.
func TestRiskControlCorrelatesByEgress(t *testing.T) {
	p, fc := poolWithEgress(t, map[string]string{"a1": "hk1", "a2": "hk1", "a3": "sg2"})
	if n := p.ReportRiskControl("a1", "461", RiskEvent{Status: 461}); n != 1 {
		t.Errorf("first signal counted %d accounts, want 1", n)
	}
	if n := p.ReportRiskControl("a2", "461", RiskEvent{Status: 461}); n != 2 {
		t.Errorf("same-egress count = %d, want 2 (two accounts on hk1)", n)
	}
	if n := p.ReportRiskControl("a3", "461", RiskEvent{Status: 461}); n != 1 {
		t.Errorf("other-egress count = %d, want 1 (sg2 has one account)", n)
	}
	// Outside the window the old signals no longer correlate.
	fc.Advance(6 * time.Minute)
	if n := p.ReportRiskControl("a1", "461", RiskEvent{Status: 461}); n != 1 {
		t.Errorf("count outside the window = %d, want 1 (expired signals do not correlate)", n)
	}
	if got := len(p.RiskEvents()); got != 4 {
		t.Errorf("recorded events = %d, want 4 — evidence is not erased by the window", got)
	}
}

// RISK-005: 451 takes the same path as 461 — disabled, recorded, no permanence.
func TestRestricted451FollowsSamePath(t *testing.T) {
	p, _ := poolWithEgress(t, nil)
	p.ReportRiskControl("a1", "upstream 451 (account restricted)", RiskEvent{Status: 451})
	events := p.RiskEvents()
	if len(events) != 1 || events[0].Status != 451 {
		t.Fatalf("events = %+v, want one 451", events)
	}
	if p.PermanentOf("a1") {
		t.Error("451 must not be permanent")
	}
}

// RISK-006: an operator can undo a risk-control disable, and the evidence stays.
func TestRiskControlIsOperatorRecoverable(t *testing.T) {
	p, _ := poolWithEgress(t, nil)
	p.ReportRiskControl("a1", "461", RiskEvent{Status: 461})
	p.Enable("a1")
	if got := p.StateOf("a1"); got != StateAvailable {
		t.Errorf("state after Enable = %v, want available", got)
	}
	if len(p.RiskEvents()) != 1 {
		t.Error("the recorded evidence must survive the operator's recovery")
	}
}

// RISK-007: a signal for an account the pool does not know is still recorded (the
// evidence is about the upstream), and does not panic.
func TestRiskControlUnknownAccount(t *testing.T) {
	p, _ := poolWithEgress(t, nil)
	n := p.ReportRiskControl("ghost", "461", RiskEvent{Status: 461})
	if n != 1 {
		t.Errorf("count = %d, want 1 (the ghost is recorded on the default egress)", n)
	}
	if len(p.RiskEvents()) != 1 {
		t.Error("the event was not recorded")
	}
}
