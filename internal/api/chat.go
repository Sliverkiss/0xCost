package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// handleChat is POST /v1/chat/completions (SDD §5.1/§5.2).
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChatRequest(r)
	if err != nil {
		s.MapError(w, err)
		return
	}
	msgs, err := req.toAdapterMsgs()
	if err != nil {
		s.MapError(w, err)
		return
	}
	am := msgs

	// Streaming is served again (ADR 0034, reversing the client-side half of ADR
	// 0017): `stream: true` selects SSE delivery in p.run. The choice is the
	// client's own field — never a deployment knob — and both upstream channels
	// answer SSE either way (internal/mimo/sse.go), so the two transports carry
	// the same answer from the same pipeline.

	// The two OpenAI-standard knobs are validated before any pool interaction:
	// a typo must not cost an upstream turn, and it must be named (OSTD-003/005).
	if err := req.reasoningEffortErr(); err != nil {
		s.MapError(w, err)
		return
	}
	if err := req.webSearchOptionsErr(); err != nil {
		s.MapError(w, err)
		return
	}
	// Image parts (IMG-*): the level and the count are checked before any account
	// work, because an image is the most expensive thing a request can carry
	// (three upstream calls each).
	if err := req.imageDetailErr(); err != nil {
		s.MapError(w, err)
		return
	}
	if err := req.mediaErr(s.maxImages); err != nil {
		s.MapError(w, err)
		return
	}

	// Search switch (M19, TDD WS-001/002/003). OpenAI's web_search_options is
	// the standard spelling and means "search this turn"; web_search_status
	// keeps the finer disabled|auto|enabled control and wins when both are
	// given, and the ":online" model suffix stays as the shorthand.
	search, err := req.searchStatus()
	if err != nil {
		s.MapError(w, err)
		return
	}
	if search == "" && req.WebSearchOptions != nil {
		search = "enabled"
	}
	requested := req.Model
	if base, ok := strings.CutSuffix(requested, onlineSuffix); ok {
		requested = base
		if search == "" {
			search = "enabled"
		}
	}
	if search == "" {
		search = "disabled"
	}

	// Model resolution (SDD §5.2, TDD API-013). A superseded id is resolved onto
	// the id the upstream serves first: it is not a model the platform will run
	// (the audio surface refuses it, chat answers it with the busy frame), and the
	// client keeps seeing the id it asked for in the response.
	requested = s.resolveModel(requested)
	model, fastchat, err := s.resolveChatModel(requested)
	if err != nil {
		s.MapError(w, err)
		return
	}

	// Client-side char caps BEFORE any pool interaction (SDD §2.8, TDD
	// API-016: zero upstream requests recorded).
	cap := s.queryCharCap(model)
	for _, m := range msgs {
		if n := len([]rune(m.Content)); n > cap {
			// Log it: this rejection used to be silent on the server side, so an
			// operator could not even tell which client sent what (ADR 0015).
			s.logger.Printf("chat: rejected request from %s: %s message of %d chars over the %d-char cap (model %s, %d messages)",
				r.RemoteAddr, m.Role, n, cap, model, len(msgs))
			s.MapError(w, &queryTooLongError{Limit: cap, Actual: n, Model: model})
			return
		}
	}
	// The cap the platform enforces is on the query text it receives, and the
	// turn planner splices every system message into the first user query — so
	// a client's system prompt — or the protocol prompt a tool layer injects in
	// front of us — spends the same budget as the user's own message. Checked
	// here so the failure is ours (fast, free, with numbers) instead of the
	// platform's, which costs an account call and returns prose (ADR 0015).
	if composed, overhead := adapter.ComposedFirstQueryLen(am); composed > cap {
		s.logger.Printf("chat: rejected request from %s: composed first query of %d chars (incl. %d chars of system/injected preamble) over the %d-char cap (model %s, %d messages)",
			r.RemoteAddr, composed, overhead, cap, model, len(msgs))
		s.MapError(w, &queryTooLongError{Limit: cap, Actual: composed, Model: model, Overhead: overhead})
		return
	}

	// Dropped-field warning header (SDD §5.2, TDD API-008/009, IMG-012).
	if warn := req.warningsHeader(); warn != "" {
		w.Header().Set("X-Mimo-Warnings", warn)
	}

	p := &chatPipeline{
		srv:       s,
		msgs:      am,
		model:     model,
		fastchat:  fastchat,
		thinking:  req.thinkingEnabled(s.thinkingDefaultFor(model)),
		search:    search,
		citations: req.citationsWanted(),
		stream:    req.Stream,
		temp:      req.Temperature,
		topP:      req.TopP,
		requested: req.Model,
	}
	p.run(w, r)
}

// chatPipeline carries one client request through pool → upstream → response
// mapping (SDD §3 data flow). `stream` is the client's own `stream` field and
// `sw` is the SSE writer that carries the same answer when it is set (ADR 0034).
// The id/created/model echoed to the client are decided once per request, so a
// retry before the first byte cannot change what the client is told.
type chatPipeline struct {
	srv      *Server
	msgs     []adapter.Msg
	model    string
	fastchat bool
	stream   bool
	sw       *streamWriter
	id       string
	created  int64
	// modelEcho is the model name returned to the client: the id the client
	// named when it named one, else the resolved one.
	modelEcho string
	thinking  bool
	temp      *float64
	topP      *float64
	requested string
	// search is the upstream webSearchStatus (disabled|auto|enabled) and
	// citations says whether citations are returned (M19).
	search    string
	citations bool
}

// attemptPlan is the resolved plan for one upstream attempt: the queries to
// replay, in order, against one fresh upstream conversation.
type attemptPlan struct {
	queries        []string
	media          [][]adapter.MediaRef // media[i] belongs to queries[i] (IMG-006)
	conversationID string
	prevDialogID   string
}

// run executes one request against a leased account with §7.1 classification and
// §7.2 retry policy. A turn is always one-shot (ADR 0017): no session identity is
// resolved, nothing is read from or written to a conversation store, and the
// replay below is always a full replay into a fresh upstream conversation.
func (p *chatPipeline) run(w http.ResponseWriter, r *http.Request) {
	s := p.srv
	// One identity per request: a retried attempt must not change the id the
	// client could already have seen (it never sees two — nothing is written
	// before the first upstream delta).
	p.id, p.created = adapter.NewChatID(), adapter.NowUnix()
	p.modelEcho = p.requested
	if p.modelEcho == "" {
		p.modelEcho = p.model
	}
	if p.stream {
		p.sw = newStreamWriter(w, p.id, p.modelEcho, p.created)
		// Keepalives run only while this handler is alive, and the handler waits
		// for the ticker to stop before returning, so no write can land on a
		// ResponseWriter that is already finished.
		interval := s.keepaliveInterval
		if interval <= 0 {
			interval = streamKeepaliveInterval
		}
		stop, halted := make(chan struct{}), make(chan struct{})
		defer func() { close(stop); <-halted }()
		go func() {
			defer close(halted)
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					p.sw.keepalive()
				}
			}
		}()
	}
	// clientCtx ends when the client hangs up; ctx additionally carries our own
	// request budget (ADR 0019). The two outcomes are different — a client that
	// left gets nothing, a budget that expired gets a 504 with numbers — so both
	// are kept.
	clientCtx := r.Context()
	ctx := clientCtx
	if s.requestTimeout > 0 {
		var cancelBudget context.CancelFunc
		ctx, cancelBudget = context.WithTimeout(clientCtx, s.requestTimeout)
		defer cancelBudget()
	}

	var excluded []string
	var lastErr error
	attempts := 0

	for attempts < 2 { // original + 1 retry (SDD §7.2)
		var lease *pool.Lease
		var plan *attemptPlan

		l, err := s.pool.Acquire(ctx, excluded...)
		if err != nil {
			lastErr = err
			break
		}
		lease = l
		// Backstop for the slot: every explicit path below releases, and this
		// catches a future early return (or a panic) that forgets to. Release is
		// idempotent, so it costs nothing on the normal path (ADR 0019).
		defer lease.Release()
		fp, ferr := adapter.PlanFullReplay(p.msgs, s.replayCap)
		if ferr != nil {
			lease.Release()
			p.fail(w, ferr)
			return
		}
		plan = &attemptPlan{
			queries:        fp.Queries,
			media:          fp.Media,
			conversationID: adapter.RandomHex32(), // fresh every attempt (SDD §4.2)
		}

		s.debugf("chat: full replay account=%s conversation=%s (fresh) queries=%d replayCap=%d",
			lease.ID, plan.conversationID, len(plan.queries), s.replayCap)

		// One terminal outcome per attempt (ADR 0017): the JSON body is written
		// exactly once, at the end, so every error here is pre-response and the
		// request stays retryable (SDD §7.2 commit boundary).
		err = p.execute(ctx, w, lease, plan)
		if err == nil {
			return // success (execute released via finishTurn)
		}
		lastErr = err

		// A committed stream cannot be replaced by a status code: once the first
		// delta is out, every retry below is off the table and the failure
		// travels in-band instead (ADR 0034). Account-health reporting still
		// happens — a dead account is a fact about the account, not about this
		// request.
		committed := p.sw != nil && p.sw.committed()

		// Client disconnect pre-first-byte (SDD §7.3, TDD STICKY-010):
		// neither success nor failure — release and drop. The client is
		// gone; writing an error frame would only hit a dead pipe. Tested on
		// clientCtx: our own budget expiring is a different outcome and must be
		// answered (ADR 0019).
		if clientCtx.Err() != nil {
			lease.Release()
			return
		}

		switch {
		case isAuthErr(err):
			// Defensive: an HTTP-level auth failure arrives before the body, so
			// a committed stream should not see one. The guard keeps a future
			// in-band variant from silently restarting a turn mid-answer.
			if committed {
				p.fail(w, err)
				return
			}
			// 1 transparent refresh-retry, same account (SDD §7.1, TDD
			// RENEW-005).
			if rerr := s.refreshSession(ctx, lease.ID); rerr == nil {
				err2 := p.execute(ctx, w, lease, plan)
				if err2 == nil {
					return
				}
				lastErr = err2
				if isAuthErr(lastErr) {
					s.pool.ReportDisabled(lease.ID, "session dead after refresh", false)
				}
			} else {
				// Refresh failed: dead passToken / captcha demand disables
				// the account; the client sees the original 401 (SDD §7.1,
				// TDD RENEW-006).
				var dead *mimo.ErrPassTokenDead
				var act *mimo.ErrAccountAction
				if errors.As(rerr, &dead) {
					s.pool.ReportDisabled(lease.ID, rerr.Error(), true)
				} else if errors.As(rerr, &act) {
					s.pool.ReportDisabled(lease.ID, rerr.Error(), false)
				}
				lastErr = err
			}
			lease.Release()
			p.fail(w, lastErr)
			return

		case isRiskControl(err):
			// 451/461: disabled, no retry — fail closed — and recorded for
			// classification (SDD §4.5.2). The disable is NOT permanent: the form
			// of the signal is unverified, so permanence would be a claim we
			// cannot support yet (REQ-10).
			s.pool.ReportRiskControl(lease.ID, err.Error(), pool.RiskEvent{
				Model:      p.model,
				Channel:    channelName(p.fastchat),
				Status:     upstreamStatus(err),
				BodyDigest: bodyDigest(err),
			})
			lease.Release()
			p.fail(w, err)
			return

		case isAccountDead(err):
			// 70016/captcha/OTP demand/dead passToken: disabled, no retry
			// (SDD §7.1).
			s.pool.ReportDisabled(lease.ID, err.Error(), isPermanentDead(err))
			lease.Release()
			p.fail(w, err)
			return

		case isUpstream429(err):
			// No retry for this request; the upstream's own 429 passes through
			// verbatim and the account stays available (owner ruling 2026-10:
			// no cooldown, no backoff — SDD §7.1, TDD ERRMAP-006).
			lease.Release()
			p.fail(w, err)
			return

		case errors.Is(err, mimo.ErrUpstreamModelRejected):
			// Deterministic: the channel refuses this model id, so a retry
			// can only fail again. Surface it as a client error instead of
			// spending another upstream call; the log line matters because
			// the gateway routes channels itself, so this means the catalog
			// disagrees with upstream (docs/web-surface-reverse.md §11).
			lease.Release()
			s.logger.Printf("chat: upstream rejected model %q on the %s channel: %v",
				p.model, channelName(p.fastchat), err)
			p.fail(w, err)
			return

		case errors.Is(err, mimo.ErrUpstreamBusy):
			if committed {
				p.fail(w, err)
				return
			}
			// Transient throttle ("服务器繁忙，请稍后再试"): live-observed to
			// clear within seconds, so retry once after a short pause
			// rather than failing a request that would succeed a moment
			// later. The account stays healthy — this is platform load,
			// not a dead account.
			lease.Release()
			if attempts > 0 {
				p.fail(w, err)
				return
			}
			// A retired model id is answered with this same busy frame (live
			// 2026-10-09: a superseded id returned it 3/3 while the id it redirects
			// to answered 3/3), so treat the refusal as information: rebuild the
			// superseded-id map and re-resolve before the retry. If the id was
			// retired between two refreshes, this is where the deployment notices.
			if m := s.relearnModel(ctx, p.requested); m != p.model {
				// Re-validate against the refreshed snapshot: the successor
				// must still be a served id, and the relearn may also have
				// warmed the id set a cold start never had.
				if nm, fc, nerr := s.resolveChatModel(m); nerr == nil {
					p.model, p.fastchat = nm, fc
				}
			}
			s.debugf("chat: upstream busy, retrying in %s (account=%s, model=%s)", busyRetryBackoff, lease.ID, p.model)
			attempts++
			select {
			case <-time.After(busyRetryBackoff):
			case <-ctx.Done():
				p.fail(w, s.budgetError(clientCtx, ctx, "waiting to retry a busy upstream"))
				return
			}
			continue

		case isGatewayDeadline(err, clientCtx):
			// Our own budget or the per-turn lease TTL fired (ADR 0019). Nothing
			// is wrong with the account: no cooldown, no retry, and the client
			// gets a 504 naming the knob instead of a silent hang.
			s.logger.Printf("chat: our own bound fired while account %s was serving (silence window %s, held %s): %v",
				lease.ID, lease.SilentFor(time.Now()).Round(time.Second), time.Since(lease.IssuedAt()).Round(time.Second), err)
			lease.Release()
			p.fail(w, s.budgetError(clientCtx, ctx, "waiting for the upstream"))
			return

		case isContentErr(err):
			// Content-level: fail the request, no pool action (SDD §7.1).
			lease.Release()
			p.fail(w, err)
			return

		case p.isEmptyResponse(err):
			if committed {
				p.fail(w, err)
				return
			}
			// 1 retry, fresh ids, full replay (SDD §7.1, TDD EMPTY-001).
			lease.Release()
			attempts++
			continue

		default:
			// PASSFAIL (owner ruling 2026-10): an upstream failure — 5xx,
			// transport, stall — is passed through to the client as answered,
			// with no account penalty and no retry. A committed stream already
			// carries its answer in-band; a break after the first delta cannot
			// be retried either way.
			lease.Release()
			p.fail(w, err)
			return
		}
	}

	// Retries exhausted or pool-level failure — pre-first-byte, so a plain
	// HTTP error JSON is correct (TDD API-020).
	p.fail(w, lastErr)
}

// execute runs the turn plan on the leased account and delivers the response.
// Nothing reaches the client until the whole answer is collected, so every
// failure here is pre-response and the request stays retryable (SDD §7.2 commit
// boundary, ADR 0017). execute never releases the lease — success releases via
// finishTurn, error paths leave it to the caller.
func (p *chatPipeline) execute(ctx context.Context, w http.ResponseWriter, lease *pool.Lease, plan *attemptPlan) error {
	s := p.srv
	cookie, ph, err := s.creds(ctx, lease.ID)
	if err != nil {
		return err
	}

	id, created, modelEcho := p.id, p.created, p.modelEcho

	var usageSum adapter.Usage
	var lastDialogID string

	for qi, query := range plan.queries {
		prev := plan.prevDialogID
		if lastDialogID != "" {
			prev = lastDialogID
		}
		attemptStart := time.Now()
		// Attachments for this turn (IMG-006): the images the client sent with
		// this message are resolved and uploaded on the leased account, per
		// attempt — a parse result belongs to the session that created it, so
		// nothing is cached across attempts.
		var medias []any
		if qi < len(plan.media) && len(plan.media[qi]) > 0 {
			uploadStart := time.Now()
			items, uerr := s.uploadTurnAttachments(ctx, cookie, ph, p.model, plan.media[qi])
			if uerr != nil {
				return uerr
			}
			medias = items
			s.debugf("chat: uploaded %d attachment(s) for turn %d/%d in %s",
				len(items), qi+1, len(plan.queries), time.Since(uploadStart).Round(time.Millisecond))
		}
		chatReq := mimo.ChatRequest{
			Query:            query,
			Model:            p.model,
			Fastchat:         p.fastchat,
			ConversationID:   plan.conversationID,
			PreviousDialogID: prev,
			EnableThinking:   p.thinking,
			WebSearchStatus:  p.search,
			Temperature:      p.temp,
			TopP:             p.topP,
			MultiMedias:      medias,
			// Debug trace: which upstream events this turn actually
			// produced (M19 — the answer to "why are citations empty?").
			OnEvent: func(name string, dataLen int, prefix string) {
				s.debugf("chat: upstream event %s (%d bytes) %s", name, dataLen, prefix)
			},
			OnFirstByte: func() {
				s.pool.RecordFirstByte(lease.ID, time.Since(attemptStart))
				// first-byte policy (ADR 0019): give the slot back the moment the
				// upstream starts answering, because the generation itself does not
				// need to occupy capacity. The account is still pinned to this
				// request by lease.ID, and Release is idempotent so the later
				// finishTurn (and the deferred backstop) are no-ops.
				if s.slotHold == slotHoldFirstByte && !lease.Released() {
					s.debugf("chat: slot released at first byte (account=%s, slot policy %s)", lease.ID, s.slotHold)
					lease.Release()
				}
			},
			// The dialogId of this turn, recorded so the log line can name the
			// upstream conversation a failure belonged to.
			OnDialogID: func(dialogID string) {
				lastDialogID = dialogID
			},
		}
		isFinal := qi == len(plan.queries)-1
		// Only the last turn of a replay is the answer: earlier turns exist to
		// rebuild the upstream conversation, so their pieces are collected and
		// dropped (one delivery point, in either transport — SDD §5.4).
		if isFinal && p.sw != nil {
			chatReq.OnPiece = p.onPiece
		}

		// Per-turn bound (ADR 0036, amending ADR 0019): the lease TTL is a
		// **silence** window, and every upstream byte moves it (the reader calls
		// OnActivity → lease.Touch). A turn is therefore cut for going quiet, not
		// for taking long: a search that keeps fetching pages runs until it
		// answers, while a wedged turn dies on time and its slot is reclaimed.
		// The cancel stays armed on the lease so a reaped slot still takes its
		// in-flight upstream call down with it; the operator's wall clock
		// (MIMO2API_REQUEST_TIMEOUT_MS) rides on ctx, not here.
		turnCtx, cancelTurn := context.WithCancel(ctx)
		lease.ArmExpiry(cancelTurn)
		chatReq.IdleTimeout = s.leaseTTL
		chatReq.OnActivity = lease.Touch
		res, err := s.clientFor(lease.Egress).Chat(turnCtx, cookie, ph, chatReq)
		if cancelTurn != nil {
			cancelTurn()
			lease.ArmExpiry(nil)
		}
		if err != nil {
			// Client disconnect (SDD §7.3, TDD API-024): abort upstream (already
			// done via ctx); neither success nor failure, and nothing was sent.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		usageSum = adapter.AddUsage(usageSum, adapter.MapUsage(res.Usage))
		if res.DialogID != "" {
			lastDialogID = res.DialogID
		}
		if isFinal {
			// The one and only delivery point (SDD §5.4, ADR 0017). Every earlier
			// turn of a replay is collected and only its tokens are kept in the
			// upstream conversation.
			if res.Content == "" && res.Reasoning == "" {
				return mimo.ErrEmptyResponse
			}
			cits := res.Citations
			if !p.citations {
				cits = nil // include_citations:false → search still ran
			}
			for _, warn := range res.CitationWarnings {
				s.debugf("chat: skipping a web_search payload: %s", warn)
			}
			if len(cits) > 0 {
				s.debugf("chat: turn returned %d web-search citations", len(cits))
			}
			// Thinking off means the client asked for an answer, not the model's
			// working: the streaming path used to skip reasoning pieces at this
			// point, so the non-streaming path must strip it too (TDD API-010
			// companion).
			reasoning := res.Reasoning
			if !p.thinking {
				reasoning = ""
			}
			if p.sw != nil {
				// Streaming: the answer already reached the client delta by
				// delta, so this is only the tail. The slot goes back before the
				// tail frames for the same reason as the JSON path — the slot is
				// upstream capacity, not patience with a slow client (ADR 0019).
				usage := usageSum
				p.finishTurn(lease, lastDialogID)
				return p.sw.finish(cits, usage)
			}
			comp := adapter.ShapeCompletion(id, modelEcho, created, res.Content, reasoning, usageSum, cits...)
			// Encode before releasing so a marshal failure is still a clean
			// pre-response error (one delivery point, ADR 0017) …
			var body bytes.Buffer
			if err := encodeJSON(&body, comp); err != nil {
				return err
			}
			// … and hand the slot back BEFORE writing to the client: the slot is
			// capacity for upstream work, not for a slow client (ADR 0019).
			p.finishTurn(lease, lastDialogID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, werr := w.Write(body.Bytes()); werr != nil {
				return werr
			}
			return nil
		}
	}

	// Unreachable: the final query always delivers or returns above. Kept as a
	// defensive empty-response so a future change to the loop cannot silently
	// answer 200 with nothing.
	return mimo.ErrEmptyResponse
}

// isGatewayDeadline reports whether this failure is our own budget dying rather
// than the upstream failing or the client leaving (ADR 0019).
//
// The distinction is load-bearing: a client that hung up must not be answered, a
// platform-side failure must cool the account, and our own deadline must do
// neither — the account did nothing wrong and a retry would only spend another
// upstream call on a request we already gave up on.
// Every deadline in this stack is ours: the request budget, the per-turn lease
// TTL, and the upstream client timeout (which is derived from the TTL). So a
// context deadline at this point means our own clock expired — the *request's*
// ctx need not be the one that expired (the TTL fires while the budget is still
// alive), which is exactly why this does not test ctx.Err().
func isGatewayDeadline(err error, clientCtx context.Context) bool {
	if clientCtx.Err() != nil {
		return false // the client left; that is not a deadline
	}
	// context.DeadlineExceeded is the operator's wall clock (when set);
	// ErrSilentUpstream is the turn's silence window expiring **before any
	// byte** arrived. Both are our own clock, not the account's fault, so
	// neither may cool an account down (ADR 0019/0036). ErrIdleStall is
	// deliberately NOT here: silence after the upstream started answering is a
	// stall, and it keeps its transport classification (POOLATTR-012).
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, mimo.ErrSilentUpstream)
}

// budgetError renders the deadline that fired as a client-facing error, naming
// the knob the operator can change (ADR 0019).
func (s *Server) budgetError(clientCtx, ctx context.Context, where string) error {
	if clientCtx.Err() != nil {
		return clientCtx.Err()
	}
	// The per-turn lease TTL can fire while the request budget is still alive;
	// the shorter of the two is what actually bit.
	budget := s.requestTimeout
	lease := false
	if s.leaseTTL > 0 && (budget <= 0 || s.leaseTTL < budget) {
		budget, lease = s.leaseTTL, true
	}
	return &requestTimeoutError{Budget: budget, Where: where, Lease: lease}
}

// onPiece forwards one upstream delta to the client's stream (ADR 0034).
// Thinking off suppresses reasoning chunks for the same reason the non-streaming
// path drops the field: the client asked for an answer, not the model's working.
func (p *chatPipeline) onPiece(piece adapter.Piece) error {
	if piece.Type == "reasoning" && !p.thinking {
		return nil
	}
	return p.sw.piece(piece)
}

// fail reports a failure in whatever shape the response already has: a JSON
// error body before the stream is committed, an in-band SSE error frame after —
// the status line is gone by then, so a status code cannot be revised, and the
// error contract still holds inside the frame (all four keys, ADR 0032/0033).
func (p *chatPipeline) fail(w http.ResponseWriter, err error) {
	if p.sw == nil || !p.sw.committed() {
		p.srv.MapError(w, err)
		return
	}
	rec := newErrorCapturingWriter()
	p.srv.MapError(rec, err)
	p.sw.failJSON(rec.bytes())
}

// finishTurn records success and releases the lease. There is no conversation
// store to advance: every request is one-shot (ADR 0017, superseding ADR 0009).
func (p *chatPipeline) finishTurn(lease *pool.Lease, lastDialogID string) {
	p.srv.pool.ReportSuccess(lease.ID)
	p.srv.debugf("chat: turn ok account=%s dialog=%s", lease.ID, lastDialogID)
	lease.Release()
}

// creds resolves the upstream cookie + ph for an account, performing
// on-demand login when no cached session exists (SDD §4.7) and persisting
// the result.
//
// The whole resolution — renewal fallback included — runs under the shared
// per-account loginGate (BUGS-FOUND #2, recovery.go): a burst of requests
// that all find the session missing runs ONE login chain, not one each, so
// one new-device OTP challenge is started, not one per concurrent request.
// creds holds a pool lease while it waits (the callers acquired one), which
// the gate design accounts for: losers wait on the flight bounded by their
// own request ctx, never by the winner's progress.
func (s *Server) creds(ctx context.Context, accountID string) (cookie, ph string, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		// Fast path first, outside the gate: a cached session needs no login
		// and must not queue behind one. The winner's flight ends only after
		// it persisted (or failed to persist), so a loser re-entering the
		// loop sees the store's final state — no TOCTOU on the session.
		acct, ok := s.store.Get(accountID)
		if !ok {
			return "", "", fmt.Errorf("account %s not found in store", accountID)
		}
		if acct.Session != nil && acct.Session.Cookie != "" {
			return acct.Session.Cookie, acct.Session.PH, nil
		}
		// A live new-device challenge means the account is already waiting on
		// the operator: another login would auto-start a second challenge and
		// a second SMS (BUGS-FOUND #2). This check is what makes the
		// guarantee hold for a request that arrives after the winner's flight
		// already ended, not only for one that waited on it.
		if p := s.otp.pendingFor(accountID); p != nil {
			return "", "", &mimo.ErrOTPPending{Challenge: p.Challenge}
		}
		if acct.Phone == "" || acct.Password == "" {
			return "", "", fmt.Errorf("account %s has no session and no login credentials", accountID)
		}

		won, finish, werr := s.logins.acquire(ctx, accountID)
		if werr != nil {
			return "", "", werr
		}
		if !won {
			// A concurrent resolution (another request, or the background
			// recovery) finished while this request waited. Re-read what it
			// left instead of running a login of our own: the session if it
			// landed one, the pending challenge if it stopped at the
			// new-device gate. Losers must not error and must not start
			// another challenge (BUGS-FOUND #2).
			acct, ok := s.store.Get(accountID)
			if ok && acct.Session != nil && acct.Session.Cookie != "" {
				return acct.Session.Cookie, acct.Session.PH, nil
			}
			if p := s.otp.pendingFor(accountID); p != nil {
				return "", "", &mimo.ErrOTPPending{Challenge: p.Challenge}
			}
			if s.recovery.InFlight(accountID) {
				// The ADR 0014 fail-fast: a background re-login is running;
				// answer with account_action and let the caller retry.
				return "", "", &mimo.ErrAccountAction{
					Reason: "credential revoked; automated re-login in progress — retry in a few seconds",
				}
			}
			continue // winner left nothing usable: become the next winner
		}
		cookie, ph, err = s.credsNow(ctx, accountID, acct)
		finish()
		return cookie, ph, err
	}
	return "", "", fmt.Errorf("account %s: credential resolution raced three times without a result", accountID)
}

// credsNow is the winner's half of creds: the renewal attempt and, when it
// cannot renew, the password login — run while holding the account's login
// flight, so exactly one request per account performs it per burst.
func (s *Server) credsNow(ctx context.Context, accountID string, acct accounts.Account) (cookie, ph string, err error) {
	// Renewal before password login: a stored passToken re-mints the session
	// with no user interaction, while the password path re-runs the whole
	// verification chain (new-device OTP = a human, SDD §2.3 / TDD LIVE-006).
	if acct.UserID != "" && acct.PassToken != "" {
		sess, rerr := s.clientFor(acct.Egress).WithDevice(acct.Device).Renew(ctx, acct.UserID, acct.PassToken)
		if rerr == nil {
			applySession(&acct, sess)
			if uerr := s.store.Update(acct); uerr != nil {
				s.logger.Printf("accounts: persist renewed session for %s: %v", accountID, uerr)
			}
			return sess.Cookie, sess.PH, nil
		}
		// The credential is dead (or the renewal was refused): recover in the
		// background instead of blocking this request on a full login. ADR 0014
		// — a password login on a known device needs no operator and no SMS, so
		// the account heals within seconds and the caller simply retries.
		if dead, reason := renewalFailureClass(rerr); dead {
			s.logger.Printf("accounts: renewal for %s failed (%v) — %s; scheduling asynchronous credential recovery", accountID, rerr, reason)
			s.RecoverCredentialAsync(accountID, reason)
			return "", "", &mimo.ErrAccountAction{
				Reason: "credential revoked; automated re-login in progress — retry in a few seconds (" + reason + ")",
				Cause:  rerr,
			}
		}
		s.logger.Printf("accounts: renewal for %s failed (%v), falling back to password login", accountID, rerr)
	}
	sess, err := s.clientFor(acct.Egress).WithDevice(acct.Device).Login(ctx, acct.Phone, acct.Password)
	if err != nil {
		// New-device OTP demand auto-started a challenge: register it for
		// the admin submit endpoint (SDD §2.2 amendment). The request
		// fails with account-action; the account goes available again
		// once the operator submits the code.
		if pending, ok := errOTPPendingUnwrap(err); ok {
			s.otp.add(&otpPending{AccountID: accountID, Challenge: pending.Challenge, CreatedAt: time.Now()})
		}
		// Keep the device identity the attempt harvested even though it did not
		// finish: otherwise the next attempt looks like new hardware again and
		// the operator's pending code no longer belongs to any device we claim.
		if persistDeviceFromError(&acct, err) {
			if uerr := s.store.Update(acct); uerr != nil {
				s.logger.Printf("accounts: persist device for %s: %v", accountID, uerr)
			}
		}
		return "", "", err
	} // Keep the stored identity when the flow could not harvest a fresh one
	// (the landing chain plants passToken only on some passport variants, and
	// blanking it would break the next renewal); the device identity is merged
	// the same way (session_apply.go).
	applySession(&acct, sess)
	if err := s.store.Update(acct); err != nil {
		s.logger.Printf("accounts: persist session for %s: %v", accountID, err)
	}
	return sess.Cookie, sess.PH, nil
}

// refreshSession performs one transparent STS renewal and persists the new
// session (SDD §2.3/§7.1).
func (s *Server) refreshSession(ctx context.Context, accountID string) error {
	acct, ok := s.store.Get(accountID)
	if !ok {
		return fmt.Errorf("account %s not found in store", accountID)
	}
	if acct.UserID == "" || acct.PassToken == "" {
		return fmt.Errorf("account %s has no stored passToken to renew", accountID)
	}
	sess, err := s.clientFor(acct.Egress).WithDevice(acct.Device).Renew(ctx, acct.UserID, acct.PassToken)
	if err != nil {
		return err
	}
	applySession(&acct, sess)
	if err := s.store.Update(acct); err != nil {
		s.logger.Printf("accounts: persist renewed session for %s: %v", accountID, err)
	}
	return nil
}

// Classification helpers (SDD §7.1). These depend on the error alone, so the
// chat pipeline and the audio endpoints share them.
func isUpstream429(err error) bool {
	var ue *mimo.UpstreamError
	return errors.As(err, &ue) && ue.StatusCode == http.StatusTooManyRequests
}

func isAccountDead(err error) bool {
	var ue *mimo.UpstreamError
	if errors.As(err, &ue) && (ue.StatusCode == 451 || ue.StatusCode == 461) {
		return true
	}
	var dead *mimo.ErrPassTokenDead
	if errors.As(err, &dead) {
		return true
	}
	var act *mimo.ErrAccountAction
	if errors.As(err, &act) {
		return true
	}
	// OTP auto-started: the account can't serve until the code is submitted
	// via /v1/admin/otp/submit (SDD §2.2 amendment).
	var otpPending *mimo.ErrOTPPending
	return errors.As(err, &otpPending)
}

// isPermanentDead: only a dead passToken never auto-recovers (SDD §4.5.2). The
// credential itself is dead, which is account-scoped evidence — unlike 451/461,
// whose form we have never observed and which therefore stay recoverable.
func isPermanentDead(err error) bool {
	var dead *mimo.ErrPassTokenDead
	return errors.As(err, &dead)
}

func (p *chatPipeline) isEmptyResponse(err error) bool {
	return errors.Is(err, mimo.ErrEmptyResponse)
}

// busyRetryAfterSeconds is the Retry-After handed to the client when the
// upstream is still busy after the in-request retry.
const busyRetryAfterSeconds = 2

// busyRetryBackoff is how long the gateway waits before retrying a request the
// upstream answered with the transient "服务器繁忙" frame. It is deliberately
// short: the live observation is that the condition clears within a couple of
// seconds, and the client is already waiting on this request.
const busyRetryBackoff = 800 * time.Millisecond

// channelName names the upstream chat channel for logs (SDD §2.4).
func channelName(fastchat bool) string {
	if fastchat {
		return "fastchat/ultraspeed"
	}
	return "normal"
}

func isContentErr(err error) bool {
	var cf *mimo.ErrContentFilter
	var us *mimo.ErrUpstreamStream
	if errors.As(err, &cf) || errors.As(err, &us) {
		return true
	}
	// Image input failures are the client's to fix (IMG-*): an unfetchable URL,
	// bytes that are not an image, a file over the cap. Cooling the account down
	// over a bad URL would punish the one party that did nothing wrong.
	return errors.Is(err, errUnsupportedImageURL) ||
		errors.Is(err, errImageURLBlocked) ||
		errors.Is(err, errImageFetch) ||
		errors.Is(err, errUnsupportedImageType) ||
		errors.Is(err, errImageTooLarge) ||
		asTooManyImages(err) != nil ||
		asImageDetail(err) != nil
}

// onlineSuffix is the model-id shorthand for web_search_status=enabled
// ("mimo-v2.6-flash:online"), mirroring the convention OpenAI-compatible
// clients already use (M19, TDD WS-003).
const onlineSuffix = ":online"
