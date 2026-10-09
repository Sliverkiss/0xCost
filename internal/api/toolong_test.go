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
	"io"
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
	if got := adapter.MaxQueryChars("mimo-v2.6-pro"); got != 102400 {
		t.Fatalf("non-fastchat cap = %d, want the measured 102400 platform ceiling", got)
	}
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	long := strings.Repeat("a", 95000) // measured served (and blocked by the old 90,000 guess)
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

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}), "")
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

// TOOLONG-008: the ceiling applies to the query text the platform receives, not
// to each message on its own — the planner splices system messages into the
// first user query, so a preamble (a client system prompt, or the protocol
// prompt a tool layer injects in front of us) spends the same budget. Live
// measurement: 1,760 chars of injected protocol + a 100,000-char message is
// served, the same protocol with 101,000 chars is refused by the platform. Both
// halves are under the cap here, so only the composed check can catch it — and
// it must catch it locally, without spending an account call.
func TestComposedQueryOverCapIsRejectedLocally(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := testutil.ChatBody("mimo-v2.6-pro", false,
		[2]string{"system", strings.Repeat("p", 2000)},
		[2]string{"user", strings.Repeat("a", 101000)})
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (each message fits, the composed query does not)", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	text := string(got)
	if !strings.Contains(text, `"code":"context_too_long"`) {
		t.Errorf("body = %s, want code context_too_long", text)
	}
	// The caller must be able to see where the characters went: 2000 of them are
	// preamble it may not realise it is paying for.
	if !strings.Contains(text, "preamble") {
		t.Errorf("body = %s, want the preamble size named", text)
	}
	if ts.Upstream.ChatHits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (the platform would have refused this too)", ts.Upstream.ChatHits())
	}
}

// TOOLONG-009: the same shape just under the ceiling is still served — the
// composed check must not become a blanket ban on system prompts.
func TestComposedQueryUnderCapIsServed(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := testutil.ChatBody("mimo-v2.6-pro", false,
		[2]string{"system", strings.Repeat("p", 2000)},
		[2]string{"user", strings.Repeat("a", 100000)})
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (composed 102,007 chars is under the 102,400 ceiling): %s", resp.StatusCode, raw)
	}
	if ts.Upstream.ChatHits() != 1 {
		t.Errorf("upstream hits = %d, want 1", ts.Upstream.ChatHits())
	}
}

// STREAM-001: the gateway has no streaming path at all (ADR 0017), so a request
// that asks for SSE is refused by name with the fix, before any account work — an
// OpenAI client that got a one-shot JSON body instead would fail on the parse.
func TestStreamingRefused(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := `{"model":"mimo-v2.6-pro","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: the gateway has no streaming path", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	got := string(raw)
	if !strings.Contains(got, `"code":"stream_unsupported"`) {
		t.Errorf("body = %s, want code stream_unsupported", body)
	}
	if !strings.Contains(got, "stream") || !strings.Contains(got, "false") {
		t.Errorf("body = %s, want the fix named", got)
	}
	if ts.Upstream.ChatHits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (refused before any account work)", ts.Upstream.ChatHits())
	}

	// The non-streaming request on the same stack is served normally.
	resp2, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("non-streaming status = %d, want 200", resp2.StatusCode)
	}
	if ts.Upstream.ChatHits() != 1 {
		t.Errorf("upstream hits = %d, want 1 after the non-streaming request", ts.Upstream.ChatHits())
	}
}
