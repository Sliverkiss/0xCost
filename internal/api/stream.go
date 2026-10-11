package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// This file is the client-facing SSE half of chat: the upstream has always been
// SSE (internal/mimo/sse.go) and the OpenAI chunk shapers in internal/adapter
// were left in place, so re-adding streaming is this writer plus the wiring
// (ADR 0034, reversing the client-side half of ADR 0017).
//
// One property is load-bearing and comes from the deleted design: **nothing is
// written until the first upstream delta arrives**, so a failure before that
// point is still pre-response and the request can fail over to another account
// (SDD §7.2 commit boundary, ADR 0019). A keepalive is therefore never sent
// before the stream starts — it would commit the response and disable failover.
// The cost is accepted and stated: a client sees nothing until the upstream's
// own time-to-first-byte, which is where the latency saving comes from anyway.
//
// Every write goes through the mutex: the keepalive ticker runs in its own
// goroutine (chat.go), and net/http.ResponseWriter is not safe for concurrent
// use. The internal helpers are lock-free so a public method can compose them
// without re-entering the lock.

// streamKeepaliveInterval is how often a comment line is written while the
// upstream is still generating. SSE comments are ignored by every OpenAI client
// and keep intermediaries from closing an idle connection.
const streamKeepaliveInterval = 15 * time.Second

// streamWriter emits one OpenAI chat.completion stream.
type streamWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	model   string
	created int64

	// started is the commit boundary: true once the response headers and the
	// role chunk have gone out. Before that the request is still retryable.
	started bool
	// broken records a write failure (the client left); further writes are
	// dropped and the pipeline stops early.
	broken bool
}

func newStreamWriter(w http.ResponseWriter, id, model string, created int64) *streamWriter {
	sw := &streamWriter{w: w, id: id, model: model, created: created}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}
	return sw
}

// committed reports whether the response has been committed. The pipeline reads
// it to decide whether a failure can still be a status code (run) — and the
// keepalive goroutine writes to the response — so the flag lives under the lock.
func (sw *streamWriter) committed() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.started
}

// piece writes one content or reasoning delta, committing the stream if needed.
func (sw *streamWriter) piece(p adapter.Piece) error {
	if p.Text == "" {
		return nil
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.pieceLocked(p)
}

// finish closes a successful stream: the terminal choice chunk, the citations
// extension chunk (search only), the usage chunk, then the [DONE] sentinel.
// The order is the one the surviving shapers document (SDD §5.4): a usage chunk
// with choices:[] can never clobber finish_reason, and clients that ignore a
// choices-less chunk still see a well-formed stream.
func (sw *streamWriter) finish(cits []adapter.Citation, usage adapter.Usage) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if err := sw.startLocked(); err != nil {
		return err
	}
	if err := sw.chunkLocked(adapter.ShapeFinishChunk(sw.id, sw.model, sw.created)); err != nil {
		return err
	}
	if len(cits) > 0 {
		if err := sw.chunkLocked(adapter.ShapeCitationsChunk(sw.id, sw.model, sw.created, cits)); err != nil {
			return err
		}
	}
	if err := sw.chunkLocked(adapter.ShapeUsageChunk(sw.id, sw.model, sw.created, usage)); err != nil {
		return err
	}
	return sw.doneLocked()
}

// keepalive writes an SSE comment. It is a no-op before the stream is
// committed, because committing early would disable account failover.
func (sw *streamWriter) keepalive() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !sw.started || sw.broken {
		return
	}
	if _, err := io.WriteString(sw.w, ": keepalive\n\n"); err != nil {
		sw.broken = true
		return
	}
	sw.flushLocked()
}

// failJSON reports a failure **inside** an already-committed stream: the error
// object MapError produced is re-emitted as an SSE data frame. Our error
// contract still holds (all four keys, a real code, ADR 0032) and the message
// still carries the platform's own wording where there was any (ADR 0033) — it
// simply travels in-band, because the status line is already sent.
func (sw *streamWriter) failJSON(body []byte) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !sw.started || sw.broken {
		return
	}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Error) == 0 {
		env.Error = json.RawMessage(`{"message":"upstream failure","type":"api_error","param":null,"code":"upstream_failure"}`)
	}
	frame, err := json.Marshal(struct {
		Error json.RawMessage `json:"error"`
	}{Error: env.Error})
	if err != nil {
		sw.broken = true
		return
	}
	if _, err := io.WriteString(sw.w, "data: "+string(frame)+"\n\n"); err != nil {
		sw.broken = true
		return
	}
	sw.flushLocked()
	_ = sw.doneLocked()
}

// startLocked commits the response and writes the role chunk. Idempotent.
func (sw *streamWriter) startLocked() error {
	if sw.started {
		return nil
	}
	sw.started = true
	h := sw.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Some reverse proxies buffer a response until it is large enough; this
	// tells them not to, so deltas reach the client as they are generated.
	h.Set("X-Accel-Buffering", "no")
	sw.w.WriteHeader(http.StatusOK)
	return sw.chunkLocked(adapter.ShapeRoleChunk(sw.id, sw.model, sw.created))
}

func (sw *streamWriter) pieceLocked(p adapter.Piece) error {
	if err := sw.startLocked(); err != nil {
		return err
	}
	return sw.chunkLocked(adapter.ShapeTextChunk(sw.id, sw.model, sw.created, p))
}

// chunkLocked writes one data frame and flushes it.
func (sw *streamWriter) chunkLocked(c adapter.Chunk) error {
	if sw.broken {
		return nil
	}
	body := adapter.EncodeChunk(c)
	if body == "" {
		return nil // a chunk that cannot be encoded is not worth killing a live stream for
	}
	if _, err := io.WriteString(sw.w, "data: "+body+"\n\n"); err != nil {
		sw.broken = true
		return err
	}
	sw.flushLocked()
	return nil
}

func (sw *streamWriter) doneLocked() error {
	if sw.broken {
		return nil
	}
	if _, err := io.WriteString(sw.w, "data: [DONE]\n\n"); err != nil {
		sw.broken = true
		return err
	}
	sw.flushLocked()
	return nil
}

func (sw *streamWriter) flushLocked() {
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
}

// errorCapturingWriter records what MapError would have sent, so a failure that
// happens after the SSE response is committed can be re-emitted in-band without
// teaching the error mapper about transports.
type errorCapturingWriter struct {
	header http.Header
	status int
	body   strings.Builder
	wrote  bool
}

func newErrorCapturingWriter() *errorCapturingWriter {
	return &errorCapturingWriter{header: http.Header{}}
}

func (c *errorCapturingWriter) Header() http.Header { return c.header }

func (c *errorCapturingWriter) WriteHeader(status int) {
	if c.wrote {
		return
	}
	c.status = status
	c.wrote = true
}

func (c *errorCapturingWriter) Write(p []byte) (int, error) {
	c.wrote = true
	return c.body.Write(p)
}

// bytes returns the encoded error body MapError produced.
func (c *errorCapturingWriter) bytes() []byte { return []byte(c.body.String()) }
