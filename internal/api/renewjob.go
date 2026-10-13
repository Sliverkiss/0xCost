package api

// The hourly renew job (owner ruling, 2026-10 — verbatim: "那个过期后自动
// 重新token续期的逻辑我们砍掉，改成只通过定时任务每小时自动续期，定时巡检
// 比被调用接口造成问题要稳定").
//
// The request paths (chat/audio) never renew and never log in: they read the
// stored session and pass upstream errors through. This file is the ONLY
// thing that keeps sessions fresh:
//
//   * one round immediately at startup, then one round per tick
//     (MIMO2API_RENEW_INTERVAL, default 1h);
//   * every enabled account with a passToken, walked serially in ID order —
//     no concurrent upstream renewal burst;
//   * per account: Renew first; a dead credential (renewalFailureClass)
//     runs one password Login on the same round; anything else is a logged
//     failure;
//   * no retry storm, no backoff: a failure simply waits for the next tick
//     (same ruling that removed cooldown/backoff from the pool).

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
)

// renewClock is the job's clock; production uses the real one, tests a fake
// they can Advance across a tick.
type renewClock interface {
	Now() time.Time
}

// renewJobTimeout bounds one account's renew-or-login attempt so one wedged
// upstream call cannot stall the whole serial round.
const renewJobTimeout = 90 * time.Second

// renewTick waits one interval, honoring ctx cancellation.
func renewTick(ctx context.Context, interval time.Duration) error {
	t := time.NewTimer(interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunRenewJob runs renew rounds until ctx is cancelled: one immediately
// (startup freshness — "启动时立即先跑一轮"), then one per interval. Accounts
// are walked serially in ID order.
func (s *Server) RunRenewJob(ctx context.Context, interval time.Duration) {
	s.renewRound(ctx) // startup round: no wait
	for {
		if err := renewTick(ctx, interval); err != nil {
			return
		}
		s.renewRound(ctx)
	}
}

// SetRenewClock installs the job's clock (tests pass a fake; the real clock
// is the default).
func (s *Server) SetRenewClock(c renewClock) {
	s.renewClockMu.Lock()
	s.renewClockVal = c
	s.renewClockMu.Unlock()
}

// renewNow returns the job's clock's now, or time.Now when unset.
func (s *Server) renewNow() time.Time {
	s.renewClockMu.Lock()
	c := s.renewClockVal
	s.renewClockMu.Unlock()
	if c == nil {
		return time.Now()
	}
	return c.Now()
}

// renewRound walks every enabled account with a passToken, serially in ID
// order, and renews or re-logs-in each. One log line per account.
func (s *Server) renewRound(ctx context.Context) {
	accts := s.store.Accounts()
	sort.Slice(accts, func(i, j int) bool { return accts[i].ID < accts[j].ID })
	for _, acct := range accts {
		if ctx.Err() != nil {
			return
		}
		if !acct.Enabled || acct.UserID == "" || acct.PassToken == "" {
			continue
		}
		// Re-read inside the round: an earlier iteration's login (or a
		// concurrent admin submit) may have rotated this account's session.
		fresh, ok := s.store.Get(acct.ID)
		if ok {
			acct = fresh
		}
		s.renewOne(ctx, acct)
	}
}

// renewOne services one account: Renew, and — only when the credential is
// dead — one password Login. The loginGate keeps even this path
// single-flighted per account, so the job and any other writer can never run
// two logins for the same account at once.
func (s *Server) renewOne(ctx context.Context, acct accounts.Account) {
	actx, cancel := context.WithTimeout(ctx, renewJobTimeout)
	defer cancel()

	client := s.clientFor(acct.Egress).WithDevice(acct.Device)
	sess, err := client.Renew(actx, acct.UserID, acct.PassToken)
	if err == nil {
		applySession(&acct, sess)
		if uerr := s.store.Update(acct); uerr != nil {
			s.logger.Printf("[renew-job] %s failed: persist renewed session: %v", acct.ID, uerr)
			return
		}
		s.pool.ReportSuccess(acct.ID)
		s.logger.Printf("[renew-job] %s renewed", acct.ID)
		return
	}
	dead, reason := renewalFailureClass(err)
	if !dead {
		// Transient: log it, the next tick retries. No backoff by ruling —
		// "不重试风暴，不退避，一小时后自然重试".
		s.logger.Printf("[renew-job] %s failed: %v", acct.ID, err)
		return
	}
	if acct.Phone == "" || acct.Password == "" {
		s.logger.Printf("[renew-job] %s failed: credential is dead (%s) and no login credentials are stored", acct.ID, reason)
		return
	}
	// Dead credential: one password login on the same round, under the
	// per-account login gate (two concurrent password logins from one IP is
	// exactly the pattern risk control reacts to).
	won, finish, werr := s.logins.acquire(actx, acct.ID)
	if werr != nil {
		s.logger.Printf("[renew-job] %s failed: dead credential (%s); login gate: %v", acct.ID, reason, werr)
		return
	}
	if !won {
		// Another flight (an admin OTP submit, normally) resolved the
		// account while this round waited: its outcome is on the store.
		fresh, ok := s.store.Get(acct.ID)
		if ok && fresh.Session != nil && fresh.Session.Cookie != "" {
			s.pool.ReportSuccess(acct.ID)
			s.logger.Printf("[renew-job] %s login-ok (via a concurrent resolution)", acct.ID)
		} else {
			s.logger.Printf("[renew-job] %s failed: credential is dead (%s) and the concurrent resolution left no session", acct.ID, reason)
		}
		return
	}
	defer finish()
	sess, lerr := client.Login(actx, acct.Phone, acct.Password)
	if lerr != nil {
		// A human gate (new-device OTP, captcha) is not a failure to retry:
		// keep the device identity the attempt harvested, register the
		// challenge for the operator, disable the account with the reason.
		if pending, ok := errOTPPendingUnwrap(lerr); ok {
			s.otp.add(&otpPending{AccountID: acct.ID, Challenge: pending.Challenge, CreatedAt: s.renewNow()})
			if persistDeviceFromError(&acct, lerr) {
				if uerr := s.store.Update(acct); uerr != nil {
					s.logger.Printf("[renew-job] %s failed: persist device: %v", acct.ID, uerr)
				}
			}
			s.pool.ReportDisabled(acct.ID, "credential revoked; new-device OTP verification pending (operator: /v1/admin/otp/submit)", false)
			s.logger.Printf("[renew-job] %s failed: new-device OTP pending — operator must submit the code", acct.ID)
			return
		}
		if persistDeviceFromError(&acct, lerr) {
			if uerr := s.store.Update(acct); uerr != nil {
				s.logger.Printf("[renew-job] %s failed: persist device: %v", acct.ID, uerr)
			}
		}
		s.pool.ReportDisabled(acct.ID, "credential revoked; re-login needs a human gate: "+lerr.Error(), false)
		s.logger.Printf("[renew-job] %s failed: login: %v", acct.ID, lerr)
		return
	}
	// The login rotated the passToken (a full login does, a renewal never
	// does), so applySession must run — dropping it would leave the dead
	// credential on file and the account would die again on the next round.
	applySession(&acct, sess)
	if uerr := s.store.Update(acct); uerr != nil {
		s.logger.Printf("[renew-job] %s failed: persist recovered session: %v", acct.ID, uerr)
		return
	}
	s.pool.Enable(acct.ID)
	s.pool.ReportSuccess(acct.ID)
	s.logger.Printf("[renew-job] %s login-ok", acct.ID)
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
