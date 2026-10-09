package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// twoAccounts gives the retry tests a second landing spot.
func twoAccounts() []testserver.TestAccount {
	return []testserver.TestAccount{
		{ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie},
		{ID: "acc2", PH: "cGgtbW9jazI=", Cookie: `userId=223456; serviceToken=st-mock-2; xiaomichatbot_serviceToken="st-mock-2"; xiaomichatbot_ph="cGgtbW9jazI="`},
	}
}

// API-001: non-stream happy path — body shape per SDD §5.4.
func TestAPINonStreamHappyPath(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	var comp adapter.Completion
	if err := json.Unmarshal(body, &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(comp.ID, "chatcmpl-") {
		t.Errorf("id = %q", comp.ID)
	}
	if comp.Object != "chat.completion" || comp.Created == 0 {
		t.Errorf("object/created = %q/%d", comp.Object, comp.Created)
	}
	if comp.Model != "mimo-v2.6-pro" {
		t.Errorf("model echo = %q", comp.Model)
	}
	if len(comp.Choices) != 1 {
		t.Fatalf("choices = %d", len(comp.Choices))
	}
	ch := comp.Choices[0]
	if ch.Message.Role != "assistant" || ch.Message.Content != "Hello world" {
		t.Errorf("message = %+v", ch.Message)
	}
	if ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", ch.FinishReason)
	}
	if comp.Usage.PromptTokens != 2242 || comp.Usage.CompletionTokens != 78 {
		t.Errorf("usage = %+v", comp.Usage)
	}
	if ch.Message.ReasoningContent != "" {
		// reasoning_content omitted when empty (API-001).
		t.Errorf("reasoning_content must be absent when empty")
	}
}

// API-005: GET /v1/models — list shape with ultraspeed included.
func TestAPIModelsList(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Get("/v1/models", "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q", list.Object)
	}
	found := map[string]bool{}
	for _, m := range list.Data {
		if m.Object != "model" || m.Created == 0 || m.OwnedBy == "" {
			t.Errorf("model entry malformed: %+v", m)
		}
		found[m.ID] = true
	}
	if !found["mimo-v2.6-pro-ultraspeed-studio"] || !found["mimo-v2.6-pro"] {
		t.Errorf("catalog missing pro/ultraspeed: %v", found)
	}
}

// API-006: GET /v1/models/{id} — known, alias, unknown.
func TestAPIModelByID(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	cases := []struct {
		id   string
		want int
	}{
		{"mimo-v2.6-pro", http.StatusOK},
		{"mimo-v2.6-pro-studio", http.StatusOK}, // alias normalized
		{"no-such-model", http.StatusNotFound},
	}
	for _, tc := range cases {
		resp, err := ts.Get("/v1/models/"+tc.id, "")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("id %q: status = %d, want %d", tc.id, resp.StatusCode, tc.want)
			continue
		}
		if tc.want == http.StatusNotFound {
			if !strings.Contains(string(body), "mimo-v2.6-pro") {
				t.Errorf("404 must list available ids: %s", body)
			}
		}
	}
}

// API-008: tools stripped + X-Mimo-Warnings.
func TestAPIToolsStripped(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	body := `{"model":"mimo-v2.6-pro","stream":false,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function"}],"tool_choice":"auto","functions":[{}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	warn := resp.Header.Get("X-Mimo-Warnings")
	for _, field := range []string{"tools", "tool_choice", "functions"} {
		if !strings.Contains(warn, field) {
			t.Errorf("warnings %q missing %q", warn, field)
		}
	}
	// Never forwarded upstream.
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	for _, banned := range []string{"tools", "tool_choice", "functions"} {
		if strings.Contains(reqs[0].RawBody, banned) {
			t.Errorf("upstream payload leaked %q: %s", banned, reqs[0].RawBody)
		}
	}
}

// API-009: max_tokens ignored with warning.
func TestAPIMaxTokensIgnored(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	body := `{"model":"mimo-v2.6-pro","stream":false,"max_tokens":100,"max_completion_tokens":50,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	warn := resp.Header.Get("X-Mimo-Warnings")
	if !strings.Contains(warn, "max_tokens") || !strings.Contains(warn, "max_completion_tokens") {
		t.Errorf("warnings = %q", warn)
	}
	if strings.Contains(ts.Upstream.Requests()[0].RawBody, "max_tokens") {
		t.Error("max_tokens leaked upstream")
	}
}

// API-010: enable_thinking → reasoning_content on the message (the streaming
// delta variant died with the SSE path, ADR 0017).
func TestAPIReasoningContent(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEThinking}},
	}}, nil)
	body := `{"model":"mimo-v2.6-pro","stream":false,"enable_thinking":true,` +
		`"messages":[{"role":"user","content":"2+2"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	comp := testutil.ParseCompletion(t, testutil.ReadJSON(t, resp))
	if len(comp.Choices) != 1 {
		t.Fatalf("choices = %d", len(comp.Choices))
	}
	msg := comp.Choices[0].Message
	if msg.ReasoningContent != "Let me check the math." {
		t.Errorf("reasoning_content = %q", msg.ReasoningContent)
	}
	if msg.Content != "The answer is 4." {
		t.Errorf("content = %q", msg.Content)
	}
}

// API-011: reasoning:{"type":"enabled"} alias — same behavior.
func TestAPIReasoningAlias(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEThinking}},
	}}, nil)
	body := `{"model":"mimo-v2.6-pro","stream":false,"reasoning":{"type":"enabled"},` +
		`"messages":[{"role":"user","content":"2+2"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	comp := testutil.ParseCompletion(t, testutil.ReadJSON(t, resp))
	if got := comp.Choices[0].Message.ReasoningContent; got != "Let me check the math." {
		t.Errorf("reasoning_content = %q", got)
	}
}

// API-010 companion: thinking off strips reasoning entirely.
func TestAPIThinkingOffStripsReasoning(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEThinking}},
	}}, nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "2+2"}), "")
	if err != nil {
		t.Fatal(err)
	}
	comp := testutil.ParseCompletion(t, testutil.ReadJSON(t, resp))
	if got := comp.Choices[0].Message.ReasoningContent; got != "" {
		t.Errorf("reasoning leaked with thinking off: %q", got)
	}
	if comp.Choices[0].Message.Content != "The answer is 4." {
		t.Errorf("content = %q, want the answer without the reasoning block", comp.Choices[0].Message.Content)
	}
}

// API-012: content parts — text parts joined; image part → 400.
func TestAPIContentParts(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	// Text parts.
	body := `{"model":"mimo-v2.6-pro","stream":false,"messages":[{"role":"user","content":` +
		`[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("text parts: status = %d", resp.StatusCode)
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 || reqs[0].Query != "hello \nworld" {
		t.Errorf("joined query = %q", reqs[0].Query)
	}
	// Image part → 400.
	body = `{"model":"mimo-v2.6-pro","stream":false,"messages":[{"role":"user","content":` +
		`[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`
	resp, err = ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("image part: status = %d, want 400", resp.StatusCode)
	}
}

// API-014: no user message → 400 invalid_request_error.
func TestAPINoUserMessage(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"system", "be nice"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_, typ, code := testutil.ParseError(t, body)
	if typ != "invalid_request_error" || code != "" {
		t.Errorf("error = %q %q", typ, code)
	}
}

// API-015: replay cap exceeded → 400 with guidance.
func TestAPIReplayCap(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{ReplayCap: 3})
	var msgs [][2]string
	for i := 0; i < 5; i++ {
		msgs = append(msgs, [2]string{"user", "q" + string(rune('a'+i))})
	}
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, msgs...), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "replay cap") {
		t.Errorf("body lacks guidance: %s", body)
	}
}

// API-016: client-side char caps — zero upstream requests.
func TestAPICharCaps(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	big := strings.Repeat("字", 50001)
	// Normal model: cap 90000 — under.
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", big}), "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("90k model: status = %d", resp.StatusCode)
	}
	// Ultraspeed: cap 50000 — over.
	resp, err = ts.Chat(testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", big}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("50k model: status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_, _, code := testutil.ParseError(t, body)
	if code != "context_too_long" {
		t.Errorf("code = %q", code)
	}
	if n := ts.Upstream.ChatHits(); n != 1 {
		t.Errorf("upstream chat hits = %d, want 1 (only the legal request)", n)
	}
}

// API-018: /healthz — state counts only, no credentials.
func TestAPIHealthz(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	resp, err := ts.Get("/healthz", "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, secret := range []string{"st-mock-1", "st-mock-2", "userId", "cookie", "123456"} {
		if strings.Contains(s, secret) {
			t.Errorf("healthz leaked %q: %s", secret, s)
		}
	}
	var h struct {
		Status   string `json:"status"`
		Accounts struct {
			Total     int `json:"total"`
			Available int `json:"available"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Accounts.Total != 2 || h.Accounts.Available != 2 {
		t.Errorf("healthz = %+v", h)
	}
}

// API-022: temperature/top_p passthrough into upstream modelConfig.
func TestAPISamplingPassthrough(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	body := `{"model":"mimo-v2.6-pro","stream":false,"temperature":0.3,"top_p":0.9,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].Temperature == nil || *reqs[0].Temperature != 0.3 {
		t.Errorf("temperature = %v", reqs[0].Temperature)
	}
	if reqs[0].TopP == nil || *reqs[0].TopP != 0.9 {
		t.Errorf("topP = %v", reqs[0].TopP)
	}

	// Absent when not provided.
	resp, err = ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	r2 := ts.Upstream.Requests()[1]
	if strings.Contains(r2.RawBody, "temperature") || strings.Contains(r2.RawBody, "topP") {
		t.Errorf("sampling fields must be absent: %s", r2.RawBody)
	}
}

var (
	goldenChatID  = regexp.MustCompile(`"id":"chatcmpl-[0-9a-fA-F]+"`)
	goldenCreated = regexp.MustCompile(`"created":\d+`)
)
