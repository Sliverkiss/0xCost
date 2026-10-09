package api_test

// The OpenAI-standard spellings of the two knobs this gateway exposes: thinking
// (reasoning_effort) and search (web_search_options). A standard client must be
// able to ask for both without learning a field of ours, and a typo must fail
// loudly instead of quietly doing nothing.
//
// Documentation: SDD §5.2, docs/TDD.md OSTD-001..007.

import (
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
