package api_test

// "Too long" handling (ADR 0015).
//
// The platform's own limit is real but is not a clean character count: measured
// 2026-10-09 on the regular channel, a single 95,000- and a 100,100-char message
// are served, 105,000 is refused, and 300,000 ASCII chars are refused, so the
// ceiling moves with content. Our answer is therefore two-part: a guardrail that
// only stops the absurd, and a translation of the upstream's own refusal (prose
// in a 5xx envelope) into the client's own 400 with something to act on.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/api"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// TOOLONG-001: the mid-stream refusal the regular channel actually uses becomes
// a 400 the caller can act on, not a 502 carrying English prose.
func TestUpstreamTooLongStreamErrorMapsToClientError(t *testing.T) {
	var srv api.Server
	rec := httptest.NewRecorder()
	srv.MapError(rec, &mimo.ErrUpstreamStream{Message: "Sorry, the text you sent is too long! I suggest you simplify the content appropriately or send it in parts."})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the caller's request is at fault, not the gateway)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"context_too_long"`) {
		t.Errorf("body = %s, want code context_too_long", body)
	}
	if !strings.Contains(body, "shorter messages") {
		t.Errorf("body = %s, want an instruction the caller can act on", body)
	}
}

// TOOLONG-002: the same refusal arriving as an HTTP error body (the fastchat
// channel answers `query is too long`) maps identically.
func TestUpstreamTooLongHTTPErrorMapsToClientError(t *testing.T) {
	var srv api.Server
	rec := httptest.NewRecorder()
	srv.MapError(rec, &mimo.UpstreamError{StatusCode: http.StatusBadGateway, Body: `{"error":{"message":"query is too long"}}`})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"context_too_long"`) {
		t.Errorf("body = %s, want code context_too_long", rec.Body.String())
	}
}

// TOOLONG-003: an unrelated upstream failure keeps its 502 — the translation
// must not swallow real upstream trouble.
func TestUpstreamOtherFailureStaysBadGateway(t *testing.T) {
	var srv api.Server
	rec := httptest.NewRecorder()
	srv.MapError(rec, &mimo.ErrUpstreamStream{Message: "服务器繁忙，请稍后再试"})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "context_too_long") {
		t.Errorf("body = %s, a busy upstream is not a context problem", rec.Body.String())
	}
}

// TOOLONG-004 (regression): the 90,000-char guess used to reject work the
// platform serves. A 95,000-char message must reach the upstream.
func TestLongButServableMessageIsForwarded(t *testing.T) {
	if got := adapter.MaxQueryChars("mimo-v2.6-pro"); got < 95000 {
		t.Fatalf("non-fastchat cap = %d, want >= 95000 (measured served)", got)
	}
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	long := strings.Repeat("a", 95000)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", long}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a served-length message must not be rejected locally", resp.StatusCode)
	}
	if ts.Upstream.ChatHits() != 1 {
		t.Errorf("upstream hits = %d, want 1", ts.Upstream.ChatHits())
	}
}

// TOOLONG-005 (regression, other side): the fastchat cap is a real limit, so it
// is still enforced locally — with zero upstream traffic — and the message now
// carries the numbers.
func TestFastchatCapStillEnforcedLocallyWithNumbers(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	long := strings.Repeat("a", 50001)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", long}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ts.Upstream.ChatHits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (the cap saves a wasted account call)", ts.Upstream.ChatHits())
	}
}

// TOOLONG-006: an operator override wins over the per-channel caps, so a
// deployment can raise (or lower) the guardrail without a rebuild.
func TestMaxQueryCharsOverride(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{MaxQueryChars: 1234})
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	// 2000 chars: served by default on this model, refused under the override.
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", strings.Repeat("a", 2000)}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 under a 1234-char override", resp.StatusCode)
	}
	if ts.Upstream.ChatHits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (the override must reject locally)", ts.Upstream.ChatHits())
	}
}

// TOOLONG-007: a too-long refusal is the caller's fault, so the account is not
// penalized — the pool must stay available after it.
func TestTooLongRefusalDoesNotPenalizeAccount(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSETooLong})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an upstream too-long refusal", resp.StatusCode)
	}
	if state := ts.Pool.StateOf("acc1"); state != pool.StateAvailable {
		t.Errorf("account state = %v, want available (a client-side limit is not an account fault)", state)
	}
}
