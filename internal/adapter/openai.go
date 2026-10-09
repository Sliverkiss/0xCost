package adapter

import (
	"encoding/json"
	"time"
)

// Delta is the OpenAI streaming delta object. Fields are omitempty so a
// terminal chunk serializes delta as {} (SDD §5.4).
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChunkChoice is the single element of the choices array in a streaming chunk.
// FinishReason is a pointer so it serializes as null until termination.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// UsageDetails carries cached/reasoning token detail mapped from upstream
// nativeUsage (SDD §5.4).
type UsageDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// Usage is the OpenAI usage object. Detail fields are pointer-typed so they
// are omitted entirely when upstream sent none.
type Usage struct {
	PromptTokens      int           `json:"prompt_tokens"`
	CompletionTokens  int           `json:"completion_tokens"`
	TotalTokens       int           `json:"total_tokens"`
	PromptDetails     *UsageDetails `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *UsageDetails `json:"completion_tokens_details,omitempty"`
}

// Chunk is one OpenAI chat.completion.chunk frame. JSON keys are exactly
// {id, object, created, model, choices} — or {id, object, created, model,
// choices:[], usage} for the final usage chunk (SDD §5.4, TDD API-003).
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// NewChatID mints a chatcmpl-<uuid> id shared by all chunks of one stream
// (SDD §5.4: one id for the whole stream).
func NewChatID() string {
	return "chatcmpl-" + RandomHex32()
}

// ShapeRoleChunk returns the first chunk of a stream: role-only delta
// ({"role":"assistant"}), finish_reason null (TDD API-003).
func ShapeRoleChunk(id, model string, created int64) Chunk {
	return Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant"}, FinishReason: nil}},
	}
}

// ShapeTextChunk returns a delta chunk carrying either content or
// reasoning_content (never both — the splitter never emits both in one piece).
func ShapeTextChunk(id, model string, created int64, piece Piece) Chunk {
	d := Delta{}
	switch piece.Type {
	case "reasoning":
		d.ReasoningContent = piece.Text
	default:
		d.Content = piece.Text
	}
	return Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChunkChoice{{Index: 0, Delta: d, FinishReason: nil}},
	}
}

// ShapeFinishChunk returns the terminal choice chunk: delta {} and
// finish_reason "stop".
func ShapeFinishChunk(id, model string, created int64) Chunk {
	stop := "stop"
	return Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{}, FinishReason: &stop}},
	}
}

// ShapeUsageChunk returns the usage chunk with choices:[] so it cannot
// clobber finish_reason (SDD §5.4 bug note from mimo-web-proxy).
func ShapeUsageChunk(id, model string, created int64, usage Usage) Chunk {
	return Chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChunkChoice{},
		Usage:   &usage,
	}
}

// MiMoUsage is the upstream `usage` SSE event payload (SDD §2.5). Emitted
// multiple times; last one wins.
type MiMoUsage struct {
	PromptTokens     int          `json:"promptTokens"`
	CompletionTokens int          `json:"completionTokens"`
	TotalTokens      int          `json:"totalTokens"`
	NativeUsage      *NativeUsage `json:"nativeUsage,omitempty"`
}

// NativeUsage mirrors upstream nativeUsage (snake_case fields).
type NativeUsage struct {
	PromptTokens      int           `json:"prompt_tokens"`
	CompletionTokens  int           `json:"completion_tokens"`
	TotalTokens       int           `json:"total_tokens"`
	PromptDetails     *NativeCached `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *NativeThink  `json:"completion_tokens_details,omitempty"`
}

// NativeCached is nativeUsage.prompt_tokens_details.
type NativeCached struct {
	CachedTokens int `json:"cached_tokens"`
}

// NativeThink is nativeUsage.completion_tokens_details.
type NativeThink struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// MapUsage converts the last upstream usage event into the OpenAI usage
// object. Nil → zeros, never invented (TDD API-004).
func MapUsage(u *MiMoUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.NativeUsage != nil {
		if u.NativeUsage.PromptDetails != nil {
			out.PromptDetails = &UsageDetails{CachedTokens: u.NativeUsage.PromptDetails.CachedTokens}
		}
		if u.NativeUsage.CompletionDetails != nil {
			out.CompletionDetails = &UsageDetails{ReasoningTokens: u.NativeUsage.CompletionDetails.ReasoningTokens}
		}
	}
	return out
}

// AddUsage accumulates usage across replayed turns (the chain-replay path
// sums usage over its calls).
func AddUsage(a, b Usage) Usage {
	sum := Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
	if (a.PromptDetails != nil || b.PromptDetails != nil) && cachedOf(a)+cachedOf(b) > 0 {
		sum.PromptDetails = &UsageDetails{CachedTokens: cachedOf(a) + cachedOf(b)}
	}
	if (a.CompletionDetails != nil || b.CompletionDetails != nil) && reasoningOf(a)+reasoningOf(b) > 0 {
		sum.CompletionDetails = &UsageDetails{ReasoningTokens: reasoningOf(a) + reasoningOf(b)}
	}
	return sum
}

func cachedOf(u Usage) int {
	if u.PromptDetails != nil {
		return u.PromptDetails.CachedTokens
	}
	return 0
}

func reasoningOf(u Usage) int {
	if u.CompletionDetails != nil {
		return u.CompletionDetails.ReasoningTokens
	}
	return 0
}

// CompletionMessage is the non-streaming response message (SDD §5.4).
type CompletionMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// CompletionChoice is the single element of the non-streaming choices array.
type CompletionChoice struct {
	Index        int               `json:"index"`
	Message      CompletionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

// Completion is the non-streaming response body (SDD §5.4).
type Completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   Usage              `json:"usage"`
}

// ShapeCompletion assembles the non-streaming response. reasoning_content is
// omitted entirely when empty (TDD API-001).
func ShapeCompletion(id, model string, created int64, content, reasoning string, usage Usage) Completion {
	return Completion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []CompletionChoice{{
			Index:        0,
			Message:      CompletionMessage{Role: "assistant", Content: content, ReasoningContent: reasoning},
			FinishReason: "stop",
		}},
		Usage: usage,
	}
}

// EncodeChunk serializes a chunk for the SSE writer (the data: payload).
func EncodeChunk(c Chunk) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// NowUnix is the current unix-seconds timestamp used for chunk `created`.
var NowUnix = func() int64 { return time.Now().Unix() }
