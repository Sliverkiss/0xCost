package mimo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/testutil"
)

// runFixture parses a fixture string through a fresh streamer and returns
// the assembled turn.
func runFixture(t *testing.T, fixture string) (*streamer, error) {
	t.Helper()
	st := newStreamer()
	_, err := st.parseEvents(context.Background(), strings.NewReader(fixture))
	st.finish()
	return st, err
}

// collectEvents parses a fixture and returns the raw events in order.
func collectEvents(t *testing.T, fixture string) []Event {
	t.Helper()
	var events []Event
	st := newStreamer()
	st.onTap = func(ev Event) { events = append(events, ev) }
	st.parseEvents(context.Background(), strings.NewReader(fixture))
	return events
}

// SSE-001: parser basics — events in order, JSON decoded, colon-without-
// space format handled (SDD §2.5).
func TestSSEParserBasics(t *testing.T) {
	events := collectEvents(t, testutil.FixtureSSEBasic)
	wantNames := []string{"dialogId", "message", "message", "message", "usage", "finish"}
	if len(events) != len(wantNames) {
		t.Fatalf("event count = %d, want %d", len(events), len(wantNames))
	}
	for i, name := range wantNames {
		if events[i].Name != name {
			t.Errorf("event[%d].Name = %q, want %q", i, events[i].Name, name)
		}
	}
	st, err := runFixture(t, testutil.FixtureSSEBasic)
	if err != nil {
		t.Fatal(err)
	}
	if st.dialogID != "dlg-0001" {
		t.Errorf("dialogID = %q, want dlg-0001", st.dialogID)
	}
	if st.content.String() != "Hello world" {
		t.Errorf("content = %q, want %q", st.content.String(), "Hello world")
	}
	if st.usage == nil || st.usage.PromptTokens != 2242 || st.usage.CompletionTokens != 78 {
		t.Errorf("usage = %+v, want 2242/78", st.usage)
	}
}

// SSE-002: CRLF tolerance — identical event sequence as SSE-001 (SDD §2.5).
func TestSSECRLFTolerance(t *testing.T) {
	events := collectEvents(t, testutil.FixtureSSECRLF)
	wantNames := []string{"dialogId", "message", "message", "message", "usage", "finish"}
	if len(events) != len(wantNames) {
		t.Fatalf("event count = %d, want %d", len(events), len(wantNames))
	}
	for i, name := range wantNames {
		if events[i].Name != name {
			t.Errorf("event[%d].Name = %q, want %q", i, events[i].Name, name)
		}
	}
	st, err := runFixture(t, testutil.FixtureSSECRLF)
	if err != nil {
		t.Fatal(err)
	}
	if st.content.String() != "Hello world" {
		t.Errorf("content = %q", st.content.String())
	}
}

// SSE-003: `event: x` with-space tolerance — same sequence (SDD §2.5).
func TestSSESpaceTolerance(t *testing.T) {
	events := collectEvents(t, testutil.FixtureSSENoSpace)
	wantNames := []string{"dialogId", "message", "usage", "finish"}
	if len(events) != len(wantNames) {
		t.Fatalf("event count = %d, want %d", len(events), len(wantNames))
	}
	for i, name := range wantNames {
		if events[i].Name != name {
			t.Errorf("event[%d].Name = %q, want %q", i, events[i].Name, name)
		}
	}
}

// SSE-005: event dispatch semantics — error fails the turn;
// sensitive_query → content filter; web_search/doc/tip_* ignored; finish
// [DONE] ends the turn; dialogId captured (SDD §2.5).
func TestSSEEventDispatch(t *testing.T) {
	// error event → turn failure.
	_, err := runFixture(t, testutil.FixtureSSEError)
	var ue *ErrUpstreamStream
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "upstream exploded") {
		t.Fatalf("error fixture: err = %v, want ErrUpstreamStream", err)
	}

	// sensitive_query → content filter.
	_, err = runFixture(t, testutil.FixtureSSESensitive)
	var cf *ErrContentFilter
	if !errors.As(err, &cf) {
		t.Fatalf("sensitive fixture: err = %v, want ErrContentFilter", err)
	}

	// Ignorable events never fail the turn and finish ends it.
	noise := "event:web_search\ndata:{\"type\":null,\"content\":\"searching\",\"usage\":null}\n\n" +
		"event:doc\ndata:{\"content\":\"parsing\"}\n\n" +
		"event:tip_ratio\ndata:{\"content\":\"1.0\"}\n\n" +
		"event:tip_truncate\ndata:{\"content\":\"0.5\"}\n\n" +
		"event:dialogId\ndata:{\"type\":null,\"content\":\"dlg-x\",\"usage\":null}\n\n" +
		"event:message\ndata:{\"type\":\"text\",\"content\":\"fine\",\"usage\":null}\n\n" +
		"event:finish\ndata:{\"type\":null,\"content\":\"[DONE]\",\"usage\":null}\n\n"
	st := newStreamer()
	done, err := st.parseEvents(context.Background(), strings.NewReader(noise))
	if err != nil {
		t.Fatalf("noise events must not fail the turn: %v", err)
	}
	if !done {
		t.Error("finish [DONE] must end the turn")
	}
	st.finish()
	if st.dialogID != "dlg-x" {
		t.Errorf("dialogID = %q, want dlg-x", st.dialogID)
	}
}

// SSE-012: usage last-wins across multiple usage events (SDD §2.5).
func TestSSEUsageLastWins(t *testing.T) {
	st, err := runFixture(t, testutil.FixtureSSEMultiUsage)
	if err != nil {
		t.Fatal(err)
	}
	if st.usage == nil {
		t.Fatal("usage = nil")
	}
	if st.usage.PromptTokens != 30 || st.usage.CompletionTokens != 9 || st.usage.TotalTokens != 39 {
		t.Errorf("usage = %+v, want last event (30/9/39)", st.usage)
	}
}

// Thinking-fixture coverage through the full streamer (splitter unit
// behavior itself is pinned in adapter tests; these verify the SSE layer
// feeds it correctly).
func TestSSEThinkingFixtures(t *testing.T) {
	// Single block (SSE-006).
	st, err := runFixture(t, testutil.FixtureSSEThinking)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.reason.String(); got != "Let me check the math." {
		t.Errorf("reasoning = %q", got)
	}
	if got := st.content.String(); got != "The answer is 4." {
		t.Errorf("content = %q", got)
	}
	if strings.ContainsRune(st.content.String(), 0) {
		t.Error("NUL leaked into content")
	}

	// Markers split across deltas (SSE-007).
	st2, err := runFixture(t, testutil.FixtureSSEThinkingSplit)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.reason.String(); got != "reasoning here" {
		t.Errorf("split reasoning = %q, want %q", got, "reasoning here")
	}
	if got := st2.content.String(); got != "before after" {
		t.Errorf("split content = %q, want %q", got, "before after")
	}

	// Unterminated block → all reasoning (SSE-009).
	st3, err := runFixture(t, testutil.FixtureSSEThinkingUnterminated)
	if err != nil {
		t.Fatal(err)
	}
	if got := st3.reason.String(); got != "loose thought continues" {
		t.Errorf("unterminated reasoning = %q", got)
	}
	if got := st3.content.String(); got != "pure text " {
		t.Errorf("unterminated content = %q", got)
	}

	// Two blocks (SSE-011).
	st4, err := runFixture(t, testutil.FixtureSSEMultiThink)
	if err != nil {
		t.Fatal(err)
	}
	if got := st4.reason.String(); got != "first blocksecond block" {
		t.Errorf("multi reasoning = %q", got)
	}
	if got := st4.content.String(); got != "middle textafter text" {
		t.Errorf("multi content = %q", got)
	}
}

// onPiece streaming: pieces are forwarded incrementally (the splitter
// holds back a partial-marker tail, so deltas may merge — count is not
// fixed; the order and the joined text are).
func TestSSEOnPiece(t *testing.T) {
	var pieces []adapter.Piece
	st := newStreamer()
	st.onPiece = func(p adapter.Piece) error {
		pieces = append(pieces, p)
		return nil
	}
	if _, err := st.parseEvents(context.Background(), strings.NewReader(testutil.FixtureSSEBasic)); err != nil {
		t.Fatal(err)
	}
	st.finish()
	if len(pieces) < 2 {
		t.Fatalf("pieces = %d, want incremental emission (>=2)", len(pieces))
	}
	var joined strings.Builder
	for _, p := range pieces {
		joined.WriteString(p.Text)
	}
	if joined.String() != "Hello world" {
		t.Errorf("joined pieces = %q", joined.String())
	}
}

// onFirstByte fires exactly once on the first event (commit boundary,
// SDD §7.2).
func TestSSEOnFirstByte(t *testing.T) {
	calls := 0
	st := newStreamer()
	st.onFirstByte = func() { calls++ }
	st.parseEvents(context.Background(), strings.NewReader(testutil.FixtureSSEBasic))
	if calls != 1 {
		t.Errorf("onFirstByte calls = %d, want 1", calls)
	}
}
