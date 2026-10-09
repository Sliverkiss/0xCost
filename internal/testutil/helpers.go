package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

// ReadSSE drains a streaming response into its data payloads and comment
// frames (keepalive assertions, TDD API-019).
func ReadSSE(t *testing.T, resp *http.Response) (events []string, comments []string) {
	t.Helper()
	defer resp.Body.Close()
	var buf strings.Builder
	tmp := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			break
		}
	}
	for _, frame := range strings.Split(buf.String(), "\n\n") {
		frame = strings.TrimRight(frame, "\r\n")
		if frame == "" {
			continue
		}
		if strings.HasPrefix(frame, ":") {
			comments = append(comments, frame)
			continue
		}
		for _, line := range strings.Split(frame, "\n") {
			if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:"); ok {
				events = append(events, strings.TrimPrefix(data, " "))
			}
		}
	}
	return events, comments
}

// ParseChunk decodes one SSE data payload as a chat.completion.chunk.
func ParseChunk(t *testing.T, payload string) adapter.Chunk {
	t.Helper()
	var c adapter.Chunk
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		t.Fatalf("decode chunk %q: %v", payload, err)
	}
	return c
}

// ParseCompletion decodes a non-streaming completion body.
func ParseCompletion(t *testing.T, body []byte) adapter.Completion {
	t.Helper()
	var c adapter.Completion
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	return c
}

// ParseError decodes an OpenAI error body.
func ParseError(t *testing.T, body []byte) (message, typ, code string) {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return e.Error.Message, e.Error.Type, e.Error.Code
}

// ChatBody builds a minimal OpenAI chat request JSON from (role, content)
// pairs.
func ChatBody(model string, stream bool, msgs ...[2]string) string {
	var b strings.Builder
	b.WriteString(`{"model":`)
	b.WriteString(mustJSON(model))
	b.WriteString(`,"stream":`)
	if stream {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`,"messages":[`)
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"role":%s,"content":%s}`, mustJSON(m[0]), mustJSON(m[1]))
	}
	b.WriteString(`]}`)
	return b.String()
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
