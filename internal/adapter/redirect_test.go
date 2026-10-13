package adapter

import "testing"

// MODELS-REDIR: the upstream's own `redirectTo` decides what the deployment can
// serve. These are the unit cases for that map; the API-level cases live in
// internal/api/models_redirect_test.go.

func TestResolveRedirectFollowsChains(t *testing.T) {
	// The live map has a two-hop chain: flash-studio → flash → v2.6-flash.
	redirects := map[string]string{
		"mimo-v2-flash-studio": "mimo-v2-flash",
		"mimo-v2-flash":        "mimo-v2.6-flash",
		"mimo-v2-tts":          "mimo-v2.5-tts",
	}
	cases := map[string]string{
		"mimo-v2-flash-studio": "mimo-v2.6-flash", // transitive
		"mimo-v2-flash":        "mimo-v2.6-flash",
		"mimo-v2-tts":          "mimo-v2.5-tts",
		"mimo-v2.6-flash":      "mimo-v2.6-flash", // current: unchanged
		"who-knows":            "who-knows",       // unknown: unchanged
	}
	for in, want := range cases {
		if got := ResolveRedirect(in, redirects); got != want {
			t.Errorf("ResolveRedirect(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ResolveRedirect("mimo-v2-flash", nil); got != "mimo-v2-flash" {
		t.Errorf("a nil map changed the id: %q", got)
	}
}

func TestResolveRedirectSurvivesCycles(t *testing.T) {
	// A cycle must terminate and leave the id as it was: an upstream (or a typo)
	// that points two ids at each other must not spin or invent a target.
	redirects := map[string]string{"a": "b", "b": "a"}
	if got := ResolveRedirect("a", redirects); got != "b" && got != "a" {
		t.Errorf("ResolveRedirect(a) = %q; want a or b, and it must terminate", got)
	}
	self := map[string]string{"a": "a"}
	if got := ResolveRedirect("a", self); got != "a" {
		t.Errorf("a self-redirect changed the id: %q", got)
	}
}

func TestUsableDropsSupersededIDs(t *testing.T) {
	entries := []CatalogEntry{
		{ID: "mimo-v2.6-flash", Page: PageChat},
		{ID: "mimo-v2-flash", Page: PageChat, RedirectTo: "mimo-v2.6-flash"},
		{ID: "mimo-v2.5-tts", Page: PageTTS},
		{ID: "mimo-v2-tts", Page: PageTTS, RedirectTo: "mimo-v2.5-tts"},
	}
	got := Usable(entries)
	if len(got) != 2 {
		t.Fatalf("Usable kept %d entries, want the 2 current ones", len(got))
	}
	for _, e := range got {
		if e.Deprecated() {
			t.Errorf("%s is superseded and must not be advertised", e.ID)
		}
	}
}

// A superseded row stays superseded when another account's row for the same id
// carries no redirect field: losing the field would put the id back on the menu.
func TestMergeKeepsRedirect(t *testing.T) {
	superseded := CatalogEntry{ID: "mimo-v2-flash", Page: PageChat, RedirectTo: "mimo-v2.6-flash"}
	plain := CatalogEntry{ID: "mimo-v2-flash", Page: PageChat}
	merged := MergeCatalog([]CatalogEntry{plain, superseded})
	if len(merged) != 1 {
		t.Fatalf("merge produced %d rows", len(merged))
	}
	if merged[0].RedirectTo != "mimo-v2.6-flash" {
		t.Errorf("merge lost the redirect: %+v", merged[0])
	}
	if !merged[0].Deprecated() {
		t.Error("merged row is no longer recognised as superseded")
	}
}
