// Package server exposes the OpenAI-compatible HTTP surface and drives the
// identity pool plus the upstream chatglm client.
package server

import (
	"encoding/json"
	"fmt"
)

// ChatRequest is the OpenAI chat completion request the gateway accepts.
type ChatRequest struct {
	Model               string         `json:"model"`
	Messages            []ChatMessage  `json:"messages"`
	Stream              bool           `json:"stream"`
	StreamOptions       *StreamOptions `json:"stream_options,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	TopP                *float64       `json:"top_p,omitempty"`
	MaxTokens           *int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	Stop                any            `json:"stop,omitempty"`
	N                   *int           `json:"n,omitempty"`
	Logprobs            *bool          `json:"logprobs,omitempty"`
	TopLogprobs         *int           `json:"top_logprobs,omitempty"`
	Tools               []Tool         `json:"tools,omitempty"`
	ToolChoice          any            `json:"tool_choice,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	User                string         `json:"user,omitempty"`
	PresencePenalty     *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64       `json:"frequency_penalty,omitempty"`
	ResponseFormat      any            `json:"response_format,omitempty"`
	Seed                *int64         `json:"seed,omitempty"`
}

// ChatMessage is one OpenAI message.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
}

// ToolCall is an assistant tool invocation in the OpenAI shape.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a client-declared function tool.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// StreamOptions mirrors OpenAI's stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ContentText renders message content (string or typed parts) as text.
func (m ChatMessage) ContentText() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" {
				out += p.Text
			}
		}
		return out
	}
	return ""
}

// HasImageParts reports whether any message carries image content.
func (m ChatMessage) HasImageParts() bool {
	var parts []map[string]any
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return false
	}
	for _, p := range parts {
		if t, _ := p["type"].(string); t == "image_url" || t == "input_image" {
			return true
		}
	}
	return false
}

// ValidationError describes a client-side request problem.
type ValidationError struct {
	Param   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Param == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Param, e.Message)
}

// Validate enforces the supported subset of the OpenAI API. Features whose
// semantics the upstream cannot honour are rejected with 400 rather than
// silently ignored.
func (r *ChatRequest) Validate() *ValidationError {
	if len(r.Messages) == 0 {
		return &ValidationError{Param: "messages", Message: "messages must not be empty"}
	}
	if r.N != nil && *r.N > 1 {
		return &ValidationError{Param: "n", Message: "n > 1 is not supported"}
	}
	if r.Logprobs != nil && *r.Logprobs {
		return &ValidationError{Param: "logprobs", Message: "logprobs is not supported"}
	}
	if r.TopLogprobs != nil {
		return &ValidationError{Param: "top_logprobs", Message: "top_logprobs is not supported"}
	}
	if r.ResponseFormat != nil {
		var rf struct {
			Type string `json:"type"`
		}
		if raw, err := json.Marshal(r.ResponseFormat); err == nil && json.Unmarshal(raw, &rf) == nil {
			if rf.Type != "" && rf.Type != "text" {
				return &ValidationError{Param: "response_format", Message: "only text responses are supported"}
			}
		}
	}
	// A completion is generated in response to the client's final turn. A
	// trailing assistant/system message means the client is replaying history
	// with nothing to answer; every attempt would return an empty stream.
	switch last := r.Messages[len(r.Messages)-1].Role; last {
	case "user", "tool":
	default:
		return &ValidationError{Param: "messages", Message: "the last message must be a user or tool message"}
	}
	return nil
}

// Wire shapes for responses.

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []choice     `json:"choices"`
	Usage   *usageObject `json:"usage,omitempty"`
}

type choice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     any             `json:"logprobs"`
}

type responseMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type usageObject struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
	Logprobs     any        `json:"logprobs"`
}

type chunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []chunkToolCall `json:"tool_calls,omitempty"`
}

type chunkToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function *chunkFunctionD `json:"function,omitempty"`
}

type chunkFunctionD struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chunkResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usageObject  `json:"usage,omitempty"`
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelsResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// errorResponse is the OpenAI error envelope.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}
