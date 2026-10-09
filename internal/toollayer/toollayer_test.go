package toollayer

// Unit cases for the pieces the API-level tests cannot see: which credential is
// presented, what is deliberately not forwarded, and what a dead middleware
// looks like to the caller.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TOOL-UNIT-001: the caller's own credential wins; the configured one is only a
// fallback for deployments that run without client keys. The distinction is what
// keeps session state scoped per caller.
func TestCredentialSelection(t *testing.T) {
	withKey := New("http://example.invalid/v1", "fallback", nil, false)
	cases := []struct {
		name    string
		header  http.Header
		wantHas string
	}{
		{"caller wins", http.Header{"Authorization": {"Bearer caller"}}, "Bearer caller"},
		{"fallback used", http.Header{}, "Bearer fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withKey.credentialFor(tc.header); got != tc.wantHas {
				t.Errorf("credentialFor = %q, want %q", got, tc.wantHas)
			}
		})
	}

	noKey := New("http://example.invalid/v1", "", nil, false)
	if got := noKey.credentialFor(http.Header{}); got != "" {
		t.Errorf("credentialFor with nothing configured = %q, want empty", got)
	}
}

// TOOL-UNIT-002: the URL is normalised once, so no call site has to think about
// a trailing slash.
func TestURLNormalised(t *testing.T) {
	c := New("http://toolforge:8080/v1/chat/completions/", "", nil, false)
	if got := c.URL(); got != "http://toolforge:8080/v1/chat/completions" {
		t.Errorf("URL = %q", got)
	}
}

// TOOL-UNIT-003: only the credential and the X-Mimo-* extensions travel; an
// unrelated caller header is not our business to forward.
func TestOnlyIdentityHeadersTravel(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "", nil, false)
	caller := http.Header{
		"Authorization":     {"Bearer caller"},
		"X-Mimo-Session-Id": {"sess-1"},
		"X-Mimo-Extra":      {"kept"},
		"Cookie":            {"nope=1"},
		"X-Forwarded-For":   {"10.0.0.1"},
	}
	if err := c.Forward(context.Background(), []byte(`{"model":"m"}`), caller, httptest.NewRecorder()); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got.Get("Authorization") != "Bearer caller" {
		t.Errorf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get("X-Mimo-Session-Id") != "sess-1" || got.Get("X-Mimo-Extra") != "kept" {
		t.Errorf("X-Mimo-* not carried: %v", got)
	}
	if got.Get("Cookie") != "" || got.Get("X-Forwarded-For") != "" {
		t.Errorf("caller's unrelated headers travelled: %v", got)
	}
}

// TOOL-UNIT-004: nothing listening means ErrUnavailable, which the API maps to a
// 502 — never a request answered as if the tools had been honoured.
func TestUnreachableMiddleware(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := New(srv.URL, "", nil, false)

	err := c.Forward(context.Background(), []byte(`{"model":"m"}`), http.Header{}, httptest.NewRecorder())
	if err != ErrUnavailable {
		t.Fatalf("Forward error = %v, want ErrUnavailable", err)
	}
}

// TOOL-UNIT-005: the answer is copied through with its status and content type,
// and hop-by-hop headers are not echoed to our caller.
func TestAnswerCopiedThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	c := New(srv.URL, "", nil, false)
	if err := c.Forward(context.Background(), []byte(`{}`), http.Header{}, rec); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the middleware's own status", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Connection") != "" {
		t.Errorf("hop-by-hop header echoed: %q", rec.Header().Get("Connection"))
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("body = %q", rec.Body.String())
	}
}
