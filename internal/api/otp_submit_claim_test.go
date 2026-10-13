package api

// OTP submit claim (BUGS-FOUND #1, sweep 2026-10).
//
// handleOTPSubmit checked that a challenge exists but never claimed it, so
// two concurrent submits both ran SubmitOTP upstream and both ran the
// applySession → store.Update → pool.Enable side effects. The claim removes
// the challenge before the upstream verification; a wrong code (70002-class)
// restores it so the operator's retry window survives.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testutil"
)

// submitPOST posts one OTP submit and returns the response status. Response
// bodies are not asserted here — the interesting evidence is upstream, in
// the passport's recorded identity calls.
func (s *otpAdminStack) submitPOST(hs *httptest.Server, id, code string) int {
	body := fmt.Sprintf(`{"id":%q,"code":%q}`, id, code)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/admin/otp/submit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-admin-test")
	resp, err := hs.Client().Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// countVerifySubmits counts upstream OTP code verifications: each SubmitOTP
// performs exactly one POST /identity/auth/verify{Phone,Email}. Race-free
// via the passport mutex (a recovery goroutine may append concurrently).
func countVerifySubmits(p *testutil.MockPassport) int {
	n := 0
	for _, c := range p.IdentityCallsSnapshot() {
		if (c.Path == "/identity/auth/verifyPhone" || c.Path == "/identity/auth/verifyEmail") && c.Method == http.MethodPost {
			n++
		}
	}
	return n
}

// ADOTP-005 (BUGS-FOUND #1): two concurrent submits of the correct code run
// exactly ONE upstream verification and one applySession/enable chain. The
// loser of the claim finds the challenge gone — 404 — instead of verifying
// a second time.
func TestOTPSubmitClaimDeduplicatesConcurrentSubmits(t *testing.T) {
	s := newOTPAdminStack(t)
	hs := httptest.NewServer(s.apiSrv.Handler())
	t.Cleanup(hs.Close)

	// Seed the challenge: the renew job's login on the dead passToken.
	ch := s.seedOTPChallenge(t)
	before := countVerifySubmits(s.passport)

	var wg sync.WaitGroup
	const submits = 2
	for i := 0; i < submits; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.submitPOST(hs, ch.ID, "123456")
		}()
	}
	wg.Wait()

	if got := countVerifySubmits(s.passport) - before; got != 1 {
		t.Fatalf("concurrent submits of the correct code caused %d upstream verifications, want 1", got)
	}
	if got := s.pendingList("sk-admin-test"); len(got) != 0 {
		t.Errorf("pending after double submit = %d, want 0", len(got))
	}
	if s.pool.StateOf("acc-otp-1") != pool.StateAvailable {
		t.Errorf("pool state = %s, want available", s.pool.StateOf("acc-otp-1"))
	}
}

// ADOTP-006 (BUGS-FOUND #1): a wrong code restores the challenge — the
// operator's retry window survives — and a subsequent correct submit still
// completes the login.
func TestOTPSubmitClaimRestoresOnWrongCode(t *testing.T) {
	s := newOTPAdminStack(t)
	hs := httptest.NewServer(s.apiSrv.Handler())
	t.Cleanup(hs.Close)

	ch := s.seedOTPChallenge(t)

	// Wrong code: rejected, and the challenge must still be there for a retry.
	s.passport.OTPWrongCode = true
	if got := s.submitPOST(hs, ch.ID, "000000"); got != http.StatusBadRequest {
		t.Fatalf("wrong code = %d, want 400", got)
	}
	if got := s.pendingList("sk-admin-test"); len(got) != 1 || got[0].ID != ch.ID {
		t.Fatalf("challenge not restored after a wrong code: %+v", got)
	}

	// Correct code within the same window: completes.
	s.passport.OTPWrongCode = false
	if got := s.submitPOST(hs, ch.ID, "123456"); got != http.StatusOK {
		t.Fatalf("correct code after a retry = %d, want 200", got)
	}
	if got := s.pendingList("sk-admin-test"); len(got) != 0 {
		t.Errorf("pending after successful retry = %d, want 0", len(got))
	}
	acct, ok := s.store.Get("acc-otp-1")
	if !ok || acct.Session == nil || acct.Session.Cookie == "" {
		t.Fatalf("session not persisted after the retry: %+v", acct)
	}
}
