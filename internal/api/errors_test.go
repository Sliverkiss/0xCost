package api_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// ERRMAP-001..012: the full §7.4 error-mapping table, end-to-end through the
// HTTP surface. Each row drives the real triggering path (pool state,
// upstream script) rather than calling MapError directly, per TDD §6 wave 6
// ("every triggering path exists; close the table last").

// twoAccounts (from api_test.go): distinct scripted identities for
// cooldown/exclusion rows.

// holdSlot starts a stalled chat turn in the background so the pool's single
// in-flight slot stays taken while the caller probes the busy mapping.
//
// Why a goroutine: the gateway is non-streaming (ADR 0017), so ts.Chat only
// returns once the whole body has arrived — a stalled turn cannot be awaited
// inline the way it could when the first SSE byte released the caller. The
// returned function waits for the stalled turn to finish.
func holdSlot(t *testing.T, ts *testserver.TestServer) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
		if err == nil && resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	// Let the lease be taken before the caller queues a second request.
	time.Sleep(150 * time.Millisecond)
	return func() { <-done }
}

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

// ERRMAP-003: all accounts cooling → 429 pool_parked + Retry-After = ceil
// (nearest recovery − now).
func TestERRMAPPoolParked(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Park both: 429 ladder base is 60s, second hit doubles to 120s.
	for _, ph := range []string{testutil.TestPH, testutil.TestPH2} {
		ts.Upstream.Script(ph, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})
	}
	for i := 0; i < 2; i++ {
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
		if err != nil {
			t.Fatal(err)
		}
		readErr(t, resp) // drain; the 429s park the accounts
	}
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusTooManyRequests || typ != "rate_limit_error" || code != "pool_parked" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("missing Retry-After header")
	}
	n, err := strconv.Atoi(ra)
	if err != nil || n < 1 {
		t.Errorf("Retry-After = %q", ra)
	}
}

// ERRMAP-004: all slots busy, queue wait exceeded → 429 pool_busy +
// Retry-After (SDD §7.4 row 4).
func TestERRMAPPoolBusy(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		MaxInflight: 1,
		QueueWait:   50 * time.Millisecond,
	})
	// Occupy the single slot: stall mid-stream so the handler keeps the lease
	// while the second request queues.
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{
		Body:           testutil.FixtureSSEStall,
		StallMidStream: 2 * time.Second,
		MidStreamSplit: 134,
	})
	release := holdSlot(t, ts)
	defer release()
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusTooManyRequests || typ != "rate_limit_error" || code != "pool_busy" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("missing Retry-After header")
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

// ERRMAP-006: upstream 429 surfaced → 429 rate_limit_error /
// upstream_rate_limited, and the account parks (cooldown, no retry).
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
	// The serving account parked.
	st1, st2 := ts.Pool.StateOf("acc1"), ts.Pool.StateOf("acc2")
	if st1 != pool.StateCooldown && st2 != pool.StateCooldown {
		t.Errorf("neither account cooled: %v/%v", st1, st2)
	}
}

// ERRMAP-007: upstream 401 after the refresh retry → 401
// invalid_request_error / upstream_auth_expired.
func TestERRMAPUpstreamAuthExpired(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Script BOTH identities (round-robin selection); renewal is impossible
	// (no passToken stored) so the refresh retry fails → mapped 401.
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

// ERRMAP-008: upstream 5xx pre-first-byte → 502 api_error / upstream_failure
// on failure; the 1 allowed retry lands on a different account (SDD
// §7.1/§7.2), so a healthy second account recovers the request.
func TestERRMAPUpstream5xx(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusBadGateway})
	// acc2 also fails → both sick → client sees the mapped 502.
	ts.Upstream.Script(testutil.TestPH2, testutil.ScriptStep{StatusCode: http.StatusBadGateway})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusBadGateway || typ != "api_error" || code != "upstream_failure" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	// Both accounts were tried exactly once (original + 1 retry, §7.2).
	phs := map[string]int{}
	for _, r := range ts.Upstream.Requests() {
		phs[r.PH]++
	}
	if phs[testutil.TestPH] != 1 || phs[testutil.TestPH2] != 1 {
		t.Errorf("per-account hits = %v, want 1 each (retry switch)", phs)
	}
	if ts.Pool.StateOf("acc1") != pool.StateCooldown {
		t.Errorf("acc1 state = %v, want cooldown after 5xx", ts.Pool.StateOf("acc1"))
	}
}

// ERRMAP-008 (recovery half): one sick account + one healthy → the retry
// recovers; the client never sees the 502.
func TestERRMAPUpstream5xxRetryRecovers(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Round-robin serves acc2 first (cursor starts at band index 1), so the
	// 502 script goes on the second identity; the retry lands on acc1.
	ts.Upstream.Script(testutil.TestPH2, testutil.ScriptStep{StatusCode: http.StatusBadGateway})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s, want 200 via retry switch", resp.StatusCode, b)
	}
	if ts.Pool.StateOf("acc2") != pool.StateCooldown {
		t.Errorf("acc2 state = %v, want cooldown after 5xx", ts.Pool.StateOf("acc2"))
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

// ERRMAP-010: query over the channel char cap → 400 context_too_long, zero
// upstream requests (cross-ref API-016). mimo-v2.6-pro-ultraspeed-studio caps
// at 50000 chars.
func TestERRMAPContextTooLong(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	long := strings.Repeat("a", 50001)
	body := testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", long})
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusBadRequest || typ != "invalid_request_error" || code != "context_too_long" {
		t.Fatalf("got %d %q %q", status, typ, code)
	}
	if ts.Upstream.ChatHits() != 0 {
		t.Errorf("char-cap rejection must not reach upstream, hits = %d", ts.Upstream.ChatHits())
	}
}

// ERRMAP-011: Retry-After privacy invariant — across all 429 rows the
// Retry-After reflects only local recovery deadlines, and no response body
// or header ever carries phone/userId/cookie/passToken.
func TestERRMAPRetryAfterPrivacy(t *testing.T) {
	// Parked row.
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	for _, ph := range []string{testutil.TestPH, testutil.TestPH2} {
		ts.Upstream.Script(ph, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})
	}
	for i := 0; i < 2; i++ {
		resp, _ := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
		readErr(t, resp)
	}
	resp, _ := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	status, _, typ, code := readErr(t, resp)
	if status != http.StatusTooManyRequests || typ != "rate_limit_error" || code != "pool_parked" {
		t.Fatalf("parked row: got %d %q %q", status, typ, code)
	}
	ra := resp.Header.Get("Retry-After")
	n, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After not numeric: %q", ra)
	}
	// Nearest recovery is within the 60s/120s ladder; header seconds are
	// ceil(nearest recovery − now).
	if n < 1 || n > 120 {
		t.Errorf("Retry-After = %d, want 1..120", n)
	}

	// Busy row.
	ts2 := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		MaxInflight: 1,
		QueueWait:   50 * time.Millisecond,
	})
	ts2.Upstream.Script(testutil.TestPH, testutil.ScriptStep{
		Body:           testutil.FixtureSSEStall,
		StallMidStream: 2 * time.Second,
		MidStreamSplit: 134,
	})
	release2 := holdSlot(t, ts2)
	defer release2()
	resp2, _ := ts2.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	status2, _, _, code2 := readErr(t, resp2)
	if status2 != http.StatusTooManyRequests || code2 != "pool_busy" {
		t.Fatalf("busy row: got %d %q", status2, code2)
	}
	if ra2 := resp2.Header.Get("Retry-After"); ra2 == "" {
		t.Error("pool_busy row missing Retry-After")
	}

	// Upstream-429 row: surfaced mapping; nothing leaks in the body.
	ts3 := testserver.NewTestServer(t, oneAccount(), nil)
	ts3.Upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusTooManyRequests})
	resp3, _ := ts3.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	status3, msg3, typ3, code3 := readErr(t, resp3)
	if status3 != http.StatusTooManyRequests || typ3 != "rate_limit_error" || code3 != "upstream_rate_limited" {
		t.Fatalf("upstream 429 row: got %d %q %q", status3, typ3, code3)
	}
	// Privacy: no credential anywhere. The mock cookies carry "st-mock" and
	// "userId=123456"; none may appear in any response.
	if strings.Contains(msg3, "st-mock") || strings.Contains(msg3, "userId") ||
		strings.Contains(msg3, "passToken") || strings.Contains(msg3, "serviceToken") {
		t.Errorf("429 body leaks identity: %q", msg3)
	}
	for _, hdr := range []string{"Retry-After", "Content-Type", "X-Mimo-Warnings"} {
		if v := resp.Header.Get(hdr); strings.Contains(v, "st-mock") || strings.Contains(v, "123456") {
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
