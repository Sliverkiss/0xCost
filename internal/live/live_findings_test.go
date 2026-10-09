package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/session"
)

// noMutation is the identity mutation for harnesses that use the account as-is.
func noMutation(map[string]any) {}

// recordFindings prints a findings block and, when MIMO2API_LIVE_RECORD names
// a file, appends it there. Return values are never invented: everything here
// comes from an observed response.
func recordFindings(t *testing.T, id, title string, lines []string) {
	t.Helper()
	block := fmt.Sprintf("### %s — %s (%s)\n\n```\n%s\n```\n", id, title, time.Now().Format("2006-01-02 15:04 MST"), strings.Join(lines, "\n"))
	t.Logf("\n%s", block)
	if path := os.Getenv("MIMO2API_LIVE_RECORD"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Errorf("append findings to %s: %v", path, err)
			return
		}
		defer f.Close()
		if _, err := f.WriteString("\n" + block); err != nil {
			t.Errorf("write findings: %v", err)
		}
	}
}

// LIVE-007 (opt-in via MIMO2API_LIVE_HAMMER=1): does the upstream 429 carry a
// structured deadline? Drives the raw client so the upstream status, body and
// headers are visible (the API layer deliberately hides Retry-After).
func TestLive007RateLimitSemantics(t *testing.T) {
	liveGate(t)
	if os.Getenv("MIMO2API_LIVE_HAMMER") != "1" {
		t.Skip("opt-in: set MIMO2API_LIVE_HAMMER=1 to burn the account's short-term rate budget")
	}
	h := newLiveHarness(t, noMutation, true)
	acct := h.storedSession(t)
	if acct.Session == nil || acct.Session.Cookie == "" {
		t.Skip("no cached session to hammer with")
	}
	ctx := context.Background()
	const burst = 30
	var lines []string
	lines = append(lines, fmt.Sprintf("burst=%d requests, no pacing, model=mimo-v2.6-pro", burst))
	deadlineField := ""
	for i := 1; i <= burst; i++ {
		_, err := h.client.Chat(ctx, acct.Session.Cookie, acct.Session.PH, mimo.ChatRequest{
			Query: "hi",
			Model: "mimo-v2.6-pro",
		})
		if err == nil {
			continue
		}
		var ue *mimo.UpstreamError
		if errors.As(err, &ue) {
			lines = append(lines, fmt.Sprintf("request %d -> %d", i, ue.StatusCode))
			lines = append(lines, fmt.Sprintf("body: %s", truncate(strings.TrimSpace(ue.Body), 500)))
			for _, hk := range []string{"Retry-After", "X-RateLimit-Reset", "X-RateLimit-Remaining", "X-RateLimit-Limit"} {
				if v := ue.Headers.Get(hk); v != "" {
					lines = append(lines, fmt.Sprintf("header %s: %s", hk, v))
				}
			}
			all := strings.ToLower(ue.Body) + " " + strings.ToLower(strings.Join(ue.Headers.Values("Retry-After"), " "))
			for _, f := range []string{"retry_after", "retryafter", "reset_at", "deadline", "cooldown", "expire"} {
				if strings.Contains(all, f) {
					deadlineField = f
					break
				}
			}
			lines = append(lines, fmt.Sprintf("structured deadline field: %q", deadlineField))
			if ue.StatusCode != http.StatusTooManyRequests {
				lines = append(lines, "NOTE: first non-200 was not a 429 — the burst hit a different guard")
			}
			break
		}
		lines = append(lines, fmt.Sprintf("request %d -> transport/other error: %v", i, err))
		break
	}
	if len(lines) == 1 {
		lines = append(lines, fmt.Sprintf("no non-200 within %d unpaced requests — no 429 observed", burst))
	}
	recordFindings(t, "LIVE-007", "upstream 429 semantics", lines)
}

// LIVE-008 (needs codes): captcha probability of plain-form password logins
// from this egress IP. Runs MIMO2API_LIVE_LOGINS cold logins (default 5) and
// counts captcha / OTP / success outcomes. Requires one code per OTP demand
// in MIMO2API_LIVE_OTP_CODES (comma separated); without codes the run would
// dispatch SMS the operator cannot complete, so it skips.
func TestLive008CaptchaProbability(t *testing.T) {
	liveGate(t)
	codes := splitCodes(os.Getenv("MIMO2API_LIVE_OTP_CODES"))
	n := 5
	if v := os.Getenv("MIMO2API_LIVE_LOGINS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	acct, _ := loadOperatorAccount(t)
	if len(codes) < n {
		t.Skipf("needs %d one-time codes in MIMO2API_LIVE_OTP_CODES (got %d); each cold login sends an SMS", n, len(codes))
	}
	h := newLiveHarness(t, noMutation, true)
	ctx := context.Background()

	var captcha, otp, success, other int
	var lines []string
	for i := 0; i < n; i++ {
		// Login builds its own cookie jar per call, so each attempt presents
		// the passport a cold client (the new-device gate keys on that).
		c := h.client
		sess, err := c.Login(ctx, acct.Phone, acct.Password)
		switch {
		case err == nil:
			success++
			lines = append(lines, fmt.Sprintf("login %d: success (userId=%s)", i+1, sess.UserID))
		default:
			var pending *mimo.ErrOTPPending
			if errors.As(err, &pending) {
				otp++
				sub, serr := c.SubmitOTP(ctx, pending.Challenge, codes[len(codes)-1-i])
				if serr != nil {
					lines = append(lines, fmt.Sprintf("login %d: OTP demand, submit failed: %v", i+1, serr))
					other++
					continue
				}
				lines = append(lines, fmt.Sprintf("login %d: OTP demand, completed (userId=%s)", i+1, sub.UserID))
				success++
				continue
			}
			var act *mimo.ErrAccountAction
			if errors.As(err, &act) && strings.Contains(strings.ToLower(act.Reason), "captcha") {
				captcha++
				lines = append(lines, fmt.Sprintf("login %d: CAPTCHA (%s)", i+1, truncate(act.Reason, 160)))
				continue
			}
			other++
			lines = append(lines, fmt.Sprintf("login %d: other error: %v", i+1, err))
		}
		time.Sleep(pace)
	}
	lines = append(lines, fmt.Sprintf("summary: success=%d otp=%d captcha=%d other=%d over %d cold logins", success, otp, captcha, other, n))
	if captcha >= 2 {
		lines = append(lines, "VERDICT: >=2 captcha in the run — the EUI-encrypted login variant is required before shipping login (SDD §2.2, Appendix B Q2)")
	} else {
		lines = append(lines, "VERDICT: plain form posts stay below the captcha threshold observed here")
	}
	recordFindings(t, "LIVE-008", "captcha probability for plain-form logins", lines)
}

// LIVE-009: fastchat (ultraspeed) streaming — are repeated usage events
// last-wins? Captures every usage frame and compares it with the terminal
// frame's totals.
func TestLive009FastchatUsageMerge(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, noMutation, true)
	catalog := h.do(t, http.MethodGet, "/v1/models", nil)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(catalog.Body, &list); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	ultra := ""
	for _, m := range list.Data {
		if adapter.IsFastchatModel(m.ID) {
			ultra = m.ID
			break
		}
	}
	if ultra == "" {
		t.Skip("catalog has no ultraspeed model")
	}
	r := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody(ultra, true, userMsg("count from 1 to 5, digits only")))
	if r.Status != http.StatusOK {
		t.Fatalf("fastchat stream (%s) = %d %s", ultra, r.Status, truncate(string(r.Body), 400))
	}
	frames, done := parseSSE(t, string(r.Body))
	var usages []struct {
		prompt, completion, total int
	}
	for _, f := range frames {
		if f.Usage != nil {
			usages = append(usages, struct{ prompt, completion, total int }{f.Usage.PromptTokens, f.Usage.CompletionTokens, f.Usage.TotalTokens})
		}
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("model=%s frames=%d done=%v usage_events=%d", ultra, len(frames), done, len(usages)))
	for i, u := range usages {
		lines = append(lines, fmt.Sprintf("usage[%d] prompt=%d completion=%d total=%d", i, u.prompt, u.completion, u.total))
	}
	if len(usages) == 0 {
		lines = append(lines, "VERDICT: no usage event in the fastchat stream — SSE-012 analogue cannot be validated")
	} else {
		monotonic := true
		for i := 1; i < len(usages); i++ {
			if usages[i].total < usages[i-1].total {
				monotonic = false
			}
		}
		lines = append(lines, fmt.Sprintf("last total >= all earlier totals: %v", monotonic))
		if !monotonic {
			lines = append(lines, "VERDICT: totals DECREASE across usage events — last-wins would under-report; document + fix the merge rule")
		} else if len(usages) > 1 {
			lines = append(lines, "VERDICT: repeated usage events are monotonic — last-wins is safe")
		} else {
			lines = append(lines, "VERDICT: a single usage event — no merge question for this model/run")
		}
	}
	if !done {
		lines = append(lines, "NOTE: stream ended without data: [DONE]")
	}
	recordFindings(t, "LIVE-009", "fastchat repeated usage events", lines)
}

// phase1State is the saved stickiness snapshot for LIVE-010 phase 2.
type phase1State struct {
	SavedAt    int64           `json:"saved_at"`
	AccountID  string          `json:"account_id"`
	Question   string          `json:"question"`
	Answer     string          `json:"answer"`
	Fact       string          `json:"fact"`
	Entries    []session.Entry `json:"entries"`
	AccountRef string          `json:"account_ref"`
}

func phase1Path() string {
	if p := os.Getenv("MIMO2API_LIVE_PHASE1_STATE"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "mimo2api-live-phase1.json")
}

// LIVE-010: does a sticky conversation survive upstream GC without an
// explicit /conversation/save? Two-phase by construction (24h gap).
//
//	phase 1 (default): run a turn, snapshot the session store, print the resume
//	                   instruction.
//	phase 2: MIMO2API_LIVE_PHASE=2 — restore the snapshot, send a delta turn
//	         with the same history, and check whether the upstream still has
//	         the context.
func TestLive010ConversationSurvivesGC(t *testing.T) {
	liveGate(t)
	const fact = "my lucky token is ZQ7"
	phase := os.Getenv("MIMO2API_LIVE_PHASE")
	if phase == "" || phase == "1" {
		h := newLiveHarness(t, noMutation, true)
		r := h.do(t, http.MethodPost, "/v1/chat/completions",
			chatBody("mimo-v2.6-pro", false, userMsg(fact+", remember it")))
		if r.Status != http.StatusOK {
			t.Fatalf("phase 1 turn = %d %s", r.Status, truncate(string(r.Body), 300))
		}
		c := decodeChat(t, r)
		if len(c.Choices) != 1 {
			t.Fatalf("phase 1 choices = %d", len(c.Choices))
		}
		snap := phase1State{
			SavedAt:    time.Now().Unix(),
			AccountID:  h.acct.ID,
			Question:   fact + ", remember it",
			Answer:     c.Choices[0].Message.Content,
			Fact:       fact,
			Entries:    h.sessions.Snapshot(),
			AccountRef: h.path,
		}
		b, _ := json.MarshalIndent(snap, "", "  ")
		path := phase1Path()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write phase-1 state: %v", err)
		}
		t.Logf("phase 1 saved %d sticky entries to %s; resume with MIMO2API_LIVE_PHASE=2 after >=24h", len(snap.Entries), path)
		return
	}

	// Phase 2.
	raw, err := os.ReadFile(phase1Path())
	if err != nil {
		t.Skipf("no phase-1 state at %s: run the suite once without MIMO2API_LIVE_PHASE=2 first", phase1Path())
	}
	var snap phase1State
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse phase-1 state: %v", err)
	}
	if age := time.Since(time.Unix(snap.SavedAt, 0)); age < 20*time.Hour {
		t.Skipf("phase-1 snapshot is only %s old; the GC question needs >=24h", age.Round(time.Minute))
	}
	h := newLiveHarness(t, noMutation, true)
	h.sessions.Restore(snap.Entries)
	r := h.do(t, http.MethodPost, "/v1/chat/completions",
		chatBody("mimo-v2.6-pro", false,
			userMsg(snap.Question),
			assistantMsg(snap.Answer),
			userMsg("what is my lucky token?")))
	lines := []string{
		fmt.Sprintf("snapshot age: %s, restored entries: %d", time.Since(time.Unix(snap.SavedAt, 0)).Round(time.Minute), len(snap.Entries)),
		fmt.Sprintf("phase 2 status: %d", r.Status),
	}
	if r.Status != http.StatusOK {
		lines = append(lines, fmt.Sprintf("body: %s", truncate(string(r.Body), 300)))
		lines = append(lines, "VERDICT: the delta turn failed after the gap — inspect the upstream error before amending SDD §4.3")
		recordFindings(t, "LIVE-010", "sticky conversation across upstream GC", lines)
		return
	}
	c := decodeChat(t, r)
	answer := c.Choices[0].Message.Content
	lines = append(lines, fmt.Sprintf("answer: %s", truncate(answer, 300)))
	if strings.Contains(answer, "ZQ7") {
		lines = append(lines, "VERDICT: context survived the gap — upstream auto-save suffices, no /conversation/save call needed")
	} else {
		lines = append(lines, "VERDICT: context LOST after the gap — SDD §4.3 must be amended to call POST /open-apis/chat/conversation/save on store")
	}
	recordFindings(t, "LIVE-010", "sticky conversation across upstream GC", lines)
}

// splitCodes parses a comma-separated one-time-code list.
func splitCodes(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
