package api_test

// Upstream in-band error handling (docs/web-surface-reverse.md §11.6).
//
// The upstream reuses `event:error` for two conditions that need opposite
// treatment: a transient platform throttle ("服务器繁忙，请稍后再试", live-observed
// to clear within seconds) and a deterministic model rejection ("模型名称错误",
// returned when an ultraspeed model is sent to the normal channel). Failing
// both as a content error — the old behaviour — turns a retryable blip into a
// client-visible 502 and burns an upstream call on a hopeless request.

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// sseErrorFrame renders an in-band upstream error frame exactly as the channel
// sends it (dialogId first, then error, then finish).
func sseErrorFrame(msg string) string {
	return "event:dialogId\ndata:{\"type\":null,\"content\":\"dlg-1\"}\n\n" +
		"event:error\ndata:{\"type\":\"text\",\"content\":\"" + msg + "\"}\n\n" +
		"event:finish\ndata:{\"content\":\"[DONE]\"}\n\n"
}

func chatOnce(t *testing.T, ts *testserver.TestServer, body string) (int, string) {
	t.Helper()
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// BUSY-001: a busy frame before any content is retried, and the client sees a
// clean success when the retry lands.
func TestUpstreamBusyRetriedThenSucceeds(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ph := acct[0].PH
	// The mock keeps the LAST scripted step sticky, so the retry needs its own
	// (successful) step — otherwise the retry would be served busy forever.
	ts.Upstream.Script(ph,
		testutil.ScriptStep{Body: sseErrorFrame("服务器繁忙，请稍后再试")},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
	)

	status, body := chatOnce(t, ts, testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 after the busy retry", status, body)
	}
	if n := len(ts.Upstream.Requests()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (busy turn + retry)", n)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "Hello world") {
		t.Errorf("body = %q, want a completed answer from the successful retry", body)
	}
}

// BUSY-003 (owner ruling 2026-10-14, "upstream is busy的800ms改成0，直接重
// 试，最多3次"): all three retries are live and each is immediate. Busy on the
// first three attempts and success on the fourth still yields 200, with no
// waiting between attempts (the ruling moved the backoff to 0).
func TestUpstreamBusyAllThreeRetriesLand(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ph := acct[0].PH
	ts.Upstream.Script(ph,
		testutil.ScriptStep{Body: sseErrorFrame("服务器繁忙，请稍后再试")},
		testutil.ScriptStep{Body: sseErrorFrame("服务器繁忙，请稍后再试")},
		testutil.ScriptStep{Body: sseErrorFrame("服务器繁忙，请稍后再试")},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
	)

	start := time.Now()
	status, body := chatOnce(t, ts, testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200: busy ×3 then success must be served by the third retry", status, body)
	}
	if n := len(ts.Upstream.Requests()); n != 4 {
		t.Fatalf("upstream calls = %d, want 4 (original + 3 retries)", n)
	}
	// Immediate retries: three busy frames cost no backoff wait at all (0ms,
	// was 800ms each). Local-serve latency only.
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("took %s for busy ×3 + success; retries must be immediate, not 800ms-paced", waited)
	}
}

// BUSY-002: busy on every attempt (4 total: original + 3 retries) → the client
// gets 503 upstream_busy with a Retry-After, not a 502 content failure, and the
// account is NOT disabled: the condition is platform load, not a dead account.
func TestUpstreamBusyTwiceMapsTo503(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ph := acct[0].PH
	// Single sticky step: busy on every attempt, including all three retries.
	ts.Upstream.Script(ph,
		testutil.ScriptStep{Body: sseErrorFrame("服务器繁忙，请稍后再试")},
	)

	status, body := chatOnce(t, ts, testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503 upstream_busy", status, body)
	}
	if !containsEvent(body, "upstream_busy") {
		t.Errorf("body = %q, want code upstream_busy", body)
	}
	// The platform's own sentence travels with it (ADR 0033): "the upstream is
	// busy" is our reading, 服务器繁忙，请稍后再试 is its evidence.
	if !strings.Contains(body, "服务器繁忙") {
		t.Errorf("body = %q, want the upstream's own wording passed through", body)
	}
	// The retry cap: original + 3 retries = 4 upstream calls, then the 503.
	if n := len(ts.Upstream.Requests()); n != 4 {
		t.Errorf("upstream calls = %d, want 4 (original + 3 busy retries, then give up)", n)
	}
	// Platform load, not a dead account: the account must stay selectable.
	if got := ts.Pool.StateOf("acc1"); got != pool.StateAvailable {
		t.Errorf("account state after busy = %v, want %v (busy must not cool the account)", got, pool.StateAvailable)
	}
}

// MODEL-001: a model-name rejection is deterministic — exactly one upstream
// call, a 400 to the client, and no retry.
func TestUpstreamModelRejectedFailsFast(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ph := acct[0].PH
	ts.Upstream.Script(ph, testutil.ScriptStep{Body: sseErrorFrame("模型名称错误")})

	status, body := chatOnce(t, ts, testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", "hello"}))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 model_not_available_on_channel", status, body)
	}
	if !strings.Contains(body, "模型名称错误") {
		t.Errorf("body = %q, want the upstream's own wording passed through (ADR 0033)", body)
	}
	if strings.Count(body, "模型名称错误") > 1 {
		t.Errorf("body = %q, repeats the upstream wording", body)
	}
	if !containsEvent(body, "model_not_available_on_channel") {
		t.Errorf("body = %q, want code model_not_available_on_channel", body)
	}
	if n := len(ts.Upstream.Requests()); n != 1 {
		t.Errorf("upstream calls = %d, want exactly 1 (a rejected model must not be retried)", n)
	}
	if got := ts.Pool.StateOf("acc1"); got == pool.StateDisabled {
		t.Errorf("account state = %v, want not disabled (a model rejection is not an account fault)", got)
	}
}

func containsEvent(body, want string) bool { return strings.Contains(body, want) }
