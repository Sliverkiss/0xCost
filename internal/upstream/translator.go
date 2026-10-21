package upstream

import (
	"fmt"
	"strings"
	"time"
)

// Finish reasons emitted on the OpenAI surface.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishError     = "error"
)

// TokenStats is the usage estimate reported to clients. The upstream SSE
// protocol does not expose token counts, so these are estimates derived from
// the translated text. Estimated is true for that reason.
type TokenStats struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"-"`
}

// Usage returns the OpenAI usage object.
func (t TokenStats) Usage() map[string]any {
	return map[string]any{
		"prompt_tokens":     t.PromptTokens,
		"completion_tokens": t.CompletionTokens,
		"total_tokens":      t.TotalTokens,
	}
}

// EstimateTokens approximates a token count from text. Chinese text is roughly
// one token per CJK character and per punctuation mark; Latin text averages
// about four characters per token. The two are counted separately so mixed
// answers are not underestimated by a single divisor.
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		switch {
		case r >= 0x2E80 && r <= 0x9FFF, // CJK radicals .. unified ideographs
			r >= 0xF900 && r <= 0xFAFF, // compatibility ideographs
			r >= 0xFF00 && r <= 0xFFEF, // fullwidth forms
			r >= 0x3000 && r <= 0x303F, // CJK punctuation
			r >= 0xAC00 && r <= 0xD7AF: // Hangul
			cjk++
		default:
			other++
		}
	}
	n := cjk + (other+3)/4
	if n < 1 && (cjk > 0 || other > 0) {
		n = 1
	}
	return n
}

// Delta is one incremental OpenAI delta produced from an upstream frame.
type Delta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is one OpenAI streaming tool_calls entry. Index identifies the
// call; id/type/function are emitted once on the first fragment.
type ToolCallDelta struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function *FunctionDelta `json:"function,omitempty"`
}

// FunctionDelta is the function fragment of a tool call.
type FunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall is the non-streaming OpenAI tool call shape.
type ToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function ToolCallFunctionOut `json:"function"`
}

// ToolCallFunctionOut is the non-streaming function object.
type ToolCallFunctionOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Translator converts the upstream snapshot stream into deltas.
//
// Upstream re-sends each part in full (parts keyed by logic_id; text items may
// be cumulative or incremental depending on the frame). The translator
// reconciles both shapes per part:
//
//   - each part tracks the last emitted text/think prefix;
//   - a new snapshot that starts with the emitted prefix yields the suffix as a
//     delta (cumulative mode);
//   - a snapshot that does not extend it is treated as a fresh fragment
//     (incremental mode, used by the thinking/search samples);
//   - identical snapshots (the repeated final frame) yield nothing.
//
// The first observed ordering of logic_ids defines output order. Tool calls are
// emitted once, when a part carrying tool_calls reaches status "finish" with a
// non-empty call object; empty think-tool close frames are ignored.
type Translator struct {
	Model string

	order   []string
	state   map[string]*partState
	tools   []ToolCall
	toolIDs map[string]bool

	// promptText is used for the usage estimate only.
	promptText string
}

type partState struct {
	textSent  string
	thinkSent string
	toolDone  bool
}

// NewTranslator builds a translator for one request.
func NewTranslator(model, promptText string) *Translator {
	return &Translator{
		Model:      model,
		state:      map[string]*partState{},
		toolIDs:    map[string]bool{},
		promptText: promptText,
	}
}

// FrameResult is the translation of one upstream frame.
type FrameResult struct {
	// Delta carries reasoning/content/tool-call increments. Role is set only on
	// the first delta produced.
	Delta Delta
	// Finish is non-empty when the frame terminates the message.
	Finish string
	// Err is set for upstream error frames.
	Err error
}

// Consume translates one frame. Subsequent frames must be passed in order.
func (t *Translator) Consume(f *Frame) FrameResult {
	var res FrameResult
	if f == nil {
		return res
	}
	if f.Status == "error" || hasPartError(f) {
		res.Err = frameError(f)
		res.Finish = FinishError
		return res
	}

	var reasoning, content strings.Builder
	for i := range f.Parts {
		part := &f.Parts[i]
		st := t.partState(part)
		for _, c := range part.Content {
			switch c.Type {
			case "think":
				if d := reconcile(&st.thinkSent, c.Think); d != "" {
					reasoning.WriteString(d)
				}
			case "tool_calls":
				if call := t.noteTool(part, c); call != nil {
					res.Delta.ToolCalls = append(res.Delta.ToolCalls, ToolCallDelta{
						Index: len(t.tools) - 1,
						ID:    call.ID,
						Type:  "function",
						Function: &FunctionDelta{
							Name:      call.Function.Name,
							Arguments: call.Function.Arguments,
						},
					})
				}
			case "tool_result":
				// Web-search results are server-side tool execution: their content
				// already informs the model, so they are not replayed to OpenAI
				// clients as tool_calls. Any readable payload is folded into text.
				if txt := c.TextValue(); txt != "" {
					if d := reconcile(&st.textSent, txt); d != "" {
						content.WriteString(d)
					}
				}
			default:
				txt := c.TextValue()
				if txt == "" {
					continue
				}
				if d := reconcile(&st.textSent, txt); d != "" {
					content.WriteString(d)
				}
			}
		}
	}

	res.Delta.ReasoningContent = reasoning.String()
	res.Delta.Content = content.String()
	if f.Status == "finish" || f.Status == "intervene" {
		if len(t.tools) > 0 {
			res.Finish = FinishToolCalls
		} else {
			res.Finish = FinishStop
		}
	}
	return res
}

func (t *Translator) partState(part *Part) *partState {
	key := part.LogicID
	if key == "" {
		key = part.ID
	}
	st, ok := t.state[key]
	if !ok {
		st = &partState{}
		t.state[key] = st
		t.order = append(t.order, key)
	}
	return st
}

// noteTool records a completed upstream tool call exactly once. It returns the
// call in OpenAI shape when this frame is the first to carry a complete call.
//
// A call is only complete once its part reaches status "finish": upstream may
// publish a call item while its arguments are still being produced, and
// emitting that snapshot would mark the part done and suppress the complete
// arguments that follow.
func (t *Translator) noteTool(part *Part, c Content) *ToolCall {
	if c.ToolCalls == nil || part.Status != "finish" {
		return nil
	}
	key := part.LogicID
	if key == "" {
		key = part.ID
	}
	st := t.state[key]
	if st != nil && st.toolDone {
		return nil
	}
	name := strings.TrimSpace(c.ToolCalls.Name)
	args := c.ToolCalls.ArgumentsString()
	if name == "" || name == "finish" {
		// Empty close frames and the model's internal "finish" marker carry no
		// client-visible call. Calls with no parameters ("{}") are real tools.
		return nil
	}
	if st != nil {
		st.toolDone = true
	}
	id := c.ToolCalls.ID
	if id == "" {
		id = fmt.Sprintf("call_%d_%d", len(t.tools), time.Now().UnixNano())
	}
	if t.toolIDs[id] {
		return nil
	}
	t.toolIDs[id] = true
	call := ToolCall{
		ID:   id,
		Type: "function",
		Function: ToolCallFunctionOut{
			Name:      name,
			Arguments: args,
		},
	}
	t.tools = append(t.tools, call)
	return &call
}

// reconcile updates *sent with the complete text observed for a part and
// returns the newly visible suffix.
func reconcile(sent *string, full string) string {
	if full == "" {
		return ""
	}
	prev := *sent
	if full == prev {
		return ""
	}
	if strings.HasPrefix(full, prev) {
		*sent = full
		return full[len(prev):]
	}
	// The snapshot does not extend the previous value: treat it as a fresh
	// incremental fragment. Common when a part switches from think to text or
	// resends a shortened snapshot.
	*sent = prev + full
	return full
}

func hasPartError(f *Frame) bool {
	for _, p := range f.Parts {
		if p.Status == "error" {
			return true
		}
	}
	return false
}

func frameError(f *Frame) error {
	msg := ""
	if f.LastError != nil {
		msg = firstString(f.LastError, "err_msg", "message", "msg")
		if msg == "" {
			msg = flattenValue(f.LastError)
		}
	}
	for _, p := range f.Parts {
		if p.Status != "error" {
			continue
		}
		if m := p.MetaData.ToolResultExtra; m != nil {
			if s := firstString(m, "err_msg", "message"); s != "" && msg == "" {
				msg = s
			}
		}
	}
	if msg == "" {
		msg = "upstream returned an error frame"
	}
	return &APIError{Kind: ErrUpstream, Status: 502, Message: msg}
}

// Tools returns the tool calls completed so far.
func (t *Translator) Tools() []ToolCall { return append([]ToolCall(nil), t.tools...) }

// Usage estimates token usage for the request and the generated text.
func (t *Translator) Usage(promptText, completion string, reasoning string) TokenStats {
	prompt := EstimateTokens(promptText)
	if prompt == 0 && t.promptText != "" {
		prompt = EstimateTokens(t.promptText)
	}
	completionTokens := EstimateTokens(completion) + EstimateTokens(reasoning)
	return TokenStats{
		PromptTokens:     prompt,
		CompletionTokens: completionTokens,
		TotalTokens:      prompt + completionTokens,
		Estimated:        true,
	}
}
