package adapter

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Real MiMo thinking markers per SDD §2.5 / thinking.js: the raw stream sends
// the marker followed by a NUL byte ("mind\0" ... "</mind>\0"); the splitter
// strips NULs and matches the bare markers.
const (
	openMk  = ThinkOpen
	closeMk = ThinkClose
)

// ============================================================================
// Thinking splitter — TDD SSE-006..012 (SDD §2.5)
// ============================================================================

// SSE-006: basic think-block split; NUL never emitted.
func TestSplitterBasicThinkBlock(t *testing.T) {
	if openMk != "<think>" || closeMk != "</think>" {
		t.Fatalf("package markers %q/%q must match SDD §2.5", openMk, closeMk)
	}
	s := NewThinkSplitter()
	in := openMk + "\x00Let me think.\x00" + closeMk + "\x00Answer"
	var pieces []Piece
	pieces = append(pieces, s.Push(in)...)
	pieces = append(pieces, s.Flush()...)
	r, c := MergePieces(pieces)
	if r != "Let me think." {
		t.Errorf("reasoning=%q", r)
	}
	if c != "Answer" {
		t.Errorf("content=%q", c)
	}
	sawReasoning, sawContent := false, false
	for _, p := range pieces {
		if strings.Contains(p.Text, "\x00") || strings.Contains(p.Text, openMk) || strings.Contains(p.Text, closeMk) {
			t.Errorf("emitted text contains NUL or marker: %q", p.Text)
		}
		switch p.Type {
		case "reasoning":
			sawReasoning = true
		case "content":
			sawContent = true
		default:
			t.Errorf("unknown piece type %q", p.Type)
		}
	}
	if !sawReasoning || !sawContent {
		t.Errorf("expected both reasoning and content pieces, got %+v", pieces)
	}
}

// SSE-007: markers split across chunk boundaries, property-tested over every
// 1-byte split point of a fixture.
func TestSplitterMarkerSplitAcrossChunksProperty(t *testing.T) {
	fixture := "pre" + openMk + "\x00reasoning" + closeMk + "\x00post"
	for i := 1; i < len(fixture); i++ {
		s := NewThinkSplitter()
		var pieces []Piece
		pieces = append(pieces, s.Push(fixture[:i])...)
		pieces = append(pieces, s.Push(fixture[i:])...)
		pieces = append(pieces, s.Flush()...)
		r, c := MergePieces(pieces)
		if r != "reasoning" || c != "prepost" {
			t.Fatalf("split at %d: reasoning=%q content=%q", i, r, c)
		}
		for _, p := range pieces {
			if strings.Contains(p.Text, openMk) || strings.Contains(p.Text, closeMk) || strings.Contains(p.Text, "\x00") {
				t.Fatalf("split at %d: leaked marker/NUL in %q", i, p.Text)
			}
		}
	}
}

// SSE-007 (two-marker variant): both open and close markers straddled mid-word.
func TestSplitterBothMarkersStraddled(t *testing.T) {
	s := NewThinkSplitter()
	var pieces []Piece
	pieces = append(pieces, s.Push("hello "+openMk[:2])...)
	pieces = append(pieces, s.Push(openMk[2:]+"\x00secret")...)
	pieces = append(pieces, s.Push(" thoughts "+closeMk[:1])...)
	pieces = append(pieces, s.Push(closeMk[1:]+"\x00answer")...)
	pieces = append(pieces, s.Flush()...)
	r, c := MergePieces(pieces)
	if r != "secret thoughts " || c != "hello answer" {
		t.Fatalf("reasoning=%q content=%q", r, c)
	}
}

// SSE-008: stray NULs outside markers stripped from output.
func TestSplitterStrayNULStripped(t *testing.T) {
	s := NewThinkSplitter()
	pieces := s.Push("a\x00b\x00c")
	pieces = append(pieces, s.Flush()...)
	if len(pieces) != 1 || pieces[0].Text != "abc" {
		t.Fatalf("pieces=%+v", pieces)
	}
}

// SSE-009: unterminated think block — everything remaining is reasoning.
func TestSplitterUnterminatedThinkBlock(t *testing.T) {
	s := NewThinkSplitter()
	var pieces []Piece
	pieces = append(pieces, s.Push(openMk+"\x00partial")...)
	pieces = append(pieces, s.Push(" reasoning")...)
	pieces = append(pieces, s.Flush()...)
	r, c := MergePieces(pieces)
	if r != "partial reasoning" || c != "" {
		t.Fatalf("reasoning=%q content=%q", r, c)
	}
}

// SSE-010: no markers at all — everything is content.
func TestSplitterNoMarkers(t *testing.T) {
	s := NewThinkSplitter()
	var pieces []Piece
	pieces = append(pieces, s.Push("just ")...)
	pieces = append(pieces, s.Push("text")...)
	pieces = append(pieces, s.Flush()...)
	r, c := MergePieces(pieces)
	if r != "" || c != "just text" {
		t.Fatalf("reasoning=%q content=%q", r, c)
	}
}

// SSE-011: two separate think blocks interleaved with text; order preserved.
func TestSplitterMultipleThinkBlocks(t *testing.T) {
	s := NewThinkSplitter()
	var pieces []Piece
	pieces = append(pieces, s.Push("a"+openMk+"\x001"+closeMk+"\x00b"+openMk+"\x002"+closeMk+"\x00c")...)
	pieces = append(pieces, s.Flush()...)
	wantOrder := []struct {
		typ  string
		text string
	}{
		{"content", "a"}, {"reasoning", "1"}, {"content", "b"}, {"reasoning", "2"}, {"content", "c"},
	}
	var flat []string
	for _, p := range pieces {
		flat = append(flat, p.Type+":"+p.Text)
	}
	var wantFlat []string
	for _, w := range wantOrder {
		wantFlat = append(wantFlat, w.typ+":"+w.text)
	}
	if strings.Join(flat, "|") != strings.Join(wantFlat, "|") {
		t.Errorf("order/segments differ:\n got %v\nwant %v", flat, wantFlat)
	}
}

// ============================================================================

// ============================================================================
// Chunk shaping — TDD API-003 (SDD §5.4)
// ============================================================================

func TestChunkShape(t *testing.T) {
	id := NewChatID()
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id %q", id)
	}
	role := ShapeRoleChunk(id, "mimo-v2.6-pro", 123)
	var m map[string]any
	if err := json.Unmarshal([]byte(EncodeChunk(role)), &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 5 { // exactly {id, object, created, model, choices}
		t.Errorf("chunk keys = %v", m)
	}
	if m["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v", m["object"])
	}
	ch0 := role.Choices[0]
	if ch0.Index != 0 || ch0.Delta.Role != "assistant" || ch0.Delta.Content != "" || ch0.FinishReason != nil {
		t.Errorf("role chunk choice = %+v", ch0)
	}

	txt := ShapeTextChunk(id, "m", 123, Piece{Type: "content", Text: "hi"})
	if txt.Choices[0].Delta.Content != "hi" || txt.Choices[0].Delta.Role != "" {
		t.Errorf("text chunk = %+v", txt.Choices[0])
	}
	th := ShapeTextChunk(id, "m", 123, Piece{Type: "reasoning", Text: "hmm"})
	if th.Choices[0].Delta.ReasoningContent != "hmm" || th.Choices[0].Delta.Content != "" {
		t.Errorf("reasoning chunk = %+v", th.Choices[0])
	}
	fin := ShapeFinishChunk(id, "m", 123)
	if fin.Choices[0].Delta != (Delta{}) || fin.Choices[0].FinishReason == nil || *fin.Choices[0].FinishReason != "stop" {
		t.Errorf("finish chunk = %+v", fin.Choices[0])
	}
	u := ShapeUsageChunk(id, "m", 123, Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	if len(u.Choices) != 0 || u.Usage == nil || u.Usage.TotalTokens != 3 {
		t.Errorf("usage chunk = %+v", u)
	}
	if role.ID != txt.ID || txt.ID != fin.ID || fin.ID != u.ID {
		t.Errorf("chunk ids diverge")
	}
}

// ============================================================================
// Usage mapping — TDD API-004 (SDD §5.4)
// ============================================================================

func TestMapUsage(t *testing.T) {
	t.Run("last-event mapping with native details", func(t *testing.T) {
		u := &MiMoUsage{
			PromptTokens:     2242,
			CompletionTokens: 78,
			TotalTokens:      2320,
			NativeUsage: &NativeUsage{
				PromptDetails:     &NativeCached{CachedTokens: 100},
				CompletionDetails: &NativeThink{ReasoningTokens: 42},
			},
		}
		got := MapUsage(u)
		if got.PromptTokens != 2242 || got.CompletionTokens != 78 || got.TotalTokens != 2320 {
			t.Errorf("usage = %+v", got)
		}
		if got.PromptDetails == nil || got.PromptDetails.CachedTokens != 100 {
			t.Errorf("prompt details = %+v", got.PromptDetails)
		}
		if got.CompletionDetails == nil || got.CompletionDetails.ReasoningTokens != 42 {
			t.Errorf("completion details = %+v", got.CompletionDetails)
		}
	})
	t.Run("nil usage maps to zeros, never invented", func(t *testing.T) {
		got := MapUsage(nil)
		if got != (Usage{}) {
			t.Errorf("nil usage = %+v", got)
		}
	})
	t.Run("details omitted when upstream sent none", func(t *testing.T) {
		got := MapUsage(&MiMoUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
		if got.PromptDetails != nil || got.CompletionDetails != nil {
			t.Errorf("unexpected details = %+v", got)
		}
	})
}

func TestAddUsage(t *testing.T) {
	a := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, PromptDetails: &UsageDetails{CachedTokens: 4}}
	b := Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
	s := AddUsage(a, b)
	if s.PromptTokens != 17 || s.CompletionTokens != 8 || s.TotalTokens != 25 {
		t.Errorf("sum = %+v", s)
	}
	if s.PromptDetails == nil || s.PromptDetails.CachedTokens != 4 {
		t.Errorf("cached sum = %+v", s.PromptDetails)
	}
}

// ============================================================================
// System-message splice — TDD API-017 (SDD §5.3)
// ============================================================================

func TestSystemSplice(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "A"},
		{Role: "system", Content: "B"},
		{Role: "user", Content: "U"},
	}
	plan, err := PlanFullReplay(msgs, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Queries) != 1 || plan.Queries[0] != "A\n\nB\n\n---\n\nU" {
		t.Fatalf("queries = %+v", plan.Queries)
	}
}

// ============================================================================
// Turn planning (SDD §5.3)
// ============================================================================

func TestPlanFullReplay(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
	}
	plan, err := PlanFullReplay(msgs, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Queries) != 2 {
		t.Fatalf("queries = %+v", plan.Queries)
	}
	if plan.Queries[0] != "sys\n\n---\n\nq1" {
		t.Errorf("q0 = %q", plan.Queries[0])
	}
	if plan.Queries[1] != "q2" {
		t.Errorf("q1 = %q", plan.Queries[1])
	}

	if _, err := PlanFullReplay([]Msg{{Role: "system", Content: "only"}}, 20); err == nil {
		t.Errorf("system-only messages must be rejected")
	}
	var tooMany []Msg
	for i := 0; i < 25; i++ {
		tooMany = append(tooMany, Msg{Role: "user", Content: "x"})
	}
	if _, err := PlanFullReplay(tooMany, 20); err == nil {
		t.Errorf("25 turns over cap 20 must be rejected")
	}
}

func TestPlanDelta(t *testing.T) {
	msgs := []Msg{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
	}
	plan, err := PlanDelta(msgs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Queries) != 1 || plan.Queries[0] != "q2" {
		t.Fatalf("queries = %+v", plan.Queries)
	}
	if _, err := PlanDelta(msgs, 3); err == nil {
		t.Errorf("no delta beyond frontier must be rejected")
	}
}

// ============================================================================
// Model normalization — TDD API-013 (SDD §2.6, §5.2)
// ============================================================================

func TestNormalizeModel(t *testing.T) {
	cases := []struct {
		in, def  string
		want     string
		fastchat bool
		wantErr  bool
	}{
		{in: "mimo-v2.6-pro-studio", def: "mimo-v2.6-pro", want: "mimo-v2.6-pro"},
		{in: "mimo-v2.6-pro-ultraspeed-studio", def: "mimo-v2.6-pro", want: "mimo-v2.6-pro-ultraspeed-studio", fastchat: true},
		{in: "auto", def: "mimo-v2.6-pro", want: "mimo-v2.6-pro"},
		{in: "", def: "mimo-v2.6-pro", want: "mimo-v2.6-pro"},
		{in: "foo", def: "mimo-v2.6-pro", wantErr: true},
	}
	for _, tc := range cases {
		got, fc, err := NormalizeModel(tc.in, tc.def, DefaultModels)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", tc.in)
				continue
			}
			if !strings.Contains(err.Error(), "mimo-v2.6-pro") || !strings.Contains(err.Error(), tc.in) {
				t.Errorf("%q: error should name model and available ids: %v", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want || fc != tc.fastchat {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", tc.in, got, fc, tc.want, tc.fastchat)
		}
	}
}

// ADR 0015: the fastchat cap is the platform's own limit (a 60,000-char message
// is refused upstream with "query is too long"); every other channel gets a
// guardrail, not a guess — the old 90,000 rejected work the platform serves
// (95,000 and 100,100-char messages are answered; 105,000 is refused).
func TestMaxQueryChars(t *testing.T) {
	if got := MaxQueryChars("mimo-v2.6-pro-ultraspeed-studio"); got != 50000 {
		t.Errorf("fastchat cap = %d, want 50000 (the Studio's ultraspeedQueryLimitLength)", got)
	}
	if got := MaxQueryChars("mimo-v2.6-pro"); got != 102400 {
		t.Errorf("normal cap = %d, want the measured 102400-character platform ceiling", got)
	}
	if MaxQueryChars("mimo-v2.6-pro") < 101000 {
		t.Error("the cap must not sit below the largest message the platform was measured to serve (101,000)")
	}
	if MaxQueryChars("mimo-v2.6-pro") > 102400 {
		t.Error("the cap must not sit above the measured boundary: it would forward what the platform refuses")
	}
}

func TestRandomHex32(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := RandomHex32()
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(s) {
			t.Fatalf("bad id %q", s)
		}
		if seen[s] {
			t.Fatalf("duplicate id %q", s)
		}
		seen[s] = true
	}
}

func TestShapeCompletion(t *testing.T) {
	c := ShapeCompletion("chatcmpl-x", "mimo-v2.6-pro", 99, "answer", "", Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	b, _ := json.Marshal(c)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, has := msg["reasoning_content"]; has {
		t.Errorf("reasoning_content must be omitted when empty")
	}
	if msg["content"] != "answer" || msg["role"] != "assistant" {
		t.Errorf("message = %v", msg)
	}
	if m["object"] != "chat.completion" || m["model"] != "mimo-v2.6-pro" {
		t.Errorf("shape = %v", m)
	}
}

// The composed-length helper must mirror what the planner sends: the preamble
// counts, and it is exactly the systems joined plus the "\n\n---\n\n"
// separator the planner uses (ADR 0015).
func TestComposedFirstQueryLen(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "abc"},
		{Role: "system", Content: "def"},
		{Role: "user", Content: "hello"},
		{Role: "user", Content: "second"},
	}
	composed, overhead := ComposedFirstQueryLen(msgs)
	// "abc" + "\n\n" + "def" + "\n\n---\n\n" + "hello"
	wantOverhead := len([]rune("abc\n\ndef\n\n---\n\n"))
	if overhead != wantOverhead {
		t.Fatalf("overhead = %d, want %d", overhead, wantOverhead)
	}
	if composed != wantOverhead+5 {
		t.Fatalf("composed = %d, want %d (preamble + first user message)", composed, wantOverhead+5)
	}

	// No system preamble: composed is just the first user message.
	if composed, overhead := ComposedFirstQueryLen([]Msg{{Role: "user", Content: "hey"}}); composed != 3 || overhead != 0 {
		t.Fatalf("composed = %d overhead = %d, want 3/0", composed, overhead)
	}

	// Blank system messages are dropped, exactly as the planner drops them.
	if composed, overhead := ComposedFirstQueryLen([]Msg{{Role: "system", Content: "   "}, {Role: "user", Content: "hey"}}); composed != 3 || overhead != 0 {
		t.Fatalf("composed = %d overhead = %d, want 3/0 (a blank system message is not a preamble)", composed, overhead)
	}
}
