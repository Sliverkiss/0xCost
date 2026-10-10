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
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// These cases cover the client-facing SSE transport (ADR 0034, restoring the
// streaming half of ADR 0017). The upstream is SSE in both transports, so
// everything here is about what the *client* receives: frame order, the commit
// boundary, and where an error can still be a status code.

// frame is one parsed data frame of a chat stream.
type frame struct {
	Raw   string
	Done  bool
	Chunk adapter.Chunk
}

// readStreamFrames reads an SSE chat stream into its data frames, in order.
// Comment lines (keepalives) are skipped, which is what a client does.
func readStreamFrames(t *testing.T, resp *http.Response) []frame {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return parseStreamFrames(t, string(body))
}

// parseStreamFrames turns a raw SSE body into its data frames.
func parseStreamFrames(t *testing.T, body string) []frame {
	t.Helper()
	var out []frame
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		payload := ""
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			out = append(out, frame{Raw: payload, Done: true})
			continue
		}
		var c adapter.Chunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("frame is not a chunk: %v (raw=%s)", err, payload)
		}
		out = append(out, frame{Raw: payload, Chunk: c})
	}
	return out
}

// streamBody builds a chat body with stream:true plus optional extra fields.
func streamBody(model, extra string, msgs ...[2]string) string {
	b := testutil.ChatBody(model, true, msgs...)
	if extra == "" {
		return b
	}
	return strings.Replace(b, `"stream":true`, `"stream":true,`+extra, 1)
}

func contentOf(frames []frame) string {
	var sb strings.Builder
	for _, f := range frames {
		for _, c := range f.Chunk.Choices {
			sb.WriteString(c.Delta.Content)
		}
	}
	return sb.String()
}

func reasoningOf(frames []frame) string {
	var sb strings.Builder
	for _, f := range frames {
		for _, c := range f.Chunk.Choices {
			sb.WriteString(c.Delta.ReasoningContent)
		}
	}
	return sb.String()
}

func lastFrameIsDone(t *testing.T, frames []frame) {
	t.Helper()
	if len(frames) == 0 || !frames[len(frames)-1].Done {
		t.Errorf("stream does not end with [DONE]: %+v", frames)
	}
}

// STREAM-011: the happy path — role chunk, content deltas, terminal choice
// chunk, usage chunk, [DONE]; one id and one model for the whole stream.
func TestStreamShape(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s, want 200", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	frames := readStreamFrames(t, resp)
	if len(frames) < 6 {
		t.Fatalf("frames = %d, want role + 3 content + finish + usage + [DONE]", len(frames))
	}
	if got := frames[0].Chunk.Choices[0].Delta.Role; got != "assistant" {
		t.Errorf("first chunk role = %q, want assistant", got)
	}
	if fr := frames[0].Chunk.Choices[0].FinishReason; fr != nil {
		t.Errorf("first chunk finish_reason = %v, want null", *fr)
	}
	if got := contentOf(frames); got != "Hello world" {
		t.Errorf("content = %q, want the fixture's three deltas joined", got)
	}
	// One identity, one model, every frame a chunk (SDD §5.4).
	id := frames[0].Chunk.ID
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %q, want a chatcmpl- id", id)
	}
	for _, f := range frames {
		if f.Done {
			continue
		}
		if f.Chunk.ID != id || f.Chunk.Model != "mimo-v2.6-pro" {
			t.Errorf("frame identity drifted: id=%q model=%q", f.Chunk.ID, f.Chunk.Model)
		}
		if f.Chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", f.Chunk.Object)
		}
	}
	// Terminal choice chunk: empty delta, finish_reason stop.
	var sawFinish bool
	for _, f := range frames {
		if f.Done {
			continue
		}
		for _, c := range f.Chunk.Choices {
			if c.FinishReason != nil {
				sawFinish = true
				if *c.FinishReason != "stop" {
					t.Errorf("finish_reason = %q, want stop", *c.FinishReason)
				}
				if c.Delta.Content != "" || c.Delta.Role != "" {
					t.Errorf("terminal delta is not empty: %+v", c.Delta)
				}
			}
		}
	}
	if !sawFinish {
		t.Error("no terminal choice chunk with finish_reason")
	}
	// The usage chunk carries the mapped numbers and cannot clobber a
	// finish_reason (choices:[]).
	var usage *adapter.Usage
	for _, f := range frames {
		if f.Done {
			continue
		}
		if f.Chunk.Usage != nil {
			if len(f.Chunk.Choices) != 0 {
				t.Errorf("usage chunk has %d choices, want 0", len(f.Chunk.Choices))
			}
			u := *f.Chunk.Usage
			usage = &u
		}
	}
	if usage == nil {
		t.Fatal("no usage chunk")
	}
	if usage.PromptTokens != 2242 || usage.CompletionTokens != 78 || usage.TotalTokens != 2320 {
		t.Errorf("usage = %+v, want the fixture's numbers", *usage)
	}
	lastFrameIsDone(t, frames)
}

// STREAM-012: thinking deltas ride the same stream as reasoning_content, and
// the platform's marker bytes never reach the client.
func TestStreamThinkingDeltas(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEThinking})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "2+2?"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	frames := readStreamFrames(t, resp)
	if got := reasoningOf(frames); got != "Let me check the math." {
		t.Errorf("reasoning = %q, want the think block's text", got)
	}
	if got := contentOf(frames); got != "The answer is 4." {
		t.Errorf("content = %q, want the answer only", got)
	}
	for _, f := range frames {
		if strings.Contains(f.Raw, "<think>") || strings.Contains(f.Raw, `\u0000`) {
			t.Errorf("marker leaked into the stream: %s", f.Raw)
		}
	}
	lastFrameIsDone(t, frames)
}

// STREAM-013: thinking off (reasoning_effort:none) means no reasoning_content
// anywhere in the stream — the same rule the JSON path follows (TDD API-010).
func TestStreamThinkingOffSuppressesReasoning(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEThinking})

	resp, err := ts.Chat(streamBody("mimo-v2.6-pro", `"reasoning_effort":"none"`, [2]string{"user", "2+2?"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := readStreamFrames(t, resp)
	if got := reasoningOf(frames); got != "" {
		t.Errorf("reasoning = %q, want nothing when thinking is off", got)
	}
	if got := contentOf(frames); got != "The answer is 4." {
		t.Errorf("content = %q (the answer must survive the suppression)", got)
	}
	lastFrameIsDone(t, frames)
}

// STREAM-014: a web-search answer carries the citation extension chunk
// (choices:[] keeps it ignorable), and include_citations:false drops it while
// leaving the answer intact.
func TestStreamSearchCitations(t *testing.T) {
	t.Run("citations present", func(t *testing.T) {
		ts := testserver.NewTestServer(t, oneAccount(), nil)
		ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEWebSearch})
		resp, err := ts.Chat(streamBody("mimo-v2.6-pro", `"web_search_status":"enabled"`, [2]string{"user", "搜一下"}), "")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		frames := readStreamFrames(t, resp)
		var cites []adapter.Citation
		for _, f := range frames {
			if len(f.Chunk.Citations) > 0 {
				if len(f.Chunk.Choices) != 0 {
					t.Errorf("citation chunk has choices, want none")
				}
				if f.Chunk.Usage != nil {
					t.Errorf("citation chunk carries usage")
				}
				cites = f.Chunk.Citations
			}
		}
		if len(cites) == 0 {
			t.Fatalf("no citation chunk in the stream: %+v", frames)
		}
		if cites[0].URL == "" {
			t.Errorf("citation has no url: %+v", cites[0])
		}
		lastFrameIsDone(t, frames)
	})

	t.Run("include_citations false", func(t *testing.T) {
		ts := testserver.NewTestServer(t, oneAccount(), nil)
		ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEWebSearch})
		resp, err := ts.Chat(streamBody("mimo-v2.6-pro",
			`"web_search_status":"enabled","include_citations":false`, [2]string{"user", "搜一下"}), "")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		frames := readStreamFrames(t, resp)
		for _, f := range frames {
			if len(f.Chunk.Citations) > 0 {
				t.Errorf("citations leaked despite include_citations:false: %s", f.Raw)
			}
		}
		if got := contentOf(frames); got == "" {
			t.Error("the answer was suppressed along with the citations")
		}
		lastFrameIsDone(t, frames)
	})
}

// STREAM-015: an upstream failure after the answer started cannot become a
// status code, so it travels in-band with the full error contract (all four
// keys, a real code — ADR 0032/0033) and then the stream ends.
func TestStreamMidStreamErrorInBand(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEError})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the response was already committed)", resp.StatusCode)
	}
	frames := readStreamFrames(t, resp)
	if got := contentOf(frames); got != "partial" {
		t.Errorf("content = %q, want the deltas that arrived before the failure", got)
	}
	var errFrame string
	for _, f := range frames {
		if strings.Contains(f.Raw, `"error"`) {
			errFrame = f.Raw
		}
	}
	if errFrame == "" {
		t.Fatalf("no in-band error frame: %+v", frames)
	}
	var env struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(errFrame), &env); err != nil {
		t.Fatalf("error frame is not an error object: %v (%s)", err, errFrame)
	}
	if env.Error.Message == "" || env.Error.Type == "" || env.Error.Code == "" {
		t.Errorf("error frame is missing keys: %s", errFrame)
	}
	if !strings.Contains(env.Error.Message, "upstream exploded") {
		t.Errorf("message = %q, want the platform's own wording passed through", env.Error.Message)
	}
	lastFrameIsDone(t, frames)
}

// STREAM-016: a failure before the first delta is still pre-response, so the
// request fails over to another account and the client sees one clean stream —
// never a partial stream followed by a retry.
func TestStreamPreFirstByteFailover(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	// Round-robin serves acc2 first (the cursor starts on the second identity),
	// so the 502 goes there and the retry lands on acc1 with a live fixture.
	ts.Upstream.Script(testutil.TestPH2, testutil.ScriptStep{StatusCode: http.StatusBadGateway})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s, want 200 via failover", resp.StatusCode, raw)
	}
	frames := readStreamFrames(t, resp)
	if got := contentOf(frames); got != "Hello world" {
		t.Errorf("content = %q, want the full answer from the second account", got)
	}
	lastFrameIsDone(t, frames)
	phs := map[string]int{}
	for _, r := range ts.Upstream.Requests() {
		phs[r.PH]++
	}
	if phs[testutil.TestPH] != 1 || phs[testutil.TestPH2] != 1 {
		t.Errorf("per-account hits = %v, want one attempt each (the failed one wrote nothing)", phs)
	}
	if ts.Pool.StateOf("acc2") != pool.StateCooldown {
		t.Errorf("acc2 state = %v, want cooldown (a real transport failure is still evidence)", ts.Pool.StateOf("acc2"))
	}
}

// STREAM-017: an answer that never arrives is a JSON error, not an empty SSE
// stream — nothing was committed, so the client can still read a status code.
func TestStreamEmptyAnswerStaysJSON(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	for _, ph := range []string{testutil.TestPH, testutil.TestPH2} {
		ts.Upstream.Script(ph, testutil.ScriptStep{Body: testutil.FixtureSSEEmpty})
	}
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("status = 200, want an error: an empty answer is a failure")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON (nothing was streamed)", ct)
	}
	status, _, typ, code := readErr(t, resp)
	if status == 0 || typ == "" || code == "" {
		t.Errorf("error body incomplete: %d %q %q", status, typ, code)
	}
}

// STREAM-018: the ultraspeed channel (fastchat prefix upstream) serves the same
// stream shape — the transport is ours, not the channel's.
func TestStreamUltraspeedChannel(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEFastchat})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro-ultraspeed-studio", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	frames := readStreamFrames(t, resp)
	if got := contentOf(frames); got != "fast reply" {
		t.Errorf("content = %q, want the fastchat fixture's text", got)
	}
	lastFrameIsDone(t, frames)
	// The fastchat path is the one the ultraspeed model must use upstream.
	for _, r := range ts.Upstream.Requests() {
		if !strings.Contains(r.Path, "/fastchat/open-apis/bot/chat") {
			t.Errorf("upstream path = %q, want the fastchat prefix", r.Path)
		}
	}
}

// STREAM-022: while the upstream is silent mid-answer the committed stream is
// kept alive with SSE comments (the old API-019 contract), and the answer still
// assembles normally.
func TestStreamKeepaliveDuringUpstreamSilence(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		KeepaliveInterval: 30 * time.Millisecond,
		RequestTimeout:    10 * time.Second,
		LeaseTTL:          10 * time.Second,
	})
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{
		Body:           testutil.FixtureSSEBasic,
		MidStreamSplit: 200,
		StallMidStream: 200 * time.Millisecond,
	})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, ": keepalive") {
		t.Errorf("no keepalive during upstream silence: %q", body)
	}
	frames := parseStreamFrames(t, body)
	if got := contentOf(frames); got != "Hello world" {
		t.Errorf("content = %q, want the whole answer despite the keepalives", got)
	}
	lastFrameIsDone(t, frames)
	// Comments are comments: nothing may leak into a data frame.
	for _, f := range frames {
		if f.Done {
			continue
		}
		if strings.Contains(f.Raw, "keepalive") {
			t.Errorf("keepalive leaked into a data frame: %s", f.Raw)
		}
	}
}

// STREAM-023: the wire bytes match the recorded OpenAI golden, with only the
// per-request id and created timestamp normalized — the strongest shape check
// this project has for the stream (internal/testutil/testdata, TDD API-002).
func TestStreamMatchesGoldensFrameByFrame(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := normalizeStream(t, string(raw))
	want := string(testutil.FixtureOpenAIStreamExpected)
	if got != want {
		t.Errorf("stream does not match the golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

var (
	idRe      = regexp.MustCompile(`"id":"chatcmpl-[0-9a-f]+"`)
	createdRe = regexp.MustCompile(`"created":\d+`)
)

// normalizeStream replaces the per-request id and timestamp so a run can be
// compared with the recorded golden byte-for-byte.
func normalizeStream(t *testing.T, body string) string {
	t.Helper()
	var frames []string
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(block), ":") {
			continue // comments are not part of the golden
		}
		line := strings.TrimSpace(block)
		if line == "data: [DONE]" {
			frames = append(frames, line)
			continue
		}
		// Substitution, not re-marshalling: a decode/encode round trip would
		// reorder the keys and hide a real shape difference behind a
		// normalization step.
		line = idRe.ReplaceAllString(line, `"id":"chatcmpl-00000000000000000000000000000000"`)
		line = createdRe.ReplaceAllString(line, `"created":0`)
		frames = append(frames, line)
	}
	// The golden separates frames with a blank line, including after [DONE].
	return strings.Join(frames, "\n\n") + "\n\n"
}
