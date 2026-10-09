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
// fastchat (ultraspeed): **50,000**, the Studio's own
// `ultraspeedQueryLimitLength: 5e4` (SPA global store,
// docs/web-surface-reverse.md §11) — a real, sharp limit: a single 60,000-char
// message is refused by the upstream with `query is too long` while 50,000 is
// served. Rejecting it locally saves a wasted account call and gives the client
// the number instead of an English prose error.
//
// Every other channel: **150,000**, a guardrail rather than the upstream's rule.
// The old 90,000 was an unmeasured guess and it blocked requests the platform
// serves: measured 2026-10-09, a single 95,000- and even a 100,100-char message
// are answered normally, 105,000 is refused ("the text you sent is too long"),
// and 300,000 ASCII chars are refused — i.e. the real ceiling moves with content
// (token budget, not a clean character count), so guessing it locally only ever
// means rejecting work the upstream would have done. The guardrail is here for
// absurd payloads (nothing else bounds a chat body); genuine "too long" cases
// are translated from the upstream's own refusal into our 400 (errors.go).
func MaxQueryChars(model string) int {
	if IsFastchatModel(model) {
		return 50000
	}
	return 150000
}
