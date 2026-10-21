// Package upstream implements the chatglm.cn API client: signed HTTP requests,
// guest identity calls, and translation of the upstream SSE protocol into
// OpenAI-compatible responses.
package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Frame is one `data:` event from /backend-api/assistant/stream. Every frame is
// a full snapshot of the assistant message; `parts` carry the accumulated
// content of each message part (see docs/reverse/SSE.md).
type Frame struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversation_id"`
	AssistantID    string         `json:"assistant_id"`
	Parts          []Part         `json:"parts"`
	CreatedAt      string         `json:"created_at"`
	Status         string         `json:"status"` // init | finish | error | intervene
	LastError      map[string]any `json:"last_error"`
	MetaData       FrameMeta      `json:"meta_data"`
}

// FrameMeta is the message-level metadata block.
type FrameMeta struct {
	InputQuestionType string  `json:"input_question_type"`
	IfPlusModel       bool    `json:"if_plus_model"`
	PlusModelAvail    bool    `json:"plus_model_available"`
	IfIncreasePush    bool    `json:"if_increase_push"`
	AnswerType        string  `json:"answer_type"`
	TotalTime         float64 `json:"total_time"`
}

// Part is one message part (an assistant answer, a thinking block, a tool
// call/result, or a hybrid of them).
type Part struct {
	ID       string    `json:"id"`
	LogicID  string    `json:"logic_id"`
	Role     string    `json:"role"`
	Model    string    `json:"model"`
	Status   string    `json:"status"` // init | finish | error
	Content  []Content `json:"content"`
	MetaData PartMeta  `json:"meta_data"`
}

// PartMeta carries streaming-phase hints.
type PartMeta struct {
	ShowType        string         `json:"show_type"`
	ToolResultExtra map[string]any `json:"tool_result_extra"`
}

// UpstreamToolCall is the upstream tool invocation object. Arguments is a JSON
// string in the normal case.
type UpstreamToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ArgumentsString normalises Arguments to a JSON string. Upstream sends a
// string in the normal case but has been observed to send an object.
func (t UpstreamToolCall) ArgumentsString() string {
	if len(t.Arguments) == 0 {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(t.Arguments, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	return string(t.Arguments)
}

// Content is one typed content item. The upstream has ~17 content types; we
// keep the raw object so unknown/future types can still be folded into text
// without a schema change.
type Content struct {
	Type      string            `json:"type"`
	Text      string            `json:"text,omitempty"`
	Think     string            `json:"think,omitempty"`
	ToolCalls *UpstreamToolCall `json:"tool_calls,omitempty"`
	Code      string            `json:"code,omitempty"`
	Language  string            `json:"language,omitempty"`
	Image     []Image           `json:"image,omitempty"`
	Raw       json.RawMessage   `json:"-"`
}

// Image is one generated/search image entry.
type Image struct {
	ImageURL string `json:"image_url"`
	Title    string `json:"title,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// UnmarshalJSON decodes the known fields and retains the full object in Raw.
func (c *Content) UnmarshalJSON(raw []byte) error {
	type alias Content
	var a alias
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	*c = Content(a)
	c.Raw = append(c.Raw[:0], raw...)
	return nil
}

// Detail returns the raw content object as a map (nil when absent).
func (c Content) Detail() map[string]any {
	if len(c.Raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(c.Raw, &m) != nil {
		return nil
	}
	return m
}

// TextValue extracts a textual payload from a content item regardless of its
// type: `content` (string, array of strings/objects, or object), `text`,
// `think`, `title`+`url`, or a JSON fallback. This is how the less common types
// (rag_slices, quote_result, function_result, browser_result, widget, form,
// report, file, video) are folded into ordinary content.
func (c Content) TextValue() string {
	switch c.Type {
	case "text":
		return c.Text
	case "think":
		return c.Think
	case "code":
		if c.Code == "" {
			return ""
		}
		lang := strings.TrimSpace(c.Language)
		if lang == "" {
			lang = "python"
		}
		return "```" + lang + "\n" + c.Code + "\n```"
	case "image", "search_picture":
		var b strings.Builder
		for _, img := range c.Image {
			if img.ImageURL == "" {
				continue
			}
			alt := img.Title
			if alt == "" {
				alt = "image"
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "![%s](%s)", alt, img.ImageURL)
		}
		return b.String()
	}
	m := c.Detail()
	if m == nil {
		return ""
	}
	if v, ok := m["content"]; ok {
		return flattenValue(v)
	}
	if v, ok := m["text"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	if v, ok := m["todo_list"]; ok {
		return flattenTodo(v)
	}
	if v, ok := m["search_card"]; ok {
		return flattenSearchCard(v)
	}
	if v, ok := m["file"]; ok {
		return flattenMedia(v, "file")
	}
	if v, ok := m["video"]; ok {
		return flattenMedia(v, "video")
	}
	if v, ok := m["report"]; ok {
		return flattenValue(v)
	}
	if v, ok := m["widget"]; ok {
		return flattenValue(v)
	}
	if v, ok := m["form"]; ok {
		return flattenValue(v)
	}
	return ""
}

// flattenValue renders arbitrary JSON payloads as readable text. Strings pass
// through; arrays join with newlines; objects try common textual keys first and
// fall back to compact JSON.
func flattenValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s := flattenValue(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "content", "title", "name", "url", "snippet"} {
			if val, ok := t[key]; ok {
				if s := flattenValue(val); s != "" {
					return s
				}
			}
		}
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func flattenTodo(v any) string {
	items, ok := v.([]any)
	if !ok {
		return flattenValue(v)
	}
	var b strings.Builder
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			if s := flattenValue(item); s != "" {
				b.WriteString("- " + s + "\n")
			}
			continue
		}
		mark := "[ ]"
		if done, ok := m["completed"].(bool); ok && done {
			mark = "[x]"
		}
		title, _ := m["title"].(string)
		if title == "" {
			title, _ = m["content"].(string)
		}
		if title != "" {
			fmt.Fprintf(&b, "- %s %s\n", mark, title)
		}
	}
	return strings.TrimSpace(b.String())
}

func flattenSearchCard(v any) string {
	items, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[string]any); ok {
			return flattenValue(m)
		}
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title, _ := m["title"].(string)
		url, _ := m["url"].(string)
		switch {
		case title != "" && url != "":
			fmt.Fprintf(&b, "- [%s](%s)\n", title, url)
		case title != "":
			fmt.Fprintf(&b, "- %s\n", title)
		}
	}
	return strings.TrimSpace(b.String())
}

func flattenMedia(v any, kind string) string {
	items, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[string]any); ok {
			items = []any{m}
		} else {
			return ""
		}
	}
	var b strings.Builder
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		url := firstString(m, "url", "file_url", "video_url", "download_url")
		name := firstString(m, "name", "file_name", "title")
		switch {
		case url != "" && name != "":
			fmt.Fprintf(&b, "[%s: %s](%s)\n", kind, name, url)
		case url != "":
			fmt.Fprintf(&b, "[%s](%s)\n", kind, url)
		case name != "":
			fmt.Fprintf(&b, "[%s: %s]\n", kind, name)
		}
	}
	return strings.TrimSpace(b.String())
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
