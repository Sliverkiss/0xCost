package mimo

// Web-search citation parsing (M19). The upstream returns results inline as
// `event:web_search` with a JSON array; before M19 the streamer dropped the
// event entirely, so a client could not see which pages the answer was built
// from. Parsing must be intolerant of nothing that could break a turn: a
// malformed payload is skipped, never fatal — search is a side channel, and a
// client that asked for an answer still gets one.

import (
	"context"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testutil"
)

// SSE-015: the real fixture's two results become citations, in arrival order,
// with the usedContent flag mapped and the page body dropped.
func TestSSE015WebSearchCitationsFromFixture(t *testing.T) {
	st, err := runFixture(t, testutil.FixtureSSEWebSearch)
	if err != nil {
		t.Fatalf("parseEvents: %v", err)
	}
	want := []struct {
		title, url, display, snippet string
		used                         bool
	}{
		{"MiMo Studio 官方文档", "https://aistudio.xiaomimimo.com/docs", "aistudio.xiaomimimo.com", "MiMo Studio 提供联网检索与语音能力。", true},
		{"示例站点", "https://www.example.cn/post", "www.example.cn", "示例摘要。", false},
	}
	if len(st.citations) != len(want) {
		t.Fatalf("citations = %d, want %d (%+v)", len(st.citations), len(want), st.citations)
	}
	for i, w := range want {
		got := st.citations[i]
		if got.Index != i {
			t.Errorf("citation[%d].Index = %d, want %d", i, got.Index, i)
		}
		if got.Title != w.title || got.URL != w.url || got.DisplayURL != w.display || got.Snippet != w.snippet {
			t.Errorf("citation[%d] = %+v, want title=%q url=%q display=%q snippet=%q",
				i, got, w.title, w.url, w.display, w.snippet)
		}
		if got.Used != w.used {
			t.Errorf("citation[%d].Used = %v, want %v", i, got.Used, w.used)
		}
	}
	// The page body must not survive into the citation: it is tens of KB and
	// would destroy a client's context budget.
	for _, c := range st.citations {
		if strings.Contains(c.Title+c.Snippet+c.DisplayURL, "页面正文") {
			t.Errorf("citation %d carries the upstream page body: %+v", c.Index, c)
		}
	}
	// Citations must not leak into the answer text.
	if _, content, _, _ := st.finish(); strings.Contains(content, "aistudio.xiaomimimo.com") {
		t.Errorf("content = %q, want no citation text in the answer", content)
	}
}

// SSE-016: a bad payload never breaks the turn, and every tolerated shape that
// should survive does.
func TestSSE016WebSearchTolerant(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		want      int
		wantError bool
		// wantUsed asserts the first citation's Used flag when the case has
		// one; the upstream has sent usedContent as a boolean AND as a string.
		wantUsed *bool
	}{
		{name: "malformed", data: `{not json at all`, wantError: true},
		{name: "null", data: `null`},
		{name: "empty array", data: `[]`},
		{name: "wrapped object", data: `{"data":[{"name":"a","url":"https://a.test"}]}`, want: 1},
		{name: "entry without url dropped", data: `[{"name":"no url","url":""},{"name":"ok","url":"https://b.test"}]`, want: 1},
		{name: "live shape: boolean true", data: `[{"url":"https://c.test","usedContent":true}]`, want: 1, wantUsed: boolPtr(true)},
		{name: "live shape: boolean false", data: `[{"url":"https://c.test","usedContent":false}]`, want: 1, wantUsed: boolPtr(false)},
		{name: "older capture: string True", data: `[{"url":"https://c.test","usedContent":"True"}]`, want: 1, wantUsed: boolPtr(true)},
		{name: "string FALSE", data: `[{"url":"https://c.test","usedContent":"FALSE"}]`, want: 1, wantUsed: boolPtr(false)},
		// The two live regressions: a boolean usedContent and an OBJECT
		// metadata each killed the entire 16-result payload when the struct
		// was strict (docs/web-surface-reverse.md §5 had both wrong).
		{name: "full live item", data: `[{"id":"2026-10-09_abc_1","text":"# 标题\n\n正文","metadata":{},"name":"t","url":"https://d.test","displayUrl":"d.test","snippet":"s","usedContent":false}]`, want: 1},
		{name: "metadata as string still parses", data: `[{"metadata":"{}","name":"t","url":"https://e.test"}]`, want: 1},
		{name: "name as an object costs only the title", data: `[{"name":{"zh":"t"},"url":"https://f.test","snippet":"s"}]`, want: 1},
		{name: "usedContent 1", data: `[{"url":"https://g.test","usedContent":1}]`, want: 1, wantUsed: boolPtr(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cits, err := parseWebSearch(tc.data)
			if tc.wantError {
				if err == nil {
					t.Fatal("err = nil, want a reported parse failure (never a silent skip)")
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantUsed != nil {
				if len(cits) == 0 {
					t.Fatalf("no citations parsed from %s", tc.data)
				}
				if cits[0].Used != *tc.wantUsed {
					t.Errorf("Used = %v, want %v", cits[0].Used, *tc.wantUsed)
				}
			}
			if len(cits) != tc.want {
				t.Fatalf("citations = %d (%+v), want %d", len(cits), cits, tc.want)
			}
			for _, c := range cits {
				if strings.TrimSpace(c.URL) == "" {
					t.Errorf("a citation without a URL survived: %+v", c)
				}
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// SSE-018: a field with an unexpected type degrades that field only —
// the citation survives (the live payloads are schema-loose).
func TestSSE018FieldToleranceDoesNotDropCitations(t *testing.T) {
	cits, err := parseWebSearch(`[{"name":{"zh":"标题"},"url":"https://h.test","snippet":null,"metadata":{},"usedContent":"True"}]`)
	if err != nil {
		t.Fatalf("err = %v, want nil (an odd field is not a parse failure)", err)
	}
	if len(cits) != 1 {
		t.Fatalf("citations = %d, want 1 (name object must not drop the citation)", len(cits))
	}
	if cits[0].URL != "https://h.test" || cits[0].Title != "" || !cits[0].Used {
		t.Errorf("citation = %+v, want url kept, title empty, used true", cits[0])
	}
}

// SSE-017: several web_search events in one turn accumulate, with indices
// continuing across events (the upstream may search more than once).
func TestSSE017WebSearchMultipleEvents(t *testing.T) {
	frames := "event:dialogId\ndata:{\"content\":\"dlg-1\"}\n\n" +
		"event:web_search\ndata:[{\"name\":\"one\",\"url\":\"https://one.test\"}]\n\n" +
		"event:web_search\ndata:[{\"name\":\"two\",\"url\":\"https://two.test\"},{\"name\":\"three\",\"url\":\"https://three.test\"}]\n\n" +
		"event:message\ndata:{\"type\":\"text\",\"content\":\"hi\"}\n\n" +
		"event:finish\ndata:{\"content\":\"[DONE]\"}\n\n"
	st := newStreamer()
	if _, err := st.parseEvents(context.Background(), strings.NewReader(frames)); err != nil {
		t.Fatalf("parseEvents: %v", err)
	}
	if len(st.citations) != 3 {
		t.Fatalf("citations = %d (%+v), want 3", len(st.citations), st.citations)
	}
	for i, c := range st.citations {
		if c.Index != i {
			t.Errorf("citation[%d].Index = %d, want %d (indices continue across events)", i, c.Index, i)
		}
	}
}
