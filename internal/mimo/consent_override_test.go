package mimo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yg/mimo2api/internal/testutil"
)

// consentOverrideEnv boots a login-context probe + MockPassport whose
// serviceLoginAuth2 is scripted with the live code=0 OTP demand. disableAgree
// builds the client with DisableCrossRegionAgree set — the exact flag the
// operator override (LoginWithConsent) has to bypass.
func consentOverrideEnv(t *testing.T, disableAgree bool) (*Client, *testutil.MockPassport) {
	t.Helper()
	passport := testutil.NewMockPassport("http://upstream.invalid")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/user/mi/get" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"code":401,"loginUrl":%q}`, passport.LoginURLFor())
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() {
		up.Close()
		passport.Close()
	})
	client := New(Options{
		Base:                    up.URL,
		PassportHosts:           []string{passport.Host()},
		DisableCrossRegionAgree: disableAgree,
	})
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	return client, passport
}

// CONSENT-OVERRIDE-001/002: on the same consent-chain setup with the client
// flag disabled, (a) Login refuses with the sentinel ErrCrossRegionConsent and
// issues ZERO consent POSTs, while (b) LoginWithConsent performs the consent
// POST with userId/status=1/needClose=false and proceeds down the identity
// chain to the new-device OTP demand.
func TestConsentOverrideRegardlessOfFlag(t *testing.T) {
	client, passport := consentOverrideEnv(t, true)
	passport.SetConsentChain(true)

	// (a) Login honours the flag: refuse, never POST the agreement.
	_, err := client.Login(context.Background(), "13800000001", "pw")
	if !errors.Is(err, ErrCrossRegionConsent) {
		t.Fatalf("Login err = %v, want ErrCrossRegionConsent", err)
	}
	var action *ErrAccountAction
	if !errors.As(err, &action) {
		t.Fatalf("Login err = %v, want the account-action wrapper around the sentinel", err)
	}
	if len(passport.ConsentCalls) != 0 {
		t.Fatalf("Login POSTed consent %d times with the flag disabled, want 0", len(passport.ConsentCalls))
	}

	// (b) LoginWithConsent overrides the flag on the same setup.
	_, err = client.LoginWithConsent(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("LoginWithConsent err = %v, want ErrOTPPending (identity steps reached)", err)
	}
	if len(passport.ConsentCalls) != 1 {
		t.Fatalf("consent calls = %d, want 1", len(passport.ConsentCalls))
	}
	c := passport.ConsentCalls[0]
	if c["userId"] != "123456" || c["status"] != "1" || c["needClose"] != "false" {
		t.Errorf("consent form = %v, want userId=123456/status=1/needClose=false", c)
	}
	if pending.Challenge == nil || pending.Challenge.Notify == "" {
		t.Errorf("challenge = %+v, want a started OTP challenge", pending.Challenge)
	}
	t.Logf("recorded consent POST body: userId=%q status=%q needClose=%q (xrw=%q referer=%q)",
		c["userId"], c["status"], c["needClose"], c["_xrw"], c["_referer"])
}
