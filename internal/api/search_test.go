package api_test

// Search switch and citation passthrough at the OpenAI edge (M19).
//
// web_search_status is the client's handle on the upstream search switch, and
// ":online" is the shorthand OpenAI-compatible clients already send. Both must
// reach the upstream as modelConfig.webSearchStatus, and the results the
// upstream returns inline must come back as citations — otherwise the client
// pays for a search it cannot see.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// searchBody builds a chat request with the M19 fields spliced in.
func searchBody(model string, stream bool, extra map[string]any) string {
	var b map[string]any
	if err := json.Unmarshal([]byte(testutil.ChatBody(model, stream, [2]string{"user", "什么是最新消息？"})), &b); err != nil {
		panic(err)
	}
	for k, v := range extra {
		b[k] = v
	}
	raw, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// WS-001: web_search_status reaches the upstream switch verbatim; omitting it
// leaves search off (it costs latency and tokens).
func TestWS001SearchStatusForwarded(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"explicit auto", searchBody("mimo-v2.6-flash", false, map[string]any{"web_search_status": "auto"}), "auto"},
		{"explicit enabled", searchBody("mimo-v2.6-flash", false, map[string]any{"web_search_status": "enabled"}), "enabled"},
		{"omitted", testutil.ChatBody("mimo-v2.6-flash", false, [2]string{"user", "hi"}), "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct := oneAccount()
			ts := testserver.NewTestServer(t, acct, nil)
			ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

			status, _ := chatOnce(t, ts, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			reqs := ts.Upstream.Requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(reqs))
			}
			if reqs[0].WebSearchStatus != tc.want {
				t.Errorf("upstream webSearchStatus = %q, want %q", reqs[0].WebSearchStatus, tc.want)
			}
		})
	}
}

// WS-002: an unknown switch is rejected before any upstream call — silently
// coercing it would hide a client typo behind a working-looking request.
func TestWS002InvalidSearchStatusRejected(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)

	status, body := chatOnce(t, ts, searchBody("mimo-v2.6-flash", false, map[string]any{"web_search_status": "sometimes"}))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, body)
	}
	if !strings.Contains(body, "unsupported_search_status") {
		t.Errorf("body = %q, want code unsupported_search_status", body)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (rejected client-side)", n)
	}
}

// WS-003: the ":online" suffix is shorthand for enabled, the model id sent
// upstream is the bare id, and the response echoes the id the client asked for.
func TestWS003OnlineSuffix(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	status, body := chatOnce(t, ts, testutil.ChatBody("mimo-v2.6-flash:online", true, [2]string{"user", "hi"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(reqs))
	}
	if reqs[0].Model != "mimo-v2.6-flash" {
		t.Errorf("upstream model = %q, want the bare id mimo-v2.6-flash", reqs[0].Model)
	}
	if reqs[0].WebSearchStatus != "enabled" {
		t.Errorf("upstream webSearchStatus = %q, want enabled (suffix shorthand)", reqs[0].WebSearchStatus)
	}
	if !strings.Contains(body, "mimo-v2.6-flash:online") {
		t.Errorf("stream = %s, want the requested id echoed including :online", body)
	}
}

// WS-004: an explicit switch wins over the suffix — the client's precise
// instruction beats the shorthand.
func TestWS004ExplicitStatusBeatsSuffix(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := searchBody("mimo-v2.6-flash:online", false, map[string]any{"web_search_status": "disabled"})
	if status, resp := chatOnce(t, ts, body); status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, resp)
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 || reqs[0].WebSearchStatus != "disabled" {
		t.Fatalf("upstream webSearchStatus = %+v, want disabled (explicit field wins)", reqs)
	}
}

// WS-005: the upstream's inline results come back as annotations plus the
// top-level citations array on a non-streaming answer.
func TestWS005NonStreamCitations(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEWebSearch})

	status, body := chatOnce(t, ts, searchBody("mimo-v2.6-flash", false, map[string]any{"web_search_status": "auto"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	for _, want := range []string{
		`"annotations":[{"type":"url_citation"`,
		`"url":"https://aistudio.xiaomimimo.com/docs"`,
		`"citations":[{"index":0`,
		`"display_url":"aistudio.xiaomimimo.com"`,
		`"title":"MiMo Studio 官方文档"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s\nwant it to contain %s", body, want)
		}
	}
	// The page body must not be forwarded: tens of KB of upstream text would
	// wreck the client's context.
	if strings.Contains(body, "页面正文") {
		t.Errorf("body carries the upstream page text: %s", body)
	}
}

// WS-006: include_citations:false suppresses the citation payload while the
// search itself still runs (the model may rely on it).
func TestWS006IncludeCitationsFalse(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEWebSearch})

	body := searchBody("mimo-v2.6-flash", false, map[string]any{
		"web_search_status": "auto", "include_citations": false,
	})
	status, resp := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, resp)
	}
	if strings.Contains(resp, "citations") || strings.Contains(resp, "annotations") {
		t.Errorf("body = %s, want no citation payload with include_citations:false", resp)
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 || reqs[0].WebSearchStatus != "auto" {
		t.Fatalf("upstream = %+v, want the search to still run", reqs)
	}
}

// WS-007: on a stream the citations arrive as one extra chunk with empty
// choices, placed before the finish chunk so a client that stops at
// finish_reason still sees the sources.
func TestWS007StreamCitationsChunkOrder(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEWebSearch})

	status, body := chatOnce(t, ts, testutil.ChatBody("mimo-v2.6-flash", true, [2]string{"user", "hi"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	citIdx := strings.Index(body, `"citations":[`)
	finIdx := strings.Index(body, `"finish_reason":"stop"`)
	doneIdx := strings.Index(body, "[DONE]")
	if citIdx < 0 {
		t.Fatalf("stream = %s, want a citations chunk", body)
	}
	if finIdx < 0 || doneIdx < 0 {
		t.Fatalf("stream = %s, want finish chunk and [DONE]", body)
	}
	if !(citIdx < finIdx && finIdx < doneIdx) {
		t.Errorf("chunk order: citations@%d finish@%d done@%d, want citations before finish before [DONE]", citIdx, finIdx, doneIdx)
	}
	if !strings.Contains(body, `"choices":[]`) {
		t.Errorf("stream = %s, want the citations chunk to carry empty choices", body)
	}
	// It must be exactly one chunk, not one per result.
	if n := strings.Count(body, `"citations":[`); n != 1 {
		t.Errorf("citations chunks = %d, want 1", n)
	}
}

// WS-008: a turn whose upstream returns nothing searchable carries no
// citations at all — no empty array pretending sources were lost.
func TestWS008NoCitationsWhenUpstreamSilent(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	status, body := chatOnce(t, ts, searchBody("mimo-v2.6-flash", false, map[string]any{"web_search_status": "enabled"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	if strings.Contains(body, "citations") || strings.Contains(body, "annotations") {
		t.Errorf("body = %s, want no citation fields when the upstream sent none", body)
	}
}

// wsStreamText pulls the concatenated content deltas out of an SSE body.
func wsStreamText(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ch); err != nil {
			continue
		}
		for _, c := range ch.Choices {
			out.WriteString(c.Delta.Content)
		}
	}
	return out.String()
}

// WS-009: citations never disturb the answer text itself.
func TestWS009SearchTurnStillAnswers(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEWebSearch})

	resp, err := ts.Chat(searchBody("mimo-v2.6-flash", true, map[string]any{"web_search_status": "auto"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if got := wsStreamText(t, string(raw)); got != "检索到两条来源。" {
		t.Errorf("streamed text = %q, want %q (citations must not leak into content)", got, "检索到两条来源。")
	}
}
