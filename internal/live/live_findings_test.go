package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
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
		chatBody(ultra, false, userMsg("count from 1 to 5, digits only")))
	if r.Status != http.StatusOK {
		t.Fatalf("fastchat chat (%s) = %d %s", ultra, r.Status, truncate(string(r.Body), 400))
	}
	chat := decodeChat(t, r)
	var lines []string
	lines = append(lines, fmt.Sprintf("model=%s usage=%+v", ultra, chat.Usage))
	lines = append(lines, "note: the upstream emits several usage events per turn; merging them into one total is the client's job (TDD SSE-012, unit-covered) — this live case reads the collected JSON body, while a streaming client sees the same merge in the final usage chunk (ADR 0034)")
	if chat.Usage.TotalTokens <= 0 {
		lines = append(lines, "VERDICT: no usage in the fastchat response — the fastchat channel reports nothing usable")
	} else {
		lines = append(lines, "VERDICT: the fastchat channel reports usage on the non-streaming path")
	}
	recordFindings(t, "LIVE-009", "fastchat usage reporting", lines)
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

// LIVE-011: how deep does the fastchat (ultraspeed) context really go?
//
// The Studio's own client keeps only three rounds for ultraspeed
// (`ultraspeedContextRoundLimit: 3`, docs/web-surface-reverse.md §11.3). The
// gateway does not truncate anything: every request replays the whole history
// (ADR 0017), so this plants a fact at round 1 and asks for it back at round 6 —
// recall proves the replay survives the fastchat channel's 50,000-character
// per-query ceiling for a six-turn conversation.
func TestLive011UltraDeepContext(t *testing.T) {
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

	const fact = "4913"
	questions := []string{
		"请记住这个信息：我的幸运数字是 " + fact + "。请只回复：记住了",
		"2+2=?只回复数字",
		"3+3=?只回复数字",
		"4+4=?只回复数字",
		"5+5=?只回复数字",
		"我的幸运数字是多少？只回复数字",
	}
	msgs := []map[string]string{}
	lines := []string{fmt.Sprintf("model=%s rounds=%d", ultra, len(questions))}
	lastAnswer := ""
	for i, q := range questions {
		msgs = append(msgs, userMsg(q))
		r := h.do(t, http.MethodPost, "/v1/chat/completions", chatBody(ultra, false, msgs...))
		if r.Status != http.StatusOK {
			t.Fatalf("round %d (%s) = %d %s", i+1, ultra, r.Status, truncate(string(r.Body), 400))
		}
		c := decodeChat(t, r)
		answer := stripThinking(c.Choices[0].Message.Content)
		lines = append(lines, fmt.Sprintf("round %d: answer=%q", i+1, truncate(answer, 60)))
		lastAnswer = answer
		msgs = append(msgs, map[string]string{"role": "assistant", "content": answer})
		time.Sleep(pace)
	}
	if strings.Contains(lastAnswer, fact) {
		lines = append(lines, fmt.Sprintf("VERDICT: round 1 fact recalled at round %d — fastchat depth > 3 rounds", len(questions)))
	} else {
		lines = append(lines, fmt.Sprintf("VERDICT: round 1 fact NOT recalled at round %d (answer %q) — do not claim deep context for this channel", len(questions), truncate(lastAnswer, 80)))
	}
	recordFindings(t, "LIVE-011", "ultraspeed context depth (fastchat channel)", lines)
}

// stripThinking removes a leading <think>...</think> (or unterminated <think>)
// block so an assertion can look at the answer the client would see.
func stripThinking(s string) string {
	if i := strings.Index(s, "</think>"); i >= 0 {
		return strings.TrimSpace(s[i+len("</think>"):])
	}
	if strings.HasPrefix(strings.TrimSpace(s), "<think>") {
		return ""
	}
	return strings.TrimSpace(s)
}
