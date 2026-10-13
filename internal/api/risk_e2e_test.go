package api_test

// Risk-control signals end to end (TDD §3.5.2 RISK-001..005, SDD §4.5.2).
//
// 451/461 had no end-to-end test at all before this file: the mock could serve
// them, but nothing drove the path. These cases pin the fail-closed disable, the
// absence of the permanent flag, the recorded evidence, and the cross-account
// correlation that will eventually tell the three shapes apart.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// chatRaw posts one chat request and returns the status and body, so the
// assertions can talk about what the client actually received.
func chatRaw(t *testing.T, ts *testserver.TestServer, body string) (int, string) {
	t.Helper()
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// RISK-001/002: a 461 disables the account (fail closed), records the evidence
// with the request's model, channel and status, and sets no permanent flag.
func TestRiskControl461DisablesAndRecords(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{
		StatusCode: 461, Body: `{"msg":"account restricted"}`,
	})
	if status, body := chatRaw(t, ts, testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"})); status == http.StatusOK {
		t.Fatalf("status = 200 body = %s, want an upstream restriction error", body)
	}

	if got := ts.Pool.StateOf("acc1"); got != pool.StateDisabled {
		t.Errorf("state = %v, want disabled", got)
	}
	perm := false
	for _, s := range ts.Pool.Snapshot() {
		if s.ID == "acc1" {
			perm = s.Permanent
		}
	}
	if perm {
		t.Error("461 must not be permanent: the form of the signal is unverified (REQ-10)")
	}
	events := ts.Pool.RiskEvents()
	if len(events) == 0 {
		t.Fatal("no risk-control event recorded")
	}
	ev := events[len(events)-1]
	if ev.Status == 0 {
		t.Errorf("event status = %d, want the upstream's own status", ev.Status)
	}
	if ev.Channel == "" {
		t.Error("event channel is empty; the shape question needs the channel")
	}
	if ev.AccountID != "acc1" || ev.Egress != pool.DefaultEgress {
		t.Errorf("event account/egress = %s/%s, want acc1/direct", ev.AccountID, ev.Egress)
	}
	if ev.BodyDigest == "" {
		t.Error("event body digest is empty")
	}
	if strings.Contains(ev.BodyDigest, "restricted") {
		t.Error("the digest must not carry the body itself")
	}
}

// RISK-004: two accounts behind one egress label correlate; a third behind another
// egress does not join them.
func TestRiskControlCorrelatesSameEgressEndToEnd(t *testing.T) {
	two := []testserver.TestAccount{
		{ID: "acc1", PH: "ph-1", Cookie: testutil.TestCookie, Egress: "hk1"},
		{ID: "acc2", PH: "ph-2", Cookie: testutil.TestCookie, Egress: "hk1"},
		{ID: "acc3", PH: "ph-3", Cookie: testutil.TestCookie, Egress: "sg2"},
	}
	ts := testserver.NewTestServer(t, two, nil)
	for _, ph := range []string{"ph-1", "ph-2", "ph-3"} {
		ts.Upstream.Script(ph, testutil.ScriptStep{StatusCode: 461, Body: `{"msg":"restricted"}`})
	}
	// Two accounts behind hk1 signal; both are disabled and recorded.
	for i := 0; i < 2; i++ {
		chatRaw(t, ts, testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}))
	}
	events := ts.Pool.RiskEvents()
	if len(events) < 2 {
		t.Fatalf("recorded events = %d, want >= 2 (two accounts signalled)", len(events))
	}
	hk1 := map[string]bool{}
	for _, ev := range events {
		if ev.Egress == "hk1" {
			hk1[ev.AccountID] = true
		}
	}
	if len(hk1) != 2 {
		t.Errorf("accounts correlated on hk1 = %d, want 2 (%v)", len(hk1), hk1)
	}
	for _, ev := range events {
		if ev.AccountID == "acc3" {
			t.Error("acc3 is behind sg2 and must not appear in an hk1 correlation")
		}
	}
}

// RISK-006: an operator re-enable brings the account back and keeps the evidence.
func TestRiskControlOperatorRecoveryKeepsEvidence(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: 451, Body: `{"msg":"restricted"}`})
	chatRaw(t, ts, testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}))
	if got := ts.Pool.StateOf("acc1"); got != pool.StateDisabled {
		t.Fatalf("state = %v, want disabled", got)
	}
	before := len(ts.Pool.RiskEvents())
	ts.Pool.Enable("acc1")
	if got := ts.Pool.StateOf("acc1"); got != pool.StateAvailable {
		t.Errorf("state after Enable = %v, want available", got)
	}
	if len(ts.Pool.RiskEvents()) != before {
		t.Error("recovery must not erase the recorded evidence")
	}
}
