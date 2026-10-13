package api_test

// The OpenAI-standard spellings of the two knobs this gateway exposes: thinking
// (reasoning_effort) and search (web_search_options). A standard client must be
// able to ask for both without learning a field of ours, and a typo must fail
// loudly instead of quietly doing nothing.
//
// Documentation: SDD §5.2, docs/TDD.md OSTD-001..007, OSTD-011.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// ostdBody builds a non-streaming chat request with the standard fields spliced
// in (searchBody is the same helper with the M19 fields).
func ostdBody(model string, extra map[string]any) string {
	return searchBody(model, false, extra)
}

func scriptedServer(t *testing.T) (*testserver.TestServer, []testserver.TestAccount) {
	t.Helper()
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})
	return ts, acct
}

// OSTD-001: reasoning_effort turns the upstream thinking switch on, and "none"
// is the explicit off switch.
func TestOSTD001ReasoningEffortEnablesThinking(t *testing.T) {
	cases := []struct {
		name   string
		effort string
		want   bool
	}{
		{"minimal", "minimal", true},
		{"low", "low", true},
		{"medium", "medium", true},
		{"high", "high", true},
		{"none", "none", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := scriptedServer(t)
			status, body := chatOnce(t, ts, ostdBody("mimo-v2.6-pro", map[string]any{"reasoning_effort": tc.effort}))
			if status != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", status, body)
			}
			reqs := ts.Upstream.Requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(reqs))
			}
			if reqs[0].EnableThinking != tc.want {
				t.Errorf("upstream enableThinking = %v, want %v (reasoning_effort=%q)", reqs[0].EnableThinking, tc.want, tc.effort)
			}
		})
	}
}

// OSTD-002: when the standard field is present it decides — including when it
// says "none" and an older spelling asked for thinking.
func TestOSTD002ReasoningEffortBeatsLegacySpelling(t *testing.T) {
	ts, _ := scriptedServer(t)
	body := ostdBody("mimo-v2.6-pro", map[string]any{"enable_thinking": true, "reasoning_effort": "none"})
	status, raw := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if got := ts.Upstream.Requests()[0].EnableThinking; got {
		t.Errorf("upstream enableThinking = true, want false: reasoning_effort=none must win over enable_thinking")
	}
}

// OSTD-003: a misspelled effort is rejected before any upstream call and names
// the accepted values.
func TestOSTD003InvalidReasoningEffortRejected(t *testing.T) {
	ts, _ := scriptedServer(t)
	status, body := chatOnce(t, ts, ostdBody("mimo-v2.6-pro", map[string]any{"reasoning_effort": "very-high"}))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, body)
	}
	if !strings.Contains(body, "unsupported_reasoning_effort") {
		t.Errorf("body = %s, want an unsupported_reasoning_effort error", body)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (rejected before the pool)", n)
	}
}

// OSTD-004: web_search_options turns search on, and the granularity it cannot
// honour is reported rather than silently assumed to have worked.
func TestOSTD004WebSearchOptionsTurnsSearchOn(t *testing.T) {
	ts, _ := scriptedServer(t)
	body := ostdBody("mimo-v2.6-flash", map[string]any{
		"web_search_options": map[string]any{"search_context_size": "medium"},
	})
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.StatusCode, raw)
	}
	if got := ts.Upstream.Requests()[0].WebSearchStatus; got != "enabled" {
		t.Errorf("upstream webSearchStatus = %q, want %q", got, "enabled")
	}
	warn := resp.Header.Get("X-Mimo-Warnings")
	if !strings.Contains(warn, "web_search_options.search_context_size") {
		t.Errorf("X-Mimo-Warnings = %q, want it to name the dropped search_context_size", warn)
	}
}

// OSTD-005: an unknown search_context_size is rejected before any upstream call.
func TestOSTD005InvalidSearchContextSizeRejected(t *testing.T) {
	ts, _ := scriptedServer(t)
	body := ostdBody("mimo-v2.6-flash", map[string]any{
		"web_search_options": map[string]any{"search_context_size": "huge"},
	})
	status, raw := chatOnce(t, ts, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	if !strings.Contains(raw, "unsupported_search_context_size") {
		t.Errorf("body = %s, want an unsupported_search_context_size error", raw)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// OSTD-006: the finer control still wins when both search spellings are given.
func TestOSTD006ExplicitSearchStatusWins(t *testing.T) {
	ts, _ := scriptedServer(t)
	body := ostdBody("mimo-v2.6-flash", map[string]any{
		"web_search_options": map[string]any{"search_context_size": "low"},
		"web_search_status":  "auto",
	})
	status, raw := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if got := ts.Upstream.Requests()[0].WebSearchStatus; got != "auto" {
		t.Errorf("upstream webSearchStatus = %q, want %q (explicit web_search_status wins)", got, "auto")
	}
}

// OSTD-008: the response body is OpenAI's shape, not a lookalike. The keys a client
// validating against that schema expects are all present, including the ones that are
// null and the usage detail objects that are zero — a client should never have to
// branch on their absence.
func TestOSTD008ResponseCarriesTheOpenAIShape(t *testing.T) {
	ts, _ := scriptedServer(t)
	status, raw := chatOnce(t, ts, ostdBody("mimo-v2.6-flash", nil))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	for _, k := range []string{"id", "object", "created", "model", "choices", "usage"} {
		if _, ok := body[k]; !ok {
			t.Errorf("response has no %q: %s", k, raw)
		}
	}
	if body["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", body["object"])
	}
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %q, want a chatcmpl- prefixed id", id)
	}
	ch, _ := body["choices"].([]any)
	if len(ch) != 1 {
		t.Fatalf("choices = %v, want exactly one", body["choices"])
	}
	choice, _ := ch[0].(map[string]any)
	for _, k := range []string{"index", "message", "logprobs", "finish_reason"} {
		if _, ok := choice[k]; !ok {
			t.Errorf("choices[0] has no %q: %s", k, raw)
		}
	}
	if v, ok := choice["logprobs"]; !ok || v != nil {
		t.Errorf("choices[0].logprobs = %v (present=%v), want null", v, ok)
	}
	msg, _ := choice["message"].(map[string]any)
	for _, k := range []string{"role", "content", "refusal"} {
		if _, ok := msg[k]; !ok {
			t.Errorf("choices[0].message has no %q: %s", k, raw)
		}
	}
	if v, ok := msg["refusal"]; !ok || v != nil {
		t.Errorf("message.refusal = %v (present=%v), want null", v, ok)
	}
	u, _ := body["usage"].(map[string]any)
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details"} {
		if _, ok := u[k]; !ok {
			t.Errorf("usage has no %q: %s", k, raw)
		}
	}
	// Each detail object carries its own counter and only its own: OpenAI's prompt
	// details hold cached_tokens, its completion details hold reasoning_tokens.
	// Padding one object with the other's key is shape noise a schema validator sees.
	prompt, _ := u["prompt_tokens_details"].(map[string]any)
	if _, ok := prompt["cached_tokens"]; !ok {
		t.Errorf("usage.prompt_tokens_details has no cached_tokens: %s", raw)
	}
	if _, ok := prompt["reasoning_tokens"]; ok {
		t.Errorf("usage.prompt_tokens_details carries reasoning_tokens, which belongs to the completion object: %s", raw)
	}
	completion, _ := u["completion_tokens_details"].(map[string]any)
	if _, ok := completion["reasoning_tokens"]; !ok {
		t.Errorf("usage.completion_tokens_details has no reasoning_tokens: %s", raw)
	}
	if _, ok := completion["cached_tokens"]; ok {
		t.Errorf("usage.completion_tokens_details carries cached_tokens, which belongs to the prompt object: %s", raw)
	}
}

// OSTD-009: an error names the field it is about, the way OpenAI's errors do — and
// stays silent (null) rather than guessing when the failure is not about one field.
func TestOSTD009ErrorParamNamesTheOffendingField(t *testing.T) {
	ts, _ := scriptedServer(t)
	status, raw := chatOnce(t, ts, ostdBody("mimo-v2.6-flash", map[string]any{"reasoning_effort": "very-high"}))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	var body struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, raw)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", body.Error.Type)
	}
	if body.Error.Param == nil || *body.Error.Param != "reasoning_effort" {
		t.Errorf("param = %v, want reasoning_effort", body.Error.Param)
	}
	if body.Error.Code != "unsupported_reasoning_effort" {
		t.Errorf("code = %q, want unsupported_reasoning_effort", body.Error.Code)
	}

	// A failure that is not about a request field must not invent one.
	// Warmed snapshot (as main.go serves with): an unknown id is a catalog
	// 404, which is no single field's fault.
	ts.API.RefreshModelRedirects(context.Background())
	status, raw = chatOnce(t, ts, ostdBody("no-such-model", nil))
	if status == http.StatusOK {
		t.Fatalf("status = 200 for an unknown model, want an error: %s", raw)
	}
	if !strings.Contains(raw, `"param":null`) {
		t.Errorf("body = %s, want param:null when no single field is at fault", raw)
	}
}

// OSTD-010: the warning header is built from client-supplied keys, so it is bounded
// and stable. Measured before the bound: 3000 unknown fields produced a header past
// 64 KiB and the caller could not parse the response at all (Python's http.client
// raises LineTooLong over 65536 bytes) — extra metadata cost the client its answer.
func TestOSTD010WarningHeaderIsBoundedAndStable(t *testing.T) {
	ts, _ := scriptedServer(t)
	body := func() string {
		m := map[string]any{"model": "mimo-v2.6-flash", "stream": false,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		for i := 0; i < 3000; i++ {
			m[fmt.Sprintf("unknown_field_number_%04d_padded_to_be_long", i)] = 1
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	resp, err := ts.Chat(body(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.StatusCode, raw)
	}
	warn := resp.Header.Get("X-Mimo-Warnings")
	if warn == "" {
		t.Fatal("no warning header: unknown fields must still be reported")
	}
	if len(warn) > 1024 {
		t.Errorf("warning header is %d bytes, want a bounded one (a client cannot parse a header line over 64 KiB)", len(warn))
	}
	if !strings.Contains(warn, "+2992 more") {
		t.Errorf("warnings = %q, want the truncated count reported", warn)
	}
	if n := strings.Count(warn, "unknown_field_number"); n > maxWarnFieldsCapped() {
		t.Errorf("warnings = %q, want at most %d names listed", warn, maxWarnFieldsCapped())
	}

	// Same request twice: the same header. The names came out of a JSON object, so
	// the order used to change run to run.
	resp2, err := ts.Chat(body(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	_, _ = io.ReadAll(resp2.Body)
	if again := resp2.Header.Get("X-Mimo-Warnings"); again != warn {
		t.Errorf("warning header changed between identical requests:\n  %s\n  %s", warn, again)
	}
}

// maxWarnFieldsCapped mirrors the package's cap for the assertion above.
func maxWarnFieldsCapped() int { return 8 }

// OSTD-007: the model id reaches the upstream exactly as the client wrote it —
// the options travel in standard fields, never in a renamed model.
func TestOSTD007ModelIDUnchangedByStandardFields(t *testing.T) {
	ts, _ := scriptedServer(t)
	body := ostdBody("mimo-v2.6-pro", map[string]any{
		"reasoning_effort":   "high",
		"web_search_options": map[string]any{"search_context_size": "high"},
	})
	status, raw := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if got := ts.Upstream.Requests()[0].Model; got != "mimo-v2.6-pro" {
		t.Errorf("upstream model = %q, want %q", got, "mimo-v2.6-pro")
	}
}

// OSTD-013: every error body carries all four keys, and `code` is a real string —
// never absent, never null (ADR 0032). OpenAI's error object always has the key, and
// this gateway's rule is that a key OpenAI sends as null is sent as null rather than
// omitted (message.refusal, choices[0].logprobs) — but a *code* is information, so
// leaving it empty was the wrong half of that rule: a client switching on the code
// got nothing to switch on. Every failure this surface can report now has a name, and
// one name is used for one meaning (a malformed body is `malformed_request_body` on
// the chat, audio and admin surfaces alike).
func TestOSTD013ErrorBodyAlwaysCarriesFourKeys(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})
	// Warm the catalog snapshot the way main.go does before serving: an
	// unknown model id is a 404 only against a known catalog, and the snapshot
	// is what the chat gate validates against (a cold one passes ids through).
	ts.API.RefreshModelRedirects(context.Background())

	cases := []struct {
		name       string
		call       func() (*http.Response, error)
		wantStatus int
		wantCode   string
	}{
		{"body-level failure", func() (*http.Response, error) {
			return ts.Post("/v1/chat/completions", "k1",
				`{"model":"mimo-v2.6-flash","stream":false,"messages":[]}`)
		}, http.StatusBadRequest, "malformed_request_body"},
		{"no sendable user message", func() (*http.Response, error) {
			return ts.Post("/v1/chat/completions", "k1",
				`{"model":"mimo-v2.6-flash","stream":false,"messages":[{"role":"system","content":"x"}]}`)
		}, http.StatusBadRequest, "no_user_message"},
		{"over the replay cap", func() (*http.Response, error) {
			var msgs []map[string]any
			for i := 0; i < 40; i++ {
				msgs = append(msgs, map[string]any{"role": "user", "content": "hi"})
			}
			raw, err := json.Marshal(map[string]any{
				"model": "mimo-v2.6-flash", "stream": false, "messages": msgs})
			if err != nil {
				t.Fatal(err)
			}
			return ts.Post("/v1/chat/completions", "k1", string(raw))
		}, http.StatusBadRequest, "too_many_turns"},
		{"unknown model", func() (*http.Response, error) {
			return ts.Post("/v1/chat/completions", "k1", ostdBody("gpt-4o", nil))
		}, http.StatusNotFound, "model_not_found"},
		{"unknown model id on the listing", func() (*http.Response, error) {
			return ts.Get("/v1/models/gpt-4o", "k1")
		}, http.StatusNotFound, "model_not_found"},
		{"unknown voice", func() (*http.Response, error) {
			return ts.Post("/v1/audio/speech", "k1",
				`{"model":"mimo-v2.5-tts","input":"hi","voice":"nope"}`)
		}, http.StatusBadRequest, "unknown_voice"},
		{"unsupported reasoning_effort", func() (*http.Response, error) {
			return ts.Post("/v1/chat/completions", "k1",
				ostdBody("mimo-v2.6-flash", map[string]any{"reasoning_effort": "ultra"}))
		}, http.StatusBadRequest, "unsupported_reasoning_effort"},
		{"bad key", func() (*http.Response, error) {
			return ts.Chat(ostdBody("mimo-v2.6-flash", nil), "wrong")
		}, http.StatusUnauthorized, "invalid_api_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.call()
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", resp.StatusCode, raw, tc.wantStatus)
			}
			var env struct {
				Error map[string]json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("body is not an error envelope: %s", raw)
			}
			for _, k := range []string{"message", "type", "param", "code"} {
				if _, ok := env.Error[k]; !ok {
					t.Errorf("error body is missing the %q key: %s", k, raw)
				}
			}
			var got *string
			if rc, ok := env.Error["code"]; ok {
				_ = json.Unmarshal(rc, &got)
			}
			switch {
			case got == nil:
				t.Errorf("code is null: every failure this surface reports has a code (ADR 0032)")
			case *got != tc.wantCode:
				t.Errorf("code = %q, want %q", *got, tc.wantCode)
			}
		})
	}
}

// OSTD-012: the retired `session_id` carrier is reported, not silently accepted.
// It pinned the session a streaming request continued; streaming and the session
// store were deleted, not switched off (ADR 0017), so the key sits outside
// knownRequestFields and a client that still sends it is told. Silence would be the
// worse failure: the client would believe it is pinning a conversation this gateway
// no longer has.
func TestOSTD012RetiredSessionIDIsReported(t *testing.T) {
	ts, _ := scriptedServer(t)
	resp, err := ts.Chat(ostdBody("mimo-v2.6-flash", map[string]any{"session_id": "s-abc"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 — an unknown field is reported, never rejected", resp.StatusCode, raw)
	}
	if warn := resp.Header.Get("X-Mimo-Warnings"); !strings.Contains(warn, "session_id") {
		t.Errorf("warnings = %q, want the retired session_id carrier named", warn)
	}

	// And a request that does not carry it stays clean.
	resp2, err := ts.Chat(ostdBody("mimo-v2.6-flash", nil), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	_, _ = io.ReadAll(resp2.Body)
	if warn := resp2.Header.Get("X-Mimo-Warnings"); warn != "" {
		t.Errorf("a plain request carried warnings = %q, want none", warn)
	}
}

// OSTD-011: a request that says nothing about thinking follows the model's own
// declared default (ADR 0031) — the upstream's catalog declares thinking on for
// every chat model, which is also where the Studio's own UI starts. A plain OpenAI
// body therefore gets the model's working in `reasoning_content`; before this it
// got nothing, because the switch was off unless a client opted in.
func TestOSTD011ThinkingFollowsTheModelsDeclaredDefault(t *testing.T) {
	thinkingServer := func(t *testing.T) *testserver.TestServer {
		t.Helper()
		acct := oneAccount()
		ts := testserver.NewTestServer(t, acct, nil)
		ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEThinking})
		// Warm the catalog snapshot the way main.go does at startup: the declared
		// defaults reach the request path through that snapshot, and a cold one
		// would silently fall back to the deployment default (ADR 0031).
		ts.API.RefreshModelRedirects(context.Background())
		return ts
	}

	// (1) No thinking field at all: the catalog declares thinking on for flash, so
	// the upstream call carries the switch and the response carries the working.
	ts := thinkingServer(t)
	resp, err := ts.Chat(ostdBody("mimo-v2.6-flash", nil), "")
	if err != nil {
		t.Fatal(err)
	}
	comp := testutil.ParseCompletion(t, testutil.ReadJSON(t, resp))
	if reqs := ts.Upstream.Requests(); len(reqs) == 0 || !reqs[0].EnableThinking {
		t.Errorf("upstream enableThinking = false for a plain body; the model declares thinking on")
	}
	if got := comp.Choices[0].Message.ReasoningContent; got != "Let me check the math." {
		t.Errorf("reasoning_content = %q, want the model's working", got)
	}
	if comp.Choices[0].Message.Content != "The answer is 4." {
		t.Errorf("content = %q, want the answer", comp.Choices[0].Message.Content)
	}

	// (2) The standard field still turns it off, and then no reasoning comes back.
	ts2 := thinkingServer(t)
	resp2, err := ts2.Chat(ostdBody("mimo-v2.6-flash", map[string]any{"reasoning_effort": "none"}), "")
	if err != nil {
		t.Fatal(err)
	}
	comp2 := testutil.ParseCompletion(t, testutil.ReadJSON(t, resp2))
	if ts2.Upstream.Requests()[0].EnableThinking {
		t.Errorf("reasoning_effort=none did not turn the upstream switch off")
	}
	if got := comp2.Choices[0].Message.ReasoningContent; got != "" {
		t.Errorf("reasoning leaked with thinking off: %q", got)
	}

	// (3) The older spelling's explicit false is honoured too. It is a pointer
	// precisely so "not sent" (follow the model) and "false" (off) differ.
	ts3 := thinkingServer(t)
	if _, err := ts3.Chat(ostdBody("mimo-v2.6-flash", map[string]any{"enable_thinking": false}), ""); err != nil {
		t.Fatal(err)
	}
	if ts3.Upstream.Requests()[0].EnableThinking {
		t.Errorf("enable_thinking:false did not turn the upstream switch off")
	}

	// (4) An id this account's catalog does not describe (mimo-v2.6-pro carries no
	// thinkingDefaultOn in this fixture — and no row at all on the live account for
	// some ids) still goes out with the switch on: the gateway follows the
	// platform's own behaviour for its chat models instead of an operator knob, so
	// the only way to disagree with the provider is the standard request field.
	ts4 := thinkingServer(t)
	if _, err := ts4.Chat(ostdBody("mimo-v2.6-pro", nil), ""); err != nil {
		t.Fatal(err)
	}
	if !ts4.Upstream.Requests()[0].EnableThinking {
		t.Errorf("an undescribed model did not follow the platform's declared default (on)")
	}
}
