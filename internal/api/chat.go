package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/session"
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

	// Model resolution (SDD §5.2, TDD API-013).
	model, fastchat, err := adapter.NormalizeModel(req.Model, s.defaultModel, nil)
	if err != nil {
		s.MapError(w, err)
		return
	}

	// Client-side char caps BEFORE any pool interaction (SDD §2.7, TDD
	// API-016: zero upstream requests recorded).
	cap := adapter.MaxQueryChars(model)
	for _, m := range msgs {
		if len([]rune(m.Content)) > cap {
			s.MapError(w, errContextTooLong)
			return
		}
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
		temp:      req.Temperature,
		topP:      req.TopP,
		stream:    req.Stream,
		requested: req.Model,
	}
	p.run(w, r)
}

// chatPipeline carries one client request through pool → session →
// upstream → response mapping (SDD §3 data flow).
type chatPipeline struct {
	srv       *Server
	msgs      []adapter.Msg
	model     string
	fastchat  bool
	thinking  bool
	temp      *float64
	topP      *float64
	stream    bool
	requested string
}

// attemptPlan is the resolved plan for one upstream attempt.
type attemptPlan struct {
	queries        []string
	conversationID string
	prevDialogID   string
	entry          *session.Entry // sticky hit (streaming only); nil = fresh
}

// sseWriter serializes SSE writes: the keepalive goroutine writes comments
// concurrently with turn writes (SDD §5.4, TDD API-019).
type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	fl http.Flusher
}

func (s *sseWriter) data(payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

func (s *sseWriter) comment(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": %s\n\n", text)
	if s.fl != nil {
		s.fl.Flush()
	}
}

// run: sticky lookup (streaming only), acquire, execution with §7.1
// classification and §7.2 retry policy.
func (p *chatPipeline) run(w http.ResponseWriter, r *http.Request) {
	s := p.srv
	ctx := r.Context()

	// Non-streaming NEVER consults the session store (SDD §4.2, TDD
	// NOSTICK-003).
	var stickyEntry *session.Entry
	if p.stream {
		if e, ok := s.sessions.Lookup(p.msgs); ok {
			stickyEntry = &e
		}
	}

	var excluded []string
	var lastErr error
	attempts := 0

	for attempts < 2 { // original + 1 retry (SDD §7.2)
		var lease *pool.Lease
		var plan *attemptPlan

		if stickyEntry != nil && attempts == 0 {
			// Sticky hit: prefer the stored account (SDD §4.3). Falls back
			// to a normal pick (wasSticky=false) when the account is
			// busy/cooling.
			l, wasSticky, err := s.pool.AcquireSticky(ctx, stickyEntry.AccountID)
			if err != nil {
				lastErr = err
				break
			}
			lease = l
			if wasSticky {
				if dp, derr := adapter.PlanDelta(p.msgs, stickyEntry.MsgCount); derr == nil {
					plan = &attemptPlan{
						queries:        dp.Queries,
						conversationID: stickyEntry.ConversationID,
						prevDialogID:   stickyEntry.PreviousDialogID,
						entry:          stickyEntry,
					}
				}
			}
		}
		if lease == nil {
			l, err := s.pool.Acquire(ctx, excluded...)
			if err != nil {
				lastErr = err
				break
			}
			lease = l
		}
		if plan == nil {
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
		}

		delivered, err := p.execute(ctx, w, lease, plan)
		if err == nil {
			return // success (execute released via finishTurn)
		}
		if delivered {
			// Stream committed and honestly terminated inside execute
			// (SDD §7.2 commit boundary). No retry, no replay.
			lease.Release()
			return
		}
		lastErr = err

		// Client disconnect pre-first-byte (SDD §7.3, TDD STICKY-010):
		// neither success nor failure — release and drop. The client is
		// gone; writing an error frame would only hit a dead pipe.
		if ctx.Err() != nil {
			lease.Release()
			return
		}

		switch {
		case isAuthErr(err):
			// 1 transparent refresh-retry, same account (SDD §7.1, TDD
			// RENEW-005).
			if rerr := s.refreshSession(ctx, lease.ID); rerr == nil {
				delivered2, err2 := p.execute(ctx, w, lease, plan)
				if err2 == nil {
					return
				}
				if delivered2 {
					lease.Release()
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

		case p.isAccountDead(err):
			// 451/461/70016/captcha: disabled, no retry (SDD §7.1).
			s.pool.ReportDisabled(lease.ID, err.Error(), p.isPermanentDead(err))
			lease.Release()
			s.MapError(w, err)
			return

		case p.isUpstream429(err):
			// No retry for this request; cooldown ladder (SDD §7.1, TDD
			// ERRMAP-006).
			s.pool.Report429(lease.ID)
			lease.Release()
			s.MapError(w, err)
			return

		case isContentErr(err):
			// Content-level: fail the request, no pool action (SDD §7.1).
			lease.Release()
			s.MapError(w, err)
			return

		case p.isEmptyResponse(err):
			// 1 retry, fresh ids, full replay (SDD §7.1, TDD SSE-013).
			lease.Release()
			stickyEntry = nil
			attempts++
			continue

		default:
			// Upstream sick: 5xx/transport/stall — 1 retry on another
			// account, cooldown from 30s (SDD §7.1).
			s.pool.Report5xx(lease.ID)
			lease.Release()
			if stickyEntry != nil {
				// The sticky slot pointed at a sick account: discard it so
				// the retry builds a fresh conversation (TDD STICKY-008).
				s.sessions.Delete(stickyEntry.AccountID, stickyEntry.ConversationID)
				stickyEntry = nil
			}
			excluded = append(excluded, lease.ID)
			attempts++
			continue
		}
	}

	// Retries exhausted or pool-level failure — pre-first-byte, so a plain
	// HTTP error JSON is correct (TDD API-020).
	s.MapError(w, lastErr)
}

// execute runs the turn plan on the leased account and delivers the
// response. Commit is lazy: the first client-visible byte is written only
// when the first content/reasoning piece arrives, so empty turns and
// pre-byte failures stay retryable (SDD §7.2). Returns (delivered, err):
// delivered=true means the client stream is complete (success or honest
// error termination) and no retry may happen. execute never releases the
// lease — success paths release via finishTurn, error paths leave it to
// the caller.
func (p *chatPipeline) execute(ctx context.Context, w http.ResponseWriter, lease *pool.Lease, plan *attemptPlan) (bool, error) {
	s := p.srv
	cookie, ph, err := s.creds(ctx, lease.ID)
	if err != nil {
		return false, err
	}

	id := adapter.NewChatID()
	created := adapter.NowUnix()
	modelEcho := p.requested
	if modelEcho == "" {
		modelEcho = p.model
	}

	sw := &sseWriter{w: w}
	sw.fl, _ = w.(http.Flusher)

	committed := false // first client byte written (SDD §7.2 boundary)
	kaStop := make(chan struct{})
	kaDone := make(chan struct{})
	kaStarted := false
	defer func() {
		if kaStarted {
			close(kaStop)
			<-kaDone
		}
	}()

	// commit writes the SSE headers + role chunk exactly once and starts
	// the keepalive ticker (SDD §5.4, TDD API-019).
	commit := func() error {
		if committed {
			return nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		committed = true
		if err := sw.data(adapter.EncodeChunk(adapter.ShapeRoleChunk(id, modelEcho, created))); err != nil {
			return err
		}
		kaStarted = true
		go func() {
			defer close(kaDone)
			t := time.NewTicker(s.keepalive)
			defer t.Stop()
			for {
				select {
				case <-kaStop:
					return
				case <-t.C:
					sw.comment("keepalive")
				}
			}
		}()
		return nil
	}

	var usageSum adapter.Usage
	var lastDialogID string
	var gotContent bool

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
			Temperature:      p.temp,
			TopP:             p.topP,
			OnFirstByte: func() {
				s.pool.RecordFirstByte(lease.ID, time.Since(attemptStart))
			},
			// Capture the dialogId the moment it arrives so a client
			// disconnect mid-stream still advances the sticky anchor (SDD
			// §7.3, TDD STICKY-010).
			OnDialogID: func(dialogID string) {
				lastDialogID = dialogID
			},
		}
		isFinal := qi == len(plan.queries)-1
		if p.stream && isFinal {
			// Only the final replay turn streams; earlier turns collect
			// (SDD §5.3B). Reasoning is stripped when thinking is off (TDD
			// API-010).
			chatReq.OnPiece = func(piece adapter.Piece) error {
				if err := ctx.Err(); err != nil {
					return err // client disconnect propagates upstream (§7.3)
				}
				if piece.Type == "reasoning" && !p.thinking {
					return nil
				}
				if err := commit(); err != nil {
					return err
				}
				gotContent = true
				return sw.data(adapter.EncodeChunk(adapter.ShapeTextChunk(id, modelEcho, created, piece)))
			}
		}

		res, err := s.client.Chat(ctx, cookie, ph, chatReq)
		if err != nil {
			if ctx.Err() != nil {
				// Client disconnect (SDD §7.3, TDD API-024/STICKY-010):
				// abort upstream (already done via ctx), keep the sticky
				// entry when a dialogId exists, count neither success nor
				// failure.
				if plan.entry != nil && lastDialogID != "" {
					s.sessions.UpdateDialog(plan.entry.AccountID, plan.entry.ConversationID, lastDialogID)
				}
				return committed, ctx.Err()
			}
			if committed {
				// Post-first-byte failure: honest termination, never a
				// restart (SDD §7.3, TDD API-021/STICKY-009). Content-level
				// errors carry no pool action; everything else is sick.
				if !isContentErr(err) {
					s.pool.Report5xx(lease.ID)
				}
				if plan.entry != nil {
					s.sessions.Delete(plan.entry.AccountID, plan.entry.ConversationID)
				}
				s.terminateStream(sw, id, modelEcho, created, err)
				return true, err
			}
			return false, err
		}
		usageSum = adapter.AddUsage(usageSum, adapter.MapUsage(res.Usage))
		if res.DialogID != "" {
			lastDialogID = res.DialogID
		}
		if !p.stream && qi == len(plan.queries)-1 {
			// Non-streaming JSON delivery (SDD §5.4).
			if res.Content == "" && res.Reasoning == "" {
				return false, mimo.ErrEmptyResponse
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			comp := adapter.ShapeCompletion(id, modelEcho, created, res.Content, res.Reasoning, usageSum)
			if err := encodeJSON(w, comp); err != nil {
				return false, err
			}
			p.finishTurn(lease, plan, lastDialogID)
			return false, nil
		}
	}

	// Streaming finish (SDD §5.4): finish chunk, usage chunk with
	// choices:[], [DONE].
	if !committed || !gotContent {
		// Zero pieces reached the client: empty response — retryable
		// pre-byte (SDD §7.1, TDD SSE-013).
		return false, mimo.ErrEmptyResponse
	}
	if err := sw.data(adapter.EncodeChunk(adapter.ShapeFinishChunk(id, modelEcho, created))); err != nil {
		return true, err
	}
	if err := sw.data(adapter.EncodeChunk(adapter.ShapeUsageChunk(id, modelEcho, created, usageSum))); err != nil {
		return true, err
	}
	if err := sw.data("[DONE]"); err != nil {
		return true, err
	}
	p.finishTurn(lease, plan, lastDialogID)
	return true, nil
}

// finishTurn records success and advances the sticky store (SDD §4.3, TDD
// STICKY-011). Miss-path streaming turns store a fresh entry; non-streaming
// never touches the store (SDD §4.2).
func (p *chatPipeline) finishTurn(lease *pool.Lease, plan *attemptPlan, lastDialogID string) {
	p.srv.pool.ReportSuccess(lease.ID)
	if !p.stream {
		lease.Release()
		return
	}
	if plan.entry != nil {
		p.srv.sessions.UpdateDialog(plan.entry.AccountID, plan.entry.ConversationID, lastDialogID)
	} else if lastDialogID != "" {
		now := time.Now().Unix()
		p.srv.sessions.Save(session.Entry{
			AccountID:        lease.ID,
			ConversationID:   plan.conversationID,
			PreviousDialogID: lastDialogID,
			Fingerprint:      adapter.Fingerprint(p.msgs),
			MsgCount:         adapter.CountNonSystem(p.msgs),
			CreatedAt:        now,
			UpdatedAt:        now,
		})
	}
	lease.Release()
}

// terminateStream appends the honest error-termination frames (SDD §7.3:
// error-text content delta → delta:{} + finish_reason:"stop" → [DONE];
// never a hang, never restarted deltas).
func (s *Server) terminateStream(sw *sseWriter, id, model string, created int64, err error) {
	piece := adapter.Piece{Type: "content", Text: "\n\n[upstream error] " + err.Error()}
	_ = sw.data(adapter.EncodeChunk(adapter.ShapeTextChunk(id, model, created, piece)))
	_ = sw.data(adapter.EncodeChunk(adapter.ShapeFinishChunk(id, model, created)))
	_ = sw.data("[DONE]")
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
	sess, err := s.client.Login(ctx, acct.Phone, acct.Password)
	if err != nil {
		// New-device OTP demand auto-started a challenge: register it for
		// the admin submit endpoint (SDD §2.2 amendment). The request
		// fails with account-action; the account goes available again
		// once the operator submits the code.
		if pending, ok := errOTPPendingUnwrap(err); ok {
			s.otp.add(&otpPending{AccountID: accountID, Challenge: pending.Challenge, CreatedAt: time.Now()})
		}
		return "", "", err
	}
	// Keep the stored identity when the flow could not harvest a fresh one
	// (the landing chain plants passToken only on some passport variants, and
	// blanking it would break the next renewal).
	if sess.UserID != "" {
		acct.UserID = sess.UserID
	}
	if sess.PassToken != "" {
		acct.PassToken = sess.PassToken
	}
	acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
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
	sess, err := s.client.Renew(ctx, acct.UserID, acct.PassToken)
	if err != nil {
		return err
	}
	acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
	if err := s.store.Update(acct); err != nil {
		s.logger.Printf("accounts: persist renewed session for %s: %v", accountID, err)
	}
	return nil
}

// Classification helpers (SDD §7.1).
func (p *chatPipeline) isUpstream429(err error) bool {
	var ue *mimo.UpstreamError
	return errors.As(err, &ue) && ue.StatusCode == http.StatusTooManyRequests
}

func (p *chatPipeline) isAccountDead(err error) bool {
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

// isPermanentDead: 461 and a dead passToken never auto-recover (SDD §4.6).
func (p *chatPipeline) isPermanentDead(err error) bool {
	var ue *mimo.UpstreamError
	if errors.As(err, &ue) && ue.StatusCode == 461 {
		return true
	}
	var dead *mimo.ErrPassTokenDead
	return errors.As(err, &dead)
}

func (p *chatPipeline) isEmptyResponse(err error) bool {
	return errors.Is(err, mimo.ErrEmptyResponse)
}

func isContentErr(err error) bool {
	var cf *mimo.ErrContentFilter
	var us *mimo.ErrUpstreamStream
	return errors.As(err, &cf) || errors.As(err, &us)
}
