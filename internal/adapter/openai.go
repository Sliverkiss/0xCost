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
// PromptTokensDetails and CompletionTokensDetails are separate types on purpose:
// OpenAI's two detail objects hold different counters (prompt: cached_tokens,
// completion: reasoning_tokens), and one shared struct would pad each object with
// the other's key — shape noise a client validating against the schema would see.
// No omitempty on either: both objects are always sent, zero-filled when a counter
// does not apply, so nobody has to branch on their absence. A zero means "the
// upstream counted none", never an invented value (TDD API-004).
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Usage is the OpenAI usage object. Detail fields are pointer-typed so they
// are omitted entirely when upstream sent none.
type Usage struct {
	PromptTokens      int                      `json:"prompt_tokens"`
	CompletionTokens  int                      `json:"completion_tokens"`
	TotalTokens       int                      `json:"total_tokens"`
	PromptDetails     *PromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionDetails *CompletionTokensDetails `json:"completion_tokens_details"`
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
	// Citations is a gateway extension (M19): one extra chunk with
	// choices:[] carries the web-search results. Standard clients ignore a
	// chunk with no choices, so the stream stays compatible.
	Citations []Citation `json:"citations,omitempty"`
}

// ShapeCitationsChunk returns the extension chunk carrying web-search
// citations. It is emitted at most once, right before the usage chunk, and
// only when the upstream actually returned results.
func ShapeCitationsChunk(id, model string, created int64, cits []Citation) Chunk {
	return Chunk{
		ID:        id,
		Object:    "chat.completion.chunk",
		Created:   created,
		Model:     model,
		Choices:   []ChunkChoice{},
		Citations: cits,
	}
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

// MapUsage converts the last upstream usage event into the OpenAI usage object.
// Both detail objects are always present — zero-filled when the upstream counted
// nothing — because OpenAI's own responses always carry them and a client should
// not have to branch on a missing key. Nil → zeros, never invented (TDD API-004).
func MapUsage(u *MiMoUsage) Usage {
	out := Usage{PromptDetails: &PromptTokensDetails{}, CompletionDetails: &CompletionTokensDetails{}}
	if u == nil {
		return out
	}
	out.PromptTokens = u.PromptTokens
	out.CompletionTokens = u.CompletionTokens
	out.TotalTokens = u.TotalTokens
	if u.NativeUsage != nil {
		if u.NativeUsage.PromptDetails != nil {
			out.PromptDetails.CachedTokens = u.NativeUsage.PromptDetails.CachedTokens
		}
		if u.NativeUsage.CompletionDetails != nil {
			out.CompletionDetails.ReasoningTokens = u.NativeUsage.CompletionDetails.ReasoningTokens
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
	// Always present, exactly like MapUsage: both objects are part of the shape.
	sum.PromptDetails = &PromptTokensDetails{CachedTokens: cachedOf(a) + cachedOf(b)}
	sum.CompletionDetails = &CompletionTokensDetails{ReasoningTokens: reasoningOf(a) + reasoningOf(b)}
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

// CompletionMessage is the non-streaming response message (SDD §5.4). Field set
// follows OpenAI's ChatCompletionMessage: refusal is always present (null when the
// model refused nothing) because a client validating against that schema expects
// the key, and annotations appear when a search turn produced citations.
type CompletionMessage struct {
	Role             string       `json:"role"`
	Content          string       `json:"content"`
	Refusal          *string      `json:"refusal"`
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	Annotations      []Annotation `json:"annotations,omitempty"`
}

// CompletionChoice is the single element of the non-streaming choices array.
// logprobs is emitted as null: this gateway never returns token log-probabilities,
// and OpenAI's own responses carry the key with null for clients that do not ask
// for them.
type CompletionChoice struct {
	Index        int               `json:"index"`
	Message      CompletionMessage `json:"message"`
	Logprobs     any               `json:"logprobs"`
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
	// Citations is a gateway extension (M19) duplicating the message
	// annotations at the top level so a client can read sources without
	// walking choices.
	Citations []Citation `json:"citations,omitempty"`
}

// ShapeCompletion assembles the non-streaming response. reasoning_content is
// omitted entirely when empty (TDD API-001); citations arrive via the optional
// slice (M19) and are omitted when there are none.
func ShapeCompletion(id, model string, created int64, content, reasoning string, usage Usage, cits ...Citation) Completion {
	return Completion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []CompletionChoice{{
			Index: 0,
			Message: CompletionMessage{
				Role:             "assistant",
				Content:          content,
				ReasoningContent: reasoning,
				Annotations:      Annotations(cits),
			},
			FinishReason: "stop",
		}},
		Usage:     usage,
		Citations: cits,
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
