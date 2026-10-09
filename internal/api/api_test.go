package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

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

// API-002: stream happy path — role chunk → content deltas → finish →
// usage (choices:[]) → [DONE] (SDD §5.4).
func TestAPIStreamHappyPath(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("content-type = %q", ct)
	}
	events, comments := testutil.ReadSSE(t, resp)
	if len(events) < 5 {
		t.Fatalf("events = %d, want >=5: %v", len(events), events)
	}
	if len(comments) != 0 {
		t.Errorf("no keepalives expected on a fast stream, got %v", comments)
	}
	// First event: role-only delta.
	first := testutil.ParseChunk(t, events[0])
	if len(first.Choices) != 1 || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk = %+v", first.Choices)
	}
	if first.Object != "chat.completion.chunk" {
		t.Errorf("object = %q", first.Object)
	}
	// One id for the whole stream.
	chatID := first.ID
	var content strings.Builder
	sawFinish, sawUsage, sawDone := false, false, false
	for _, e := range events[1:] {
		if e == "[DONE]" {
			sawDone = true
			continue
		}
		c := testutil.ParseChunk(t, e)
		if c.ID != chatID {
			t.Errorf("chunk id changed: %q vs %q", c.ID, chatID)
		}
		if len(c.Choices) == 0 {
			if c.Usage == nil {
				t.Errorf("choices-empty chunk without usage: %s", e)
			} else {
				sawUsage = true
			}
			continue
		}
		if c.Choices[0].FinishReason != nil && *c.Choices[0].FinishReason == "stop" {
			sawFinish = true
			continue
		}
		content.WriteString(c.Choices[0].Delta.Content)
	}
	if content.String() != "Hello world" {
		t.Errorf("streamed content = %q", content.String())
	}
	if !sawFinish || !sawUsage || !sawDone {
		t.Errorf("finish/usage/done = %v/%v/%v", sawFinish, sawUsage, sawDone)
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

// API-010: enable_thinking → reasoning deltas before content.
func TestAPIReasoningDeltas(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEThinking}},
	}}, nil)
	body := `{"model":"mimo-v2.6-pro","stream":true,"enable_thinking":true,` +
		`"messages":[{"role":"user","content":"2+2"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	var reasoning, content strings.Builder
	fr, fc := -1, -1
	for i, e := range events {
		if e == "[DONE]" {
			continue
		}
		c := testutil.ParseChunk(t, e)
		for _, ch := range c.Choices {
			if ch.Delta.ReasoningContent != "" {
				reasoning.WriteString(ch.Delta.ReasoningContent)
				if fr < 0 {
					fr = i
				}
			}
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if fc < 0 {
					fc = i
				}
			}
		}
	}
	if reasoning.String() != "Let me check the math." {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if content.String() != "The answer is 4." {
		t.Errorf("content = %q", content.String())
	}
	if fr < 0 || fc < 0 || fr > fc {
		t.Errorf("reasoning first=%d content first=%d", fr, fc)
	}
}

// API-011: reasoning:{"type":"enabled"} alias — same behavior.
func TestAPIReasoningAlias(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEThinking}},
	}}, nil)
	body := `{"model":"mimo-v2.6-pro","stream":true,"reasoning":{"type":"enabled"},` +
		`"messages":[{"role":"user","content":"2+2"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	var reasoning strings.Builder
	for _, e := range events {
		if e == "[DONE]" {
			continue
		}
		for _, ch := range testutil.ParseChunk(t, e).Choices {
			reasoning.WriteString(ch.Delta.ReasoningContent)
		}
	}
	if reasoning.String() != "Let me check the math." {
		t.Errorf("reasoning = %q", reasoning.String())
	}
}

// API-010 companion: thinking off strips reasoning entirely.
func TestAPIThinkingOffStripsReasoning(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEThinking}},
	}}, nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "2+2"}), "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	for _, e := range events {
		if e == "[DONE]" {
			continue
		}
		for _, ch := range testutil.ParseChunk(t, e).Choices {
			if ch.Delta.ReasoningContent != "" {
				t.Errorf("reasoning leaked with thinking off: %q", ch.Delta.ReasoningContent)
			}
		}
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

// API-019: keepalive comments during upstream silence.
func TestAPIKeepalive(t *testing.T) {
	fixture := testutil.FixtureSSEStall
	// Split AFTER the first message delta so a piece reaches the client,
	// commit happens, and the stall then proves keepalives fire mid-silence.
	firstMsg := strings.Index(fixture, "event:message")
	split := strings.Index(fixture[firstMsg+1:], "event:message") + firstMsg + 1
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{
			Body:           fixture,
			StallMidStream: 300 * time.Millisecond,
			MidStreamSplit: split,
		}},
	}}, &testserver.TestServerOptions{Keepalive: 50 * time.Millisecond})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	events, comments := testutil.ReadSSE(t, resp)
	if len(comments) == 0 {
		t.Fatal("no keepalive comments during the stall")
	}
	for _, c := range comments {
		if !strings.HasPrefix(c, ":") {
			t.Errorf("comment frame malformed: %q", c)
		}
	}
	// Comments never appear inside data payloads.
	for _, e := range events {
		if strings.Contains(e, ": keepalive") {
			t.Errorf("keepalive leaked into data payload: %q", e)
		}
	}
	if len(events) == 0 {
		t.Fatal("no data events")
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

// API-002 (golden, TDD §2.1 `openai_stream_expected.txt`): the byte-level
// chunk sequence for the `sse_basic` upstream stream must match the committed
// golden with only the id/timestamp normalized. The structural assertions
// above pin parsed values; this pins frame order and shape, which is what a
// client's SSE parser actually has to survive.
func TestAPI002GoldenChunkSequence(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := normalizeStreamForGolden(string(raw))
	want := testutil.LoadFixture(t, "openai_stream_expected.txt")
	if got != want {
		t.Errorf("stream does not match the golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// normalizeStreamForGolden rewrites the two per-request values (chunk id and
// creation timestamp) so a golden can be compared across runs.
func normalizeStreamForGolden(s string) string {
	s = goldenChatID.ReplaceAllString(s, `"id":"chatcmpl-00000000000000000000000000000000"`)
	s = goldenCreated.ReplaceAllString(s, `"created":0`)
	return s
}

var (
	goldenChatID  = regexp.MustCompile(`"id":"chatcmpl-[0-9a-fA-F]+"`)
	goldenCreated = regexp.MustCompile(`"created":\d+`)
)
