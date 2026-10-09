package api_test

// End-to-end failure attribution (TDD §3.5.2 POOLATTR-012..014, SDD §4.5.1).
//
// These drive the real API stack so the classification is asserted where it
// matters: on the account's counters, not on a helper's return value.

import (
	"net/http"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// POOLATTR-012: an SSE idle-stall is a transport failure — it lands on the
// transport counter and the recorded cooldown cause says so.
func TestStallCoolsAccountAsTransport(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		IdleTimeout: 150 * time.Millisecond,
	})
	ph := testutil.TestPH
	// The stall must happen mid-stream (after the first bytes), otherwise the
	// idle watchdog — which is armed on the body read — never sees it; and the
	// body must be non-empty, since a step without one is a bare status.
	ts.Upstream.Script(ph, testutil.ScriptStep{
		Body:           testutil.FixtureSSEBasic,
		StallMidStream: 2 * time.Second,
		MidStreamSplit: 20,
	})
	body := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"})
	resp, _ := ts.Chat(body, "")
	if resp != nil {
		_ = resp.Body.Close()
	}

	f := ts.Pool.FailuresOf("acc1")
	if f[pool.ClassTransport] == 0 {
		t.Errorf("transport failures = %d, want >= 1 after a stall (5xx = %d)",
			f[pool.ClassTransport], f[pool.ClassUpstream5xx])
	}
	if f[pool.ClassUpstream5xx] != 0 {
		t.Errorf("5xx failures = %d, want 0 — a stall must not advance the 5xx ladder", f[pool.ClassUpstream5xx])
	}
	class, _, since := ts.Pool.CooldownInfoOf("acc1")
	if class != pool.ClassTransport {
		t.Errorf("recorded cooldown class = %q, want %q", class, pool.ClassTransport)
	}
	if since.IsZero() {
		t.Error("cooldown cause recorded without a timestamp")
	}
}

// POOLATTR-013: a 5xx lands on the 5xx ladder, and a stall right after it lands on
// the transport ladder — neither escalates the other (the end-to-end form of
// POOLATTR-001).
func TestFiveXxAndTransportStayOnSeparateLadders(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		IdleTimeout: 150 * time.Millisecond,
	})
	ph := testutil.TestPH
	ts.Upstream.Script(ph, testutil.ScriptStep{StatusCode: http.StatusInternalServerError})
	resp, _ := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if resp != nil {
		_ = resp.Body.Close()
	}
	f := ts.Pool.FailuresOf("acc1")
	if f[pool.ClassUpstream5xx] != 1 {
		t.Errorf("5xx failures = %d, want 1", f[pool.ClassUpstream5xx])
	}
	if f[pool.ClassTransport] != 0 {
		t.Errorf("transport failures = %d, want 0 — a 5xx is not a transport failure", f[pool.ClassTransport])
	}
	class, status, _ := ts.Pool.CooldownInfoOf("acc1")
	if class != pool.ClassUpstream5xx || status != http.StatusInternalServerError {
		t.Errorf("cooldown cause = %s/%d, want upstream-5xx/500", class, status)
	}
}

// POOLATTR-014: an audio call now cools the account too — previously a dead
// connection or a 5xx during TTS/ASR never touched the pool — while a business
// failure (the task's own audit refusal) still takes no pool action.
func TestAudioFailureAttribution(t *testing.T) {
	t.Run("upstream 5xx cools the account", func(t *testing.T) {
		ts := audioServer(t, oneAccount(), nil)
		ts.Upstream.SetAudioHTTPStatus(http.StatusInternalServerError, "boom")
		resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"短句测试。"}`)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("status = 200 body=%s, want an error", body)
		}
		f := ts.Pool.FailuresOf("acc1")
		if f[pool.ClassUpstream5xx] == 0 {
			t.Errorf("5xx failures = %d, want >= 1 after an audio 5xx", f[pool.ClassUpstream5xx])
		}
		if ts.Pool.StateOf("acc1") != pool.StateCooldown {
			t.Errorf("state = %v, want cooldown after an audio 5xx", ts.Pool.StateOf("acc1"))
		}
	})

	t.Run("business failure cools nothing", func(t *testing.T) {
		ts := audioServer(t, oneAccount(), nil)
		ts.Upstream.SetAudioStatusScript("tts", "failed")
		resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"这句会合成失败。"}`)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("status = 200 body=%s, want an error", body)
		}
		if got := ts.Pool.StateOf("acc1"); got != pool.StateAvailable {
			t.Errorf("state = %v, want available — a task-level failure is not the account's fault", got)
		}
		for class, n := range ts.Pool.FailuresOf("acc1") {
			if n != 0 {
				t.Errorf("class %s = %d, want 0", class, n)
			}
		}
	})
}
