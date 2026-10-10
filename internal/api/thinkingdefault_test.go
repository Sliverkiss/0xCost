package api

import (
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

// THK-001: the thinking default is the platform's answer, not a knob of ours
// (ADR 0031). Two layers: the request's own field decides when it is present, and
// otherwise the model's declared default applies — an id the catalog does not
// describe gets the same answer, because that is what the platform declares for its
// chat models and what OpenAI's reasoning models do. There is deliberately no
// deployment-level switch: a second, non-standard way to disagree with the provider
// is exactly what the external surface must not grow (ADR 0027).
func TestTHK001ThinkingDefaultLayers(t *testing.T) {
	s := New(Options{})
	s.redirects.mu.Lock()
	s.redirects.thinks = map[string]bool{"mimo-v2.6-flash": true, "mimo-v2.6-pro": false}
	s.redirects.mu.Unlock()

	// A model the catalog describes: its word is the default, in both directions.
	if got := s.thinkingDefaultFor("mimo-v2.6-flash"); !got {
		t.Errorf("declared true was not honoured")
	}
	if got := s.thinkingDefaultFor("mimo-v2.6-pro"); got {
		t.Errorf("declared false was not honoured")
	}

	// An id the catalog does not describe follows the platform's behaviour: its
	// chat models declare thinking on, so this gateway answers on too.
	if got := New(Options{}).thinkingDefaultFor("mimo-v2.5-pro"); !got {
		t.Errorf("an undescribed id did not follow the platform's own default (on)")
	}
	// A cold snapshot is the state of a fresh start (the request path never reads
	// the catalog, ADR 0011) and must not answer off either.
	if got := New(Options{}).thinkingDefaultFor("mimo-v2.6-flash"); !got {
		t.Errorf("a cold snapshot fell back to off")
	}
}

// THK-002: only a declaration counts. The built-in floor's zero value means "the
// catalog said nothing", not "thinking is off" — conflating the two would turn a
// failed catalog read into a silent behaviour change, and the floor is exactly what
// a deployment falls back to when the catalog is unreadable.
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
