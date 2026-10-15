package api_test

import (
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// REACT e2e: the gateway's chat path rides the v3 react channel
// (/open-apis/react/chat) for non-ultraspeed models, per the SPA's routing —
// the 2026-10-15 finding that reopened chat after the v2 channel block
// (BUGS-FOUND #7 correction). These tests pin the whole stack end-to-end.

// REACT-API-001: a non-fastchat model POSTs /open-apis/react/chat upstream
// and the react dialect's reasoningContent frames surface as the client's
// reasoning_content — no NUL think markers exist on that channel.
func TestAPIReactChannelReasoning(t *testing.T) {
	ts := testserver.NewTestServer(t, []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie,
		Script: []testutil.ScriptStep{{Body: testutil.FixtureSSEReact}},
	}}, nil)
	body := `{"model":"mimo-v2.6-pro","stream":false,"enable_thinking":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	comp := testutil.ParseCompletion(t, testutil.ReadJSON(t, resp))
	if len(comp.Choices) != 1 {
		t.Fatalf("choices = %d", len(comp.Choices))
	}
	msg := comp.Choices[0].Message
	if msg.Content != "Hello world" {
		t.Errorf("content = %q", msg.Content)
	}
	if msg.ReasoningContent != "The user said hi. I should greet them." {
		t.Errorf("reasoning_content = %q, want the react reasoningContent deltas", msg.ReasoningContent)
	}
	// The upstream call went out on the react path.
	reqs := ts.Upstream.RequestsFor(testutil.TestPH)
	if len(reqs) != 1 || reqs[0].Path != "/open-apis/react/chat" {
		t.Fatalf("upstream paths = %v, want one /open-apis/react/chat", pathsOf(reqs))
	}
	// The react web_search result reaches the client as a citation — top
	// level (the M19 extension) and on the message annotations; the payload
	// shape matches v2's, so nothing needed to change in the citation path.
	if len(comp.Citations) != 1 || comp.Citations[0].URL != "https://www.example.cn/post" {
		t.Errorf("citations = %+v, want the one web_search result", comp.Citations)
	}
	if len(msg.Annotations) != 1 {
		t.Errorf("annotations = %+v, want one", msg.Annotations)
	}
}

// REACT-API-002: an ultraspeed model stays on the fastchat v2 path — the
// react channel refuses ultraspeed ids (模型名称错误, live 2026-10-15).
func TestAPIReactUltraspeedKeepsFastchat(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reqs := ts.Upstream.RequestsFor(testutil.TestPH)
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	if reqs[0].Path != "/fastchat/open-apis/bot/chat" {
		t.Errorf("path = %q, want /fastchat/open-apis/bot/chat", reqs[0].Path)
	}
}

// pathsOf extracts the recorded paths for a failure message.
func pathsOf(reqs []testutil.RecordedRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Path
	}
	return out
}
