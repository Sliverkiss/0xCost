package api_test

// PASSFAIL-001..004: the pass-through contract (owner ruling, 2026-10-14 —
// "we don't do cooldown/backoff, failures pass through").
//
// The pool never parks, cools, or ladders an account on upstream 429/5xx/
// transport failures. Whatever the upstream answers — status, body,
// Retry-After — is what the client receives. These cases pin that end to end:
// the client sees the upstream's answer shape, and the account stays
// available for the next request.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// PASSFAIL-001: an upstream 429 passes through to the client with the
// upstream's own status, body and Retry-After, and the account stays
// available — no cooldown.
func TestPassfailUpstream429PassesThrough(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Script BOTH identities: selection is round-robin, either may serve.
	for _, ph := range []string{testutil.TestPH, testutil.TestPH2} {
		ts.Upstream.Script(ph, testutil.ScriptStep{
			StatusCode: http.StatusTooManyRequests,
			Headers:    map[string]string{"Retry-After": "7"},
		})
	}
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, msg, typ, code := readErr(t, resp)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's 429 verbatim", status)
	}
	if typ != "rate_limit_error" || code != "upstream_rate_limited" {
		t.Fatalf("got %q %q, want rate_limit_error upstream_rate_limited", typ, code)
	}
	if !strings.Contains(msg, "rate limited") {
		t.Errorf("message = %q, want the upstream's own body wording passed through", msg)
	}
	// No retry on 429: exactly one upstream hit for the request.
	if n := ts.Upstream.ChatHits(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (no 429 retry)", n)
	}
	// The serving account stayed available — the core ruling.
	st1, st2 := ts.Pool.StateOf("acc1"), ts.Pool.StateOf("acc2")
	if st1 != pool.StateAvailable || st2 != pool.StateAvailable {
		t.Errorf("accounts = %v/%v, want both available (no cooldown)", st1, st2)
	}
}

// PASSFAIL-002: the upstream's own Retry-After header travels with the 429 —
// when the upstream sent one, the client sees it.
func TestPassfailUpstream429ForwardsRetryAfter(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{
		StatusCode: http.StatusTooManyRequests,
		Headers:    map[string]string{"Retry-After": "9"},
	})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Retry-After"); got != "9" {
		t.Errorf("Retry-After = %q, want the upstream's own 9", got)
	}
}

// PASSFAIL-003: an upstream 5xx passes through (502 → client 502) with the
// upstream's status, and the account stays available.
func TestPassfailUpstream5xxPassesThrough(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusBadGateway})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want the upstream's 502 verbatim", status)
	}
	if typ != "api_error" || code != "upstream_failure" {
		t.Fatalf("got %q %q, want api_error upstream_failure", typ, code)
	}
	if got := ts.Pool.StateOf("acc1"); got != pool.StateAvailable {
		t.Errorf("account state = %v, want available (no cooldown on 5xx)", got)
	}
}

// PASSFAIL-004: repeated 429s never park the pool — the third request still
// reaches the upstream and still gets its 429 (no pool_parked, no cooldown,
// nothing but the upstream's own answer).
func TestPassfailRepeated429NeverParks(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	for _, ph := range []string{testutil.TestPH, testutil.TestPH2} {
		ts.Upstream.Script(ph, testutil.ScriptStep{
			StatusCode: http.StatusTooManyRequests,
			Headers:    map[string]string{"Retry-After": "3"},
		})
	}
	for i := 0; i < 3; i++ {
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
		if err != nil {
			t.Fatal(err)
		}
		status, _, _, code := readErr(t, resp)
		if status != http.StatusTooManyRequests || code == "pool_parked" {
			t.Fatalf("request %d: status = %d code = %q, want the upstream's 429, never pool_parked", i+1, status, code)
		}
	}
	// Every request reached the upstream — the pool never went quiet.
	if n := ts.Upstream.ChatHits(); n != 3 {
		t.Errorf("upstream hits = %d, want 3 (the pool never parked)", n)
	}
	s := ts.Pool.Summarize()
	if s.Available != 2 {
		t.Errorf("available = %d, want 2 (no account ever cooled)", s.Available)
	}
}
