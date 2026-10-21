package upstream

import (
	"fmt"
	"strings"
)

// PromptMessage is the subset of an OpenAI message the gateway consumes.
type PromptMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	Name       string `json:"name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
}

// BuildPrompt flattens an OpenAI message list into one upstream user message.
//
// Each guest identity is used for exactly one request, so the upstream thread
// is always fresh: the conversation must travel in the prompt. Roles are
// labelled and separated by blank lines, which the model reads reliably; the
// trailing "Assistant:" marker is the same convention the independent glm2api
// gateway uses.
func BuildPrompt(messages []PromptMessage) string {
	var b strings.Builder
	for _, m := range messages {
		role := strings.TrimSpace(m.Role)
		text := extractText(m.Content)
		switch role {
		case "system", "developer":
			if text == "" {
				continue
			}
			b.WriteString("System: ")
			b.WriteString(text)
			b.WriteString("\n\n")
		case "user":
			if text == "" {
				continue
			}
			b.WriteString("User: ")
			b.WriteString(text)
			b.WriteString("\n\n")
		case "assistant":
			if len(m.ToolCalls) > 0 {
				b.WriteString("Assistant: ")
				b.WriteString(text)
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "\n[tool_call %s %s(%s)]", tc.ID, tc.Function.Name, tc.Function.Arguments)
				}
				b.WriteString("\n\n")
				continue
			}
			if text == "" {
				continue
			}
			b.WriteString("Assistant: ")
			b.WriteString(text)
			b.WriteString("\n\n")
		case "tool", "function":
			if text == "" {
				continue
			}
			name := m.Name
			if name == "" {
				name = "tool"
			}
			fmt.Fprintf(&b, "Tool result (%s): %s\n\n", name, text)
		default:
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n\n", role, text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return ""
	}
	return out + "\n\nAssistant:"
}

// extractText renders OpenAI message content: a string, or an array of typed
// parts (text and image_url). Images are referenced by URL rather than
// uploaded; the upstream text model ignores the reference gracefully.
func extractText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text":
				if s, ok := m["text"].(string); ok && s != "" {
					parts = append(parts, s)
				}
			case "image_url", "input_image":
				if v, ok := m["image_url"]; ok {
					switch iv := v.(type) {
					case string:
						parts = append(parts, "[image: "+iv+"]")
					case map[string]any:
						if u, ok := iv["url"].(string); ok {
							parts = append(parts, "[image: "+u+"]")
						}
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}
