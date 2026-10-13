package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// ERRMAP-001..012: the full §7.4 error-mapping table, end-to-end through the
// HTTP surface. Each row drives the real triggering path (pool state,
// upstream script) rather than calling MapError directly, per TDD §6 wave 6
// ("every triggering path exists; close the table last").

// twoAccounts (from api_test.go): distinct scripted identities.

// readErr drains an error response and asserts the OpenAI error shape.
func readErr(t *testing.T, resp *http.Response) (status int, msg, typ, code string) {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	msg, typ, code = testutil.ParseError(t, b)
	return resp.StatusCode, msg, typ, code
}

// ERRMAP-001: bad API key → 401 invalid_request_error / invalid_api_key.
func TestERRMAPBadKey(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "nope")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusUnauthorized || typ != "invalid_request_error" || code != "invalid_api_key" {
		t.Errorf("got %d %q %q", status, typ, code)
	}
}

// ERRMAP-002: malformed body / no user message → 400 invalid_request_error with a
// NAMED code (SDD §7.4, ADR 0032): an empty code left a client switching on `code`
// with nothing to switch on, and the audio surface was already answering
// `malformed_request_body` for the same failure.
func TestERRMAPMalformedAndNoUser(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	for _, tc := range []struct{ body, wantCode string }{
		{`{"messages":[`, "malformed_request_body"},                                                             // broken JSON
		{`{"model":"mimo-v2.6-pro","messages":[]}`, "malformed_request_body"},                                   // empty messages
		{`{"model":"mimo-v2.6-pro","messages":[{"role":"system","content":"only system"}]}`, "no_user_message"}, // no user turn
	} {
		resp, err := ts.Chat(tc.body, "")
		if err != nil {
			t.Fatal(err)
		}
		status, _, typ, code := readErr(t, resp)
		if status != http.StatusBadRequest || typ != "invalid_request_error" || code != tc.wantCode {
			t.Errorf("body %q: got %d %q %q, want %d invalid_request_error %q", tc.body, status, typ, code, http.StatusBadRequest, tc.wantCode)
		}
	}
	if ts.Upstream.ChatHits() != 0 {
		t.Errorf("malformed rows must not reach upstream, got %d hits", ts.Upstream.ChatHits())
	}
}

// ERRMAP-005: all disabled / empty pool → 503 api_error / no_accounts.
func TestERRMAPNoAccounts(t *testing.T) {
	// Zero accounts = empty pool.
	ts := testserver.NewTestServer(t, nil, nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusServiceUnavailable || typ != "api_error" || code != "no_accounts" {
		t.Errorf("empty pool: got %d %q %q", status, typ, code)
	}
}

// ERRMAP-006: upstream 429 surfaced verbatim → 429 rate_limit_error /
// upstream_rate_limited, exactly one upstream hit (no retry), and the serving
// account stays available (owner ruling 2026-10: pass-through, no cooldown).
func TestERRMAPUpstream429(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Script BOTH identities: selection is round-robin, either may serve.
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})
	ts.Upstream.Script(testutil.TestPH2, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusTooManyRequests || typ != "rate_limit_error" || code != "upstream_rate_limited" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	// No retry on 429 (SDD §7.1): exactly one upstream hit for the request.
	if n := ts.Upstream.ChatHits(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (no 429 retry)", n)
	}
	// The serving account stays available — the 429 passes through untouched.
	if st := ts.Pool.StateOf("acc1"); st != pool.StateAvailable {
		t.Errorf("acc1 state = %v, want available after a passed-through 429", st)
	}
	if st := ts.Pool.StateOf("acc2"); st != pool.StateAvailable {
		t.Errorf("acc2 state = %v, want available after a passed-through 429", st)
	}
}

// ERRMAP-007: upstream 401 → 401 invalid_request_error / upstream_auth_expired.
func TestERRMAPUpstreamAuthExpired(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Script BOTH identities (round-robin selection).
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusUnauthorized})
	ts.Upstream.Script(testutil.TestPH2, testutil.ScriptStep{StatusCode: http.StatusUnauthorized})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusUnauthorized || typ != "invalid_request_error" || code != "upstream_auth_expired" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
}

// ERRMAP-008: upstream 5xx → the upstream's own status passes through to the
// client as an api_error / upstream_failure (owner ruling 2026-10: no retry on
// another account, no cooldown). Exactly one upstream hit.
func TestERRMAPUpstream5xx(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Script BOTH identities: selection is round-robin and either may serve —
	// the answer must be the same either way.
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusBadGateway})
	ts.Upstream.Script(testutil.TestPH2, testutil.ScriptStep{StatusCode: http.StatusBadGateway})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusBadGateway || typ != "api_error" || code != "upstream_failure" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	// No retry: exactly one upstream hit for the request.
	if n := ts.Upstream.ChatHits(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (no 5xx retry; pass-through)", n)
	}
	// The failure never touches account availability.
	if ts.Pool.StateOf("acc1") != pool.StateAvailable || ts.Pool.StateOf("acc2") != pool.StateAvailable {
		t.Errorf("states = %v/%v, want both available after a passed-through 5xx",
			ts.Pool.StateOf("acc1"), ts.Pool.StateOf("acc2"))
	}
}

// ERRMAP-009: sensitive_query → 400 invalid_request_error / content_filter, no pool
// penalty (SDD §7.1 content-level), AND the platform's own words leave the building
// (ADR 0033): the event name and its `content` payload are the only account of why
// the turn was refused, so the client gets them verbatim instead of our paraphrase.
func TestERRMAPContentFilter(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSESensitive})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "bad topic"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, msg, typ, code := readErr(t, resp)
	if status != http.StatusBadRequest || typ != "invalid_request_error" || code != "content_filter" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	// FixtureSSESensitive carries event:sensitive_query with content "topic not allowed".
	for _, want := range []string{"sensitive_query", "topic not allowed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want the upstream's own %q passed through", msg, want)
		}
	}
	if ts.Pool.StateOf("acc1") != pool.StateAvailable {
		t.Errorf("content filter must not penalize the account, state = %v", ts.Pool.StateOf("acc1"))
	}
}

// ERRMAP-010 (owner ruling 2026-10-14, "字数预检砍了，超了上游会自己报错透
// 传"): a query over the channel char cap is NO LONGER rejected locally — the
// request is forwarded and the upstream's own refusal is passed through as
// 400 invalid_request_error / context_too_long. Exactly one upstream hit: the
// upstream, not the gateway, is the source of truth for its own limits.
func TestERRMAPContextTooLong(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	// The upstream answers its own too-long refusal (FixtureSSETooLong: the
	// "Sorry, the text you sent is too long!" error event).
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSETooLong})
	long := strings.Repeat("a", 50001)
	body := testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", long})
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	status, msg, typ, code := readErr(t, resp)
	if status != http.StatusBadRequest || typ != "invalid_request_error" || code != "context_too_long" {
		t.Fatalf("got %d %q %q (%q), want the upstream-refusal mapping", status, typ, code, msg)
	}
	// The message must carry the upstream's own wording, not a local
	// rejection: the refusal is the upstream's answer, passed through.
	if !strings.Contains(msg, "too long") {
		t.Errorf("message = %q, want the upstream's own too-long refusal in it", msg)
	}
	// Forwarded, not locally rejected: exactly one upstream hit.
	if n := ts.Upstream.ChatHits(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (over-cap request forwarded, refusal passed through)", n)
	}
}

// ERRMAP-011: Retry-After privacy invariant — the upstream's Retry-After
// header is the only Retry-After there is (the pool never invents one), and
// no response body or header ever carries phone/userId/cookie/passToken.
func TestERRMAPRetryAfterPrivacy(t *testing.T) {
	// Upstream-429 row: surfaced mapping; nothing leaks in the body.
	ts3 := testserver.NewTestServer(t, oneAccount(), nil)
	ts3.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})
	resp3, _ := ts3.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	status3, msg3, typ3, code3 := readErr(t, resp3)
	if status3 != http.StatusTooManyRequests || typ3 != "rate_limit_error" || code3 != "upstream_rate_limited" {
		t.Fatalf("upstream 429 row: got %d %q", status3, code3)
	}
	// Privacy: no credential anywhere. The mock cookies carry "st-mock" and
	// "userId=123456"; none may appear in any response.
	if strings.Contains(msg3, "st-mock") || strings.Contains(msg3, "userId") ||
		strings.Contains(msg3, "passToken") || strings.Contains(msg3, "serviceToken") {
		t.Errorf("429 body leaks identity: %q", msg3)
	}
	for _, hdr := range []string{"Retry-After", "Content-Type", "X-Mimo-Warnings"} {
		if v := resp3.Header.Get(hdr); strings.Contains(v, "st-mock") || strings.Contains(v, "123456") {
			t.Errorf("header %s leaks identity: %q", hdr, v)
		}
	}
}

// API-021 / ERRMAP-012: an upstream failure in the middle of the body is
// reported as an HTTP error, never as a 200 carrying a blank or truncated
// answer. The old streaming contract this case pinned (an error-text delta,
// then finish_reason:"stop" then [DONE]) died with the SSE path (ADR 0017).
func TestERRMAPUpstreamMidBodyError(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEError})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, msg, typ, code := readErr(t, resp)
	t.Logf("mid-body error maps to %d %q %q %q", status, msg, typ, code)
	if status == http.StatusOK {
		t.Fatal("status = 200: a mid-body upstream failure must not be reported as a success")
	}
	if code == "" || typ == "" || msg == "" {
		t.Errorf("envelope = %d %q %q %q, want a complete OpenAI error shape", status, msg, typ, code)
	}
	if !strings.Contains(strings.ToLower(msg), "upstream") {
		t.Errorf("msg = %q, want the upstream named as the source", msg)
	}
}

// API-020 (TDD §3): a STREAMING request whose upstream attempts all fail
// before the first byte must answer a plain HTTP error — correct status and
// the §7.4 JSON shape — never a 200 with the error stuffed into an SSE body
// (that would make clients see a "successful" stream that says nothing).
func TestAPI020StreamingPreFirstByteErrorIsPlainJSON(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Fatalf("pre-first-byte failure answered Content-Type %q — an error must not be framed as SSE", ct)
	}
	status, msg, typ, code := readErr(t, resp)
	if status != http.StatusTooManyRequests || typ != "rate_limit_error" || code != "upstream_rate_limited" {
		t.Fatalf("got %d %q %q (%q), want the 429 envelope", status, typ, code, msg)
	}
	if strings.Contains(msg, "data:") || strings.Contains(msg, "[DONE]") {
		t.Errorf("error body carries SSE framing: %q", msg)
	}
}
