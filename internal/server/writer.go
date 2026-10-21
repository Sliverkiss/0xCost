package server

import (
	"sync"
	"time"

	"chatglm2api/internal/upstream"
)

// firstFlushHoldCap bounds the holdback from the start of an attempt. The
// configured grace (chat.first_flush_holdback) is measured from the first
// buffered payload; the cap guarantees that a stream which trickles in is not
// held longer than this.
const firstFlushHoldCap = 1500 * time.Millisecond

// chunkWriter emits OpenAI chat.completion.chunk objects. It tracks whether any
// content has been sent so a mid-stream failure can be distinguished from a
// failure before the client saw anything.
//
// First-flush holdback: while no payload has been committed, payloads are
// buffered for at most the configured grace (from the first buffered payload)
// or firstFlushHoldCap (from the attempt start). A failure inside that window
// can therefore rotate to another identity before the client sees anything;
// the buffered payloads of the failed attempt are dropped by beginAttempt. Once
// the holdback window ends, the buffer is flushed in order and the stream is
// live: failures are terminal (partial answer + error frame), and nothing is
// buffered again.
type chunkWriter struct {
	id      string
	model   string
	created int64
	write   func(any) error
	// begin is called once, immediately before the first byte is written. It
	// lets the handler defer the 200 status line until the upstream accepted
	// the request, so a pre-token failure can still be a real HTTP error.
	begin func()

	mu sync.Mutex
	// begun is true once the 200 status line has been committed.
	begun bool
	// sent is true once a real payload has reached the client. It excludes a
	// role-only chunk (the role is folded into the first delta) and everything
	// still in the holdback buffer, so a failure can still rotate.
	sent    bool
	roleOut bool

	// hold is the configured grace; 0 disables the holdback.
	hold        time.Duration
	attemptFrom time.Time
	holdFrom    time.Time
	pending     []any
	timer       *time.Timer
	timerGen    uint64
}

func newChunkWriter(id, model string, created time.Time, write func(any) error) *chunkWriter {
	return &chunkWriter{id: id, model: model, created: created.Unix(), write: write}
}

// setHoldback configures the first-flush grace. Call it before the first
// payload. hold <= 0 disables buffering.
func (w *chunkWriter) setHoldback(hold time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if hold < 0 {
		hold = 0
	}
	if hold > firstFlushHoldCap {
		hold = firstFlushHoldCap
	}
	w.hold = hold
}

// beginAttempt starts a new upstream attempt: buffered payloads of a failed
// attempt are dropped and the cap clock restarts. It has no effect once the
// stream is live (a live stream never rotates).
func (w *chunkWriter) beginAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.begun {
		return
	}
	w.stopTimerLocked()
	w.pending = nil
	w.holdFrom = time.Time{}
	w.attemptFrom = time.Now()
	// Nothing was committed, so the next attempt must still emit the
	// role-first chunk its client-visible answer starts with.
	w.roleOut = false
}

// started reports whether real output has been committed to the client.
func (w *chunkWriter) started() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sent && w.begun
}

// begunStream reports whether the 200 status line has been committed.
func (w *chunkWriter) begunStream() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.begun
}

func (w *chunkWriter) chunk(delta chunkDelta, finish *string, usage *usageObject) error {
	c := chunkResponse{
		ID:      w.id,
		Object:  "chat.completion.chunk",
		Created: w.created,
		Model:   w.model,
		Choices: []chunkChoice{{Index: 0, Delta: delta, FinishReason: finish, Logprobs: nil}},
		Usage:   usage,
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.emitLocked(c)
}

// emitLocked applies the holdback policy and returns whether the payload was
// committed to the client.
func (w *chunkWriter) emitLocked(payload any) error {
	if w.hold > 0 && !w.begun {
		now := time.Now()
		if w.holdFrom.IsZero() {
			w.holdFrom = now
			if w.attemptFrom.IsZero() {
				w.attemptFrom = now
			}
			w.startTimerLocked(now)
		}
		if now.Sub(w.holdFrom) < w.hold && (w.attemptFrom.IsZero() || now.Sub(w.attemptFrom) < firstFlushHoldCap) {
			w.pending = append(w.pending, payload)
			return nil
		}
	}
	if err := w.flushLocked(); err != nil {
		return err
	}
	return w.writeNowLocked(payload)
}

// abortAttempt discards the holdback buffer of a failed attempt so a rotation
// cannot leak the failed attempt's partial payloads to the client. It is a
// no-op once the stream is live: a committed stream is never rotated.
func (w *chunkWriter) abortAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.begun {
		return
	}
	w.stopTimerLocked()
	w.pending = nil
	w.holdFrom = time.Time{}
	w.roleOut = false
}

// flushLocked writes every buffered payload in order and commits the stream.
// After the flush the holdback is disabled for the rest of the stream.
func (w *chunkWriter) flushLocked() error {
	w.stopTimerLocked()
	pending := w.pending
	w.pending = nil
	w.holdFrom = time.Time{}
	w.hold = 0
	var err error
	for _, p := range pending {
		if werr := w.writeNowLocked(p); werr != nil {
			err = werr
			break
		}
	}
	return err
}

// writeNowLocked commits the status line and writes one payload.
func (w *chunkWriter) writeNowLocked(payload any) error {
	if !w.begun {
		w.begun = true
		if w.begin != nil {
			w.begin()
		}
	}
	err := w.write(payload)
	// A payload that attempted to reach the client marks the stream live even
	// if the write itself failed: a broken connection cannot be rotated into.
	w.sent = true
	return err
}

// startTimerLocked arms the grace timer so a stream that stalls after its first
// payload is flushed on time instead of waiting for the next frame.
func (w *chunkWriter) startTimerLocked(now time.Time) {
	deadline := w.hold
	if !w.attemptFrom.IsZero() {
		if rest := firstFlushHoldCap - now.Sub(w.attemptFrom); rest < deadline {
			deadline = rest
		}
	}
	if deadline < 0 {
		deadline = 0
	}
	w.timerGen++
	gen := w.timerGen
	w.timer = time.AfterFunc(deadline, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.timerGen != gen || w.begun {
			return
		}
		_ = w.flushLocked()
	})
}

func (w *chunkWriter) stopTimerLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.timerGen++
}

// delta emits an upstream translation delta. The role-first chunk is folded
// into the first delta, so a stream that never produces output never commits
// the 200 status line.
func (w *chunkWriter) delta(d upstream.Delta) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := chunkDelta{
		Content:          d.Content,
		ReasoningContent: d.ReasoningContent,
	}
	if !w.roleOut {
		out.Role = "assistant"
		w.roleOut = true
	}
	for _, tc := range d.ToolCalls {
		call := chunkToolCall{Index: tc.Index, ID: tc.ID, Type: tc.Type}
		if tc.Function != nil {
			call.Function = &chunkFunctionD{Name: tc.Function.Name, Arguments: tc.Function.Arguments}
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	c := chunkResponse{
		ID:      w.id,
		Object:  "chat.completion.chunk",
		Created: w.created,
		Model:   w.model,
		Choices: []chunkChoice{{Index: 0, Delta: out, FinishReason: nil, Logprobs: nil}},
	}
	return w.emitLocked(c)
}

// finish emits the terminal choice chunk carrying finish_reason. It forces the
// holdback buffer out first so ordering is preserved.
func (w *chunkWriter) finish(reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.flushLocked(); err != nil {
		return err
	}
	return w.emitLocked(chunkResponse{
		ID:      w.id,
		Object:  "chat.completion.chunk",
		Created: w.created,
		Model:   w.model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: &reason, Logprobs: nil}},
	})
}

// usage emits the spec-exact include_usage frame: dedicated chunk, empty
// choices array, usage object.
func (w *chunkWriter) usage(t upstream.TokenStats) error {
	u := &usageObject{
		PromptTokens:     t.PromptTokens,
		CompletionTokens: t.CompletionTokens,
		TotalTokens:      t.TotalTokens,
	}
	payload := chunkResponse{
		ID:      w.id,
		Object:  "chat.completion.chunk",
		Created: w.created,
		Model:   w.model,
		Choices: []chunkChoice{},
		Usage:   u,
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.flushLocked(); err != nil {
		return err
	}
	return w.emitLocked(payload)
}

// errorFrame is the non-standard terminal error used when the stream has
// already started: an OpenAI error object followed by [DONE] by the caller.
// It shares classifyClientError with the blocking path so both surfaces name
// the same cause.
func (w *chunkWriter) errorFrame(err error) {
	_, code, msg := classifyClientError(err)
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.flushLocked()
	_ = w.emitLocked(errorResponse{Error: errorBody{Message: msg, Type: "api_error", Code: code}})
}

// RawFrame is a pre-serialised SSE payload (the [DONE] terminator).
type RawFrame string

// done writes the [DONE] terminator.
func (w *chunkWriter) done() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.flushLocked()
	_ = w.emitLocked(RawFrame("data: [DONE]\n\n"))
}
