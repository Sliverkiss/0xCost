package live

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

// newLiveHarness loads the operator account, copies the accounts file (with
// an optional mutation) into a temp dir, and wires the full stack against it.
func newLiveHarness(t *testing.T, mutate func(acct map[string]any), otpAuto bool) *harness {
	t.Helper()
	acct, raw := loadOperatorAccount(t)
	path, copied := copyAccountsFile(t, raw, mutate)
	h := newHarness(t, path, otpAuto)
	h.acct = acct
	if copied.ID != "" {
		h.acct.ID = copied.ID
	}
	return h
}

// healthzAccounts reads the account counters from /healthz.
func (h *harness) healthzAccounts(t *testing.T) map[string]int {
	t.Helper()
	r := h.do(t, http.MethodGet, "/healthz", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("/healthz = %d %s", r.Status, truncate(string(r.Body), 200))
	}
	var out struct {
		Status   string         `json:"status"`
		Accounts map[string]int `json:"accounts"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("decode /healthz: %v (%s)", err, truncate(string(r.Body), 200))
	}
	return out.Accounts
}

// pendingChallenges lists live OTP challenges through the admin surface.
func (h *harness) pendingChallenges(t *testing.T) []struct {
	ID       string `json:"id"`
	Method   string `json:"method"`
	Notify   string `json:"notify"`
	Account  string `json:"account"`
	ExpiresA int64  `json:"expires_at"`
} {
	t.Helper()
	r := h.do(t, http.MethodGet, "/v1/admin/otp/pending", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("/v1/admin/otp/pending = %d %s", r.Status, truncate(string(r.Body), 200))
	}
	var out struct {
		Pending []struct {
			ID       string `json:"id"`
			Method   string `json:"method"`
			Notify   string `json:"notify"`
			Account  string `json:"account"`
			ExpiresA int64  `json:"expires_at"`
		} `json:"pending"`
	}
	_ = json.Unmarshal(r.Body, &out)
	return out.Pending
}

// errEnvelope is the gateway's error body.
type errEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeErr(t *testing.T, r httpResp) errEnvelope {
	t.Helper()
	var e errEnvelope
	_ = json.Unmarshal(r.Body, &e)
	return e
}

// storedAccount rereads the accounts copy from disk.
func (h *harness) storedAccountOnDisk(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatalf("read accounts copy: %v", err)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse accounts copy: %v", err)
	}
	if len(doc.Accounts) == 0 {
		t.Fatal("accounts copy has no entries")
	}
	return doc.Accounts[0]
}

// LIVE-001: cold-start login. An account with no cached session must obtain a
// session through the real login chain. A new-device OTP demand is a human
// gate, so the test needs MIMO2API_LIVE_OTP_CODE; without it the case skips
// with an explicit reason (TDD §6.1 allows env-gated skips) — no test may
// auto-solve a challenge.
func TestLive001ColdStartLogin(t *testing.T) {
	liveGate(t)
	code := os.Getenv("MIMO2API_LIVE_OTP_CODE")
	// A true cold start: no cached session AND no stored identity, so the
	// password path (SDD §2.2) is what runs. Renewal-from-passToken is
	// LIVE-006's contract. Without an operator-supplied code the harness runs
	// with OTP auto-start off, so the run cannot dispatch an SMS.
	h := newLiveHarness(t, func(acct map[string]any) {
		delete(acct, "session")
		delete(acct, "state")
		delete(acct, "user_id")
		delete(acct, "pass_token")
	}, code != "")

	r := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", false, userMsg("reply with exactly: pong")))
	switch r.Status {
	case http.StatusOK:
		t.Log("cold login completed without a challenge (trusted device)")
	default:
		e := decodeErr(t, r)
		demand := strings.Contains(strings.ToLower(e.Error.Message), "otp") ||
			strings.Contains(strings.ToLower(e.Error.Message), "verification")
		if !demand {
			t.Fatalf("first request = %d %s", r.Status, truncate(string(r.Body), 400))
		}
		if code == "" {
			// Auto-start is off: the demand must still be reported as an
			// operator action (never a generic upstream error) and the
			// account must leave the rotation.
			if !strings.Contains(strings.ToLower(e.Error.Message), "otp") {
				t.Errorf("error message = %q, want it to name the OTP demand", e.Error.Message)
			}
			if counts := h.healthzAccounts(t); counts["available"] != 0 {
				t.Errorf("/healthz = %v, want the account out of rotation while it needs a human", counts)
			}
			t.Skip("cold login reaches the new-device verification demand; set MIMO2API_LIVE_OTP_CODE (from /v1/admin/otp/pending) to complete the full login")
		}
		pending := h.pendingChallenges(t)
		if len(pending) == 0 {
			t.Fatalf("OTP demanded (%s) but no challenge registered", e.Error.Message)
		}
		t.Logf("challenge %s method=%s notify=%q account=%s expires_at=%d",
			pending[0].ID, pending[0].Method, pending[0].Notify, pending[0].Account, pending[0].ExpiresA)
		if code == "" {
			t.Skip("cold login demands a new-device OTP; set MIMO2API_LIVE_OTP_CODE (from /v1/admin/otp/pending) to complete it")
		}
		// The notify field must name the masked destination — an empty value
		// was a real bug (the masked phone lives in the trigger answer).
		if pending[0].Notify == "" {
			t.Error("pending challenge has no masked destination (notify is empty)")
		}
		sub := h.do(t, http.MethodPost, "/v1/admin/otp/submit",
			map[string]string{"id": pending[0].ID, "code": code})
		if sub.Status != http.StatusOK {
			t.Fatalf("otp submit = %d %s", sub.Status, truncate(string(sub.Body), 400))
		}
		t.Logf("otp submit: %s", truncate(string(sub.Body), 300))
		paceWait()
		again := h.do(t, http.MethodPost, "/v1/chat/completions",
			chatBody("mimo-v2.6-pro", false, userMsg("reply with exactly: pong")))
		if again.Status != http.StatusOK {
			t.Fatalf("chat after OTP submit = %d %s", again.Status, truncate(string(again.Body), 400))
		}
	}

	// Persistence: the copy must now carry the identity and the session.
	acct := h.storedAccountOnDisk(t)
	for _, field := range []string{"user_id", "pass_token", "session"} {
		if v, ok := acct[field]; !ok || v == nil || v == "" {
			t.Errorf("accounts file field %q not populated after login", field)
		}
	}
	sess, _ := acct["session"].(map[string]any)
	if sess == nil || sess["cookie"] == "" {
		t.Fatal("persisted session has no cookie")
	}
	if sess["ph"] == "" {
		t.Error("persisted session has no ph")
	}
	counts := h.healthzAccounts(t)
	if counts["available"] == 0 {
		t.Errorf("/healthz after login: %v, want an available account", counts)
	}
	// The minted session must actually serve a completion.
	paceWait()
	ok := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", false, userMsg("say: ok")))
	if ok.Status != http.StatusOK {
		t.Fatalf("chat on the minted session = %d %s", ok.Status, truncate(string(ok.Body), 300))
	}
}

// LIVE-002: the model catalog is served from the static list and contains the
// configured default plus the ultraspeed model (TDD API-005).
func TestLive002ModelCatalog(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, func(acct map[string]any) {}, true)
	r := h.do(t, http.MethodGet, "/v1/models", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("/v1/models = %d %s", r.Status, truncate(string(r.Body), 300))
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	ids := map[string]bool{}
	ultra := ""
	for _, m := range out.Data {
		ids[m.ID] = true
		if adapter.IsFastchatModel(m.ID) {
			ultra = m.ID
		}
	}
	if !ids["mimo-v2.6-pro"] {
		t.Errorf("catalog lacks mimo-v2.6-pro: %v", keys(ids))
	}
	if ultra == "" {
		t.Errorf("catalog has no ultraspeed model: %v", keys(ids))
	} else {
		t.Logf("ultraspeed model id = %s", ultra)
	}
	// /v1/models is metadata-only: no upstream call, so it works without a
	// session and must not have disturbed the pool.
	t.Logf("healthz: %v", h.healthzAccounts(t))
}

// LIVE-003: non-streaming echo through the real upstream.
func TestLive003NonStreamingEcho(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, func(acct map[string]any) {}, true)
	r := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", false, userMsg("reply with exactly: pong")))
	if r.Status != http.StatusOK {
		t.Fatalf("chat = %d %s", r.Status, truncate(string(r.Body), 400))
	}
	c := decodeChat(t, r)
	if len(c.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(c.Choices))
	}
	ch := c.Choices[0]
	if ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", ch.FinishReason)
	}
	if !strings.Contains(strings.ToLower(ch.Message.Content), "pong") {
		t.Errorf("content = %q, want it to contain pong", truncate(ch.Message.Content, 200))
	}
	if c.Usage.TotalTokens <= 0 {
		t.Errorf("usage.total_tokens = %d, want > 0 (usage: %+v)", c.Usage.TotalTokens, c.Usage)
	}
	t.Logf("id=%s model=%s usage=%+v content=%q", c.ID, c.Model, c.Usage, truncate(ch.Message.Content, 80))
}

// LIVE-004: streaming frame contract (TDD SSE-001..005 analogues live).
func TestLive004Streaming(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, func(acct map[string]any) {}, true)
	r := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", true, userMsg("count from 1 to 5, digits only")))
	if r.Status != http.StatusOK {
		t.Fatalf("stream chat = %d %s", r.Status, truncate(string(r.Body), 400))
	}
	frames, done := parseSSE(t, string(r.Body))
	if !done {
		t.Error("stream ended without data: [DONE]")
	}
	if len(frames) < 2 {
		t.Fatalf("frames = %d, want >= 2", len(frames))
	}
	// First frame: role-only delta.
	first := frames[0]
	if len(first.Choices) != 1 || first.Choices[0].Delta.Role != "assistant" || first.Choices[0].Delta.Content != "" {
		t.Errorf("first frame = %+v, want a role-only assistant delta", first.Choices)
	}
	deltas := 0
	for _, f := range frames {
		for _, c := range f.Choices {
			if c.Delta.Content != "" {
				deltas++
			}
		}
	}
	if deltas < 2 {
		t.Errorf("content deltas = %d, want >= 2", deltas)
	}
	// Documented order (SDD §5.4): content deltas → frame with
	// finish_reason:"stop" → usage chunk with choices:[] → [DONE]. The last
	// data frame is therefore the usage chunk, not the finish frame.
	finishIdx, usageIdx := -1, -1
	usageFrames := 0
	for i, f := range frames {
		for _, c := range f.Choices {
			if c.FinishReason == "stop" && finishIdx < 0 {
				finishIdx = i
			}
		}
		if f.Usage != nil {
			usageFrames++
			if usageIdx < 0 {
				usageIdx = i
			}
			if len(f.Choices) != 0 {
				t.Errorf("usage frame carries %d choices, want 0", len(f.Choices))
			}
		}
	}
	if finishIdx < 0 {
		t.Errorf("no frame carried finish_reason stop: %+v", frames)
	}
	if usageFrames == 0 {
		t.Error("no usage frame in the stream")
	}
	if finishIdx >= 0 && usageIdx >= 0 && finishIdx > usageIdx {
		t.Errorf("finish frame at %d comes after the usage frame at %d, want the documented order", finishIdx, usageIdx)
	}
	if frames[len(frames)-1].Usage == nil {
		t.Logf("note: last data frame is not the usage chunk (%+v)", frames[len(frames)-1].Choices)
	}
	t.Logf("frames=%d content_deltas=%d usage_frames=%d content=%q",
		len(frames), deltas, usageFrames, truncate(streamContent(frames), 120))
}

// LIVE-005: stickiness. Turn 2 extends the history, must hit the session
// store (delta path), and must answer with the fact from turn 1.
//
// Deviation from TDD §3.12: the upstream call count is not externally
// observable (the client owns its transport), so the store-hit counter plus
// the answered context is what this test pins. A logged single msgId
// assertion is left to the operator reading MIMO2API_LOG_LEVEL=debug output.
func TestLive005StickyTwoTurn(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, func(acct map[string]any) {}, true)

	turn1 := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", false, userMsg("my favorite number is 42, remember it")))
	if turn1.Status != http.StatusOK {
		t.Fatalf("turn 1 = %d %s", turn1.Status, truncate(string(turn1.Body), 400))
	}
	c1 := decodeChat(t, turn1)
	if len(c1.Choices) != 1 {
		t.Fatalf("turn 1 choices = %d", len(c1.Choices))
	}
	t.Logf("turn 1: id=%s answer=%q", c1.ID, truncate(c1.Choices[0].Message.Content, 120))

	paceWait()
	hitsBefore := h.sessions.Lookups()
	turn2 := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", true,
			userMsg("my favorite number is 42, remember it"),
			assistantMsg(c1.Choices[0].Message.Content),
			userMsg("what is my favorite number?")))
	if turn2.Status != http.StatusOK {
		t.Fatalf("turn 2 = %d %s", turn2.Status, truncate(string(turn2.Body), 400))
	}
	frames, done := parseSSE(t, string(turn2.Body))
	if !done {
		t.Error("turn 2 stream ended without data: [DONE]")
	}
	answer := streamContent(frames)
	if !strings.Contains(answer, "42") {
		t.Errorf("turn 2 answer = %q, want it to reference 42", truncate(answer, 200))
	}
	if got := h.sessions.Lookups(); got <= hitsBefore {
		t.Errorf("session store lookups = %d (was %d): turn 2 did not consult stickiness", got, hitsBefore)
	}
	if h.sessions.Len() == 0 {
		t.Error("session store is empty after two turns — no sticky entry was saved")
	}
	t.Logf("turn 2: deltas=%d answer=%q store_len=%d lookups=%d",
		len(frames), truncate(answer, 120), h.sessions.Len(), h.sessions.Lookups())
}

// LIVE-006: transparent refresh. With the cached session gone but the
// passToken intact, one request must re-mint the session by STS renewal —
// no password login (that would demand an OTP), no user-visible failure.
func TestLive006TransparentRefresh(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, func(acct map[string]any) {
		delete(acct, "session")
	}, true)
	if h.acct.PassToken == "" || h.acct.UserID == "" {
		t.Skip("operator account has no pass_token/user_id — nothing to renew from")
	}

	r := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", false, userMsg("say: refreshed")))
	if r.Status != http.StatusOK {
		t.Fatalf("request with a cleared session = %d %s", r.Status, truncate(string(r.Body), 400))
	}
	acct := h.storedAccountOnDisk(t)
	sess, _ := acct["session"].(map[string]any)
	if sess == nil || sess["cookie"] == "" {
		t.Fatal("renewed session was not persisted back to the accounts file")
	}
	if !strings.Contains(fmt.Sprint(sess["cookie"]), "serviceToken=") {
		t.Errorf("renewed cookie lacks a service token: %s", truncate(fmt.Sprint(sess["cookie"]), 160))
	}
	if counts := h.healthzAccounts(t); counts["available"] == 0 {
		t.Errorf("/healthz = %v, want the account available after renewal", counts)
	}
	t.Logf("renewed session persisted (cookie len %d, ph set: %v)", len(fmt.Sprint(sess["cookie"])), sess["ph"] != "")
}

// keys is a tiny helper for readable failure messages.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
