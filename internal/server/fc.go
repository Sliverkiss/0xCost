package server

import (
	"encoding/json"
	"fmt"
	"log"

	"chatglm2api/internal/config"
	"chatglm2api/internal/toolfc"
	"chatglm2api/internal/upstream"

	"github.com/Sliverkiss/toolkit/models"
)

// fcContext is the prompt-FC state for one request: the offered tools, the
// normalized tool_choice and the requested parse options.
type fcContext struct {
	tools      []models.ToolDef
	choice     toolfc.Choice
	stripThink bool
	bytes      int
	active     bool
	outcome    string
}

// prepareTools validates the client's tools, applies tool_choice and renders
// the XYML-injected upstream prompt messages.
func (s *Server) prepareTools(req *ChatRequest) ([]upstream.PromptMessage, *fcContext, *ValidationError, error) {
	if s.cfg.Config.Chat.FCMode == config.FCModeOff {
		return nil, nil, &ValidationError{Param: "tools", Message: "tools are not supported by this gateway (chat.fc_mode=off)"}, nil
	}
	rawTools, err := json.Marshal(req.Tools)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode tools: %w", err)
	}
	tools, err := toolfc.ToolsFromRaw(rawTools)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode tools: %w", err)
	}
	if len(tools) != len(req.Tools) {
		return nil, nil, &ValidationError{Param: "tools", Message: "every tool must be a function with a non-empty name"}, nil
	}
	choice, err := toolfc.ParseChoice(req.ToolChoice, tools)
	if err != nil {
		return nil, nil, &ValidationError{Param: "tool_choice", Message: err.Error()}, nil
	}
	fc := &fcContext{
		tools:      tools,
		choice:     choice,
		stripThink: s.cfg.Config.Chat.StripThinkTags,
	}
	if choice.Mode == toolfc.ChoiceNone {
		// tool_choice "none" means the model must not call tools: send the plain
		// conversation with no XYML injection.
		fc.outcome = "choice_none"
		return promptMessages(req.Messages), fc, nil, nil
	}

	rawMessages, err := json.Marshal(req.Messages)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode messages: %w", err)
	}
	messages, err := toolfc.MessagesFromRaw(rawMessages)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode messages: %w", err)
	}
	injection := toolfc.BuildInjection(messages, tools, choice)
	fc.active = injection.Active
	fc.bytes = injection.Bytes
	return toolfc.PromptMessages(injection.Messages), fc, nil, nil
}

// promptMessages flattens client messages without function-call injection.
func promptMessages(messages []ChatMessage) []upstream.PromptMessage {
	out := make([]upstream.PromptMessage, 0, len(messages))
	for _, m := range messages {
		pm := upstream.PromptMessage{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
		var content any
		if len(m.Content) > 0 {
			if err := json.Unmarshal(m.Content, &content); err == nil {
				pm.Content = content
			}
		}
		for _, tc := range m.ToolCalls {
			var entry struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			entry.ID = tc.ID
			entry.Type = tc.Type
			entry.Function.Name = tc.Function.Name
			entry.Function.Arguments = tc.Function.Arguments
			pm.ToolCalls = append(pm.ToolCalls, entry)
		}
		out = append(out, pm)
	}
	return out
}

// logInjection records the injection decision at request level.
func (t *turnRunner) logInjection() {
	if t.fc == nil || !t.srv.cfg.Config.LogRequests {
		return
	}
	log.Printf("[fc] %s tools=%d choice=%s inject=%v bytes=%d",
		t.id, len(t.fc.tools), t.fc.describeChoice(), t.fc.active, t.fc.bytes)
}

// logFC records the parse outcome once per response.
func (t *turnRunner) logFC(res *turnResult, finish string) {
	if t.fc == nil || !t.srv.cfg.Config.LogRequests {
		return
	}
	outcome := res.fcOutcome
	if outcome == "" {
		outcome = "no_calls"
	}
	log.Printf("[fc] %s outcome=%s finish=%s", t.id, outcome, finish)
}

// fcOutcome returns the parse outcome recorded by streamTurn ("" without FC).
func fcOutcome(f *fcContext) string {
	if f == nil {
		return ""
	}
	return f.outcome
}

// apply parses the aggregated model text into tool calls (non-streaming).
func (f *fcContext) apply(res *turnResult) {
	if f == nil {
		return
	}
	if !f.active {
		f.outcome = "choice_none"
		res.fcOutcome = f.outcome
		return
	}
	result := toolfc.Reassemble(res.content, f.tools, f.stripThink)
	if result.Err != nil {
		f.outcome = "parse_error"
		res.fcOutcome = f.outcome
		return
	}
	f.outcome = result.Outcome()
	res.fcOutcome = f.outcome
	if clean, removed := toolfc.CleanText(res.reasoning); removed {
		res.reasoning = clean
	}
	if len(result.Calls) > 0 {
		res.tools = append(res.tools, toolfc.ToUpstreamCalls(result.Calls)...)
		res.finish = upstream.FinishToolCalls
	}
	// Content keeps any non-protocol preamble; markup itself is always stripped.
	res.content = result.Content
}

// streamToolDeltas renders parsed calls as OpenAI streaming tool_calls deltas,
// offset past any native upstream calls already emitted.
func streamToolDeltas(calls []models.ToolCall, offset int) []upstream.ToolCallDelta {
	if len(calls) == 0 {
		return nil
	}
	out := make([]upstream.ToolCallDelta, 0, len(calls))
	for i, call := range calls {
		args := "{}"
		if len(call.Arguments) > 0 {
			if raw, err := json.Marshal(call.Arguments); err == nil {
				args = string(raw)
			}
		}
		out = append(out, upstream.ToolCallDelta{
			Index: offset + i,
			ID:    call.ID,
			Type:  "function",
			Function: &upstream.FunctionDelta{
				Name:      call.Name,
				Arguments: args,
			},
		})
	}
	return out
}

// emittedToolCalls converts upstream call deltas into the response shape.
func emittedToolCalls(deltas []upstream.ToolCallDelta) []upstream.ToolCall {
	out := make([]upstream.ToolCall, 0, len(deltas))
	for _, d := range deltas {
		if d.Function == nil {
			continue
		}
		out = append(out, upstream.ToolCall{
			ID:   d.ID,
			Type: "function",
			Function: upstream.ToolCallFunctionOut{
				Name:      d.Function.Name,
				Arguments: d.Function.Arguments,
			},
		})
	}
	return out
}

// describeChoice renders the normalized tool_choice for logs.
func (f *fcContext) describeChoice() string {
	if f == nil {
		return "none"
	}
	if f.choice.Mode == toolfc.ChoiceNamed {
		return "named:" + f.choice.Tool
	}
	return string(f.choice.Mode)
}
