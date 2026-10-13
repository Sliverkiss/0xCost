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

// truncateUTF8 must pass short strings through untouched and never split a

// NormalizeModel: a nil/empty catalog is an error (there is deliberately no
// built-in fallback list), a default not in the catalog falls back to the first
// entry, and the -studio alias resolves.
func TestNormalizeModelCatalogBranches(t *testing.T) {
	t.Run("nil catalog errors (no fallback list)", func(t *testing.T) {
		_, _, err := NormalizeModel("mimo-v2.6-flash", "mimo-v2.6-pro", nil)
		if err == nil {
			t.Fatal("nil catalog must not resolve: the account catalog is the single source of truth")
		}
		var nf *ErrModelNotFound
		if !errors.As(err, &nf) {
			t.Errorf("err = %T (%v), want *ErrModelNotFound", err, err)
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
		m, _, err := NormalizeModel("  AUTO  ", "mimo-v2.6-pro", []string{"mimo-v2.6-pro", "mimo-v2.6-flash"})
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
		m, _, err := NormalizeModel("  mimo-v2.6-pro  ", "x", []string{"mimo-v2.6-pro", "mimo-v2.6-flash"})
		if err != nil || m != "mimo-v2.6-pro" {
			t.Errorf("got (%q,%v)", m, err)
		}
	})
}

// AddUsage must sum reasoning detail tokens across replayed turns and omit the
// detail object entirely when the sum is zero.
func TestAddUsageDetailBranches(t *testing.T) {
	a := Usage{CompletionDetails: &CompletionTokensDetails{ReasoningTokens: 5}}
	b := Usage{CompletionDetails: &CompletionTokensDetails{ReasoningTokens: 7}}
	sum := AddUsage(a, b)
	if sum.CompletionDetails == nil || sum.CompletionDetails.ReasoningTokens != 12 {
		t.Errorf("reasoning sum = %+v, want 12", sum.CompletionDetails)
	}
	// Both nil → omitted.
	// Both detail objects are part of OpenAI's shape, so the sum always carries
	// them — zero-filled rather than missing, and never invented.
	plain := AddUsage(Usage{PromptTokens: 1}, Usage{CompletionTokens: 2})
	if plain.PromptDetails == nil || plain.CompletionDetails == nil {
		t.Errorf("details must always be present, got %+v", plain)
	}
	if plain.PromptDetails.CachedTokens != 0 || plain.CompletionDetails.ReasoningTokens != 0 {
		t.Errorf("details must be zero when nothing was counted, got %+v", plain)
	}
	zero := AddUsage(Usage{CompletionDetails: &CompletionTokensDetails{}}, Usage{})
	if zero.CompletionDetails == nil || zero.CompletionDetails.ReasoningTokens != 0 {
		t.Errorf("zero-sum completion details must be present and zero, got %+v", zero.CompletionDetails)
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
