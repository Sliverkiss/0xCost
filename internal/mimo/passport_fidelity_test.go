package mimo

// Passport-surface fidelity (docs/login-chain-reverse.md §5.1, SDD §2.4).
//
// The chain used to post serviceLoginAuth2 with three headers and no cookie
// context. A real client navigates to the login page first (which is where the
// device anchor is issued) and then submits the form as Chrome would, with the
// client hints and sec-fetch triplet. These cases pin both halves.

import (
	"context"
	"strings"
	"testing"
)

// DEV-008: the login chain loads the login page before it posts the form, and
// the device cookies that page issues reach serviceLoginAuth2.
func TestLoginPrimesPageBeforeAuth2(t *testing.T) {
	client, passport := loginEnv(t)

	if _, err := client.Login(context.Background(), "13800000000", "pw"); err != nil {
		t.Fatal(err)
	}

	if len(passport.LoginPageCalls) != 1 {
		t.Fatalf("login page navigations = %d, want 1", len(passport.LoginPageCalls))
	}
	if len(passport.Auth2Headers) != 1 {
		t.Fatalf("auth2 calls = %d, want 1", len(passport.Auth2Headers))
	}
	ck := passport.Auth2Headers[0].Get("Cookie")
	for _, want := range []string{"deviceId=wb_mock-device-1", "pass_ua=web", "uLocale=zh_CN", "JSESSIONID=mock-jsess-1"} {
		if !strings.Contains(ck, want) {
			t.Errorf("auth2 Cookie %q missing the page-issued %q", ck, want)
		}
	}
	// The page navigation itself must look like Chrome loading a document.
	nav := passport.LoginPageCalls[0]
	if got := nav.Get("sec-fetch-mode"); got != "navigate" {
		t.Errorf("login page sec-fetch-mode = %q, want navigate", got)
	}
	if got := nav.Get("Upgrade-Insecure-Requests"); got != "1" {
		t.Errorf("login page Upgrade-Insecure-Requests = %q, want 1", got)
	}
	if !strings.Contains(nav.Get("Accept"), "text/html") {
		t.Errorf("login page Accept = %q, want an HTML accept", nav.Get("Accept"))
	}
}

// DEV-009: the stored device identity wins nothing, but it must survive on top
// of the page-issued cookies — a login presents both, exactly like a browser
// with a cookie jar that already knows the device.
func TestAuth2CarriesStoredAndPrimedDevice(t *testing.T) {
	client, passport := loginEnv(t)
	stored := map[string]string{"deviceId": "wb_stored-1", "uLocale": "zh_CN"}

	if _, err := client.WithDevice(stored).Login(context.Background(), "13800000000", "pw"); err != nil {
		t.Fatal(err)
	}
	ck := passport.Auth2Headers[0].Get("Cookie")
	// The stored deviceId is the sticky one; the page keeps it (it only mints an
	// anchor for a client that has none) and adds its own session cookies.
	for _, want := range []string{"deviceId=wb_stored-1", "pass_ua=web", "uLocale=zh_CN", "JSESSIONID=mock-jsess-1"} {
		if !strings.Contains(ck, want) {
			t.Errorf("auth2 Cookie %q missing %q", ck, want)
		}
	}
}

// DEV-010: the form POST carries Chrome's header set for this form — HTML
// Accept, client hints, the navigation triplet, Origin and Referer naming the
// page it was submitted from.
func TestAuth2SendsBrowserFormHeaders(t *testing.T) {
	client, passport := loginEnv(t)

	if _, err := client.Login(context.Background(), "13800000000", "pw"); err != nil {
		t.Fatal(err)
	}
	h := passport.Auth2Headers[0]
	checks := map[string]string{
		"Content-Type":              "application/x-www-form-urlencoded",
		"sec-fetch-mode":            "navigate",
		"sec-fetch-site":            "same-origin",
		"sec-fetch-dest":            "document",
		"Upgrade-Insecure-Requests": "1",
	}
	for k, want := range checks {
		if got := h.Get(k); got != want {
			t.Errorf("auth2 header %s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"Origin", "Referer", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "Accept-Language", "User-Agent"} {
		if h.Get(k) == "" {
			t.Errorf("auth2 header %s missing", k)
		}
	}
	if !strings.Contains(h.Get("Accept"), "text/html") {
		t.Errorf("auth2 Accept = %q, want the HTML form accept", h.Get("Accept"))
	}
	if !strings.HasPrefix(h.Get("Origin"), passport.URL()) {
		t.Errorf("auth2 Origin = %q, want the passport origin", h.Get("Origin"))
	}
}

// DEV-011: the identity/* hops are same-origin XHRs from the account SPA: JSON
// Accept, X-Requested-With, CORS mode, Origin — not a document navigation.
func TestIdentityHopsSendXHRHeaders(t *testing.T) {
	client, passport := otpEnv(t)

	if _, err := client.Login(context.Background(), "13800000000", "pw"); err == nil {
		t.Fatal("want the OTP demand, got a completed login")
	}
	calls := passport.IdentityCalls
	if len(calls) == 0 {
		t.Fatal("no identity calls recorded")
	}
	for _, c := range calls {
		if got := c.Headers.Get("X-Requested-With"); got != "XMLHttpRequest" {
			t.Errorf("%s: X-Requested-With = %q, want XMLHttpRequest", c.Path, got)
		}
		if got := c.Headers.Get("sec-fetch-mode"); got != "cors" {
			t.Errorf("%s: sec-fetch-mode = %q, want cors", c.Path, got)
		}
		if got := c.Headers.Get("sec-ch-ua"); got == "" {
			t.Errorf("%s: sec-ch-ua missing", c.Path)
		}
		if !strings.Contains(c.Headers.Get("Accept"), "application/json") {
			t.Errorf("%s: Accept = %q, want a JSON accept", c.Path, c.Headers.Get("Accept"))
		}
	}
}
