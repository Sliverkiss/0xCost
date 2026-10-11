package api

// Asynchronous credential recovery (ADR 0014).
//
// The account's passToken can be revoked by risk control at any moment (live:
// a credential with a 30-day lifetime died after 3.5 hours). Everything
// automated mints from it, so until today a revocation meant a human: the
// password login demanded a new-device code. That is no longer true once the
// device anchor is stored — a password login on a known device completes in
// seconds with no verification — so recovery can run by itself, in the
// background, without an operator and without an SMS.
//
// Rules this file enforces:
//   * single flight per account (two concurrent password logins from one IP is
//     exactly the pattern risk control reacts to);
//   * the triggering request does not wait: it fails with account_action and the
//     account heals within seconds, so the next request works;
//   * a recovery that lands on a human gate (OTP, captcha, cross-region
//     consent) persists the identity it harvested, disables the account with the
//     reason and stops — no implicit SMS (ADR 0013);
//   * failed attempts back off, so a broken credential cannot be hammered on
//     every sweep.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
)

const (
	// recoveryTimeout bounds one background login attempt.
	recoveryTimeout = 90 * time.Second
	// recoveryCooldown is the minimum gap between two attempts for one account
	// after a failed one. The 12-hour sweep and a live conversation can both
	// discover the same dead credential; the second must not re-login.
	recoveryCooldown = 5 * time.Minute
)

// recoveryState tracks the asynchronous credential recoveries.
type recoveryState struct {
	mu       sync.Mutex
	inflight map[string]bool
	lastAtom map[string]time.Time
	lastErr  map[string]string
	attempts map[string]int
}

func newRecoveryState() *recoveryState {
	return &recoveryState{
		inflight: map[string]bool{},
		lastAtom: map[string]time.Time{},
		lastErr:  map[string]string{},
		attempts: map[string]int{},
	}
}

// InFlight reports whether a recovery is running for the account.
func (r *recoveryState) InFlight(accountID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight[accountID]
}

// Snapshot returns the recovery view for the admin surface: which accounts are
// being recovered, how many attempts, and the last failure.
func (r *recoveryState) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	inFlight := make([]string, 0, len(r.inflight))
	for id, yes := range r.inflight {
		if yes {
			inFlight = append(inFlight, id)
		}
	}
	lastErr := map[string]string{}
	for id, msg := range r.lastErr {
		if msg != "" {
			lastErr[id] = msg
		}
	}
	attempts := map[string]int{}
	for id, n := range r.attempts {
		if n > 0 {
			attempts[id] = n
		}
	}
	return map[string]any{
		"in_flight":  inFlight,
		"attempts":   attempts,
		"last_error": lastErr,
	}
}

// claim reserves the account for one attempt. It returns false when an attempt
// is already running or the cooldown has not elapsed — the caller then simply
// leaves the current failure to the next trigger.
func (r *recoveryState) claim(accountID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[accountID] {
		return false
	}
	if last, ok := r.lastAtom[accountID]; ok && now.Sub(last) < recoveryCooldown {
		return false
	}
	r.inflight[accountID] = true
	return true
}

func (r *recoveryState) release(accountID, errMsg string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight[accountID] = false
	r.lastAtom[accountID] = now
	// The counter and the cooldown both move on release: an "attempt" here means a
	// finished attempt (REC-002/003 rely on that), and the cooldown measures the gap
	// between finished ones.
	r.attempts[accountID]++
	r.lastErr[accountID] = errMsg
}

// RecoverCredentialAsync starts a background password login for the account and
// returns immediately: true when this call started one, false when one is
// already running, the cooldown has not elapsed, or the account cannot be
// recovered automatically (no stored password, disabled on purpose).
//
// Wired to both discovery points: the 12-hour sweep and any request that finds
// the credential dead (SDD §2.3, §7.1).
func (s *Server) RecoverCredentialAsync(accountID, reason string) bool {
	acct, ok := s.store.Get(accountID)
	if !ok || !acct.Enabled || acct.Phone == "" || acct.Password == "" {
		return false
	}
	if s.pool.PermanentOf(accountID) {
		return false
	}
	if !s.recovery.claim(accountID, time.Now()) {
		return false
	}
	s.logger.Printf("accounts: credential recovery started for %s (%s)", accountID, reason)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
		defer cancel()
		err := s.recoverCredential(ctx, accountID)
		if err != nil {
			s.recovery.release(accountID, err.Error(), time.Now())
			s.logger.Printf("accounts: credential recovery for %s failed: %v", accountID, err)
			return
		}
		s.recovery.release(accountID, "", time.Now())
		s.logger.Printf("accounts: credential recovery for %s completed — account back in rotation", accountID)
	}()
	return true
}

// recoverCredential runs one password login and persists what it returns.
//
// A human gate (new-device OTP, captcha, cross-region consent) is not a
// failure to retry: the identity is persisted, the account is taken out of
// rotation with the reason, and the operator finishes it through the existing
// admin flow.
func (s *Server) recoverCredential(ctx context.Context, accountID string) error {
	acct, ok := s.store.Get(accountID)
	if !ok {
		return errors.New("account not found in store")
	}
	sess, err := s.clientFor(acct.Egress).WithDevice(acct.Device).Login(ctx, acct.Phone, acct.Password)
	if err != nil {
		// Keep the device anchor even though the login stopped, and record it:
		// a later attempt is then the same device (persist_device.go).
		if persistDeviceFromError(&acct, err) {
			if uerr := s.store.Update(acct); uerr != nil {
				s.logger.Printf("accounts: persist device for %s: %v", accountID, uerr)
			}
		}
		var pending *mimo.ErrOTPPending
		if errors.As(err, &pending) {
			s.otp.add(&otpPending{AccountID: accountID, Challenge: pending.Challenge, CreatedAt: time.Now()})
			s.pool.ReportDisabled(accountID, "credential revoked; new-device OTP verification pending (operator: /v1/admin/otp/submit)", false)
			return nil
		}
		var action *mimo.ErrAccountAction
		if errors.As(err, &action) {
			s.pool.ReportDisabled(accountID, "credential revoked; re-login needs a human gate: "+action.Reason, false)
			return nil
		}
		return err
	}
	// The login rotated the passToken (a full login does, a renewal never does),
	// so applySession must run — dropping it would leave the dead credential on
	// file and the account would die again on the next renewal.
	applySession(&acct, sess)
	if uerr := s.store.Update(acct); uerr != nil {
		return errors.New("persist recovered session: " + uerr.Error())
	}
	s.pool.Enable(accountID)
	s.pool.ReportSuccess(accountID)
	return nil
}

// queryCharCap is the character cap applied to one chat message: the operator
// override when set, otherwise the model channel's own cap (SDD §2.8, ADR 0015).
func (s *Server) queryCharCap(model string) int {
	if s.maxQueryChars > 0 {
		return s.maxQueryChars
	}
	return adapter.MaxQueryChars(model)
}

// IsDeadCredential reports whether a renewal/keepalive error means the stored
// credential itself is gone (a login is required), as opposed to a transient
// upstream failure that the next sweep should simply retry.
func IsDeadCredential(err error) bool {
	dead, _ := renewalFailureClass(err)
	return dead
}

// renewalFailureClass tells a dead credential (needs a login) from a transient
// renewal failure (needs another renewal later).
func renewalFailureClass(err error) (dead bool, reason string) {
	if err == nil {
		return false, ""
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "70016", "passToken expired", "登录验证失败"):
		return true, "passToken revoked or expired (code 70016)"
	case containsAny(msg, "captcha"):
		return true, "renewal refused with a captcha demand"
	case containsAny(msg, "20024"):
		return true, "renewal hit the SMS rate limit"
	default:
		return false, msg
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
