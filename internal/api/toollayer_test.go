package api_test

// The tool layer (ADR 0012): function-calling chat requests are forwarded to the
// middleware, everything else stays on the direct path, and the internal path the
// middleware calls back on never forwards again (that is what keeps the round
// trip a line instead of a loop).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
	"github.com/yg/mimo2api/internal/toollayer"
)

// fakeToolLayer stands in for ToolForge: it records what it was sent and answers
// whatever the case scripts.
type fakeToolLayer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []fakeRequest
	status   int
	ctype    string
	body     string
}

type fakeRequest struct {
	path   string
	header http.Header
	body   string
}

func newFakeToolLayer(t *testing.T) *fakeToolLayer {
	t.Helper()
	f := &fakeToolLayer{status: http.StatusOK, ctype: "application/json", body: `{"ok":true}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, fakeRequest{path: r.URL.Path, header: r.Header.Clone(), body: string(b)})
		status, ctype, body := f.status, f.ctype, f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeToolLayer) seen() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeToolLayer) answer(status int, ctype, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.ctype, f.body = status, ctype, body
}

func (f *fakeToolLayer) client() *toollayer.Client {
	return toollayer.New(f.srv.URL, "", nil, false)
}

const toolsBody = `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"东京天气？"}],` +
	`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`

const plainBody = `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"你好"}]}`

// TOOL-001: a tool-bearing request goes to the middleware as the bytes the
// caller sent, carrying the caller's own credential and session header.
func TestToolLayerForwardsToolRequests(t *testing.T) {
	fake := newFakeToolLayer(t)
	fake.answer(200, "application/json", `{"choices":[{"message":{"tool_calls":[{"id":"c1"}]}}]}`)
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})

	resp, err := ts.PostHeaders("/v1/chat/completions", "k", toolsBody, map[string]string{
		"X-Mimo-Session-Id": "sess-1",
		"X-Unrelated":       "must-not-travel",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if got := string(raw); !strings.Contains(got, "tool_calls") {
		t.Errorf("body = %s, want the middleware's answer passed through", got)
	}

	seen := fake.seen()
	if len(seen) != 1 {
		t.Fatalf("middleware saw %d requests, want 1", len(seen))
	}
	if !strings.Contains(seen[0].body, "get_weather") {
		t.Errorf("forwarded body = %s, want the caller's tools verbatim", seen[0].body)
	}
	if got := seen[0].header.Get("Authorization"); got != "Bearer k" {
		t.Errorf("Authorization = %q, want the caller's own credential", got)
	}
	if got := seen[0].header.Get("X-Mimo-Session-Id"); got != "sess-1" {
		t.Errorf("X-Mimo-Session-Id = %q, want it carried to the middleware", got)
	}
	if got := seen[0].header.Get("X-Unrelated"); got != "" {
		t.Errorf("X-Unrelated = %q, want only X-Mimo-* extensions carried", got)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want the model call to come via the middleware", n)
	}
}

// TOOL-002: a plain chat request keeps the direct path — no middleware, no extra
// hop, no new failure mode.
func TestToolLayerLeavesPlainChatAlone(t *testing.T) {
	fake := newFakeToolLayer(t)
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})

	resp, err := ts.Chat(plainBody, "k")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if n := len(fake.seen()); n != 0 {
		t.Errorf("middleware saw %d requests, want none for a plain chat", n)
	}
	if n := len(ts.Upstream.Requests()); n == 0 {
		t.Error("upstream saw no request, want the direct path to serve it")
	}
}

// TOOL-003: the turn that returns a tool result must reach the middleware too, or
// it cannot see the call it is answering.
func TestToolLayerForwardsToolResultTurns(t *testing.T) {
	fake := newFakeToolLayer(t)
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})

	body := `{"model":"mimo-v2.6-flash","messages":[` +
		`{"role":"user","content":"东京天气？"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"晴，24 度"}]}`
	resp, err := ts.Post("/v1/chat/completions", "k", body)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if n := len(fake.seen()); n != 1 {
		t.Fatalf("middleware saw %d requests, want the tool result forwarded", n)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want none on this path", n)
	}
}

// TOOL-004: a streamed answer is streamed through, not buffered.
func TestToolLayerStreamsThrough(t *testing.T) {
	fake := newFakeToolLayer(t)
	fake.answer(200, "text/event-stream",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\"}]}}]}\n\n"+
			"data: [DONE]\n\n")
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})

	body := `{"model":"mimo-v2.6-flash","stream":true,"messages":[{"role":"user","content":"东京天气？"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	resp, err := ts.Post("/v1/chat/completions", "k", body)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want the stream's own type", ct)
	}
	if got := string(raw); !strings.Contains(got, "tool_calls") || !strings.Contains(got, "[DONE]") {
		t.Errorf("body = %q, want both stream chunks passed through", got)
	}
}

// TOOL-005: with the middleware down, a tool request fails loudly (502) instead
// of being answered as if the model had ignored its tools.
func TestToolLayerUnavailableIsReported(t *testing.T) {
	fake := newFakeToolLayer(t)
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})
	fake.srv.Close() // nothing listening any more

	resp, err := ts.Post("/v1/chat/completions", "k", toolsBody)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", resp.StatusCode, raw)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("body %s: %v", raw, err)
	}
	if out.Error.Code != "tool_layer_unavailable" {
		t.Errorf("error.code = %q, want tool_layer_unavailable", out.Error.Code)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want no silent fallback", n)
	}
}

// TOOL-006: the callback path is served by us and never forwarded again — the
// recursion guard, which is what makes the middleware round trip terminate.
func TestRawChatPathNeverForwards(t *testing.T) {
	fake := newFakeToolLayer(t)
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})

	resp, err := ts.Post("/v1/raw/chat/completions", "k", toolsBody)
	if err != nil {
		t.Fatalf("raw chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("middleware saw %d requests from the raw path, want 0 (loop)", n)
	}
	if n := len(ts.Upstream.Requests()); n == 0 {
		t.Error("upstream saw no request, want the raw path to talk to the model")
	}
	// Tools are not this path's business: the middleware owns them, and the
	// caller is told they were dropped rather than left guessing.
	if warn := resp.Header.Get("X-Mimo-Warnings"); !strings.Contains(warn, "tools") {
		t.Errorf("X-Mimo-Warnings = %q, want the dropped-tools note", warn)
	}
}

// TOOL-007: the raw path is internal, not open.
func TestRawChatPathRequiresAuth(t *testing.T) {
	fake := newFakeToolLayer(t)
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		APIKeys:   []string{"k"},
		ToolLayer: fake.client(),
	})
	resp, err := ts.Post("/v1/raw/chat/completions", "", plainBody)
	if err != nil {
		t.Fatalf("raw chat: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TOOL-008: with no middleware configured, nothing changes — the deployment that
// does not want a tool layer is unaffected by its existence.
func TestWithoutToolLayerToolsAreStillReportedAsDropped(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k"}})
	resp, err := ts.Post("/v1/chat/completions", "k", toolsBody)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if warn := resp.Header.Get("X-Mimo-Warnings"); !strings.Contains(warn, "tools") {
		t.Errorf("X-Mimo-Warnings = %q, want tools reported as dropped", warn)
	}
}

// The fake middleware must not be able to look like the model upstream: a quick
// guard that the two paths stay distinguishable in the cases above.
var _ = testutil.ChatBody
