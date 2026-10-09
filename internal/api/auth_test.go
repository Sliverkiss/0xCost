package api_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

func oneAccount() []testserver.TestAccount {
	return []testserver.TestAccount{{
		ID:     "acc1",
		PH:     testutil.TestPH,
		Cookie: testutil.TestCookie,
	}}
}

// AUTHMID-001: missing Authorization header → 401 invalid_api_key (SDD §5.1).
func TestAuthMissing(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	msg, typ, code := testutil.ParseError(t, body)
	if typ != "invalid_request_error" || code != "invalid_api_key" || msg == "" {
		t.Errorf("error = %q %q %q", msg, typ, code)
	}
}

// AUTHMID-002: wrong key → 401 same shape.
func TestAuthWrongKey(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// AUTHMID-003: valid key → request proceeds.
func TestAuthValidKey(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "k1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// AUTHMID-004: multiple keys both accepted.
func TestAuthMultipleKeys(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1", "k2"}})
	for _, key := range []string{"k1", "k2"} {
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), key)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("key %q: status = %d, want 200", key, resp.StatusCode)
		}
	}
}

// AUTHMID-005: no keys configured → open auth.
func TestAuthOpenMode(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (open auth)", resp.StatusCode)
	}
}

// AUTHMID-006: every /v1/* route is protected when keys are set.
func TestAuthAllV1Protected(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	for _, p := range []string{"/v1/models", "/v1/models/mimo-v2.6-pro"} {
		resp, err := ts.Get(p, "")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401", p, resp.StatusCode)
		}
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL()+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /v1/chat/completions: status = %d, want 401", resp.StatusCode)
	}
}

// AUTHMID-007: /healthz is always open.
func TestAuthHealthzOpen(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	resp, err := ts.Get("/healthz", "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// AUTHMID-008: non-Bearer scheme → 401 same shape.
func TestAuthNonBearerScheme(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k1"}})
	req, err := http.NewRequest(http.MethodGet, ts.URL()+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic xyz")
	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_, typ, code := testutil.ParseError(t, body)
	if typ != "invalid_request_error" || code != "invalid_api_key" {
		t.Errorf("error = %q %q", typ, code)
	}
}
