package mimo

import (
	"context"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/testutil"
)

// The v3 react channel (/open-apis/react/chat) — live-verified 2026-10-15
// during the 451 investigation (cmd/diag451, BUGS-FOUND #7 correction):
// the v2 bot/chat surface is channel-blocked (451 ????/临时封禁) for this
// account while react/chat answers 200 with an identical payload. The SPA
// routes viaFastchat→fastchat, reactEnabled→react, else studio, and falls
// back react→studio (v2) on a 404. These tests pin the client to that flow.

// REACT-001: react routing — ChatRequest.React posts /open-apis/react/chat
// with the same payload the v2 channel carries (the payload object is shared
// between both branches in the SPA's module 12251).
func TestChatReactRouting(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{Body: testutil.FixtureSSEReact})
	res, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-flash", React: true,
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
	// Payload parity with v2: same fields, same defaults.
	if reqs[0].Model != "mimo-v2.6-flash" || !strings.Contains(reqs[0].RawBody, `"multiMedias":[]`) {
		t.Errorf("react payload diverged from v2: model=%q body=%s", reqs[0].Model, reqs[0].RawBody)
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

// REACT-002: React does not touch the fastchat routing — a fastchat request
// keeps the /fastchat/open-apis/bot/chat path (the react channel answers an
// ultraspeed model with 模型名称错误, live 2026-10-15, so the two flags are
// mutually exclusive exactly as the SPA routes them).
func TestChatReactDoesNotAffectFastchat(t *testing.T) {
	client, up := chatEnv(t)
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-pro-ultraspeed-studio", Fastchat: true, React: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := up.RequestsFor(testPH)[0]
	if r.Path != "/fastchat/open-apis/bot/chat" {
		t.Errorf("path = %q, want fastchat v2 path (fastchat wins, SPA routes it first)", r.Path)
	}
}

// REACT-003: SPA-faithful fallback — a 404 on the react channel retries the
// identical body once against the v2 studio path (SPA: `catch 404 → v2`).
// The react path being gone must not fail the request.
func TestChatReact404FallsBackToV2(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH,
		testutil.ScriptStep{StatusCode: 404},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
	)
	res, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-flash", React: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := up.RequestsFor(testPH)
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (react 404 then v2 fallback)", len(reqs))
	}
	if reqs[0].Path != "/open-apis/react/chat" {
		t.Errorf("first path = %q, want react", reqs[0].Path)
	}
	if reqs[1].Path != "/open-apis/bot/chat" {
		t.Errorf("fallback path = %q, want v2 studio", reqs[1].Path)
	}
	// Same turn identity on the retry: the SPA replays the same payload.
	if reqs[0].MsgID != reqs[1].MsgID || reqs[0].ConvID != reqs[1].ConvID {
		t.Errorf("fallback must replay the same msgId/conversationId: %q/%q vs %q/%q",
			reqs[0].MsgID, reqs[0].ConvID, reqs[1].MsgID, reqs[1].ConvID)
	}
	if res.Content != "Hello world" {
		t.Errorf("content = %q, want the v2 fixture's text", res.Content)
	}
}

// REACT-004: a non-404 failure on react does NOT fall back — the upstream's
// own refusal (451/401/…) passes through verbatim, same contract as v2.
func TestChatReactNon404DoesNotFallBack(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{StatusCode: 451})
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-flash", React: true,
	})
	var ue *UpstreamError
	if err == nil || !asUpstreamError(err, &ue) || ue.StatusCode != 451 {
		t.Fatalf("err = %v, want the 451 passthrough", err)
	}
	if n := len(up.RequestsFor(testPH)); n != 1 {
		t.Errorf("requests = %d, want 1 (no fallback on a non-404)", n)
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
