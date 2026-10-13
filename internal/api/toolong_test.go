package api_test

// "Too long" handling (ADR 0015), post owner-ruling 2026-10-14.
//
// The platform's own limit is real but is not a clean character count: measured
// 2026-10-09 on the regular channel, a single 95,000- and a 100,100-char message
// are served, 105,000 is refused, and 300,000 ASCII chars are refused, so the
// ceiling moves with content. The local char precheck is gone (owner ruling
// 2026-10-14, "字数预检砍了，超了上游会自己报错透传"): the gateway forwards
// every request, and what survives here is the translation of the upstream's
// own refusal (prose in a 5xx envelope) into the client's own 400 with
// something to act on.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// TOOLLONG typo guard is not needed; tests follow.

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

// TOOLONG-004: a 95,000-char message (measured served on the regular channel)
// is forwarded and served — nothing in the gateway second-guesses the
// platform's own limits.
func TestLongButServableMessageIsForwarded(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	long := strings.Repeat("a", 95000) // measured served
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

// TOOLONG-005 (owner ruling 2026-10-14, was "fastchat cap still enforced
// locally"): an over-cap message on the fastchat channel is forwarded and the
// upstream's own refusal is passed through — 400 context_too_long, exactly one
// upstream hit, and the account stays available. The upstream, not the
// gateway, is the source of truth for its own limits.
func TestFastchatOverCapIsForwardedAndRefusedUpstream(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSETooLong})
	long := strings.Repeat("a", 50001)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", long}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400 (the upstream's own refusal): %s", resp.StatusCode, raw)
	}
	if ts.Upstream.ChatHits() != 1 {
		t.Errorf("upstream hits = %d, want 1 (over-cap is forwarded, not locally rejected)", ts.Upstream.ChatHits())
	}
	if st := ts.Pool.StateOf("acc1"); st != pool.StateAvailable {
		t.Errorf("account state = %v, want available (a client-side limit is not an account fault)", st)
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

// TOOLONG-008: the composed-query precheck is gone with the per-message
// precheck (owner ruling 2026-10-14). A system prompt plus a message that
// together sit just under the measured platform ceiling is forwarded as-is
// and served — nothing in the gateway second-guesses the platform's budget
// accounting (the refusal side is TOOLONG-005/ERRMAP-010).
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

// STREAM-001 (retired with ADR 0034): the gateway used to refuse `stream:true`
// with `400 stream_unsupported`. Streaming is served again, so the case lives on
// in internal/api/stream_test.go as the STREAM-011..021 family — frame shape,
// commit boundary, in-band errors, failover, and both upstream channels.
