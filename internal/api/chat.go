package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

	// Model resolution (SDD §5.2, TDD API-013).
	model, fastchat, err := adapter.NormalizeModel(requested, s.defaultModel, nil)
	if err != nil {
		s.MapError(w, err)
		return
	}

	// Client-side char caps BEFORE any pool interaction (SDD §2.8, TDD
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
		search:    search,
		citations: req.citationsWanted(),
		temp:      req.Temperature,
		topP:      req.TopP,
		stream:    req.Stream,
		requested: req.Model,
		sessionID: req.SessionID,
		tools:     string(req.Tools),
		cred:      credentialOf(r),
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
	// search is the upstream webSearchStatus (disabled|auto|enabled) and
	// citations says whether citations are returned (M19).
	search    string
	citations bool
	// sessionID / tools / cred feed the identity ladder (SDD §4.3.1). tools is
	// part of the derived key because a changed tool set is a different agent
	// session (ADR 0009 decision 2).
	sessionID string
	tools     string
	cred      string
}

// attemptPlan is the resolved plan for one upstream attempt.
type attemptPlan struct {
	queries        []string
	conversationID string
	prevDialogID   string
	entry          *session.Entry // verified session hit (streaming only); nil = replay
	sessionKey     string         // store key for the finished turn (streaming only)
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

// run: resolve the session (streaming only), claim it, then execute with §7.1
// classification and §7.2 retry policy.
func (p *chatPipeline) run(w http.ResponseWriter, r *http.Request) {
	s := p.srv
	ctx := r.Context()

	// Non-streaming is one-shot: no identity is resolved, the session store is
	// never read or written, and no session header is returned (SDD §4.3.1,
	// REQ-10). A session id supplied here is ignored, on purpose.
	var stickyEntry *session.Entry
	var ident session.Identity
	var sessionKey string
	var forkedSession bool
	var forkWaited bool
	var forkFrom string
	if p.stream {
		ident = session.Resolve(session.ResolveInput{
			Credential: p.cred,
			HeaderID:   r.Header.Get("X-Mimo-Session-Id"),
			BodyID:     p.sessionID,
			Model:      p.model,
			System:     adapter.SystemPreamble(p.msgs),
			Tools:      p.tools,
			FirstMsg:   firstNonSystem(p.msgs),
			Mint:       adapter.RandomHex32,
		})
		sessionKey = ident.Key
		w.Header().Set("X-Mimo-Session-Id", ident.PublicID)

		// Fork policy (REQ-8): one live chain writer per session. A second
		// request waits for the bound, then forks onto a fresh session instead
		// of interleaving into someone else's upstream dialogue.
		held, waited := s.sessions.Acquire(ctx, sessionKey, s.forkWait())
		if !held {
			forkedSession, forkWaited, forkFrom = true, waited, ident.PublicID
			ident = session.Resolve(session.ResolveInput{Credential: p.cred, Mint: adapter.RandomHex32})
			sessionKey = ident.Key
			w.Header().Set("X-Mimo-Session-Id", ident.PublicID)
			s.sessions.Acquire(ctx, sessionKey, 0)
		}
		// Exactly one release per claim — including a forked session, or its
		// in-flight count would never return to zero and every later turn on
		// that id would fork again.
		defer s.sessions.Release(sessionKey)
		if !forkedSession {
			if e, ok := s.sessions.Lookup(sessionKey, p.msgs); ok {
				stickyEntry = &e
			}
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
						sessionKey:     sessionKey,
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
				sessionKey:     sessionKey,
			}
		}

		if plan.entry != nil {
			w.Header().Set("X-Mimo-Session-Mode", "chain")
			s.debugf("session: decision=chain session=%s source=%s frontier=%d history=%d",
				ident.PublicID, ident.Source, plan.entry.MsgCount, adapter.CountNonSystem(p.msgs))
			s.debugf("chat: sticky continuation account=%s conversation=%s prevDialog=%s deltaQueries=%d msgCount=%d",
				lease.ID, plan.conversationID, plan.prevDialogID, len(plan.queries), plan.entry.MsgCount)
		} else {
			decision := "replay"
			switch {
			case !p.stream:
				decision = "one-shot"
			case forkedSession:
				decision = "fork"
			}
			if p.stream {
				w.Header().Set("X-Mimo-Session-Mode", decision)
				if forkedSession {
					s.debugf("session: decision=%s session=%s source=%s pinned=%s waited=%v frontier=0 history=%d",
						decision, ident.PublicID, ident.Source, forkFrom, forkWaited, adapter.CountNonSystem(p.msgs))
				} else {
					s.debugf("session: decision=%s session=%s source=%s frontier=0 history=%d",
						decision, ident.PublicID, ident.Source, adapter.CountNonSystem(p.msgs))
				}
			}
			s.debugf("chat: full replay account=%s conversation=%s (fresh) queries=%d replayCap=%d",
				lease.ID, plan.conversationID, len(plan.queries), s.replayCap)
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

		case isAccountDead(err):
			// 451/461/70016/captcha: disabled, no retry (SDD §7.1).
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
			s.debugf("chat: upstream busy, retrying in %s (account=%s)", busyRetryBackoff, lease.ID)
			stickyEntry = nil
			attempts++
			select {
			case <-time.After(busyRetryBackoff):
			case <-ctx.Done():
				s.MapError(w, ctx.Err())
				return
			}
			continue

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
				s.sessions.Delete(stickyEntry.SessionKey)
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
	var citations []adapter.Citation

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
			// Capture citations as they arrive so a streaming client can be
			// handed them before [DONE] (M19).
			OnCitations: func(cits []adapter.Citation) {
				citations = cits
			},
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
					s.sessions.UpdateDialog(plan.sessionKey, lastDialogID, p.msgs)
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
					s.sessions.Delete(plan.sessionKey)
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
			cits := res.Citations
			if !p.citations {
				cits = nil // include_citations:false → search still ran
			}
			for _, w := range res.CitationWarnings {
				s.debugf("chat: skipping a web_search payload: %s", w)
			}
			if len(cits) > 0 {
				s.debugf("chat: turn returned %d web-search citations", len(cits))
			}
			comp := adapter.ShapeCompletion(id, modelEcho, created, res.Content, res.Reasoning, usageSum, cits...)
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
	// Citations go out before the finish chunk so a client that stops reading
	// at finish_reason still sees them (M19). choices:[] keeps standard
	// clients compatible.
	if p.citations && len(citations) > 0 {
		s.debugf("chat: turn returned %d web-search citations", len(citations))
		if err := sw.data(adapter.EncodeChunk(adapter.ShapeCitationsChunk(id, modelEcho, created, citations))); err != nil {
			return true, err
		}
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

// finishTurn records success and advances the session store (SDD §4.3.1, TDD
// STICKY-011). Streaming turns advance the verified session or store a fresh
// one; non-streaming is one-shot and never touches the store (REQ-10).
func (p *chatPipeline) finishTurn(lease *pool.Lease, plan *attemptPlan, lastDialogID string) {
	p.srv.pool.ReportSuccess(lease.ID)
	if !p.stream {
		lease.Release()
		return
	}
	if plan.entry != nil {
		p.srv.sessions.UpdateDialog(plan.sessionKey, lastDialogID, p.msgs)
	} else if lastDialogID != "" {
		now := time.Now().Unix()
		p.srv.sessions.Save(session.Entry{
			SessionKey:       plan.sessionKey,
			AccountID:        lease.ID,
			ConversationID:   plan.conversationID,
			PreviousDialogID: lastDialogID,
			FrontierHash:     adapter.FrontierHash(p.msgs),
			MsgCount:         adapter.CountNonSystem(p.msgs),
			CreatedAt:        now,
			UpdatedAt:        now,
		})
	}
	lease.Release()
}

// firstNonSystem returns the first non-system message content — a derived-key
// input (REQ-4).
func firstNonSystem(msgs []adapter.Msg) string {
	for _, m := range msgs {
		if m.Role != "system" {
			return m.Content
		}
	}
	return ""
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
	// Renewal before password login: a stored passToken re-mints the session
	// with no user interaction, while the password path re-runs the whole
	// verification chain (new-device OTP = a human, SDD §2.3 / TDD LIVE-006).
	if acct.UserID != "" && acct.PassToken != "" {
		sess, rerr := s.client.Renew(ctx, acct.UserID, acct.PassToken)
		if rerr == nil {
			acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
			if uerr := s.store.Update(acct); uerr != nil {
				s.logger.Printf("accounts: persist renewed session for %s: %v", accountID, uerr)
			}
			return sess.Cookie, sess.PH, nil
		}
		s.logger.Printf("accounts: renewal for %s failed (%v), falling back to password login", accountID, rerr)
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

// isPermanentDead: 461 and a dead passToken never auto-recover (SDD §4.6).
func isPermanentDead(err error) bool {
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
