package adapter

import (
	"errors"
	"strings"
	"testing"
)

// ============================================================================
// Additional coverage: replay-guard counter, turn-plan errors, usage-detail
// accumulation, model resolution edge branches, splitter edge branches.
// (TDD §6.2 floors; each assertion pins observable behaviour.)
// ============================================================================

// CountNonSystem is the replay-guard frontier counter (SDD §4.2, STICKY-003):
// only non-system messages count, and that count is what PlanDelta consumes.
func TestCountNonSystem(t *testing.T) {
	cases := []struct {
		name string
		msgs []Msg
		want int
	}{
		{"nil", nil, 0},
		{"system only", []Msg{{Role: "system", Content: "s"}}, 0},
		{"no system", []Msg{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}, 2},
		{"mixed", []Msg{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: "a"},
			{Role: "system", Content: "s2"},
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountNonSystem(tc.msgs); got != tc.want {
				t.Errorf("CountNonSystem = %d, want %d", got, tc.want)
			}
		})
	}

	// The counter is exactly the frontier PlanDelta expects: with a stored
	// count equal to CountNonSystem, there is nothing new to send.
	msgs := []Msg{{Role: "system", Content: "s"}, {Role: "user", Content: "q"}}
	if _, err := PlanDelta(msgs, CountNonSystem(msgs)); err == nil {
		t.Error("frontier == CountNonSystem must leave no delta to send")
	}
}

// ErrNoUserMessage / ErrTooManyTurns must render actionable messages and be
// reachable via errors.As (the API layer maps them to 400s).
func TestTurnPlanErrors(t *testing.T) {
	t.Run("ErrNoUserMessage", func(t *testing.T) {
		_, err := PlanFullReplay([]Msg{{Role: "assistant", Content: "a"}}, 20)
		var noUser ErrNoUserMessage
		if !errors.As(err, &noUser) {
			t.Fatalf("err = %v (%T), want ErrNoUserMessage", err, err)
		}
		if got := err.Error(); !strings.Contains(got, "no sendable user message") {
			t.Errorf("Error() = %q", got)
		}
	})

	t.Run("ErrTooManyTurns", func(t *testing.T) {
		var msgs []Msg
		for i := 0; i < 3; i++ {
			msgs = append(msgs, Msg{Role: "user", Content: "x"})
		}
		_, err := PlanFullReplay(msgs, 2)
		var tooMany *ErrTooManyTurns
		if !errors.As(err, &tooMany) {
			t.Fatalf("err = %v (%T), want *ErrTooManyTurns", err, err)
		}
		if tooMany.Turns != 3 || tooMany.Cap != 2 {
			t.Errorf("fields = %+v, want Turns=3 Cap=2", tooMany)
		}
		got := err.Error()
		if !strings.Contains(got, "3 user turns") || !strings.Contains(got, "cap of 2") {
			t.Errorf("Error() = %q, must name the turn count and the cap", got)
		}
	})
}

// PlanDelta edge branches: negative frontier, empty/assistant-only tails, and
// empty user content are all rejected rather than emitting a bogus query.
func TestPlanDeltaEdgeBranches(t *testing.T) {
	t.Run("negative frontier", func(t *testing.T) {
		if _, err := PlanDelta([]Msg{{Role: "user", Content: "q"}}, -1); err == nil {
			t.Error("negative frontier must be rejected")
		}
	})
	t.Run("assistant-only tail", func(t *testing.T) {
		msgs := []Msg{{Role: "user", Content: "q1"}, {Role: "assistant", Content: "a1"}}
		if _, err := PlanDelta(msgs, 1); err == nil {
			t.Error("tail with no user message must be rejected")
		}
	})
	t.Run("empty user content skipped", func(t *testing.T) {
		msgs := []Msg{{Role: "user", Content: "q1"}, {Role: "user", Content: ""}}
		if _, err := PlanDelta(msgs, 1); err == nil {
			t.Error("empty trailing user message must be rejected, not sent")
		}
	})
	t.Run("multiple delta turns preserved in order", func(t *testing.T) {
		msgs := []Msg{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
			{Role: "user", Content: "q3"},
		}
		plan, err := PlanDelta(msgs, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Queries) != 2 || plan.Queries[0] != "q2" || plan.Queries[1] != "q3" {
			t.Errorf("queries = %+v, want [q2 q3]", plan.Queries)
		}
	})
}

// truncateUTF8 must pass short strings through untouched and never split a

// NormalizeModel: nil catalog falls back to DefaultModels, an empty catalog is
// an error, a default not in the catalog falls back to the first entry, and
// the -studio alias resolves.
func TestNormalizeModelCatalogBranches(t *testing.T) {
	t.Run("nil catalog uses defaults", func(t *testing.T) {
		m, fc, err := NormalizeModel("mimo-v2.6-flash", "mimo-v2.6-pro", nil)
		if err != nil || m != "mimo-v2.6-flash" || fc {
			t.Errorf("got (%q,%v,%v)", m, fc, err)
		}
	})
	t.Run("empty catalog errors", func(t *testing.T) {
		_, _, err := NormalizeModel("x", "y", []string{})
		var nf *ErrModelNotFound
		if !errors.As(err, &nf) {
			t.Fatalf("err = %v, want *ErrModelNotFound", err)
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Error() = %q", err.Error())
		}
	})
	t.Run("default not in catalog falls back to first", func(t *testing.T) {
		m, _, err := NormalizeModel("", "not-a-model", []string{"mimo-v2.6-pro", "mimo-v2.6-flash"})
		if err != nil || m != "mimo-v2.6-pro" {
			t.Errorf("got (%q,%v), want first catalog entry", m, err)
		}
	})
	t.Run("auto is case/space insensitive", func(t *testing.T) {
		m, _, err := NormalizeModel("  AUTO  ", "mimo-v2.6-pro", DefaultModels)
		if err != nil || m != "mimo-v2.6-pro" {
			t.Errorf("got (%q,%v)", m, err)
		}
	})
	t.Run("studio alias on default resolves", func(t *testing.T) {
		m, _, err := NormalizeModel("auto", "mimo-v2.6-edu-studio", []string{"mimo-v2.6-edu"})
		if err != nil || m != "mimo-v2.6-edu" {
			t.Errorf("got (%q,%v), want stripped alias", m, err)
		}
	})
	t.Run("whitespace around requested id trimmed", func(t *testing.T) {
		m, _, err := NormalizeModel("  mimo-v2.6-pro  ", "x", DefaultModels)
		if err != nil || m != "mimo-v2.6-pro" {
			t.Errorf("got (%q,%v)", m, err)
		}
	})
}

// AddUsage must sum reasoning detail tokens across replayed turns and omit the
// detail object entirely when the sum is zero.
func TestAddUsageDetailBranches(t *testing.T) {
	a := Usage{CompletionDetails: &UsageDetails{ReasoningTokens: 5}}
	b := Usage{CompletionDetails: &UsageDetails{ReasoningTokens: 7}}
	sum := AddUsage(a, b)
	if sum.CompletionDetails == nil || sum.CompletionDetails.ReasoningTokens != 12 {
		t.Errorf("reasoning sum = %+v, want 12", sum.CompletionDetails)
	}
	// Both nil → omitted.
	plain := AddUsage(Usage{PromptTokens: 1}, Usage{CompletionTokens: 2})
	if plain.PromptDetails != nil || plain.CompletionDetails != nil {
		t.Errorf("unexpected details = %+v", plain)
	}
	// Present but zero → still omitted (never invent a detail block).
	zero := AddUsage(Usage{CompletionDetails: &UsageDetails{}}, Usage{})
	if zero.CompletionDetails != nil {
		t.Errorf("zero-sum completion details must be omitted, got %+v", zero.CompletionDetails)
	}
}

// NowUnix supplies the chunk `created` timestamp (SDD §5.4).
func TestNowUnix(t *testing.T) {
	if NowUnix() <= 0 {
		t.Error("NowUnix() must return a positive unix-seconds timestamp")
	}
}

// Push("") is a no-op and must not disturb the splitter state.
func TestPushEmptyDelta(t *testing.T) {
	s := NewThinkSplitter()
	if got := s.Push(""); got != nil {
		t.Errorf("Push(\"\") = %+v, want nil", got)
	}
	pieces := s.Push("hi")
	pieces = append(pieces, s.Flush()...)
	if _, c := MergePieces(pieces); c != "hi" {
		t.Errorf("splitter unusable after empty push: content=%q", c)
	}
}

// Byte-at-a-time feeding exercises the partial-marker hold-back when an entire
// push is a marker prefix (holdBack hold >= len(data)).
func TestSplitterByteAtATime(t *testing.T) {
	s := NewThinkSplitter()
	full := "x" + ThinkOpen + "\x00reasoning" + ThinkClose + "\x00y"
	var pieces []Piece
	for i := 0; i < len(full); i++ {
		pieces = append(pieces, s.Push(full[i:i+1])...)
	}
	pieces = append(pieces, s.Flush()...)
	r, c := MergePieces(pieces)
	if r != "reasoning" || c != "xy" {
		t.Fatalf("byte-at-a-time: reasoning=%q content=%q", r, c)
	}
	for _, p := range pieces {
		if strings.Contains(p.Text, "<") || strings.Contains(p.Text, "\x00") {
			t.Errorf("leaked marker byte in %q", p.Text)
		}
	}
}

// STICKY-DELTA-SYS-001: a system message the client appends AFTER the stored
// frontier must ride along in the delta turn — the documented contract of
// PlanDelta ("system messages appearing in the delta tail splice into the
// first delta user query"). Filtering system messages out before slicing made
// that branch unreachable and silently dropped the message.
func TestPlanDeltaCarriesTrailingSystemMessage(t *testing.T) {
	t.Run("appended after the frontier", func(t *testing.T) {
		msgs := []Msg{
			{Role: "system", Content: "original rules"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "system", Content: "new constraint: answer in French"},
			{Role: "user", Content: "q2"},
		}
		// Frontier: system + q1 + a1 → 2 non-system messages stored.
		plan, err := PlanDelta(msgs, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Queries) != 1 {
			t.Fatalf("queries = %+v, want exactly the delta turn", plan.Queries)
		}
		if !strings.Contains(plan.Queries[0], "new constraint: answer in French") {
			t.Errorf("query = %q, want the trailing system message spliced in", plan.Queries[0])
		}
		if !strings.Contains(plan.Queries[0], "q2") {
			t.Errorf("query = %q, want it to end with the new user message", plan.Queries[0])
		}
		if strings.Contains(plan.Queries[0], "original rules") {
			t.Errorf("query = %q, must not replay the pre-frontier system message", plan.Queries[0])
		}
	})
	t.Run("pre-frontier system message does not shift the frontier", func(t *testing.T) {
		msgs := []Msg{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		}
		plan, err := PlanDelta(msgs, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Queries) != 1 || plan.Queries[0] != "q2" {
			t.Errorf("queries = %+v, want exactly [q2] with no system prefix", plan.Queries)
		}
	})
}

// FrontierHash / SystemPreamble (SDD §4.3.1): the canonical, untruncated
// inputs session identity and frontier verification are built on.
func TestFrontierHashCanonical(t *testing.T) {
	base := []Msg{{Role: "user", Content: "q1"}, {Role: "assistant", Content: "a1"}}
	if got := FrontierHash(base); len(got) != 32 {
		t.Errorf("FrontierHash = %q, want 32 hex chars", got)
	}
	if FrontierHash(nil) != "" || FrontierHash([]Msg{{Role: "system", Content: "s"}}) != "" {
		t.Error("an empty (or system-only) history must hash to the empty string")
	}
	// System messages do not participate, so a preamble change cannot alter the
	// frontier's identity.
	withSystem := append([]Msg{{Role: "system", Content: "you are terse"}}, base...)
	if FrontierHash(withSystem) != FrontierHash(base) {
		t.Error("the system preamble changed the frontier hash")
	}
	// Deterministic across calls.
	if FrontierHash(base) != FrontierHash(base) {
		t.Error("FrontierHash is not deterministic")
	}
	// An edited early turn changes the hash — the property the old 5-message
	// window lacked (SESS-012).
	grown := append(append([]Msg{}, base...), Msg{Role: "user", Content: "q2"})
	edited := append([]Msg{{Role: "user", Content: "EDITED"}}, grown[1:]...)
	if FrontierHash(edited) == FrontierHash(grown) {
		t.Error("an edited early turn did not change the frontier hash")
	}
	// Length-prefixing keeps field boundaries unambiguous.
	if FrontierHash([]Msg{{Role: "user", Content: "ab"}, {Role: "user", Content: "c"}}) ==
		FrontierHash([]Msg{{Role: "user", Content: "a"}, {Role: "user", Content: "bc"}}) {
		t.Error("content boundaries are not respected in the frontier hash")
	}
	// Unicode content is hashed as-is (no rune truncation).
	long := strings.Repeat("长", 500)
	if FrontierHash([]Msg{{Role: "user", Content: long}}) == FrontierHash([]Msg{{Role: "user", Content: strings.Repeat("长", 250)}}) {
		t.Error("long content collided with a prefix of itself")
	}
}

func TestSystemPreambleCanonical(t *testing.T) {
	if got := SystemPreamble(nil); got != "" {
		t.Errorf("SystemPreamble(nil) = %q, want empty", got)
	}
	if got := SystemPreamble([]Msg{{Role: "user", Content: "hi"}}); got != "" {
		t.Errorf("SystemPreamble without a system message = %q, want empty", got)
	}
	one := SystemPreamble([]Msg{{Role: "system", Content: "be terse"}})
	two := SystemPreamble([]Msg{{Role: "system", Content: "be terse"}, {Role: "system", Content: "and kind"}})
	if one == "" || two == "" || one == two {
		t.Errorf("preambles = %q / %q, want distinct non-empty values", one, two)
	}
	if SystemPreamble([]Msg{{Role: "system", Content: "be terse"}}) != one {
		t.Error("SystemPreamble is not deterministic")
	}
	if !strings.Contains(one, "be terse") {
		t.Errorf("preamble %q does not carry the system text", one)
	}
}
