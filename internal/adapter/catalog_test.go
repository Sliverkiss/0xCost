package adapter_test

// Catalog capability mapping and the per-id union (M19 §3). These are pure
// functions, so they carry the whole capability contract: the page type decides
// whether search is even possible, and the chat row's default decides the level.

import (
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

// CATMAP-001: page type → tasks and audio capability.
func TestCatalogPageMapping(t *testing.T) {
	cases := []struct {
		page  string
		tasks []string
		audio string
	}{
		{adapter.PageChat, []string{"chat"}, adapter.AudioNone},
		{adapter.PageTTS, []string{"tts"}, adapter.AudioTTS},
		{adapter.PageTTSVD, []string{"tts"}, adapter.AudioTTS},
		{adapter.PageTTSVC, []string{"tts"}, adapter.AudioTTS},
		{adapter.PageASR, []string{"asr"}, adapter.AudioASR},
		{"", []string{"chat"}, adapter.AudioNone}, // older rows omit pageType
	}
	for _, tc := range cases {
		t.Run("page="+tc.page, func(t *testing.T) {
			if got := adapter.Tasks(tc.page); len(got) != 1 || got[0] != tc.tasks[0] {
				t.Errorf("Tasks(%q) = %v, want %v", tc.page, got, tc.tasks)
			}
			if got := adapter.AudioForPage(tc.page); got != tc.audio {
				t.Errorf("AudioForPage(%q) = %q, want %q", tc.page, got, tc.audio)
			}
		})
	}
}

// CATMAP-002: the search level follows the Studio's own enum
// (DISABLED=0 / ENABLED=1 / ENABLED_BY_DEFAULT=2) and is only ever non-zero for
// a chat page type.
func TestCatalogWebSearchLevel(t *testing.T) {
	cases := []struct {
		page, status string
		want         int
	}{
		{adapter.PageChat, "auto", 2},
		{adapter.PageChat, "enabled", 2},
		{adapter.PageChat, "disabled", 1},
		{adapter.PageChat, "", 1}, // chat row with no stated default
		{adapter.PageTTS, "auto", 0},
		{adapter.PageASR, "auto", 0},
	}
	for _, tc := range cases {
		t.Run(tc.page+"/"+tc.status, func(t *testing.T) {
			if got := adapter.WebSearchLevel(tc.page, tc.status); got != tc.want {
				t.Errorf("WebSearchLevel(%q, %q) = %d, want %d", tc.page, tc.status, got, tc.want)
			}
		})
	}
}

// CATMAP-003: merging takes capabilities at their maximum and prefers the
// account's own row over the static floor.
func TestCatalogMerge(t *testing.T) {
	floor := adapter.CatalogEntry{ID: "m", Page: adapter.PageChat, Web: 1, Static: true}
	account := adapter.CatalogEntry{ID: "m", Page: adapter.PageChat, Web: 2, Think: true, Audio: adapter.AudioTTS, Name: "M", NToken: "1M"}
	got := floor.Merge(account)
	if got.Web != 2 || !got.Think || got.Audio != adapter.AudioTTS {
		t.Errorf("merged = %+v, want the account's higher capabilities", got)
	}
	if got.Static {
		t.Error("merged.Static = true, want false once an account reports the id")
	}
	if got.Name != "M" || got.NToken != "1M" {
		t.Errorf("merged = %+v, want the account's name and context size", got)
	}

	// Folding in a floor-only entry must not downgrade anything.
	round := got.Merge(adapter.CatalogEntry{ID: "m", Page: adapter.PageChat, Web: 1, Static: true})
	if round.Web != 2 || !round.Think || round.Static {
		t.Errorf("merged with a floor entry = %+v, want no downgrade", round)
	}

	// A richer audio capability wins over none, and tts over asr.
	if a := (adapter.CatalogEntry{Audio: adapter.AudioNone}).Merge(adapter.CatalogEntry{Audio: adapter.AudioASR}); a.Audio != adapter.AudioASR {
		t.Errorf("audio = %q, want asr", a.Audio)
	}
	if a := (adapter.CatalogEntry{Audio: adapter.AudioASR}).Merge(adapter.CatalogEntry{Audio: adapter.AudioTTS}); a.Audio != adapter.AudioTTS {
		t.Errorf("audio = %q, want tts to outrank asr", a.Audio)
	}
	// Missing page/name come from whichever side has them.
	if a := (adapter.CatalogEntry{}).Merge(adapter.CatalogEntry{Page: adapter.PageTTS, Name: "n"}); a.Page != adapter.PageTTS || a.Name != "n" {
		t.Errorf("merged = %+v, want the other side's page and name", a)
	}
}

// CATMAP-004: the union keeps one row per id, drops empty ids, preserves order
// and does not mutate its input.
func TestMergeCatalog(t *testing.T) {
	in := []adapter.CatalogEntry{
		{ID: "a", Web: 1},
		{ID: "", Web: 2},
		{ID: "b", Web: 0},
		{ID: "a", Web: 2, Think: true},
	}
	out := adapter.MergeCatalog(in)
	if len(out) != 2 {
		t.Fatalf("entries = %d (%+v), want 2 (one per id, empty dropped)", len(out), out)
	}
	if out[0].ID != "a" || out[1].ID != "b" {
		t.Errorf("order = %s,%s, want a,b (first-seen order)", out[0].ID, out[1].ID)
	}
	if out[0].Web != 2 || !out[0].Think {
		t.Errorf("a = %+v, want the merged maximum", out[0])
	}
	if in[0].Web != 1 {
		t.Error("MergeCatalog mutated its input")
	}
}

// CATMAP-005: sorting puts the static floor first (a client's configured
// default usually lives there) and is otherwise by id, without reordering the
// caller's slice.
func TestSortCatalog(t *testing.T) {
	in := []adapter.CatalogEntry{
		{ID: "z"}, {ID: "b", Static: true}, {ID: "a"}, {ID: "c", Static: true},
	}
	out := adapter.SortCatalog(in)
	got := []string{out[0].ID, out[1].ID, out[2].ID, out[3].ID}
	if strings.Join(got, ",") != "b,c,a,z" {
		t.Errorf("order = %v, want b,c,a,z (static first, then by id)", got)
	}
	if in[0].ID != "z" {
		t.Error("SortCatalog mutated its input")
	}
}
