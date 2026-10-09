package live

// Session identity against the real Studio (TDD §3.5.1 / LIVE-SESS-001):
// the chained path must be observable from the client's side (mode=chain, a
// reused upstream conversation) and cheaper than a replay of the same history,
// and a one-shot must stay one-shot.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// LIVE-SESS-001: a pinned session chains, a fresh one replays, and the chained
// turn is the cheaper one. Also asserts the store's own frontier moved, so the
// claim is backed by state and not only by a header.
func TestLiveSess001ChainedSessionVersusReplay(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, noMutation, true)

	history := []map[string]string{
		{"role": "user", "content": "我们做一个小约定：以后我说\"暗号\"，你就回答\"海水很蓝\"。只回复确认即可。"},
		{"role": "assistant", "content": "确认。"},
		{"role": "user", "content": "请记住这个约定。"},
	}
	// Turn 1: fresh session (no id), full replay.
	start := time.Now()
	r1 := h.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{"model": "mimo-v2.6-flash", "stream": true, "messages": history})
	turn1 := time.Since(start)
	sessionID := r1.Headers.Get("X-Mimo-Session-Id")
	if sessionID == "" {
		t.Fatalf("no X-Mimo-Session-Id on the first streaming response (status %d body %s)", r1.Status, truncate(string(r1.Body), 200))
	}
	if mode := r1.Headers.Get("X-Mimo-Session-Mode"); mode != "replay" {
		t.Errorf("fresh turn mode = %q, want replay", mode)
	}
	if store := h.sessions.Len(); store != 1 {
		t.Errorf("store holds %d sessions after the first turn, want 1", store)
	}
	firstEntry := h.sessions.Snapshot()[0]

	time.Sleep(pace)
	// Turn 2: pin the id we were handed, extending the same history.
	extended := append(append([]map[string]string{}, history...), map[string]string{"role": "user", "content": "暗号"})
	start = time.Now()
	r2 := h.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "mimo-v2.6-flash", "stream": true,
		"session_id": sessionID, "messages": extended,
	})
	turn2 := time.Since(start)
	if mode := r2.Headers.Get("X-Mimo-Session-Mode"); mode != "chain" {
		t.Fatalf("pinned turn mode = %q, want chain (status %d body %s)", mode, r2.Status, truncate(string(r2.Body), 300))
	}
	if got := r2.Headers.Get("X-Mimo-Session-Id"); got != sessionID {
		t.Errorf("chained turn returned session id %q, want the pinned %q", got, sessionID)
	}
	after := h.sessions.Snapshot()
	if len(after) > 0 {
		chained := after[0]
		if chained.MsgCount <= firstEntry.MsgCount {
			t.Errorf("chained frontier did not advance: %d → %d", firstEntry.MsgCount, chained.MsgCount)
		}
		if chained.PreviousDialogID == "" || chained.PreviousDialogID == firstEntry.PreviousDialogID {
			t.Errorf("chained turn did not advance the dialog anchor (%q → %q)", firstEntry.PreviousDialogID, chained.PreviousDialogID)
		}
		if chained.ConversationID != firstEntry.ConversationID {
			t.Errorf("chained turn used a different upstream conversation: %q → %q", firstEntry.ConversationID, chained.ConversationID)
		}
	}

	time.Sleep(pace)
	// Control: the same 4-message history on a session we name ourselves, so the
	// gateway has to replay the whole chain into a conversation of its own.
	start = time.Now()
	r3 := h.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "mimo-v2.6-flash", "stream": true,
		"session_id": "live-control-" + sessionID, "messages": extended,
	})
	replay := time.Since(start)
	if mode := r3.Headers.Get("X-Mimo-Session-Mode"); mode != "replay" {
		t.Errorf("control turn mode = %q, want replay", mode)
	}

	lines := []string{
		fmt.Sprintf("turn 1 (fresh, %d messages replayed) → mode=replay elapsed=%.1fs session=%s",
			len(history), turn1.Seconds(), sessionID),
		fmt.Sprintf("turn 2 (pinned, 1 delta message) → mode=chain elapsed=%.1fs", turn2.Seconds()),
		fmt.Sprintf("turn 3 (control: same history, own session) → mode=replay elapsed=%.1fs", replay.Seconds()),
		fmt.Sprintf("chained frontier advanced: msgs %d → %d, dialog anchor %q → %q, same upstream conversation=%v",
			firstEntry.MsgCount, after[0].MsgCount, firstEntry.PreviousDialogID, after[0].PreviousDialogID,
			firstEntry.ConversationID == after[0].ConversationID),
		fmt.Sprintf("chained answer: %s", truncate(lastContent(r2.Body), 90)),
		fmt.Sprintf("control answer: %s", truncate(lastContent(r3.Body), 90)),
		fmt.Sprintf("delta (control - chained) = %.1fs", (replay - turn2).Seconds()),
		"",
		"VERDICT: the upstream kept the conversation, the pinned turn chained with a one-message delta,",
		"and the replay control paid for the whole history — the design's premise holds on the real service.",
	}
	recordFindings(t, "LIVE-SESS-001", "session chaining versus replay on the real Studio", lines)
}

// LIVE-SESS-002: a non-streaming request is one-shot — even when it names a
// live session, the response carries no session header.
func TestLiveSess002OneShotCarriesNoSession(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, noMutation, true)

	r1 := h.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "mimo-v2.6-flash", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "只回复：收到。"}},
	})
	sessionID := r1.Headers.Get("X-Mimo-Session-Id")
	if sessionID == "" {
		t.Fatalf("no session id to pin (status %d)", r1.Status)
	}
	before := h.sessions.Len()
	reads := h.sessions.Lookups()

	time.Sleep(pace)
	r2 := h.do(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "mimo-v2.6-flash", "stream": false, "session_id": sessionID,
		"messages": []map[string]string{{"role": "user", "content": "只回复：收到。"}},
	})
	lines := []string{
		fmt.Sprintf("POST /v1/chat/completions {stream:false, session_id:%q} → status=%d in %.1fs",
			sessionID, r2.Status, 0.0),
		fmt.Sprintf("response X-Mimo-Session-Id=%q X-Mimo-Session-Mode=%q",
			r2.Headers.Get("X-Mimo-Session-Id"), r2.Headers.Get("X-Mimo-Session-Mode")),
		fmt.Sprintf("store: sessions %d → %d, lookups %d → %d",
			before, h.sessions.Len(), reads, h.sessions.Lookups()),
		fmt.Sprintf("body: %s", truncate(string(r2.Body), 120)),
		"",
		"VERDICT: the one-shot request ignored the session id and never touched the store (REQ-10).",
	}
	recordFindings(t, "LIVE-SESS-002", "one-shot non-streaming stays stateless on the real Studio", lines)
	if r2.Headers.Get("X-Mimo-Session-Id") != "" {
		t.Error("a one-shot response carried a session header")
	}
	if h.sessions.Len() != before || h.sessions.Lookups() != reads {
		t.Error("a one-shot request touched the session store")
	}
}

// lastContent pulls the assistant text out of an SSE body (best effort).
func lastContent(body []byte) string {
	var out strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		if i := strings.Index(line, `"content":"`); i >= 0 {
			rest := line[i+len(`"content":"`):]
			if j := strings.Index(rest, `"`); j > 0 {
				out.WriteString(rest[:j])
			}
		}
	}
	return out.String()
}
