package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/yg/mimo2api/internal/adapter"
)

// contentPart is one entry of a multi-part content array (SDD §5.2).
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// ImageURL is the image_url part (IMG-*): OpenAI's schema makes it an
	// object ({url: "data:image/png;base64,…"} or {url: "https://…"}), so this is
	// not a bare string.
	ImageURL struct {
		URL string `json:"url"`
		// Detail is OpenAI's level knob for an image ("auto"|"low"|"high"). The
		// channel's parse call takes fileUrl/objectName/model only (verified shape,
		// and the sibling client sends nothing else), so the level has nowhere to
		// go: it is validated, then reported as dropped (IMG-009/010), never
		// silently ignored.
		Detail string `json:"detail"`
	} `json:"image_url"`
}

// openAIMessage is the permissive inbound message shape: content may be a
// string or an array of parts; unknown fields are ignored (SDD §5.2 —
// decode permissively, never a strict schema).
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// chatRequest is the permissive inbound request body. Unsupported fields
// (tools, tool_choice, functions, response_format, n, seed, ...) are
// decoded only to be reported as dropped via X-Mimo-Warnings (SDD §5.2,
// TDD API-008/009).
type chatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`

	// SessionID pins the session this request continues (SDD §2.4.1, spec
	// REQ-2). It is the body carrier of the same extension the
	// X-Mimo-Session-Id header carries; the header wins when both are present.
	// Ignored on a one-shot (stream:false) request (REQ-10).
	SessionID string `json:"session_id"`

	// Thinking controls. The OpenAI-standard spelling is `reasoning_effort`
	// (none|minimal|low|medium|high): anything but "none" turns the model's
	// thinking on. The upstream switch is a boolean, so the level itself has
	// nothing to map onto and is accepted as the opt-in it implies. The two
	// spellings this API carried first — `enable_thinking` and
	// reasoning:{"type":"enabled"} — still work when the standard field is
	// absent (SDD §5.2).
	ReasoningEffort string `json:"reasoning_effort"`
	// EnableThinking is the older spelling of the switch, kept as a pointer so an
	// explicit false is distinguishable from "not sent": the deployment default is
	// on, and only an explicit value may turn reasoning off (ADR 0031).
	EnableThinking *bool `json:"enable_thinking"`
	// Search controls. OpenAI's `web_search_options` is the standard spelling
	// (present = search this turn); `web_search_status` keeps the finer
	// disabled|auto|enabled control and wins when both are given; the ":online"
	// model suffix stays as the shorthand (TDD WS-001/003, OSTD-004..006).
	WebSearchOptions *webSearchOptions `json:"web_search_options"`
	WebSearchStatus  string            `json:"web_search_status"`
	// IncludeCitations defaults to true: false suppresses the citations /
	// annotations in the response while the upstream still searches.
	IncludeCitations *bool          `json:"include_citations"`
	Reasoning        *reasoningBody `json:"reasoning"`

	// Dropped-but-recorded fields.
	MaxTokens        int             `json:"max_tokens"`
	MaxCompletionTok int             `json:"max_completion_tokens"`
	Tools            json.RawMessage `json:"tools"`
	ToolChoice       json.RawMessage `json:"tool_choice"`
	Functions        json.RawMessage `json:"functions"`
	ResponseFormat   json.RawMessage `json:"response_format"`
	N                *int            `json:"n"`
	Seed             *int            `json:"seed"`
	Stop             json.RawMessage `json:"stop"`
	User             string          `json:"user"`
	Other            []string        `json:"-"` // unknown top-level fields
}

// reasoningBody is the simple-chat-style reasoning opt-in
// (reasoning:{"type":"enabled"}, SDD §5.2).
type reasoningBody struct {
	Type string `json:"type"`
}

// webSearchOptions is OpenAI's search knob for chat/completions. The upstream
// switch knows only disabled|auto|enabled, so search_context_size is accepted,
// validated and then reported as dropped (X-Mimo-Warnings) rather than silently
// pretended to matter (SDD §5.2).
type webSearchOptions struct {
	SearchContextSize string `json:"search_context_size"`
}

// knownRequestFields is the set of top-level fields this API understands;
// anything outside it is reported (never rejected, SDD §5.2).
var knownRequestFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "temperature": true,
	"top_p": true, "enable_thinking": true, "reasoning": true,
	"reasoning_effort": true, "web_search_options": true,
	"web_search_status": true, "include_citations": true,
	"max_tokens": true, "max_completion_tokens": true, "tools": true,
	"tool_choice": true, "functions": true, "response_format": true,
	"n": true, "seed": true, "stop": true, "user": true,
}

// decodeChatRequest parses the body permissively. Unknown top-level fields
// are collected for the warnings header, never rejected (SDD §5.2).
func decodeChatRequest(r *http.Request) (*chatRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return nil, errMalformedRequest
	}
	return decodeChatBody(raw)
}

// decodeChatBody is the parser on its own, so it can be exercised directly with
// bytes a client could send (FuzzChatRequestDecode) rather than only through a
// live request.
func decodeChatBody(raw []byte) (*chatRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, errMalformedRequest
	}
	req := &chatRequest{}
	if err := json.Unmarshal(raw, req); err != nil {
		return nil, errMalformedRequest
	}
	for k := range top {
		if !knownRequestFields[k] {
			req.Other = append(req.Other, k)
		}
	}
	if len(req.Messages) == 0 {
		return nil, errMalformedRequest
	}
	return req, nil
}

// thinkingEnabled resolves the thinking switch (SDD §5.2, TDD API-010/011,
// OSTD-001..003/011, ADR 0031). OpenAI's reasoning models think unless told not to
// — `reasoning_effort` defaults to a working level and only an explicit lower value
// turns reasoning down — so this gateway's default is ON, not off:
//
//   - `reasoning_effort` decides whenever present: "none" is the explicit off
//     switch, every other accepted value means on;
//   - otherwise the older spellings decide, and an explicit false in either is off
//     (`enable_thinking:false`, `reasoning":{"type":"disabled"}`);
//   - with none of them sent, `defaultOn` applies (MIMO2API_DEFAULT_THINKING, on).
//
// The consequence a caller sees is the point of the change: a plain chat request
// now gets the model's working in `reasoning_content`, instead of a switch only an
// opted-in client could reach. Turning it off is one standard field.
func (req *chatRequest) thinkingEnabled(defaultOn bool) bool {
	if v := strings.ToLower(strings.TrimSpace(req.ReasoningEffort)); v != "" {
		return v != "none"
	}
	if req.EnableThinking != nil {
		return *req.EnableThinking
	}
	if req.Reasoning != nil && strings.TrimSpace(req.Reasoning.Type) != "" {
		return strings.EqualFold(req.Reasoning.Type, "enabled")
	}
	return defaultOn
}

// reasoningEffortErr validates the standard field's value (OpenAI: minimal,
// low, medium, high; "none" is the explicit off switch). A typo is a 400 naming
// the accepted values — never a knob that is quietly ignored.
func (r chatRequest) reasoningEffortErr() error {
	switch strings.ToLower(strings.TrimSpace(r.ReasoningEffort)) {
	case "", "none", "minimal", "low", "medium", "high":
		return nil
	default:
		return errUnsupportedReasoningEffort
	}
}

// webSearchOptionsErr validates OpenAI's search_context_size (low|medium|high).
// The size has no upstream equivalent and is reported through X-Mimo-Warnings.
func (r chatRequest) webSearchOptionsErr() error {
	if r.WebSearchOptions == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(r.WebSearchOptions.SearchContextSize)) {
	case "", "low", "medium", "high":
		return nil
	default:
		return errUnsupportedSearchContextSize
	}
}

// toAdapterMsgs projects the request's messages onto adapter.Msg: string content
// passes through, an array's text parts join, and its image parts stay attached
// to the message that carried them so the pipeline uploads each image with the
// turn it belongs to (IMG-*, SDD §5.2).
func (req *chatRequest) toAdapterMsgs() ([]adapter.Msg, error) {
	out := make([]adapter.Msg, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "" {
			return nil, errMalformedRequest
		}
		content, media, ferr := flattenContent(m.Content)
		if ferr != nil {
			return nil, ferr
		}
		out = append(out, adapter.Msg{Role: m.Role, Content: content, Media: media})
	}
	return out, nil
}

// imageDetails returns the non-empty image_url.detail values in one message
// content, in order.
func imageDetails(raw json.RawMessage) []string {
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if p.Type == "image_url" {
			if d := strings.TrimSpace(p.ImageURL.Detail); d != "" {
				out = append(out, d)
			}
		}
	}
	return out
}

// imageDetailErr validates OpenAI's image_url.detail (auto|low|high). A value
// outside that set is a 400 naming the field: the level cannot be honoured, but a
// typo must not look like it was.
func (req *chatRequest) imageDetailErr() error {
	for _, m := range req.Messages {
		for _, d := range imageDetails(m.Content) {
			switch strings.ToLower(d) {
			case "auto", "low", "high":
			default:
				return &unsupportedImageDetailError{Detail: d}
			}
		}
	}
	return nil
}

// mediaErr bounds how many images one request may carry. It runs before any
// account work, because an over-limit request must not cost a lease — and an
// image is the expensive part of a request (three upstream calls each).
func (req *chatRequest) mediaErr(maxImages int) error {
	total := 0
	for _, m := range req.Messages {
		if _, media, err := flattenContent(m.Content); err == nil {
			total += len(media)
		}
	}
	if maxImages > 0 && total > maxImages {
		return &tooManyImagesError{Count: total, Max: maxImages}
	}
	return nil
}

// flattenContent resolves a message Content RawMessage: JSON string → itself;
// array → joined text parts plus the image parts in order (image_url, IMG-001);
// anything else → malformed. Unknown part types are ignored, not rejected
// (permissive decode, SDD §5.2) — only an image part we cannot fetch is refused,
// and it is named.
func flattenContent(raw json.RawMessage) (string, []adapter.MediaRef, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		var media []adapter.MediaRef
		for _, p := range parts {
			switch p.Type {
			case "text", "":
				texts = append(texts, p.Text)
			case "image_url":
				u := strings.TrimSpace(p.ImageURL.URL)
				if u == "" {
					return "", nil, fmt.Errorf("%w: image_url.url is empty", errUnsupportedImageURL)
				}
				if !imageURLSupported(u) {
					// Say which scheme was refused. The bare sentinel used to answer
					// "unsupported image_url" and nothing else, which told the caller
					// neither what was wrong nor what is accepted.
					return "", nil, fmt.Errorf("%w: scheme %q is not fetchable, use https:, http: or data:image/...",
						errUnsupportedImageURL, schemeOf(u))
				}
				// The level travels with the image so the upload path can apply
				// it: OpenAI's field is the client's, the conversion to bytes the
				// channel receives is ours (IMG-013).
				media = append(media, adapter.MediaRef{URL: u, Detail: strings.TrimSpace(p.ImageURL.Detail)})
			default:
				// Unknown part types are ignored, not rejected (permissive
				// decode, SDD §5.2).
			}
		}
		return strings.Join(texts, "\n"), media, nil
	}
	return "", nil, errMalformedRequest
}

// schemeOf is the scheme of a URL-shaped string, shortened, for an error message.
func schemeOf(u string) string {
	scheme := u
	if i := strings.IndexByte(scheme, ':'); i >= 0 {
		scheme = scheme[:i]
	}
	if len(scheme) > 20 {
		scheme = scheme[:20] + "..."
	}
	return strings.ToLower(scheme)
}

// imageURLSupported accepts exactly what we can fetch without guessing: a data:
// URL carrying an image, or an http(s) URL (IMG-001/002). A file:// or a
// non-image data URL is a 400 naming the field, never a silent drop.
func imageURLSupported(u string) bool {
	lower := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lower, "data:"):
		// Any data: URL passes here: whether its declared type is an image is a
		// question about content, and the decode path answers it with a message
		// that names the type (unsupported_image_type). Two gates refusing the same
		// input with two different codes was how this one ended up explaining
		// nothing.
		return true
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return true
	default:
		return false
	}
}

// warningsHeader renders X-Mimo-Warnings as two separate statements, because they
// mean different things to a caller:
//
//   - "ignored unsupported fields" — the request named something we do not know at
//     all (tools, max_tokens, seed…): a stripped field.
//   - "accepted but not honoured" — a real OpenAI field we parse and validate, but
//     which nothing in the pipeline can act on (web_search_options.search_context_size:
//     the search switch has no depth). It is reported because a client that asked for
//     a bigger search context must not be left believing it got one — silence there is
//     a lie by omission. image_url.detail is deliberately NOT in this clause: the
//     gateway applies it to the uploaded bytes (applyImageDetail), so it is honoured
//     rather than reported.
func (req *chatRequest) warningsHeader() string {
	var clauses []string
	if dropped := req.droppedFields(); len(dropped) > 0 {
		clauses = append(clauses, "ignored unsupported fields: "+warnList(dropped))
	}
	if n := req.notHonouredFields(); len(n) > 0 {
		clauses = append(clauses, "accepted but not honoured: "+warnList(n))
	}
	return strings.Join(clauses, "; ")
}

// The warning lists are built from client-supplied JSON keys, so they are not
// allowed to be as long as the client wants. Measured before this bound: a request
// carrying 3000 unknown fields produced a warning header past 64 KiB and the caller
// could not parse the response at all (Python's http.client raises LineTooLong on a
// header line over 65536 bytes) — a client that sends extra metadata got no answer.
const (
	maxWarnFields   = 8
	maxWarnFieldLen = 40
)

// warnList renders a client-supplied field list for X-Mimo-Warnings: deduplicated,
// sorted, each name shortened, and the total preserved when the list is truncated.
// Sorting is not cosmetic — JSON objects decode into a map, so the same request used
// to produce a different order every time.
func warnList(names []string) string {
	seen := make(map[string]bool, len(names))
	uniq := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if len(n) > maxWarnFieldLen {
			n = n[:maxWarnFieldLen] + "..."
		}
		uniq = append(uniq, n)
	}
	sort.Strings(uniq)
	if len(uniq) > maxWarnFields {
		return fmt.Sprintf("%v +%d more", uniq[:maxWarnFields], len(uniq)-maxWarnFields)
	}
	return fmt.Sprintf("%v", uniq)
}

// notHonouredFields names the fields we understand but cannot act on. Distinct
// from droppedFields, which are fields we do not understand at all.
func (req *chatRequest) notHonouredFields() []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	// OpenAI's search_context_size has no upstream counterpart (the switch is only
	// disabled|auto|enabled): the search happens as asked, at the platform's own
	// granularity.
	if req.WebSearchOptions != nil && strings.TrimSpace(req.WebSearchOptions.SearchContextSize) != "" {
		add("web_search_options.search_context_size")
	}
	return out
}

// droppedFields names every stripped request field for X-Mimo-Warnings
// (SDD §5.2, TDD API-008/009).
func (req *chatRequest) droppedFields() []string {
	var dropped []string
	if req.MaxTokens != 0 {
		dropped = append(dropped, "max_tokens")
	}
	if req.MaxCompletionTok != 0 {
		dropped = append(dropped, "max_completion_tokens")
	}
	if req.Tools != nil {
		dropped = append(dropped, "tools")
	}
	if req.ToolChoice != nil {
		dropped = append(dropped, "tool_choice")
	}
	if req.Functions != nil {
		dropped = append(dropped, "functions")
	}
	if req.ResponseFormat != nil {
		dropped = append(dropped, "response_format")
	}
	if req.N != nil {
		dropped = append(dropped, "n")
	}
	if req.Seed != nil {
		dropped = append(dropped, "seed")
	}
	if req.Stop != nil {
		dropped = append(dropped, "stop")
	}
	if req.User != "" {
		dropped = append(dropped, "user")
	}
	return append(dropped, req.Other...)
}

// searchStatus validates web_search_status (M19). Empty means "not given" and
// is resolved later (the model suffix may still turn search on).
func (r chatRequest) searchStatus() (string, error) {
	v := strings.ToLower(strings.TrimSpace(r.WebSearchStatus))
	switch v {
	case "", "auto", "enabled", "disabled":
		return v, nil
	default:
		return "", errUnsupportedSearchStatus
	}
}

// citationsWanted reports whether the response should carry citations.
// Default true: the sources are the point of a search turn (M19).
func (r chatRequest) citationsWanted() bool {
	return r.IncludeCitations == nil || *r.IncludeCitations
}
