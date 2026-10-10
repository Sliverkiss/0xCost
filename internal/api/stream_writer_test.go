package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

// STREAM-019: the commit boundary is the first delta, not the response start.
// A keepalive before it would commit the response and take account failover off
// the table, so keepalive() must be a no-op until a piece has been written
// (ADR 0034, SDD §7.2).
func TestStreamWriterKeepaliveDoesNotCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, "chatcmpl-test", "mimo-v2.6-pro", 1700000000)

	sw.keepalive()
	if rec.Body.Len() != 0 {
		t.Errorf("keepalive wrote %q before the first delta, want nothing", rec.Body.String())
	}
	if sw.committed() {
		t.Fatal("keepalive committed the response, want it still retryable")
	}

	if err := sw.piece(adapter.Piece{Type: "content", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !sw.committed() {
		t.Fatal("the first delta did not commit the response")
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {") {
		t.Errorf("stream does not start with a data frame: %q", body)
	}
	if strings.Contains(body, ": keepalive") {
		t.Errorf("a keepalive was sent before the stream started: %q", body)
	}
	if !strings.Contains(body, `"role":"assistant"`) || !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("role/content frames missing: %q", body)
	}

	// After the commit a keepalive is sent — that is its whole purpose: an idle
	// connection must not look dead to an intermediary.
	sw.keepalive()
	if !strings.Contains(rec.Body.String(), ": keepalive") {
		t.Errorf("no keepalive after the commit: %q", rec.Body.String())
	}
}

// STREAM-020: the tail order is finish → citations → usage → [DONE], and the
// citations chunk is only emitted when there is something to carry.
func TestStreamWriterTailOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, "chatcmpl-test", "mimo-v2.6-pro", 1700000000)
	if err := sw.piece(adapter.Piece{Type: "content", Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	usage := adapter.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	if err := sw.finish([]adapter.Citation{{URL: "https://example.cn/a"}}, usage); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	iFinish := strings.Index(body, `"finish_reason":"stop"`)
	iCites := strings.Index(body, `"citations":[`)
	iUsage := strings.Index(body, `"usage":{`)
	iDone := strings.Index(body, "data: [DONE]")
	if iFinish < 0 || iUsage < 0 || iDone < 0 {
		t.Fatalf("tail frames missing: %q", body)
	}
	if !(iFinish < iCites && iCites < iUsage && iUsage < iDone) {
		t.Errorf("tail order wrong (finish=%d citations=%d usage=%d done=%d): %q", iFinish, iCites, iUsage, iDone, body)
	}
	// A usage chunk carries no choices, so it can never overwrite the
	// finish_reason a client already read.
	var usageFrame string
	for _, blk := range strings.Split(body, "\n\n") {
		if strings.Contains(blk, `"usage":{`) {
			usageFrame = blk
		}
	}
	if usageFrame == "" || !strings.Contains(usageFrame, `"choices":[]`) {
		t.Errorf("usage chunk is not choices-less: %q", usageFrame)
	}
}

// STREAM-021: with no citations the extension chunk is skipped entirely rather
// than sent empty.
func TestStreamWriterNoCitationsNoChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, "chatcmpl-test", "mimo-v2.6-pro", 1700000000)
	if err := sw.finish(nil, adapter.Usage{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "citations") {
		t.Errorf("empty citation chunk was emitted: %q", rec.Body.String())
	}
}
