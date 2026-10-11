package adapter

import (
	"fmt"
	"sort"
	"strings"
)

// Static model catalog as of the references (SDD §2.6). The ultraspeed model
// routes through the fastchat channel (SDD §2.4).
var DefaultModels = []string{
	"mimo-v2.6-pro-ultraspeed-studio",
	"mimo-v2.6-pro",
	"mimo-v2.6-flash",
	"mimo-v2.5-pro",
	"mimo-v2.5",
	"mimo-v2.1-pro",
	"mimo-v2.1-pro-preview",
	"mimo-v2-pro",
	"mimo-v2-flash",
}

// IsFastchatModel reports whether a model id must use the fastchat channel
// (routing rule: id matches /ultraspeed/i, SDD §2.4).
func IsFastchatModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "ultraspeed")
}

// ErrModelNotFound names the model and lists available ids (TDD API-013).
type ErrModelNotFound struct {
	Requested string
	Available []string
}

func (e *ErrModelNotFound) Error() string {
	return fmt.Sprintf("model %q not found; available models: %s", e.Requested, strings.Join(e.Available, ", "))
}

// NormalizeModel resolves a requested model id against a catalog:
//   - "" or "auto"        → defaultModel
//   - exact catalog match → itself
//   - "-studio" suffix    → stripped then re-matched (alias normalization,
//     multimodal.go:50-85)
//   - anything else       → *ErrModelNotFound listing available ids
//
// The returned bool reports fastchat routing (SDD §5.2).
func NormalizeModel(requested, defaultModel string, catalog []string) (model string, fastchat bool, err error) {
	if catalog == nil {
		catalog = DefaultModels
	}
	if len(catalog) == 0 {
		return "", false, &ErrModelNotFound{Requested: requested, Available: nil}
	}
	set := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		set[m] = true
	}
	resolve := func(id string) (string, bool) {
		if set[id] {
			return id, true
		}
		trimmed := strings.TrimSuffix(id, "-studio")
		if trimmed != id && set[trimmed] {
			return trimmed, true
		}
		return "", false
	}
	if requested == "" || strings.EqualFold(strings.TrimSpace(requested), "auto") {
		m, ok := resolve(defaultModel)
		if !ok {
			// defaultModel not in catalog: fall back to first catalog entry.
			m = catalog[0]
		}
		return m, IsFastchatModel(m), nil
	}
	m, ok := resolve(strings.TrimSpace(requested))
	if !ok {
		avail := append([]string(nil), catalog...)
		sort.Strings(avail)
		return "", false, &ErrModelNotFound{Requested: requested, Available: avail}
	}
	return m, IsFastchatModel(m), nil
}

// MaxQueryChars returns the client-side per-request character cap for a model
// channel (SDD §2.8).
//
// Both numbers are measured, and the platform counts **characters**, not tokens —
// checked by probing until the boundary flips (2026-10-09, one user message,
// MIMO2API_MAX_QUERY_CHARS raised so the upstream was the judge):
//
//	fastchat / ultraspeed:  49,500 served · 52,000 refused (`query is too long`)
//	every other model:     101,000 served · 102,400 served · 105,000 refused
//	                       exactly 102,400 chars served · 102,415 refused
//	                       ASCII 102,400 refused at 102,415 — same boundary as CJK
//
// So the regular channel's ceiling is **102,400 characters** (100 KiB) of the
// query text and it does not move with language (an earlier "token budget near
// 100k" reading of the same data was wrong: what varies is only how close a
// payload sits to the boundary). fastchat's is the Studio's own
// `ultraspeedQueryLimitLength: 5e4` (docs/web-surface-reverse.md §11), bracketed
// by the two probes above.
//
// Enforcing both locally costs nothing and saves a wasted account call: the
// platform refuses anything above them anyway, so this cannot reject work that
// would have been served — which is exactly what the old unmeasured 90,000 did
// (a 95,000-char message is served fine). The upstream's own refusal is still
// translated into our 400 (errors.go) for the cases that reach it.
func MaxQueryChars(model string) int {
	if IsFastchatModel(model) {
		return 50000
	}
	return 102400
}
