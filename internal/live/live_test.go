// Package live holds the LIVE-* acceptance suite (TDD §1.3, §3.12, §6.5).
//
// It is gated on MIMO2API_LIVE=1 and never runs implicitly:
//
//	MIMO2API_LIVE=1 go test ./internal/live -v
//
// Properties this suite guarantees:
//   - Sequential and rate-limit aware: no t.Parallel, a paced delay between
//     upstream calls, one pass per run.
//   - Credentials come exclusively from the operator's accounts file
//     (MIMO2API_LIVE_ACCOUNTS_FILE, default ../../data/accounts.json).
//   - The system under test gets a COPY of that file in a temp dir, so a test
//     run never mutates the operator's live session. Tests that need to
//     observe persistence assert against the copy.
//   - Environment-gated skips are expected and allowed (TDD §6.1): a cold
//     login that demands a new-device OTP skips unless the operator supplies
//     MIMO2API_LIVE_OTP_CODE, because no test may auto-solve a challenge.
package live

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/api"
	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// upstreamBase is the real service the live suite talks to (SDD §2.1).
const upstreamBase = "https://aistudio.xiaomimimo.com"

// liveKey is the bearer token the harness configures for its own API surface.
const liveKey = "sk-live-suite"

// pace is the minimum gap between upstream calls — the suite must not look
// like an attack, and rate-limit semantics are only meaningful above a floor.
const pace = 2 * time.Second

// liveGate skips everything unless MIMO2API_LIVE=1.
func liveGate(t *testing.T) {
	t.Helper()
	if os.Getenv("MIMO2API_LIVE") != "1" {
		t.Skip("live tests gated by MIMO2API_LIVE=1")
	}
}

// testAccount is the operator's live account, read from their accounts file.
type testAccount struct {
	ID        string            `json:"id"`
	Phone     string            `json:"phone"`
	Password  string            `json:"password"`
	Enabled   bool              `json:"enabled"`
	UserID    string            `json:"user_id"`
	PassToken string            `json:"pass_token"`
	Session   *accounts.Session `json:"session"`
}

// harness is the full stack under test: real upstream client, real pool, real
// session store, real HTTP surface — only the accounts file is a copy.
type harness struct {
	srv    *httptest.Server
	store  *accounts.Store
	pool   *pool.Pool
	client *mimo.Client
	path   string
	acct   testAccount
}

// accountsPath resolves the operator's accounts file.
func accountsPath() string {
	if p := os.Getenv("MIMO2API_LIVE_ACCOUNTS_FILE"); p != "" {
		return p
	}
	return filepath.Join("..", "..", "data", "accounts.json")
}

// loadOperatorAccount reads the first enabled account from the operator file.
func loadOperatorAccount(t *testing.T) (testAccount, []byte) {
	t.Helper()
	raw, err := os.ReadFile(accountsPath())
	if err != nil {
		t.Fatalf("read operator accounts file: %v", err)
	}
	var doc struct {
		Accounts []testAccount `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse operator accounts file: %v", err)
	}
	for _, a := range doc.Accounts {
		if a.Enabled && a.Phone != "" && a.Password != "" {
			return a, raw
		}
	}
	t.Fatalf("no enabled account with a phone+password in %s", accountsPath())
	return testAccount{}, nil
}

// copyAccountsFile writes raw to a temp copy and returns its path. mutate (may
// be nil) edits the copy's first account before it is written.
func copyAccountsFile(t *testing.T, raw []byte, mutate func(acct map[string]any)) (string, testAccount) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse accounts copy: %v", err)
	}
	list, _ := doc["accounts"].([]any)
	if len(list) == 0 {
		t.Fatal("accounts copy has no entries")
	}
	first, _ := list[0].(map[string]any)
	if mutate != nil {
		mutate(first)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal accounts copy: %v", err)
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write accounts copy: %v", err)
	}
	var acct testAccount
	b, _ := json.Marshal(first)
	if err := json.Unmarshal(b, &acct); err != nil {
		t.Fatalf("decode copied account: %v", err)
	}
	return path, acct
}

// newHarness wires the same stack as cmd/mimo2api (config → store → client →
// pool → session → HTTP) but with an httptest listener. otpAuto mirrors
// MIMO2API_OTP_AUTO: with it off a cold login fails fast on the verification
// demand instead of dispatching an SMS, which keeps the default (no-code) run
// of LIVE-001 free of side effects.
func newHarness(t *testing.T, accountsFilePath string, otpAuto bool) *harness {
	t.Helper()
	// Keep the upstream defaults (UA / sec-ch-ua / timezone) but point the
	// process at the copy; the client base is the real upstream.
	t.Setenv("MIMO2API_ACCOUNTS_FILE", accountsFilePath)
	t.Setenv("MIMO2API_API_KEYS", liveKey)
	t.Setenv("MIMO2API_HOST", "127.0.0.1")
	t.Setenv("MIMO2API_PORT", "0")
	t.Setenv("MIMO2API_LOG_LEVEL", "debug")
	if !otpAuto {
		t.Setenv("MIMO2API_OTP_AUTO", "false")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	store, err := accounts.Load(accountsFilePath)
	if err != nil {
		t.Fatalf("accounts.Load: %v", err)
	}
	client := mimo.New(mimo.Options{
		Base:                    upstreamBase,
		UserAgent:               cfg.UserAgent(),
		SecChUA:                 cfg.SecChUA(),
		Timezone:                cfg.Timezone(),
		IdleTimeout:             cfg.SSEIdleTimeout(),
		DisableOTPAuto:          !otpAuto,
		DisableCrossRegionAgree: !cfg.CrossRegionAgree(),
	})

	var poolAccts []accounts.Account
	for _, a := range store.Accounts() {
		if a.Enabled {
			poolAccts = append(poolAccts, a)
		}
	}
	p := pool.New(poolAccts, pool.Options{
		MaxInflight: cfg.MaxInflight(),
		QueueWait:   cfg.QueueWait(),
	})
	srv := api.New(api.Options{
		APIKeys:      cfg.APIKeys(),
		Pool:         p,
		Accounts:     store,
		Client:       client,
		DefaultModel: cfg.DefaultModel(),
		ReplayCap:    cfg.ReplayCap(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{srv: ts, store: store, pool: p, client: client, path: accountsFilePath}
}

// httpResp is a decoded response envelope.
type httpResp struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// do issues an authenticated JSON request against the harness.
func (h *harness) do(t *testing.T, method, path string, body any) httpResp {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+liveKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return httpResp{Status: resp.StatusCode, Body: b, Headers: resp.Header}
}

// chatBody builds a minimal OpenAI-shaped chat request.
func chatBody(model string, stream bool, msgs ...map[string]string) map[string]any {
	out := make([]map[string]string, 0, len(msgs))
	out = append(out, msgs...)
	return map[string]any{"model": model, "stream": stream, "messages": out}
}

// userMsg / assistantMsg are small constructors for chat history.
func userMsg(s string) map[string]string { return map[string]string{"role": "user", "content": s} }
func assistantMsg(s string) map[string]string {
	return map[string]string{"role": "assistant", "content": s}
}

// chatResponse is the subset of the OpenAI response shape this suite asserts.
type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// decodeChat decodes a non-streaming response, failing on transport errors.
func decodeChat(t *testing.T, r httpResp) chatResponse {
	t.Helper()
	var out chatResponse
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("decode chat response (status %d): %v\nbody: %s", r.Status, err, truncate(string(r.Body), 400))
	}
	return out
}

// sseFrame is one parsed `data:` payload of a stream.
type sseFrame struct {
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// storedSession reads the account's session back from the accounts copy.
func (h *harness) storedSession(t *testing.T) accounts.Account {
	t.Helper()
	acct, ok := h.store.Get(h.acct.ID)
	if !ok {
		t.Fatalf("account %s missing from store", h.acct.ID)
	}
	return acct
}

// rewriteAccount applies mutate to the first account of the copy on disk and
// reloads nothing — callers re-load the store when they need the new state.
func rewriteAccount(t *testing.T, path string, mutate func(acct map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read accounts copy: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse accounts copy: %v", err)
	}
	list, _ := doc["accounts"].([]any)
	if len(list) == 0 {
		t.Fatal("accounts copy has no entries")
	}
	first, _ := list[0].(map[string]any)
	mutate(first)
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write accounts copy: %v", err)
	}
}

// paceWait enforces the inter-call gap.
func paceWait() { time.Sleep(pace) }

// truncate keeps failure messages readable.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
