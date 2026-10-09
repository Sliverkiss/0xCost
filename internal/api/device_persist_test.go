package api

// Device identity persistence (docs/login-chain-reverse.md §5 item 1).
//
// The pool now stores a per-account device identity and replays it on every
// login, so the passport stops seeing new hardware every time. This test pins
// the wiring end to end: the stored device reaches serviceLoginAuth2, and the
// identity the login harvests is written back to the account file.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/session"
	"github.com/yg/mimo2api/internal/testutil"
)

type deviceStack struct {
	ts       *httptest.Server
	store    *accounts.Store
	path     string
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
}

// newDeviceStack wires an account that has a stored device identity but no
// cached session, so the first request must take the password-login fallback
// (the path the device plumbing has to cover).
func newDeviceStack(t *testing.T) *deviceStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	// Default Auth2 fixture = login OK; the STS hop mints serviceToken
	// "st-mock-1", which is what the upstream mock must recognise.
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, "serviceToken=st-mock-1")

	acct := accounts.Account{
		ID:       "acc-device-1",
		Enabled:  true,
		Phone:    "13800000001",
		Password: "pw",
		Device:   map[string]string{"deviceId": "dev-stored-1", "uLocale": "zh_CN"},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	f, _ := json.MarshalIndent(accounts.File{Accounts: []accounts.Account{acct}}, "", "  ")
	if err := os.WriteFile(path, f, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New([]accounts.Account{acct}, pool.Options{}, nil, nil)
	client := mimo.New(mimo.Options{
		Base:          up.URL(),
		PassportHosts: []string{passport.Host()},
	})
	apiSrv := New(Options{
		APIKeys:  []string{"sk-admin-test"},
		Pool:     p,
		Accounts: store,
		Sessions: session.New(session.Options{}),
		Client:   client,
	})
	hs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(hs.Close)
	return &deviceStack{ts: hs, store: store, path: path, passport: passport, upstream: up}
}

func (s *deviceStack) do(method, path, apiKey, body string) (*http.Response, string) {
	req, _ := http.NewRequest(method, s.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := s.ts.Client().Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// DEVPL-001: the stored device identity is replayed on the password login the
// pipeline performs, and the identity the login harvests is persisted (to the
// store and to disk) so the NEXT login is no longer a new device.
func TestLoginPresentsAndPersistsDeviceIdentity(t *testing.T) {
	s := newDeviceStack(t)
	s.upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	resp, body := s.do(http.MethodPost, "/v1/chat/completions", "sk-admin-test",
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chat = %v %q, want 200 (the login must have repaired the missing session)", resp, body)
	}

	if len(s.passport.Auth2Headers) != 1 {
		t.Fatalf("auth2 calls = %d, want exactly 1 password login", len(s.passport.Auth2Headers))
	}
	ck := s.passport.Auth2Headers[0].Get("Cookie")
	if !strings.Contains(ck, "deviceId=dev-stored-1") {
		t.Errorf("Auth2 Cookie = %q, want the stored device replayed", ck)
	}

	acct, ok := s.store.Get("acc-device-1")
	if !ok {
		t.Fatal("account vanished from the store")
	}
	if acct.Session == nil || acct.Session.Cookie == "" {
		t.Fatal("login session was not persisted")
	}
	// The passport issues its own deviceId; the server's value wins, exactly
	// as a browser lets Set-Cookie overwrite what it sent. pass_ua/uLocale are
	// harvested alongside it.
	if got := acct.Device["deviceId"]; got != "dev-mock-1" {
		t.Errorf("stored deviceId = %q, want the server-issued dev-mock-1", got)
	}
	if acct.Device["uLocale"] != "zh_CN" {
		t.Errorf("uLocale = %q, want zh_CN", acct.Device["uLocale"])
	}
	if acct.Device["pass_ua"] != "3.9" {
		t.Errorf("pass_ua = %q, want 3.9", acct.Device["pass_ua"])
	}

	// Durable: the device identity must survive a restart, which means it is
	// in the account file, not only in memory.
	onDisk, err := accounts.Load(s.path)
	if err != nil {
		t.Fatal(err)
	}
	dacct, ok := onDisk.Get("acc-device-1")
	if !ok {
		t.Fatal("account missing after reload")
	}
	if dacct.Device["deviceId"] != "dev-mock-1" {
		t.Errorf("on-disk deviceId = %q, want dev-mock-1", dacct.Device["deviceId"])
	}
	raw, _ := os.ReadFile(s.path)
	if !strings.Contains(string(raw), "\"device\"") {
		t.Errorf("account file carries no device block: %s", string(raw))
	}
}
