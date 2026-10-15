package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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

	// original + 3 retries: the only retryable failures are the transient busy
	// frame and an empty response, and the busy retry is immediate — the client
	// is already waiting, and the live observation is that the condition clears
	// in seconds (owner ruling 2026-10-14: "800ms改成0，直接重试，最多3次").
	for attempts < 4 {
		var lease *pool.Lease
		var plan *attemptPlan

		l, err := s.pool.Acquire(ctx, excluded...)
		if err != nil {
			lastErr = err
			break
		}
		lease = l
		// Backstop for the release: every explicit path below releases, and this
		// catches a future early return (or a panic) that forgets to. Release is
		// an idempotent no-op, so it costs nothing on the normal path (ADR 0019).
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
			// Upstream 401 = dead session, and the request path does not
			// repair sessions (owner ruling 2026-10: no renewal, no login, no
			// recovery from a request — the hourly renew job owns freshness).
			// Pass the error through; the account stays as it is, and the next
			// job round mints a fresh session if the credential still lives.
			lease.Release()
			p.fail(w, err)
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
			// clear within seconds. Retry immediately (owner ruling 2026-10-14:
			// no backoff wait — "直接重试，最多3次"). The account stays healthy —
			// this is platform load, not a dead account.
			lease.Release()
			if attempts >= 3 {
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
			s.debugf("chat: upstream busy, retrying immediately (attempt %d/4, account=%s, model=%s)", attempts+2, lease.ID, p.model)
			attempts++
			continue

		case isGatewayDeadline(err, clientCtx):
			// Our own budget or the per-turn lease TTL fired (ADR 0019). Nothing
			// is wrong with the account: no cooldown, no retry, and the client
			// gets a 504 naming the knob instead of a silent hang.
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
			// Non-ultraspeed models ride the v3 react channel — the SPA's own
			// routing (reactEnabled accounts, live 2026-10-15) and the one
			// channel that is open while the v2 surface sits behind the 451
			// channel block (BUGS-FOUND #7 correction). Fastchat wins when
			// both flags apply; a deployment whose react route is gone falls
			// back to v2 inside the client (SPA: `catch 404 → v2`).
			React:            !p.fastchat,
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

		// Per-turn bound (ADR 0036): the lease TTL is a **silence** window — a
		// turn is cut for going quiet, not for taking long: a search that keeps
		// fetching pages runs until it answers, while a wedged turn dies on
		// time. This is request liveness, not account capacity — there is no
		// in-flight accounting anymore (owner ruling 2026-10-14). The
		// operator's wall clock (MIMO2API_REQUEST_TIMEOUT_MS) rides on ctx, not
		// here.
		turnCtx, cancelTurn := context.WithCancel(ctx)
		chatReq.IdleTimeout = s.leaseTTL
		res, err := s.clientFor(lease.Egress).Chat(turnCtx, cookie, ph, chatReq)
		cancelTurn()
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
			// … and release the lease BEFORE writing to the client: the
			// upstream work is done; a slow client must not hold it (ADR 0019).
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
// platform-side failure is the upstream's own business (no cooldown machinery
// exists to act on it), and our own deadline means the account did nothing
// wrong — a retry would only spend another upstream call on a request we
// already gave up on.
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

// creds resolves the upstream cookie + ph for an account from the store —
// nothing else (owner ruling 2026-10: the request path never renews, never
// logs in, never schedules a recovery). Session freshness is owned by the
// hourly renew job (renewjob.go); a missing or dead session is an error the
// caller passes through, and the next job round repairs it.
func (s *Server) creds(ctx context.Context, accountID string) (cookie, ph string, err error) {
	_ = ctx // resolution is a pure store read; ctx stays for the callers' shape
	acct, ok := s.store.Get(accountID)
	if !ok {
		return "", "", fmt.Errorf("account %s not found in store", accountID)
	}
	if acct.Session == nil || acct.Session.Cookie == "" {
		return "", "", &noSessionError{AccountID: accountID}
	}
	return acct.Session.Cookie, acct.Session.PH, nil
}

// noSessionError: the account has no stored session and the request path will
// not mint one — the hourly renew job is the only writer (owner ruling
// 2026-10). The client answer carries that fact instead of a login attempt.
type noSessionError struct{ AccountID string }

func (e *noSessionError) Error() string {
	return "account " + e.AccountID + " has no stored session; the hourly renew job will restore it"
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
// upstream is still busy after the in-request retries.
const busyRetryAfterSeconds = 2

// channelName names the upstream chat channel for logs (SDD §2.4). The
// normal channel is the v3 react surface since 2026-10-15 (with the client's
// 404→v2 fallback behind it), so that is the name a 451 report carries — the
// operator needs to know which surface the refusal came from.
func channelName(fastchat bool) string {
	if fastchat {
		return "fastchat/ultraspeed"
	}
	return "react/v3"
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
