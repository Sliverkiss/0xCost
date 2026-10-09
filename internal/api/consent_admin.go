package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/pool"
)

// handleConsentAgree: POST /v1/admin/consent/agree {"account_id":"<id>"} —
// the explicit operator approval of the cross-region personal-data-transfer
// agreement.
//
// With MIMO2API_CROSS_REGION_AGREE=false the client refuses to agree and the
// demand surfaces as ErrCrossRegionConsent, leaving the account disabled with
// no API path forward (docs/login-chain-reverse.md §5 open item 3). This
// endpoint closes that dead-end: it runs the same login with a per-call
// consent override (client.LoginWithConsent), which performs the
// /pass2/userCrossAgreement/update POST regardless of the client flag, then
// either finishes the login or hands the new-device OTP demand to the
// existing /v1/admin/otp/* flow — exactly like the other login paths.
func (s *Server) handleConsentAgree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.AccountID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "",
			`body must be {"account_id": "<id>"}`, 0)
		return
	}
	id := strings.TrimSpace(body.AccountID)

	acct, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "account_not_found",
			"unknown account id: "+id, 0)
		return
	}

	// Already usable: report it and never re-run the consent handshake (a
	// repeat POST on a consumed chain answers 21301 认证失败).
	//
	// A DISABLED account is the exception: a stale cookie can still be on file
	// while the account sits out of rotation (a renewal that failed, a dead
	// session, an earlier consent refusal), and that is exactly when an
	// operator reaches for this endpoint. Refusing then would leave the same
	// dead-end this route exists to close.
	if acct.Session != nil && acct.Session.Cookie != "" && s.pool.StateOf(id) != pool.StateDisabled {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"account": id,
			"session": "existing",
		})
		return
	}

	sess, err := s.clientFor(acct.Egress).WithDevice(acct.Device).LoginWithConsent(r.Context(), acct.Phone, acct.Password)
	if err != nil {
		// New-device OTP demand: register the challenge for the existing
		// submit endpoint and disable the account until the code is entered
		// (SDD §2.2 amendment) — the operator finishes via
		// POST /v1/admin/otp/submit.
		if pending, ok := errOTPPendingUnwrap(err); ok {
			s.otp.add(&otpPending{AccountID: id, Challenge: pending.Challenge, CreatedAt: time.Now()})
			s.pool.ReportDisabled(id, "cross-region consent given; new-device OTP verification pending", false)
			ch := pending.Challenge
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "otp_pending",
				"account": id,
				"otp": pendingOTP{
					ID:        ch.ID,
					Method:    ch.Method,
					Notify:    ch.Notify,
					AccountID: id,
					ExpiresAt: ch.ExpiresAt.Unix(),
				},
			})
			return
		}
		// Anything else (transport, refusal, protocol drift) is an upstream
		// failure in the project's error-envelope convention.
		writeError(w, http.StatusBadGateway, "api_error", "upstream_failure",
			"consent login failed: "+err.Error(), 0)
		return
	}

	// Persist the session; keep the stored identity when the flow could not
	// harvest a fresh one (the landing chain plants passToken only on some
	// passport variants — same rule as creds()/handleOTPSubmit).
	if sess.UserID != "" {
		acct.UserID = sess.UserID
	}
	if sess.PassToken != "" {
		acct.PassToken = sess.PassToken
	}
	acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
	if err := s.store.Update(acct); err != nil {
		s.logger.Printf("accounts: persist consent session for %s: %v", id, err)
	}
	s.pool.Enable(id)
	s.logger.Printf("consent: account %s logged in via operator-approved cross-region consent", id)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"account": id,
		"session": "renewed_by_consent",
	})
}
