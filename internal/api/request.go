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

	// Thinking opt-ins (SDD §5.2): enable_thinking or
	// reasoning:{"type":"enabled"}.
	EnableThinking bool `json:"enable_thinking"`
	// Search controls (M19). web_search_status maps 1:1 onto the upstream
	// modelConfig.webSearchStatus; the model suffix ":online" is shorthand
	// for "enabled" (TDD WS-001/003).
	WebSearchStatus string `json:"web_search_status"`
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

// knownRequestFields is the set of top-level fields this API understands;
// anything outside it is reported (never rejected, SDD §5.2).
var knownRequestFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "temperature": true,
	"top_p": true, "enable_thinking": true, "reasoning": true,
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

// thinkingEnabled resolves the two opt-in spellings (SDD §5.2, TDD
// API-010/011).
func (req *chatRequest) thinkingEnabled() bool {
	if req.EnableThinking {
		return true
	}
	return req.Reasoning != nil && strings.EqualFold(req.Reasoning.Type, "enabled")
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
