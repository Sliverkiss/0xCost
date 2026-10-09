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
	am := make([]adapter.Msg, len(msgs))
	for i, m := range msgs {
		am[i] = adapter.Msg{Role: m.Role, Content: m.Content}
	}

	// This gateway has no streaming path at all (ADR 0017): refuse stream:true
	// up front, before any pool interaction, and name the fix. Converting it
	// silently to a one-shot JSON body would hand an SSE client a response it
	// cannot parse, and the upstream answer is a single collected turn either
	// way.
	if req.Stream {
		s.logger.Printf("chat: refused streaming request from %s (this gateway serves non-streaming chat only)", r.RemoteAddr)
		s.MapError(w, &streamUnsupportedError{})
		return
	}

	// Search switch (M19, TDD WS-001/002/003). The ":online" model suffix is
	// shorthand for web_search_status=enabled; an explicit field wins when
	// both are present.
	search, err := req.searchStatus()
	if err != nil {
		s.MapError(w, err)
		return
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
	model, fastchat, err := adapter.NormalizeModel(requested, s.defaultModel, nil)
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

	// Dropped-field warning header (SDD §5.2, TDD API-008/009).
	if dropped := req.droppedFields(); len(dropped) > 0 {
		w.Header().Set("X-Mimo-Warnings", fmt.Sprintf("ignored unsupported fields: %v", dropped))
	}

	p := &chatPipeline{
		srv:       s,
		msgs:      am,
		model:     model,
		fastchat:  fastchat,
		thinking:  req.thinkingEnabled(),
		search:    search,
		citations: req.citationsWanted(),
		temp:      req.Temperature,
		topP:      req.TopP,
		requested: req.Model,
	}
	p.run(w, r)
}

// chatPipeline carries one client request through pool → upstream → response
// mapping (SDD §3 data flow). There is no streaming state here because there is
// no streaming path: one request is one collected answer (ADR 0017).
type chatPipeline struct {
	srv       *Server
	msgs      []adapter.Msg
	model     string
	fastchat  bool
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
	conversationID string
	prevDialogID   string
}

// run executes one request against a leased account with §7.1 classification and
// §7.2 retry policy. A turn is always one-shot (ADR 0017): no session identity is
// resolved, nothing is read from or written to a conversation store, and the
// replay below is always a full replay into a fresh upstream conversation.
func (p *chatPipeline) run(w http.ResponseWriter, r *http.Request) {
	s := p.srv
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
			s.MapError(w, ferr)
			return
		}
		plan = &attemptPlan{
			queries:        fp.Queries,
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
			s.MapError(w, lastErr)
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
			s.MapError(w, err)
			return

		case isAccountDead(err):
			// 70016/captcha/OTP demand/dead passToken: disabled, no retry
			// (SDD §7.1).
			s.pool.ReportDisabled(lease.ID, err.Error(), isPermanentDead(err))
			lease.Release()
			s.MapError(w, err)
			return

		case isUpstream429(err):
			// No retry for this request; cooldown ladder (SDD §7.1, TDD
			// ERRMAP-006).
			s.pool.Report429(lease.ID)
			lease.Release()
			s.MapError(w, err)
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
			s.MapError(w, err)
			return

		case errors.Is(err, mimo.ErrUpstreamBusy):
			// Transient throttle ("服务器繁忙，请稍后再试"): live-observed to
			// clear within seconds, so retry once after a short pause
			// rather than failing a request that would succeed a moment
			// later. The account stays healthy — this is platform load,
			// not a dead account.
			lease.Release()
			if attempts > 0 {
				s.MapError(w, err)
				return
			}
			// A retired model id is answered with this same busy frame (live
			// 2026-10-09: a superseded id returned it 3/3 while the id it redirects
			// to answered 3/3), so treat the refusal as information: rebuild the
			// superseded-id map and re-resolve before the retry. If the id was
			// retired between two refreshes, this is where the deployment notices.
			if m := s.relearnModel(ctx, p.requested); m != p.model {
				if nm, fc, nerr := adapter.NormalizeModel(m, s.defaultModel, nil); nerr == nil {
					p.model, p.fastchat = nm, fc
				}
			}
			s.debugf("chat: upstream busy, retrying in %s (account=%s, model=%s)", busyRetryBackoff, lease.ID, p.model)
			attempts++
			select {
			case <-time.After(busyRetryBackoff):
			case <-ctx.Done():
				s.MapError(w, s.budgetError(clientCtx, ctx, "waiting to retry a busy upstream"))
				return
			}
			continue

		case isGatewayDeadline(err, clientCtx):
			// Our own budget or the per-turn lease TTL fired (ADR 0019). Nothing
			// is wrong with the account: no cooldown, no retry, and the client
			// gets a 504 naming the knob instead of a silent hang.
			s.logger.Printf("chat: gateway deadline while account %s was serving: %v", lease.ID, err)
			lease.Release()
			s.MapError(w, s.budgetError(clientCtx, ctx, "waiting for the upstream"))
			return

		case isContentErr(err):
			// Content-level: fail the request, no pool action (SDD §7.1).
			lease.Release()
			s.MapError(w, err)
			return

		case p.isEmptyResponse(err):
			// 1 retry, fresh ids, full replay (SDD §7.1, TDD EMPTY-001).
			lease.Release()
			attempts++
			continue

		default:
			// Upstream sick: 1 retry on another account, cooled down on the
			// ladder of its own class — a transport failure no longer advances
			// the 5xx ladder (SDD §4.5.1, TDD POOLATTR-001).
			s.reportFailure(lease.ID, err)
			lease.Release()
			excluded = append(excluded, lease.ID)
			attempts++
			continue
		}
	}

	// Retries exhausted or pool-level failure — pre-first-byte, so a plain
	// HTTP error JSON is correct (TDD API-020).
	s.MapError(w, lastErr)
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

	id := adapter.NewChatID()
	created := adapter.NowUnix()
	modelEcho := p.requested
	if modelEcho == "" {
		modelEcho = p.model
	}

	var usageSum adapter.Usage
	var lastDialogID string

	for qi, query := range plan.queries {
		prev := plan.prevDialogID
		if lastDialogID != "" {
			prev = lastDialogID
		}
		attemptStart := time.Now()
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

		// Per-turn deadline (ADR 0019): one upstream turn may not outlive the
		// lease TTL. The cancel is armed on the lease so that a slot the reaper
		// reclaims takes its in-flight upstream call down with it.
		turnCtx := ctx
		var cancelTurn context.CancelFunc
		if s.leaseTTL > 0 {
			turnCtx, cancelTurn = context.WithTimeout(ctx, s.leaseTTL)
			lease.ArmExpiry(cancelTurn)
		}
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
	return errors.Is(err, context.DeadlineExceeded)
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
	if s.leaseTTL > 0 && (budget <= 0 || s.leaseTTL < budget) {
		budget = s.leaseTTL
	}
	return &requestTimeoutError{Budget: budget, Where: where}
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
func (s *Server) creds(ctx context.Context, accountID string) (cookie, ph string, err error) {
	acct, ok := s.store.Get(accountID)
	if !ok {
		return "", "", fmt.Errorf("account %s not found in store", accountID)
	}
	if acct.Session != nil && acct.Session.Cookie != "" {
		return acct.Session.Cookie, acct.Session.PH, nil
	}
	if acct.Phone == "" || acct.Password == "" {
		return "", "", fmt.Errorf("account %s has no session and no login credentials", accountID)
	}
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
	}
	// Keep the stored identity when the flow could not harvest a fresh one
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
	return errors.As(err, &cf) || errors.As(err, &us)
}

// onlineSuffix is the model-id shorthand for web_search_status=enabled
// ("mimo-v2.6-flash:online"), mirroring the convention OpenAI-compatible
// clients already use (M19, TDD WS-003).
const onlineSuffix = ":online"
