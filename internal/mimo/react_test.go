package mimo

import (
	"context"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/testutil"
)

// The v3 chat surface, per the full re-reverse of the live SPA
// (docs/v3-endpoints.md, 2026-10-16): the upstream serves exactly two chat
// channels for this deployment. Every non-ultraspeed model posts
// /open-apis/react/chat (the SPA's Jy() routes them there on any
// reactEnabled account, which a fresh session confirmed); the ultraspeed
// model posts /fastchat/open-apis/bot/chat, its only route (react answers
// those ids with 模型名称错误). The unprefixed v2 /open-apis/bot/chat is
// not sent by this client at all — the owner ruling is v2-free, no
// fallback. These tests pin that two-channel routing.

// CHAN-001: a non-ultraspeed model posts /open-apis/react/chat with the
// shared chat payload (the SPA's module 12251 sends one payload object to
// both of its completions clients).
func TestChatReactRouting(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{Body: testutil.FixtureSSEReact})
	res, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := up.RequestsFor(testPH)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0].Path != "/open-apis/react/chat" {
		t.Errorf("path = %q, want /open-apis/react/chat", reqs[0].Path)
	}
	if reqs[0].Model != "mimo-v2.6-flash" || !strings.Contains(reqs[0].RawBody, `"multiMedias":[]`) {
		t.Errorf("react payload diverged: model=%q body=%s", reqs[0].Model, reqs[0].RawBody)
	}
	// Result: reasoning + content + citations + usage all assembled.
	if res.Content != "Hello world" {
		t.Errorf("content = %q, want %q", res.Content, "Hello world")
	}
	if res.Reasoning != "The user said hi. I should greet them." {
		t.Errorf("reasoning = %q, want the concatenated reasoningContent deltas", res.Reasoning)
	}
	if res.DialogID != "17159858" {
		t.Errorf("dialogID = %q, want the numeric react dialogId", res.DialogID)
	}
	if len(res.Citations) != 1 || res.Citations[0].URL != "https://www.example.cn/post" {
		t.Errorf("citations = %+v, want the one web_search result", res.Citations)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 7888 || res.Usage.CompletionTokens != 1230 {
		t.Errorf("usage = %+v, want 7888/1230", res.Usage)
	}
}

// (Ultraspeed → /fastchat/open-apis/bot/chat is pinned by
// TestChatFastchatRouting in client_test.go, which asserts both routing
// directions: fastchat model under /fastchat/, normal model not.)

// CHAN-003: no v2 fallback — a 404 on the react channel fails the turn.
// The SPA keeps a `catch 404 → v2` retry for itself, but this client has no
// v2 surface to retry onto (owner ruling: no compatibility, no fallback);
// the 404 passes through like any other upstream refusal.
func TestChatReact404IsAnError(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{StatusCode: 404})
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-flash",
	})
	var ue *UpstreamError
	if err == nil || !asUpstreamError(err, &ue) || ue.StatusCode != 404 {
		t.Fatalf("err = %v, want the 404 passthrough", err)
	}
	if n := len(up.RequestsFor(testPH)); n != 1 {
		t.Errorf("requests = %d, want 1 (no fallback)", n)
	}
}

// CHAN-004: the upstream's own refusal (451/401/…) passes through verbatim.
func TestChatReactNon404DoesNotFallBack(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{StatusCode: 451})
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-flash",
	})
	var ue *UpstreamError
	if err == nil || !asUpstreamError(err, &ue) || ue.StatusCode != 451 {
		t.Fatalf("err = %v, want the 451 passthrough", err)
	}
	if n := len(up.RequestsFor(testPH)); n != 1 {
		t.Errorf("requests = %d, want 1 (no fallback)", n)
	}
}

// REACT-005 (SSE layer): reasoningContent deltas stream as reasoning pieces
// directly — no NUL think markers exist on the react channel, so the
// ThinkSplitter must not be in the path for them. Tool frames (toolCalls /
// messageType:"tool") carry no client text and must not count as deltas.
func TestSSEReactReasoningAndToolFrames(t *testing.T) {
	st := newStreamer()
	var pieces []adapter.Piece
	st.onPiece = func(p adapter.Piece) error { pieces = append(pieces, p); return nil }
	if _, err := st.parseEvents(context.Background(), strings.NewReader(testutil.FixtureSSEReact)); err != nil {
		t.Fatal(err)
	}
	st.finish()

	var reasoning, content string
	for _, p := range pieces {
		switch p.Type {
		case "reasoning":
			reasoning += p.Text
		case "content":
			content += p.Text
		}
	}
	if reasoning != "The user said hi. I should greet them." {
		t.Errorf("reasoning pieces = %q, want the concatenated reasoningContent", reasoning)
	}
	if content != "Hello world" {
		t.Errorf("content pieces = %q, want the content deltas", content)
	}
	if !st.sawDeltas {
		t.Errorf("sawDeltas = false, want true (content deltas arrived)")
	}
}

// REACT-006 (SSE layer): a react stream with reasoning deltas but zero
// content still counts as answered for the sawDeltas check — reasoning IS
// a delta. (Chat()'s empty-response check separately requires content or
// reasoning, so reasoning-only turns pass both gates.)
func TestSSEReactReasoningCountsAsDelta(t *testing.T) {
	st := newStreamer()
	stream := "event:message\ndata:{\"messageType\":\"assistant\",\"reasoningContent\":\"thinking\"}\n\nevent:finish\ndata:{\"content\":\"[DONE]\"}\n\n"
	if _, err := st.parseEvents(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	st.finish()
	if !st.sawDeltas {
		t.Errorf("sawDeltas = false, want true (reasoning delta arrived)")
	}
}

// asUpstreamError unwraps err into an *UpstreamError when it is one.
func asUpstreamError(err error, target **UpstreamError) bool {
	ue, ok := err.(*UpstreamError)
	if ok {
		*target = ue
	}
	return ok
}
