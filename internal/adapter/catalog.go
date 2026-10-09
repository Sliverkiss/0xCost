package adapter

// Model catalog (M19 §3). The account's own `bot/config` is the authority for
// what this deployment can serve and what each id can do; the built-in table is
// only a floor for when that read fails. Capabilities are deliberately coarse
// and evidence-backed:
//
//   - web_search: 0 = no search surface (non-chat page type), 1 = searchable
//     but off by default, 2 = searchable and on by default. The live payload
//     only carries `webSearchDefaultStatus` (auto | disabled | absent), so the
//     level is derived from it; a model whose row is absent is 0. Note that 1
//     still means "a search happens when asked": the upstream returned results
//     even with webSearchStatus=disabled (docs/web-surface-reverse.md §5).
//   - thinking: the row's `thinkingDefaultOn` — the DEFAULT, not a limit;
//     enable_thinking can be requested regardless.
//   - audio: the page type, collapsed to tts | asr | none.

import "sort"

// Page types the Studio reports in modelConfigListNg pageType.
const (
	PageChat    = "chat"
	PageTTS     = "tts"
	PageTTSVD   = "tts-vd"
	PageTTSVC   = "tts-vc"
	PageASR     = "asr"
	AudioNone   = "none"
	AudioTTS    = "tts"
	AudioASR    = "asr"
	SourceAccnt = "account"
	SourceFallo = "static"
)

// CatalogEntry is one model as reported by an account (or by the static floor).
type CatalogEntry struct {
	ID     string
	Name   string
	Page   string
	Web    int  // 0|1|2
	Think  bool // default-on
	Audio  string
	NToken string // upstream context size when given ("1M")
	// RedirectTo is the upstream's own `redirectTo`: the id this one has been
	// superseded by. The upstream refuses the superseded id on the audio surface
	// (400 "model: [x] not support" / "modelCode cannot be empty") and answers it
	// with the busy frame on chat, and the Studio SPA never sends it — it sends
	// the target (it builds a deprecatedModelRedirectMap from exactly this field).
	// An entry with a target is therefore NOT advertised, and a request naming it
	// is resolved before it reaches the upstream.
	RedirectTo string
	Static     bool // from the built-in table, not the account catalog
}

// Deprecated reports whether the upstream has superseded this id.
func (e CatalogEntry) Deprecated() bool { return e.RedirectTo != "" }

// ResolveRedirect follows `redirectTo` until it reaches an id the upstream
// serves, guarding against cycles and against a chain that never lands (an
// unknown id is returned unchanged — the upstream's own error is more
// informative than one we invent). It is transitive because the upstream's own
// map is: mimo-v2-flash-studio → mimo-v2-flash → mimo-v2.6-flash.
func ResolveRedirect(id string, redirects map[string]string) string {
	seen := map[string]bool{id: true}
	for i := 0; i < 8; i++ {
		next, ok := redirects[id]
		if !ok || next == "" || seen[next] {
			return id
		}
		seen[next] = true
		id = next
	}
	return id
}

// Tasks maps a page type onto the OpenAI-adjacent task list we expose.
func Tasks(page string) []string {
	switch page {
	case PageTTS, PageTTSVD, PageTTSVC:
		return []string{AudioTTS}
	case PageASR:
		return []string{AudioASR}
	default:
		return []string{PageChat}
	}
}

// AudioForPage collapses the TTS variants onto the single "tts" capability.
func AudioForPage(page string) string {
	switch page {
	case PageTTS, PageTTSVD, PageTTSVC:
		return AudioTTS
	case PageASR:
		return AudioASR
	default:
		return AudioNone
	}
}

// WebSearchLevel derives the capability level from the upstream row: only chat
// models have a search surface, and "auto"/"enabled" means it is on by default.
func WebSearchLevel(page, defaultStatus string) int {
	if page != PageChat && page != "" {
		return 0
	}
	switch defaultStatus {
	case "auto", "enabled":
		return 2
	case "disabled":
		return 1
	default:
		// A chat row with no stated default: searchable, off by default.
		return 1
	}
}

func audioRank(a string) int {
	switch a {
	case AudioTTS:
		return 2
	case AudioASR:
		return 1
	default:
		return 0
	}
}

// Merge folds another account's view of the same id into e: the union is per
// id with capabilities taken at their maximum, so a model that is searchable on
// any account reads as searchable (M19 §3).
func (e CatalogEntry) Merge(o CatalogEntry) CatalogEntry {
	out := e
	if out.Web < o.Web {
		out.Web = o.Web
	}
	out.Think = out.Think || o.Think
	if audioRank(o.Audio) > audioRank(out.Audio) {
		out.Audio = o.Audio
	}
	if out.Name == "" {
		out.Name = o.Name
	}
	if out.Page == "" {
		out.Page = o.Page
	}
	if out.NToken == "" {
		out.NToken = o.NToken
	}
	// A superseded id stays superseded: the static floor has no opinion about
	// redirects, so it must never erase the account's. (The reverse — the account
	// telling us an id is current — arrives as a row without the field, which
	// this leaves alone too.)
	if out.RedirectTo == "" {
		out.RedirectTo = o.RedirectTo
	}
	// Only drop the static marker when the account itself reported the id.
	if out.Static && !o.Static {
		out.Static = false
	}
	return out
}

// MergeCatalog unions entries by id, preserving a stable id order.
func MergeCatalog(entries []CatalogEntry) []CatalogEntry {
	byID := map[string]CatalogEntry{}
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		cur, ok := byID[e.ID]
		if !ok {
			byID[e.ID] = e
			order = append(order, e.ID)
			continue
		}
		byID[e.ID] = cur.Merge(e)
	}
	out := make([]CatalogEntry, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// Usable drops the ids the upstream has superseded. A superseded id is not a
// model the deployment can serve: the audio surface refuses it outright and chat
// answers it with the busy frame, so advertising it only invites a client to ask
// for something that cannot work. It stays resolvable for a client that asks for
// it anyway (ResolveRedirect), it just is not on the menu.
func Usable(entries []CatalogEntry) []CatalogEntry {
	out := make([]CatalogEntry, 0, len(entries))
	for _, e := range entries {
		if e.Deprecated() {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SortCatalog orders entries for a stable response: the built-in table first
// (a client's configured default usually lives there), then by id.
func SortCatalog(entries []CatalogEntry) []CatalogEntry {
	out := append([]CatalogEntry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Static != out[j].Static {
			return out[i].Static
		}
		return out[i].ID < out[j].ID
	})
	return out
}
