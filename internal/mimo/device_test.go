package mimo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testutil"
)

// Device identity (docs/login-chain-reverse.md §5 item 1, device.go).
//
// Why this matters: without a stable device identity every login looks like
// new hardware, so the passport demands an SMS code on every attempt and the
// "trusted device" state can never attach. These tests pin the three moves:
// present the stored identity, harvest what the chain plants, and never wipe a
// stored identity.

// DEV-001: a login presents the stored device identity on the Auth2 POST.
func TestDeviceIdentityPresentedOnAuth2(t *testing.T) {
	client, passport := loginEnv(t)
	dev := map[string]string{
		"deviceId": "dev-stored-1",
		"pass_ua":  "3.9",
		"uLocale":  "zh_CN",
		"cUserId":  "cu-stored-1",
	}
	if _, err := client.WithDevice(dev).Login(context.Background(), "13800000001", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(passport.Auth2Headers) != 1 {
		t.Fatalf("auth2 calls = %d, want 1", len(passport.Auth2Headers))
	}
	ck := passport.Auth2Headers[0].Get("Cookie")
	if ck == "" {
		t.Fatal("Auth2 carried no Cookie header: the stored device identity was dropped")
	}
	for name, want := range dev {
		if !strings.Contains(ck, name+"="+want) {
			t.Errorf("Auth2 Cookie %q missing %s=%s", ck, name, want)
		}
	}
}

// DEV-002: with nothing stored, no device cookie is invented (the pre-fix
// behaviour stays available for tests and for accounts that never logged in).
func TestDeviceIdentityAbsentWithoutStoredDevice(t *testing.T) {
	client, passport := loginEnv(t)
	if _, err := client.Login(context.Background(), "13800000001", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if ck := passport.Auth2Headers[0].Get("Cookie"); strings.Contains(ck, "deviceId=") {
		t.Errorf("Auth2 Cookie = %q, want no device cookie", ck)
	}
}

// WithDevice must never mutate the receiver: the pooled client is shared.
func TestWithDeviceDoesNotMutateReceiver(t *testing.T) {
	client, passport := loginEnv(t)
	scoped := client.WithDevice(map[string]string{"deviceId": "dev-stored-1"})
	if scoped == client {
		t.Fatal("WithDevice returned the receiver; the pooled client would leak device identity across accounts")
	}
	if _, err := scoped.Login(context.Background(), "13800000001", "pw"); err != nil {
		t.Fatalf("scoped login: %v", err)
	}
	if ck := passport.Auth2Headers[0].Get("Cookie"); !strings.Contains(ck, "deviceId=dev-stored-1") {
		t.Fatalf("scoped Auth2 Cookie = %q", ck)
	}
	// The base client still presents nothing.
	if _, err := client.Login(context.Background(), "13800000001", "pw"); err != nil {
		t.Fatalf("base login: %v", err)
	}
	if ck := passport.Auth2Headers[1].Get("Cookie"); strings.Contains(ck, "deviceId=") {
		t.Errorf("base client leaked device identity: %q", ck)
	}
}

// DEV-003: the identity the passport plants is harvested into the session so
// the caller can persist it (before this, it was thrown away and every later
// login was a new device).
func TestDeviceIdentityHarvestedFromLogin(t *testing.T) {
	client, _ := loginEnv(t)
	sess, err := client.Login(context.Background(), "13800000001", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if ses := sess.Device; ses["deviceId"] != "dev-mock-1" || ses["pass_ua"] != "3.9" || ses["uLocale"] != "zh_CN" {
		t.Fatalf("harvested device = %+v, want the mock-planted deviceId/pass_ua/uLocale", sess.Device)
	}
	// Session cookies must NOT leak into the device blob.
	for _, k := range []string{"xiaomichatbot_serviceToken", "serviceToken", "xiaomichatbot_ph", "passToken"} {
		if _, ok := sess.Device[k]; ok {
			t.Errorf("device blob contains session cookie %q: %+v", k, sess.Device)
		}
	}
}

// DEV-004: the whole verification chain (identity/*) runs on one coherent
// device identity — that is where the trust is established, so the Auth2 POST,
// the identity calls and the OTP challenge must all present the same device.
//
// Precedence rule pinned here: a deviceId the passport issues in Set-Cookie
// WINS over the replayed one, exactly as a browser lets the server's cookie
// overwrite the one it sent. What the client owes the next login is therefore
// the server's latest value (harvested in DEV-003), not the one it replayed.
func TestDeviceIdentityReplayedOnVerificationChain(t *testing.T) {
	client, passport := otpEnv(t)
	_, err := client.WithDevice(map[string]string{"deviceId": "dev-stored-1"}).Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending (verification demanded)", err)
	}
	// The stored device was presented on Auth2 (checked in DEV-001); here the
	// question is whether the chain keeps presenting a device at all.
	if ck := passport.Auth2Headers[0].Get("Cookie"); !strings.Contains(ck, "deviceId=dev-stored-1") {
		t.Fatalf("Auth2 Cookie = %q, want the stored device replayed", ck)
	}
	call := findIdentityCall(passport, "/identity/list", "GET")
	if call == nil {
		t.Fatal("identity/list not called")
	}
	ck := call.Headers.Get("Cookie")
	if !strings.Contains(ck, "deviceId=") {
		t.Fatalf("identity/list Cookie = %q, want a device identity present", ck)
	}
	// Coherent chain: the challenge carries the same deviceId the identity hop
	// used, so the submit step cannot drift onto another device.
	used := deviceOfCookieHeader(ck)
	if used == "" {
		t.Fatalf("no deviceId parsed out of %q", ck)
	}
	if got := pending.Challenge.jar["deviceId"]; got != used {
		t.Errorf("challenge jar deviceId = %q, identity hop used %q — the verification would run on two different devices", got, used)
	}
}

// deviceOfCookieHeader returns the deviceId inside a Cookie header (test helper).
func deviceOfCookieHeader(ck string) string {
	for _, part := range strings.Split(ck, ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "deviceId="); ok {
			return v
		}
	}
	return ""
}

// DEV-005: renewal presents userId + passToken + the device identity.
func TestDeviceIdentityPresentedOnRenewal(t *testing.T) {
	client, passport := renewEnv(t)
	if _, err := client.WithDevice(map[string]string{"deviceId": "dev-stored-1"}).Renew(context.Background(), "123456", "rt-stored-1"); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(passport.ServiceLoginCalls) != 1 {
		t.Fatalf("serviceLogin calls = %d, want 1", len(passport.ServiceLoginCalls))
	}
	ck := passport.ServiceLoginCalls[0]
	for _, want := range []string{"userId=123456", "passToken=rt-stored-1", "deviceId=dev-stored-1"} {
		if !strings.Contains(ck, want) {
			t.Errorf("renewal Cookie %q missing %q", ck, want)
		}
	}
}

// DEV-006: a rotated passToken the chain plants wins over the stored one —
// writing the stored value back over a fresh credential would drop the
// refresh silently.
func TestRenewPrefersRotatedPassToken(t *testing.T) {
	client, passport := renewEnv(t)
	passport.StsPassToken = "rt-rotated-2"
	sess, err := client.Renew(context.Background(), "123456", "rt-stored-1")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if sess.PassToken != "rt-rotated-2" {
		t.Errorf("passToken = %q, want the rotated rt-rotated-2", sess.PassToken)
	}
}

// DEV-006b: when the chain plants no passToken, the stored one is kept.
func TestRenewKeepsStoredPassToken(t *testing.T) {
	client, passport := renewEnv(t)
	passport.OmitPassToken = true
	sess, err := client.Renew(context.Background(), "123456", "rt-stored-1")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if sess.PassToken != "rt-stored-1" {
		t.Errorf("passToken = %q, want the stored rt-stored-1", sess.PassToken)
	}
}

// DEV-007: merge semantics — fresh values win, values the server stayed silent
// about survive, and an empty pair stays nil (JSON-clean accounts).
func TestMergeDevice(t *testing.T) {
	stored := map[string]string{"deviceId": "dev-old", "uLocale": "zh_CN"}
	fresh := map[string]string{"deviceId": "dev-new"}
	got := MergeDevice(stored, fresh)
	if got["deviceId"] != "dev-new" {
		t.Errorf("deviceId = %q, want dev-new (fresh wins)", got["deviceId"])
	}
	if got["uLocale"] != "zh_CN" {
		t.Errorf("uLocale = %q, want zh_CN (untouched value kept)", got["uLocale"])
	}
	if MergeDevice(nil, nil) != nil {
		t.Error("MergeDevice(nil, nil) != nil — accounts would persist an empty object")
	}
	if MergeDevice(nil, map[string]string{"deviceId": "x"})["deviceId"] != "x" {
		t.Error("MergeDevice dropped a fresh identity with nothing stored")
	}
	// The stored map must not be mutated by the caller's later writes.
	merged := MergeDevice(stored, fresh)
	merged["deviceId"] = "mutated"
	if stored["deviceId"] != "dev-old" {
		t.Error("MergeDevice aliased the stored map")
	}
}

// The device cookies are a passport-domain concept: they must not be pasted
// into the upstream session cookie (that would be a fingerprint tell on a
// non-xiaomi host, and would grow a header the browser never sends there).
func TestDeviceCannotLeakIntoSessionCookie(t *testing.T) {
	client, _ := loginEnv(t)
	sess, err := client.WithDevice(map[string]string{"deviceId": "dev-stored-1"}).Login(context.Background(), "13800000001", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if strings.Contains(sess.Cookie, "deviceId=") {
		t.Errorf("upstream session cookie leaked the device identity: %q", sess.Cookie)
	}
	_ = testutil.FixtureXSSILoginOK
}
