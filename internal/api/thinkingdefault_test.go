package api

import (
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

// THK-001: the thinking default is decided in three layers, and the middle one is
// the point of the change — the platform's own per-model declaration
// (`thinkingDefaultOn`) beats the operator's deployment default, because the
// upstream catalog is the authority on how its models behave (ADR 0031).
func TestTHK001ThinkingDefaultLayers(t *testing.T) {
	off := false

	// Nothing declared, nothing configured: on. The catalog declares thinking on
	// for every chat model this account serves, and OpenAI's reasoning models think
	// unless told not to, so an unconfigured gateway must not be the odd one out.
	if got := New(Options{}).thinkingDefaultFor("mimo-v2.6-flash"); !got {
		t.Errorf("unconfigured default = false, want true")
	}
	// The knob is the fallback for an id the cached snapshot does not carry.
	if got := New(Options{DefaultThinking: &off}).thinkingDefaultFor("mimo-v2.6-flash"); got {
		t.Errorf("MIMO2API_DEFAULT_THINKING=off never reached the request path")
	}

	// A declared value wins over the knob, in both directions.
	s := New(Options{DefaultThinking: &off})
	s.redirects.mu.Lock()
	s.redirects.thinks = map[string]bool{"mimo-v2.6-flash": true, "mimo-v2.6-pro": false}
	s.redirects.mu.Unlock()
	if got := s.thinkingDefaultFor("mimo-v2.6-flash"); !got {
		t.Errorf("declared true was overridden by the deployment knob")
	}
	if got := s.thinkingDefaultFor("mimo-v2.6-pro"); got {
		t.Errorf("declared false was not honoured")
	}
	if got := s.thinkingDefaultFor("mimo-v2.5-pro"); got {
		t.Errorf("an id with no declaration should fall back to the deployment default (off)")
	}
}

// THK-002: only a declaration counts. The built-in floor's zero value means "the
// catalog said nothing", not "thinking is off" — conflating the two would turn a
// failed catalog read into a silent behaviour change, and the floor is exactly
// what a deployment falls back to when the catalog is unreadable.
func TestTHK002OnlyDeclaredDefaultsCount(t *testing.T) {
	entries := []adapter.CatalogEntry{
		{ID: "static-only", Think: false, ThinkKnown: false, Static: true},
		{ID: "declared-on", Think: true, ThinkKnown: true},
		{ID: "declared-off", Think: false, ThinkKnown: true},
	}
	m := thinkingDefaultsOf(entries)
	if _, ok := m["static-only"]; ok {
		t.Errorf("a floor entry with no declaration was read as a thinking default")
	}
	if v, ok := m["declared-on"]; !ok || !v {
		t.Errorf("declared-on missing from the snapshot: %v %v", v, ok)
	}
	if v, ok := m["declared-off"]; !ok || v {
		t.Errorf("declared-off missing from the snapshot: %v %v", v, ok)
	}

	// Merge keeps a declaration when the other side (floor, or an account whose
	// catalog does not describe the id) has none.
	merged := adapter.CatalogEntry{ID: "x"}.Merge(adapter.CatalogEntry{ID: "x", Think: true, ThinkKnown: true})
	if !merged.ThinkKnown || !merged.Think {
		t.Errorf("merge dropped the declaration: %+v", merged)
	}
	// And the floor alone never invents one.
	if got := thinkingDefaultsOf([]adapter.CatalogEntry{{ID: "x", Static: true}}); len(got) != 0 {
		t.Errorf("floor-only entry produced a default: %v", got)
	}
}
