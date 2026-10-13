package api_test

// End-to-end failure attribution (owner ruling 2026-10: no cooldown, no
// backoff — failures pass through).
//
// These drive the real API stack and assert the pass-through contract where
// it matters: the upstream's own status reaches the client, and the account's
// availability is untouched by transport/5xx/429 failures. Only risk control
// (451/461) and dead credentials still disable an account.

import (
	"net/http"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// POOLATTR-012: an SSE idle-stall is passed through as an error and takes no
// pool action — the account stays available.
//
// The window that fires here is the **turn's silence window**, which is the
// lease window (ADR 0036) — the pipeline passes it to the client, so setting
// LeaseTTL is what bounds a turn; the client's own IdleTimeout option now only
// covers calls the chat pipeline does not drive (TTS/ASR).
func TestStallPassesThroughAndKeepsAccountAvailable(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		LeaseTTL: 150 * time.Millisecond,
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
	body := testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"})
	resp, _ := ts.Chat(body, "")
	if resp != nil {
		if resp.StatusCode == http.StatusOK {
			t.Errorf("status = 200: a stall must surface as an error, not a silent success")
		}
		_ = resp.Body.Close()
	}
	if st := ts.Pool.StateOf("acc1"); st != pool.StateAvailable {
		t.Errorf("state = %v after a stall, want available (pass-through, no cooldown)", st)
	}
}

// POOLATTR-013: a 5xx reaches the client as the upstream's own status and the
// account stays available — no ladder, no state change.
func TestFiveXxPassesThroughAndKeepsAccountAvailable(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ph := testutil.TestPH
	ts.Upstream.Script(ph, testutil.ScriptStep{StatusCode: http.StatusInternalServerError})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 passed through verbatim", resp.StatusCode)
	}
	if st := ts.Pool.StateOf("acc1"); st != pool.StateAvailable {
		t.Errorf("state = %v after a 5xx, want available (pass-through, no cooldown)", st)
	}
}

// POOLATTR-014: an audio failure equally passes through — the upstream's 5xx
// reaches the caller and the account stays available — while a business
// failure (the task's own audit refusal) also takes no pool action.
func TestAudioFailurePassesThrough(t *testing.T) {
	t.Run("upstream 5xx keeps the account available", func(t *testing.T) {
		ts := audioServer(t, oneAccount(), nil)
		ts.Upstream.SetAudioHTTPStatus(http.StatusInternalServerError, "boom")
		resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"短句测试。"}`)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("status = 200 body=%s, want an error", body)
		}
		if ts.Pool.StateOf("acc1") != pool.StateAvailable {
			t.Errorf("state = %v, want available after an audio 5xx (pass-through)", ts.Pool.StateOf("acc1"))
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
	})
}
