package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"chatglm2api/internal/config"
	"chatglm2api/internal/logfmt"
	"chatglm2api/internal/pool"
	"chatglm2api/internal/toolfc"
	"chatglm2api/internal/upstream"
)

// turnResult is the outcome of one upstream exchange.
type turnResult struct {
	content   string
	reasoning string
	tools     []upstream.ToolCall
	finish    string
	usage     upstream.TokenStats
	err       error
	// fcOutcome is the prompt-FC parse outcome for request logging (empty when
	// function calling was not active).
	fcOutcome string
}

// handleChat implements POST /v1/chat/completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "cannot read request body")
		return
	}
	var req ChatRequest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body is not valid JSON: "+err.Error())
		return
	}
	if verr := req.Validate(); verr != nil {
		writeValidationError(w, verr)
		return
	}

	spec := s.cfg.Config.ModelByID(req.Model)
	modelID := spec.ID
	if strings.TrimSpace(req.Model) == "" {
		modelID = s.cfg.Config.Chat.DefaultModel
	}

	var promptMsgs []upstream.PromptMessage
	var fc *fcContext
	if len(req.Tools) > 0 {
		var ferr *ValidationError
		var perr error
		promptMsgs, fc, ferr, perr = s.prepareTools(&req)
		if ferr != nil {
			writeValidationError(w, ferr)
			return
		}
		if perr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_tool_definition", perr.Error())
			return
		}
	} else {
		promptMsgs = promptMessages(req.Messages)
	}
	prompt := upstream.BuildPrompt(promptMsgs)
	if prompt == "" {
		// Every message rendered to nothing (empty strings, unsupported
		// content parts, whitespace-only text). Sending that upstream would
		// consume an identity and return a 502; it is a client error.
		writeValidationError(w, &ValidationError{Param: "messages", Message: "the request contains no usable text content"})
		return
	}

	thinking := spec.Thinking || strings.TrimSpace(req.ReasoningEffort) != ""
	chatMode := ""
	if thinking {
		chatMode = "zero"
	}

	tr := &turnRunner{
		srv:          s,
		acquireBy:    started.Add(s.cfg.Config.Pool.AcquireWait.D()),
		modelID:      modelID,
		spec:         spec,
		prompt:       prompt,
		chatMode:     chatMode,
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
		id:           newCompletionID(),
		created:      time.Now(),
		started:      started,
		fc:           fc,
	}
	tr.logInjection()
	if req.Stream {
		tr.runStream(w, r)
		return
	}
	tr.runBlocking(w, r)
}

// turnRunner owns one client request across identity rotations.
type turnRunner struct {
	srv          *Server
	modelID      string
	spec         config.ModelSpec
	prompt       string
	chatMode     string
	includeUsage bool
	id           string
	created      time.Time
	started      time.Time
	// fc carries the prompt-FC state; nil when the request has no tools.
	fc *fcContext
	// busyRetries counts in-place 10061 requeues for this request. It is
	// shared by the blocking and streaming paths and only shapes the backoff.
	busyRetries int
	// acquireBy is the request's pool-acquire deadline. Rotation attempts share
	// one budget instead of each getting a fresh pool.acquire_wait.
	acquireBy time.Time
}

// selectLease leases an identity under the request's single acquire budget.
func (t *turnRunner) selectLease(ctx context.Context) (*pool.Lease, error) {
	if t.acquireBy.IsZero() {
		return t.srv.cfg.Pool.Select(ctx)
	}
	selCtx, cancel := context.WithDeadline(ctx, t.acquireBy)
	defer cancel()
	lease, err := t.srv.cfg.Pool.Select(selCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		// The request's acquire budget elapsed: this is pool exhaustion, not an
		// upstream failure. Report it as such so the client gets 503 +
		// Retry-After instead of a gateway error.
		return nil, pool.ErrNoIdentity
	}
	return lease, err
}

// runBlocking performs the exchange and writes one chat.completion response.
func (t *turnRunner) runBlocking(w http.ResponseWriter, r *http.Request) {
	res, status, retryAfter := t.execute(r.Context())
	if res.err != nil {
		writeUpstreamError(w, status, res.err, retryAfter)
		return
	}
	if t.fc != nil {
		t.fc.apply(res)
	}
	t.logFC(res, finishReasonOrDefault(res.finish, res.tools))
	msg := responseMessage{Role: "assistant"}
	if res.content != "" {
		content := res.content
		msg.Content = &content
	}
	msg.ReasoningContent = res.reasoning
	msg.ToolCalls = toResponseTools(res.tools)
	usage := res.usage.Usage()
	resp := chatCompletion{
		ID: t.id, Object: "chat.completion", Created: t.created.Unix(), Model: t.modelID,
		Choices: []choice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReasonOrDefault(res.finish, res.tools),
			Logprobs:     nil,
		}},
		Usage: &usageObject{
			PromptTokens:     usage["prompt_tokens"].(int),
			CompletionTokens: usage["completion_tokens"].(int),
			TotalTokens:      usage["total_tokens"].(int),
		},
	}
	writeJSON(w, http.StatusOK, resp)
	if t.srv.cfg.Config.LogRequests {
		log.Printf("[chat] %s model=%s stream=false finish=%s in=%d out=%d dur=%s",
			t.id, t.modelID, finishReasonOrDefault(res.finish, res.tools),
			res.usage.PromptTokens, res.usage.CompletionTokens, time.Since(t.started).Round(time.Millisecond))
	}
}

// runStream performs the exchange and streams OpenAI chunks.
func (t *turnRunner) runStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "stream_unsupported", "streaming is not supported by this server")
		return
	}
	// The stream headers are committed lazily by the first payload, after the
	// first-flush holdback. An upstream failure inside the holdback window can
	// therefore still rotate invisibly; an upstream failure before any token
	// is a normal HTTP error status rather than a 200 that carries an error
	// frame; once anything has been written, failures are reported in-stream
	// (the design's partial + error frame).
	writer := newChunkWriter(t.id, t.modelID, t.created, func(chunk any) error {
		if frame, ok := chunk.(RawFrame); ok {
			_, err := io.WriteString(w, string(frame))
			if err == nil {
				flusher.Flush()
			}
			return err
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	writer.begin = func() {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}

	writer.setHoldback(t.srv.cfg.Config.Chat.FirstFlushHoldback.D())
	res := t.streamUpstream(r.Context(), writer)
	if res.err != nil && !writer.begunStream() {
		// The 200 status line was never committed: report a real HTTP error
		// instead of a stream carrying an error frame.
		writeUpstreamError(w, http.StatusServiceUnavailable, res.err, 0)
		return
	}
	if res.err != nil {
		writer.errorFrame(res.err)
		writer.done()
		return
	}
	finish := finishReasonOrDefault(res.finish, res.tools)
	if err := writer.finish(finish); err != nil {
		return
	}
	t.logFC(res, finish)
	if t.includeUsage {
		if err := writer.usage(res.usage); err != nil {
			return
		}
	}
	writer.done()
	if t.srv.cfg.Config.LogRequests {
		log.Printf("[chat] %s model=%s stream=true finish=%s dur=%s",
			t.id, t.modelID, finish, time.Since(t.started).Round(time.Millisecond))
	}
}

// streamUpstream runs rotations and streams deltas to the writer. Every
// attempt that reaches the upstream ends with the cartridge discarded; only a
// 10061 busy error (the upstream explicitly refused the message) keeps the
// lease so the same request can retry it in place.
func (t *turnRunner) streamUpstream(ctx context.Context, w *chunkWriter) *turnResult {
	var lastErr *upstream.APIError
	var lease *pool.Lease
	for attempt := 0; attempt < t.srv.cfg.Config.Pool.MaxRotate; attempt++ {
		if !t.srv.startPacer.Wait(ctx) {
			if lease != nil {
				lease.Discard("cancelled")
			}
			return &turnResult{err: ctx.Err()}
		}
		if lease == nil {
			l, err := t.selectLease(ctx)
			if err != nil {
				if lastErr != nil {
					return &turnResult{err: lastErr}
				}
				return &turnResult{err: err}
			}
			lease = l
		}

		w.beginAttempt()
		res := t.srv.streamTurn(ctx, lease, t.prompt, t.chatMode, t.spec, t.fc, w)
		kept := t.endAttempt(lease, res.err, w)
		if !kept {
			lease = nil
		}
		if res.err == nil {
			return res
		}
		ae := upstream.AsAPIError(res.err)
		if ae == nil {
			return res
		}
		lastErr = ae
		if w.started() {
			// Content already reached the client. Rotating (even for a busy
			// refusal) would start a second upstream message and splice a second
			// answer onto the first, so the design requires a partial answer plus
			// a terminal error.
			if lease != nil {
				lease.Discard(ae.Kind.String())
			}
			return res
		}
		if kept {
			// 10061 busy: the upstream refused the message, so the same lease is
			// retried in place. Waiting out the backoff holds the cartridge, and
			// no other request can take it.
			t.busyRetries++
			if !sleepCtx(ctx, busyBackoff(t.busyRetries)) {
				lease.Discard("cancelled")
				return res
			}
			continue
		}
		if !rotatable(ae) {
			// The request itself is bad; another identity cannot help.
			return res
		}
		if !sleepCtx(ctx, rotationBackoff(attempt)) {
			return res
		}
	}
	if lease != nil {
		// The last attempt was a busy refusal and the budget is exhausted: the
		// cartridge served nothing, but it is no longer needed here and must not
		// stay leased forever.
		lease.Discard("busy budget exhausted")
	}
	if lastErr != nil {
		return &turnResult{err: lastErr}
	}
	return &turnResult{err: pool.ErrNoIdentity}
}

// endAttempt closes a lease according to what the attempt proved:
//
//   - success: the cartridge served its one request -> discard;
//   - 10061 busy: the upstream refused the message -> keep the lease and retry
//     it in place, so the cartridge cannot be taken by another request;
//   - anything else: discard. A client-shaped failure, an auth/budget verdict,
//     a transport error after the request was sent and a client disconnect all
//     leave a cartridge that cannot be proven virgin, and the pool never racks
//     a partially used identity.
//
// It reports whether the caller still owns the lease (busy refusal).
func (t *turnRunner) endAttempt(lease *pool.Lease, err error, w *chunkWriter) bool {
	s := t.srv
	if err == nil {
		lease.Discard("served")
		return false
	}
	ae := upstream.AsAPIError(err)
	if ae != nil && ae.Kind == upstream.ErrBusy {
		// The lease is held: no pool state change, no cooldown, no requeue.
		s.cfg.Pool.NoteBusy()
		return true
	}
	// The attempt failed before the stream was committed: drop anything the
	// holdback buffered so the next attempt starts clean.
	if w != nil {
		w.abortAttempt()
	}
	s.noteTurnFailure(err)
	if ae != nil {
		lease.Discard(ae.Kind.String())
	} else {
		lease.Discard("failed")
	}
	return false
}

// toResponseTools converts upstream tool calls to the OpenAI response shape.
func toResponseTools(tools []upstream.ToolCall) []ToolCall {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(tools))
	for _, tc := range tools {
		var out2 ToolCall
		out2.ID = tc.ID
		out2.Type = tc.Type
		out2.Function.Name = tc.Function.Name
		out2.Function.Arguments = tc.Function.Arguments
		out = append(out, out2)
	}
	return out
}

// execute runs the upstream exchange for a non-streaming request.
func (t *turnRunner) execute(ctx context.Context) (*turnResult, int, time.Duration) {
	var lastErr *upstream.APIError
	status := http.StatusServiceUnavailable
	var retryAfter time.Duration
	var lease *pool.Lease
	for attempt := 0; attempt < t.srv.cfg.Config.Pool.MaxRotate; attempt++ {
		if !t.srv.startPacer.Wait(ctx) {
			if lease != nil {
				lease.Discard("cancelled")
			}
			return &turnResult{err: ctx.Err()}, http.StatusServiceUnavailable, 0
		}
		if lease == nil {
			l, err := t.selectLease(ctx)
			if err != nil {
				if lastErr != nil {
					return &turnResult{err: lastErr}, status, retryAfter
				}
				return &turnResult{err: err}, http.StatusServiceUnavailable, 0
			}
			lease = l
		}

		res := t.srv.collectTurn(ctx, lease, t.prompt, t.chatMode, t.spec)
		kept := t.endAttempt(lease, res.err, nil)
		if !kept {
			lease = nil
		}
		if res.err == nil {
			return res, http.StatusOK, 0
		}
		ae := upstream.AsAPIError(res.err)
		if ae == nil {
			return res, http.StatusServiceUnavailable, 0
		}
		lastErr, status, retryAfter = ae, ae.Status, ae.RetryAfter
		if kept {
			t.busyRetries++
			if !sleepCtx(ctx, busyBackoff(t.busyRetries)) {
				lease.Discard("cancelled")
				return res, status, retryAfter
			}
			continue
		}
		if !rotatable(ae) {
			return res, status, retryAfter
		}
		if !sleepCtx(ctx, rotationBackoff(attempt)) {
			return res, status, retryAfter
		}
	}
	if lease != nil {
		lease.Discard("busy budget exhausted")
	}
	if lastErr == nil {
		return &turnResult{err: pool.ErrNoIdentity}, http.StatusServiceUnavailable, 0
	}
	return &turnResult{err: lastErr}, status, retryAfter
}

// rotatable reports whether another identity can plausibly succeed. Failures
// shaped by the request itself (bad parameters, login-only endpoints, an
// upstream error frame) are returned to the client unchanged; identity,
// transport and upstream-health failures rotate.
func rotatable(ae *upstream.APIError) bool {
	if ae == nil {
		return false
	}
	switch ae.Kind {
	case upstream.ErrBadRequest, upstream.ErrLoginRequired, upstream.ErrUpstream:
		return false
	}
	return true
}

// busyBackoff waits 1s, 2s, 3s… between busy retries on one identity.
func busyBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return time.Duration(n) * time.Second
}

func rotationBackoff(attempt int) time.Duration {
	d := 300 * time.Millisecond * time.Duration(1<<attempt)
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// noteTurnFailure feeds the pool-wide circuit breaker. The identity itself is
// never penalised: it is discarded by endAttempt after every attempt, so
// per-identity cooldowns, refreshes and budget notes are dead concepts.
func (s *Server) noteTurnFailure(err error) {
	ae := upstream.AsAPIError(err)
	if ae == nil {
		return
	}
	switch ae.Kind {
	case upstream.ErrServer, upstream.ErrTransport:
		// A transport error proves the upstream is unhealthy, not the request.
		s.cfg.Pool.NoteFailure()
	default:
		// Busy / budget / auth / ip gate / rate limit / request-shaped failures
		// are not pool-wide outages.
	}
}

// collectTurn reads the whole stream and aggregates it.
func (s *Server) collectTurn(ctx context.Context, lease *pool.Lease, prompt, chatMode string, spec config.ModelSpec) *turnResult {
	id := lease.Identity
	body := upstream.NewChatRequest(s.cfg.Config.Chat.AssistantID, prompt, spec.Upstream, s.cfg.Config.Chat.Networking, chatMode)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// From here on the message may reach the upstream, so the cartridge can no
	// longer be proven virgin: Release becomes illegal and the lease must end
	// in Discard (or be held across a 10061 busy retry).
	lease.MarkServed()
	resp, err := s.cfg.Upstream.OpenChatStream(ctx, id.AccessToken, id.DeviceID, id.UserAgent(), s.xff(), body)
	if err != nil {
		return &turnResult{err: err}
	}
	if !upstream.IsSSE(resp) {
		ae := upstream.ReadJSONBody(resp)
		resp.Body.Close()
		return &turnResult{err: ae}
	}
	stream := upstream.NewStream(resp, s.cfg.Upstream.IdleTimeout)
	defer stream.Close()
	tr := upstream.NewTranslator(spec.ID, prompt)
	var content, reasoning strings.Builder
	var finish string
	conversationID := ""
	for {
		raw, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return &turnResult{err: upstream.ErrTransportResponse(err)}
		}
		var frame upstream.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		if conversationID == "" && frame.ConversationID != "" {
			conversationID = frame.ConversationID
		}
		fr := tr.Consume(&frame)
		if fr.Err != nil {
			return &turnResult{err: fr.Err}
		}
		content.WriteString(fr.Delta.Content)
		reasoning.WriteString(fr.Delta.ReasoningContent)
		if fr.Finish != "" {
			finish = fr.Finish
		}
	}
	if content.Len() == 0 && reasoning.Len() == 0 && len(tr.Tools()) == 0 {
		// No answer arrived. The cartridge is still discarded by the caller:
		// it cannot be proven virgin.
		return &turnResult{err: upstream.ErrTransportResponse(errors.New("upstream returned an empty stream"))}
	}
	// One confirmed answer. Bookkeeping only: the caller discards the
	// cartridge immediately afterwards.
	s.cfg.Pool.RecordServed()
	s.cfg.Pool.NoteSuccess()
	s.cleanupConversation(id, conversationID)
	usage := tr.Usage(prompt, content.String(), reasoning.String())
	return &turnResult{
		content: content.String(), reasoning: reasoning.String(), tools: tr.Tools(),
		finish: finish, usage: usage,
	}
}

// streamTurn streams one identity attempt into the chunk writer.
func (s *Server) streamTurn(ctx context.Context, lease *pool.Lease, prompt, chatMode string, spec config.ModelSpec, fc *fcContext, w *chunkWriter) *turnResult {
	id := lease.Identity
	body := upstream.NewChatRequest(s.cfg.Config.Chat.AssistantID, prompt, spec.Upstream, s.cfg.Config.Chat.Networking, chatMode)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// From here on the message may reach the upstream, so the cartridge can no
	// longer be proven virgin: Release becomes illegal and the lease must end
	// in Discard (or be held across a 10061 busy retry).
	lease.MarkServed()
	resp, err := s.cfg.Upstream.OpenChatStream(ctx, id.AccessToken, id.DeviceID, id.UserAgent(), s.xff(), body)
	if err != nil {
		return &turnResult{err: err}
	}
	if !upstream.IsSSE(resp) {
		ae := upstream.ReadJSONBody(resp)
		resp.Body.Close()
		return &turnResult{err: ae}
	}
	stream := upstream.NewStream(resp, s.cfg.Upstream.IdleTimeout)
	defer stream.Close()
	tr := upstream.NewTranslator(spec.ID, prompt)
	var content, reasoning strings.Builder
	var finish string
	conversationID := ""
	// Prompt FC: model text is buffered through the toolkit sieve so XYML
	// markup never reaches the client and calls are emitted as deltas.
	var assembler *toolfc.Stream
	var assistantGuard *toolfc.LeakGuard
	if fc != nil && fc.active {
		assembler = toolfc.NewStream(fc.tools, fc.stripThink)
		assistantGuard = toolfc.NewLeakGuard()
	}
	var fcCalls []upstream.ToolCall
	for {
		raw, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			ae := upstream.ErrTransportResponse(err)
			return &turnResult{content: content.String(), reasoning: reasoning.String(), err: ae}
		}
		var frame upstream.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		if conversationID == "" && frame.ConversationID != "" {
			conversationID = frame.ConversationID
		}
		fr := tr.Consume(&frame)
		if fr.Err != nil {
			return &turnResult{content: content.String(), reasoning: reasoning.String(), err: fr.Err}
		}
		if assembler != nil {
			if fr.Delta.ReasoningContent != "" {
				if guard := assistantGuard.Feed(fr.Delta.ReasoningContent); guard != "" {
					if err := w.delta(upstream.Delta{ReasoningContent: guard}); err != nil {
						return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
					}
				}
				reasoning.WriteString(fr.Delta.ReasoningContent)
			}
			if len(fr.Delta.ToolCalls) > 0 {
				// Native upstream web-search calls stay on their existing path.
				if err := w.delta(upstream.Delta{ToolCalls: fr.Delta.ToolCalls}); err != nil {
					return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
				}
			}
			safe, calls := assembler.Feed(fr.Delta.Content)
			content.WriteString(fr.Delta.Content)
			if safe != "" {
				if err := w.delta(upstream.Delta{Content: safe}); err != nil {
					return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
				}
			}
			if len(calls) > 0 {
				deltas := streamToolDeltas(calls, len(fcCalls))
				fcCalls = append(fcCalls, emittedToolCalls(deltas)...)
				if err := w.delta(upstream.Delta{ToolCalls: deltas}); err != nil {
					return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
				}
				break
			}
			if fr.Finish != "" {
				finish = fr.Finish
			}
			continue
		}
		if fr.Delta.ReasoningContent != "" || fr.Delta.Content != "" || len(fr.Delta.ToolCalls) > 0 {
			if err := w.delta(fr.Delta); err != nil {
				return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
			}
		}
		content.WriteString(fr.Delta.Content)
		reasoning.WriteString(fr.Delta.ReasoningContent)
		if fr.Finish != "" {
			finish = fr.Finish
		}
	}
	if assembler != nil {
		if trailing := assistantGuard.Flush(); trailing != "" {
			if err := w.delta(upstream.Delta{ReasoningContent: trailing}); err != nil {
				return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
			}
		}
		safe, calls := assembler.Flush()
		if safe != "" {
			if err := w.delta(upstream.Delta{Content: safe}); err != nil {
				return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
			}
		}
		if len(calls) > 0 {
			deltas := streamToolDeltas(calls, len(fcCalls))
			fcCalls = append(fcCalls, emittedToolCalls(deltas)...)
			if err := w.delta(upstream.Delta{ToolCalls: deltas}); err != nil {
				return &turnResult{content: content.String(), reasoning: reasoning.String(), err: err}
			}
		}
		switch {
		case len(fcCalls) > 0:
			fc.outcome = fmt.Sprintf("calls=%d", len(fcCalls))
		case assembler.Dropped():
			fc.outcome = "dropped"
		default:
			fc.outcome = "no_calls"
		}
	}
	if content.Len() == 0 && reasoning.Len() == 0 && len(tr.Tools()) == 0 && len(fcCalls) == 0 {
		// No answer arrived; endAttempt discards the cartridge anyway.
		return &turnResult{err: upstream.ErrTransportResponse(errors.New("upstream returned an empty stream"))}
	}
	s.cfg.Pool.RecordServed()
	s.cfg.Pool.NoteSuccess()
	s.cleanupConversation(id, conversationID)
	usage := tr.Usage(prompt, content.String(), reasoning.String())
	tools := tr.Tools()
	if len(fcCalls) > 0 {
		tools = append(tools, fcCalls...)
		finish = upstream.FinishToolCalls
	}
	return &turnResult{
		content: content.String(), reasoning: reasoning.String(), tools: tools,
		finish: finish, usage: usage, fcOutcome: fcOutcome(fc),
	}
}

// cleanupTimeout bounds the best-effort upstream thread deletion.
const cleanupTimeout = 10 * time.Second

// cleanupConversation deletes the upstream thread when configured. Guest
// threads are server-side state with no value to the gateway (clients replay
// their own history), so removing them keeps identities tidy. The call is
// asynchronous: the answer is complete and the client must not wait on
// maintenance, and a slow delete must not hold a pooled identity.
func (s *Server) cleanupConversation(id *pool.Identity, conversationID string) {
	if !s.cfg.Config.Chat.DeleteConversations || conversationID == "" {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := s.cfg.Upstream.DeleteConversation(ctx, id.AccessToken, id.DeviceID, id.UserAgent(), s.xff(), conversationID, s.cfg.Config.Chat.AssistantID); err != nil {
			log.Printf("WARN: [chat] conversation cleanup %s: %v", logfmt.Label(id.UID, ""), err)
		}
	}()
}

func finishReasonOrDefault(finish string, tools []upstream.ToolCall) string {
	if finish == "" {
		if len(tools) > 0 {
			return "tool_calls"
		}
		return "stop"
	}
	return finish
}

func newCompletionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

func (s *Server) xff() string {
	if s.cfg.Egress == nil {
		return ""
	}
	return s.cfg.Egress.Next()
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeError writes the OpenAI error envelope.
func writeError(w http.ResponseWriter, status int, errType, code, msg string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Message: msg, Type: errType, Code: code}})
}

func writeValidationError(w http.ResponseWriter, verr *ValidationError) {
	writeJSON(w, http.StatusBadRequest, errorResponse{Error: errorBody{
		Message: verr.Message, Type: "invalid_request_error", Param: verr.Param, Code: "invalid_parameter",
	}})
}

// classifyClientError maps any exchange failure to the OpenAI error fields.
// The HTTP status, the error code and the message must agree across the
// blocking envelope, the streaming error frame and the pre-stream 503, so all
// three call this one function.
func classifyClientError(err error) (status int, code, msg string) {
	status = http.StatusBadGateway
	code = "upstream_error"
	msg = err.Error()
	ae := upstream.AsAPIError(err)
	if ae == nil {
		if errors.Is(err, pool.ErrNoIdentity) {
			return http.StatusServiceUnavailable, "no_identity_available",
				"no anonymous identity is currently available; please retry shortly"
		}
		return status, code, msg
	}
	msg = ae.Message
	switch ae.Kind {
	case upstream.ErrBudget:
		code, status = "guest_budget_exhausted", http.StatusTooManyRequests
	case upstream.ErrBusy:
		code, status = "upstream_busy", http.StatusTooManyRequests
	case upstream.ErrIPGate:
		code, status = "guest_resource_exhausted", http.StatusTooManyRequests
	case upstream.ErrRateLimited:
		code, status = "rate_limit_exceeded", http.StatusTooManyRequests
	case upstream.ErrTransport:
		code, status = "upstream_unavailable", http.StatusBadGateway
	case upstream.ErrServer:
		code, status = "upstream_error", http.StatusBadGateway
	case upstream.ErrBadRequest:
		code, status = "invalid_request", http.StatusBadRequest
	case upstream.ErrLoginRequired:
		code, status = "login_required", http.StatusBadGateway
	case upstream.ErrAuth:
		code, status = "auth_rejected", http.StatusBadGateway
	case upstream.ErrUpstream:
		code, status = "upstream_error_frame", http.StatusBadGateway
	}
	if ae.Status > 0 {
		status = ae.Status
	}
	return status, code, msg
}

// writeUpstreamError maps a classified failure to the OpenAI envelope.
func writeUpstreamError(w http.ResponseWriter, status int, err error, retryAfter time.Duration) {
	status, code, msg := classifyClientError(err)
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds()+0.999)))
	} else if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "5")
	}
	writeError(w, status, "api_error", code, msg)
}
