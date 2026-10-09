package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// contentPart is one entry of a multi-part content array (SDD §5.2).
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
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
	EnableThinking  bool   `json:"enable_thinking"`
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

// thinkingEnabled resolves the thinking opt-ins (SDD §5.2, TDD API-010/011,
// OSTD-001..003). The standard `reasoning_effort` decides whenever it is
// present — including "none", which is the explicit off switch — and the older
// spellings only apply when it is absent.
func (req *chatRequest) thinkingEnabled() bool {
	if v := strings.ToLower(strings.TrimSpace(req.ReasoningEffort)); v != "" {
		return v != "none"
	}
	if req.EnableThinking {
		return true
	}
	return req.Reasoning != nil && strings.EqualFold(req.Reasoning.Type, "enabled")
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

// toAdapterMsgs flattens content parts: string content passes through;
// [{type:"text"}] parts join; image parts are rejected with 400 (SDD §5.2,
// TDD API-012).
func (req *chatRequest) toAdapterMsgs() (out []struct{ Role, Content string }, err error) {
	for _, m := range req.Messages {
		if m.Role == "" {
			return nil, errMalformedRequest
		}
		content, ferr := flattenContent(m.Content)
		if ferr != nil {
			return nil, ferr
		}
		out = append(out, struct{ Role, Content string }{m.Role, content})
	}
	return out, nil
}

// flattenContent resolves a message Content RawMessage: JSON string →
// itself; array → joined text parts (image_url → error); anything else →
// malformed.
func flattenContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			switch p.Type {
			case "text", "":
				texts = append(texts, p.Text)
			case "image_url":
				return "", errUnsupportedContent
			default:
				// Unknown part types are ignored, not rejected (permissive
				// decode, SDD §5.2).
			}
		}
		return strings.Join(texts, "\n"), nil
	}
	return "", errMalformedRequest
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
	// OpenAI's search_context_size has no upstream counterpart (the switch is
	// disabled|auto|enabled): search happens as asked, and the client is told
	// the granularity was not honoured instead of assuming it was.
	if req.WebSearchOptions != nil && strings.TrimSpace(req.WebSearchOptions.SearchContextSize) != "" {
		dropped = append(dropped, "web_search_options.search_context_size")
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
