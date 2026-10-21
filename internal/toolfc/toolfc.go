// Package toolfc adapts the toolkit library (github.com/Sliverkiss/toolkit) to
// chatglm2api's prompt-based function calling: client tool/message conversion,
// XYML prompt injection, response parsing/sanitizing and stream reassembly.
//
// The upstream (chatglm.cn) has no native function calling, so tools travel as
// an XYML instruction block inside the flattened prompt and are parsed back out
// of the model text. Toolkit owns the protocol; this package only bridges it to
// the gateway's own message types.
package toolfc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Sliverkiss/toolkit/engine"
	"github.com/Sliverkiss/toolkit/fc"
	"github.com/Sliverkiss/toolkit/jsonx"
	"github.com/Sliverkiss/toolkit/models"
	"github.com/Sliverkiss/toolkit/stream"

	"chatglm2api/internal/upstream"
)

// Protocol is the markup protocol used for prompt FC. The toolkit default is
// XYML, which is also what the instruct block advertises.
const Protocol = fc.DefaultProtocol

// ChoiceMode is the normalized tool_choice policy for one request.
type ChoiceMode string

// Supported tool_choice policies.
const (
	ChoiceAuto     ChoiceMode = "auto"
	ChoiceNone     ChoiceMode = "none"
	ChoiceRequired ChoiceMode = "required"
	ChoiceNamed    ChoiceMode = "named"
)

// Choice is the normalized tool_choice value.
type Choice struct {
	Mode ChoiceMode
	Tool string
}

// Injection is the prompt-FC request transformation.
type Injection struct {
	// Active is false when tools must not be injected (tool_choice "none").
	Active bool
	// Messages are the upstream chat messages (system instruction merged).
	Messages []map[string]any
	// Bytes is the approximate injected size for logging.
	Bytes int
}

// Result is the non-streaming response transformation.
type Result struct {
	Content string
	Calls   []models.ToolCall
	// Dropped reports markup that was removed but yielded no call (malformed or
	// truncated protocol text).
	Dropped bool
	Err     error
}

// Outcome renders the parse outcome for request logging.
func (r Result) Outcome() string {
	switch {
	case r.Err != nil:
		return "parse_error"
	case len(r.Calls) > 0:
		return fmt.Sprintf("calls=%d", len(r.Calls))
	case r.Dropped:
		return "dropped"
	default:
		return "no_calls"
	}
}

// MessagesFromRaw converts OpenAI chat messages (raw JSON) into canonical
// messages using the toolkit converter.
func MessagesFromRaw(raw json.RawMessage) ([]models.Message, error) {
	value, err := decodeRaw(raw)
	if err != nil {
		return nil, err
	}
	return models.OpenAIResultToMessages(value, true), nil
}

// ToolsFromRaw converts client tool definitions (raw JSON) into canonical tool
// definitions using the toolkit converter.
func ToolsFromRaw(raw json.RawMessage) ([]models.ToolDef, error) {
	value, err := decodeRaw(raw)
	if err != nil {
		return nil, err
	}
	return models.OpenAIToolsToDefs(value), nil
}

func decodeRaw(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// ParseChoice normalizes an OpenAI tool_choice value. Invalid or unknown
// values are rejected so the client learns the request cannot be honoured.
func ParseChoice(raw any, tools []models.ToolDef) (Choice, error) {
	switch v := raw.(type) {
	case nil:
		return Choice{Mode: ChoiceAuto}, nil
	case string:
		switch strings.TrimSpace(v) {
		case "", "auto":
			return Choice{Mode: ChoiceAuto}, nil
		case "none":
			return Choice{Mode: ChoiceNone}, nil
		case "required", "any":
			return Choice{Mode: ChoiceRequired}, nil
		default:
			return Choice{}, fmt.Errorf("tool_choice %q is not supported", v)
		}
	case map[string]any:
		name := ""
		if fn, ok := v["function"].(map[string]any); ok {
			name = strings.TrimSpace(jsonx.Str(fn["name"]))
		}
		if name == "" || !hasTool(tools, name) {
			return Choice{}, fmt.Errorf("tool_choice names an unknown tool")
		}
		return Choice{Mode: ChoiceNamed, Tool: name}, nil
	default:
		return Choice{}, fmt.Errorf("tool_choice has an unsupported shape")
	}
}

func hasTool(tools []models.ToolDef, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// BuildInjection renders the XYML-injected upstream messages. tool_choice
// "none" disables injection entirely; "required" and named choices add an
// explicit directive because the upstream protocol has no tool_choice field
// (toolkit's own prompt mode does not consume it either).
func BuildInjection(messages []models.Message, tools []models.ToolDef, choice Choice) Injection {
	if choice.Mode == ChoiceNone || len(tools) == 0 {
		return Injection{}
	}
	offered := tools
	if choice.Mode == ChoiceNamed {
		offered = filterTools(tools, choice.Tool)
	}
	injected := fc.InjectPromptMessages(messages, offered, Protocol, true)
	if directive := choiceDirective(choice); directive != "" {
		if len(injected) > 0 && injected[0]["role"] == "system" {
			injected[0]["content"] = jsonx.Str(injected[0]["content"]) + "\n\n" + directive
		} else {
			injected = append([]map[string]any{{"role": "system", "content": directive}}, injected...)
		}
	}
	bytes := 0
	for _, msg := range injected {
		bytes += len(jsonx.Str(msg["content"]))
	}
	return Injection{Active: true, Messages: injected, Bytes: bytes}
}

func filterTools(tools []models.ToolDef, name string) []models.ToolDef {
	out := make([]models.ToolDef, 0, 1)
	for _, tool := range tools {
		if tool.Name == name {
			out = append(out, tool)
		}
	}
	return out
}

func choiceDirective(choice Choice) string {
	switch choice.Mode {
	case ChoiceRequired:
		return "[TOOL CHOICE] You MUST call at least one of the available tools before answering."
	case ChoiceNamed:
		return fmt.Sprintf("[TOOL CHOICE] You MUST call the tool %q before answering.", choice.Tool)
	default:
		return ""
	}
}

// PromptMessages converts the injected OpenAI-shaped messages into the upstream
// prompt message type.
func PromptMessages(messages []map[string]any) []upstream.PromptMessage {
	out := make([]upstream.PromptMessage, 0, len(messages))
	for _, msg := range messages {
		out = append(out, upstream.PromptMessage{
			Role:    jsonx.Str(msg["role"]),
			Content: msg["content"],
		})
	}
	return out
}

// Reassemble parses a completed (non-streaming) assistant text into tool calls
// and returns the client-visible content with protocol fragments removed.
func Reassemble(text string, tools []models.ToolDef, stripThink bool) Result {
	source := text
	if stripThink {
		source = fc.StripThinkTags(text)
	}

	calls, err := fc.ParseTextToCalls(source, tools, Protocol, false)
	// The toolkit sieve removes every protocol block (parsed or not) so markup
	// can never reach the client.
	content, removed := CleanText(source)
	return Result{
		Content: strings.TrimSpace(content),
		Calls:   calls,
		Dropped: err == nil && len(calls) == 0 && removed,
		Err:     err,
	}
}

// looksLikeMarkup reports whether text carries any tool-call protocol marker.
func looksLikeMarkup(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{"tool_calls", "tool_call", "invoke", "<|xyml", "<|qnml", "<|dsml", "tool_parameter"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// Stream reassembles a live model stream: content deltas are emitted as they
// become leak-free, tool calls are emitted as soon as a complete block parses.
type Stream struct {
	tools      []models.ToolDef
	stripThink bool
	r          *stream.Reassembler
	text       strings.Builder
	seenCall   bool
	dropped    bool
	produced   bool
}

// NewStream builds an assembler for one response stream.
func NewStream(tools []models.ToolDef, stripThink bool) *Stream {
	return &Stream{tools: tools, stripThink: stripThink, r: stream.NewReassembler(tools, Protocol, 96)}
}

// Feed processes one model text fragment and returns the safe content delta and
// any completed tool calls.
func (s *Stream) Feed(chunk string) (string, []models.ToolCall) {
	s.text.WriteString(chunk)
	content, calls := s.collect(s.r.Feed(chunk))
	return content, calls
}

// collect folds sieve events into a content delta and parsed calls.
func (s *Stream) collect(events []engine.SieveEvent) (string, []models.ToolCall) {
	var content strings.Builder
	var calls []models.ToolCall
	for _, event := range events {
		switch event.Type {
		case "content":
			content.WriteString(event.Text)
		case "tool_calls":
			parsed := stream.Calls(event)
			if len(parsed) > 0 {
				calls = append(calls, parsed...)
				s.seenCall = true
			}
		}
	}
	if content.Len() > 0 {
		s.produced = true
	}
	return content.String(), calls
}

// Flush ends the stream, returning any trailing safe content and, when the sieve
// could not complete a block, the calls of a full-text parse.
func (s *Stream) Flush() (string, []models.ToolCall) {
	content, calls := s.collect(s.r.Flush())
	full := s.text.String()
	if len(calls) == 0 && !s.seenCall {
		if parsed, err := fc.ParseTextToCalls(full, s.tools, Protocol, s.stripThink); err == nil && len(parsed) > 0 {
			calls = parsed
			s.seenCall = true
			// The sieve already dropped the markup; re-derive the safe text so no
			// residue emitted before the block completed reaches the client.
			content = sanitizeStreamContent(full, s.tools)
			s.produced = content != ""
		}
	}
	s.dropped = len(calls) == 0 && !s.seenCall && looksLikeMarkup(full) && !s.produced
	return content, calls
}

// Dropped reports markup that was removed without producing a call.
func (s *Stream) Dropped() bool { return s.dropped }

// LeakGuard passes text through the toolkit sieve while discarding any parsed
// calls. It is used for reasoning_content, where a model may echo the XYML
// block: the block is removed and Removed reports the sanitization.
type LeakGuard struct {
	r      *stream.Reassembler
	input  strings.Builder
	output strings.Builder
}

// NewLeakGuard builds a guard for one text stream.
func NewLeakGuard() *LeakGuard {
	return &LeakGuard{r: stream.NewReassembler(nil, Protocol, 96)}
}

// Feed returns the safe content of one fragment.
func (g *LeakGuard) Feed(chunk string) string {
	g.input.WriteString(chunk)
	return g.collect(g.r.Feed(chunk))
}

// Flush returns any trailing safe content.
func (g *LeakGuard) Flush() string { return g.collect(g.r.Flush()) }

// Removed reports that protocol markup was observed and stripped.
func (g *LeakGuard) Removed() bool {
	return looksLikeMarkup(g.input.String()) && !looksLikeMarkup(g.output.String())
}

func (g *LeakGuard) collect(events []engine.SieveEvent) string {
	var content strings.Builder
	for _, event := range events {
		if event.Type == "content" {
			content.WriteString(event.Text)
		}
	}
	g.output.WriteString(content.String())
	return content.String()
}

// CleanText returns text with every protocol block removed. It is a one-shot
// guard used on complete reasoning strings.
func CleanText(text string) (string, bool) {
	guard := NewLeakGuard()
	out := guard.Feed(text) + guard.Flush()
	return out, guard.Removed()
}

// sanitizeStreamContent re-runs a one-shot sieve to recover leak-free text.
func sanitizeStreamContent(text string, tools []models.ToolDef) string {
	sieve := fc.CreateSieve(tools, Protocol, 96)
	var out strings.Builder
	for _, event := range sieve.ProcessChunk(text) {
		if event.Type == "content" {
			out.WriteString(event.Text)
		}
	}
	for _, event := range sieve.Flush() {
		if event.Type == "content" {
			out.WriteString(event.Text)
		}
	}
	return out.String()
}

// ToUpstreamCalls converts canonical tool calls to the upstream response shape.
func ToUpstreamCalls(calls []models.ToolCall) []upstream.ToolCall {
	out := make([]upstream.ToolCall, 0, len(calls))
	for _, call := range calls {
		args := "{}"
		if len(call.Arguments) > 0 {
			if raw, err := json.Marshal(call.Arguments); err == nil {
				args = string(raw)
			}
		}
		out = append(out, upstream.ToolCall{
			ID:   call.ID,
			Type: "function",
			Function: upstream.ToolCallFunctionOut{
				Name:      call.Name,
				Arguments: args,
			},
		})
	}
	return out
}
