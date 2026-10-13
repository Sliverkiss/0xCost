package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/mimo"
)

// otpPending is one in-flight new-device OTP challenge (SDD §2.2
// amendment): the Login attempt that triggered it, the challenge handle,
// and the expiry. Challenge cookies and the code live in memory only.
type otpPending struct {
	AccountID string
	Challenge *mimo.OTPChallenge
	CreatedAt time.Time
}

// otpStore is the mutex-guarded challenge registry: 10-minute expiry,
// lazy sweep on every access (safe cleanup — nothing leaks past TTL).
type otpStore struct {
	mu    sync.Mutex
	items map[string]*otpPending
	nowFn func() time.Time
}

func newOTPStore() *otpStore {
	return &otpStore{
		items: map[string]*otpPending{},
		nowFn: time.Now,
	}
}

// add registers a pending challenge.
func (o *otpStore) add(p *otpPending) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sweepLocked()
	o.items[p.Challenge.ID] = p
}

// get returns a live challenge by id (expired entries vanish).
func (o *otpStore) get(id string) (*otpPending, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sweepLocked()
	p, ok := o.items[id]
	return p, ok
}

// remove deletes a challenge (completed or abandoned).
func (o *otpStore) remove(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.items, id)
}

// claim atomically takes the challenge out of the store (BUGS-FOUND #1):
// false means it was never live or another submit already claimed it, so
// the caller answers 404 instead of verifying upstream a second time.
func (o *otpStore) claim(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sweepLocked()
	_, ok := o.items[id]
	if ok {
		delete(o.items, id)
	}
	return ok
}

// restore puts a claimed challenge back (BUGS-FOUND #1): a wrong code is a
// retry, not an abandonment, so the operator's 10-minute window survives.
// The entry is re-inserted under its original id; a challenge that was
// claimed again in the meantime cannot exist (claim removed it), so the
// insert cannot clobber a fresher one — the only writer between claim and
// restore is the restore itself.
func (o *otpStore) restore(p *otpPending) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items[p.Challenge.ID] = p
}

// isOTPWrongCode reports whether a SubmitOTP failure is the 70002-class
// "code wrong or expired" answer — the one case where the challenge must be
// restored for the operator to retry. Transport failures and other upstream
// errors do not restore: a half-dead passport is not a reason to re-arm a
// challenge whose device session may already be consumed.
func isOTPWrongCode(err error) bool {
	return err != nil && strings.Contains(err.Error(), "70002")
}

// list returns live challenges (oldest first).
func (o *otpStore) list() []*otpPending {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sweepLocked()
	out := make([]*otpPending, 0, len(o.items))
	for _, p := range o.items {
		out = append(out, p)
	}
	// Deterministic order: oldest created first.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.Before(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// sweepLocked drops expired entries; caller holds o.mu.
func (o *otpStore) sweepLocked() {
	now := o.nowFn()
	for id, p := range o.items {
		if now.After(p.Challenge.ExpiresAt) {
			delete(o.items, id)
		}
	}
}

// pendingOTP is the public JSON shape of GET /v1/admin/otp/pending — id,
// method, masked destination, account, expiry. No secrets (no cookies, no
// code, no passToken).
type pendingOTP struct {
	ID        string `json:"id"`
	Method    string `json:"method"`
	Notify    string `json:"notify"`
	AccountID string `json:"account"`
	ExpiresAt int64  `json:"expires_at"`
}

// handleOTPPending: GET /v1/admin/otp/pending — list in-flight OTP
// challenges (SDD §2.2 amendment).
func (s *Server) handleOTPPending(w http.ResponseWriter, r *http.Request) {
	if s.otp == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "otp_disabled",
			"OTP flow not configured", 0)
		return
	}
	items := s.otp.list()
	out := make([]pendingOTP, 0, len(items))
	for _, p := range items {
		out = append(out, pendingOTP{
			ID:        p.Challenge.ID,
			Method:    p.Challenge.Method,
			Notify:    p.Challenge.Notify,
			AccountID: p.AccountID,
			ExpiresAt: p.Challenge.ExpiresAt.Unix(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"pending": out})
}

// handleOTPSubmit: POST /v1/admin/otp/submit {id, code} — complete the
// login (SubmitOTP), persist the session, re-enable the account (SDD §2.2
// amendment).
func (s *Server) handleOTPSubmit(w http.ResponseWriter, r *http.Request) {
	if s.otp == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "otp_disabled",
			"OTP flow not configured", 0)
		return
	}
	var body struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.ID == "" || body.Code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed_request_body",
			"body must be {\"id\": \"<challenge id>\", \"code\": \"<otp code>\"}", 0)
		return
	}
	p, ok := s.otp.get(body.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "otp_not_found",
			"OTP challenge not found or expired (10-minute window)", 0)
		return
	}
	// Claim the challenge BEFORE the upstream verification (BUGS-FOUND #1):
	// the removal is atomic with the existence check above only in the sense
	// that both run under the store's mutex, so a concurrent submit of the
	// same id finds it gone and 404s instead of verifying a second time.
	// Both the claim and the check re-run under the store mutex, so two
	// racing submits cannot both pass `get` and then both `remove` — the
	// loser's remove finds nothing and returns false.
	if !s.otp.claim(body.ID) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "otp_not_found",
			"OTP challenge not found or expired (10-minute window)", 0)
		return
	}

	sess, err := s.clientForAccount(p.AccountID).SubmitOTP(r.Context(), p.Challenge, body.Code)
	if err != nil {
		// Wrong code / expired: the challenge stays live for a retry (the
		// real flow allows re-entry of the code within the window). Restore
		// what the claim took — unless the account vanished mid-submit (a
		// vanished account leaves nothing to restore for) or the store
		// already holds a session (the login completed elsewhere; a restored
		// challenge would be dead weight an operator could still submit).
		if isOTPWrongCode(err) {
			if acct, aok := s.store.Get(p.AccountID); aok && (acct.Session == nil || acct.Session.Cookie == "") {
				s.otp.restore(p)
			} else {
				s.debugf("otp: wrong code for %s, but the challenge is not restored (account gone or session already landed)", p.AccountID)
			}
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "otp_rejected",
			"OTP rejected: "+err.Error(), 0)
		return
	}

	// Persist the session and re-enable the account.
	acct, aok := s.store.Get(p.AccountID)
	if !aok {
		s.otp.remove(p.Challenge.ID)
		writeError(w, http.StatusInternalServerError, "api_error", "account_not_found",
			"account vanished from store during OTP submit", 0)
		return
	}
	// Keep the stored identity when the flow could not harvest a fresh one
	// (the landing chain plants passToken only on some passport variants, and
	// blanking it would break the next renewal); the device identity the
	// verified chain planted is merged in the same step (session_apply.go).
	applySession(&acct, sess)
	if err := s.store.Update(acct); err != nil {
		s.logger.Printf("accounts: persist OTP session for %s: %v", p.AccountID, err)
	}
	s.pool.Enable(p.AccountID)
	s.otp.remove(p.Challenge.ID)
	s.logger.Printf("otp: account %s logged in via OTP (%s)", p.AccountID, p.Challenge.Method)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"account": p.AccountID,
	})
}

// errOTPPendingUnwrap extracts the challenge from a Login error (creds
// path integration).
func errOTPPendingUnwrap(err error) (*mimo.ErrOTPPending, bool) {
	var pending *mimo.ErrOTPPending
	if errors.As(err, &pending) {
		return pending, true
	}
	return nil, false
}
