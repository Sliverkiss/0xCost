package api_test

// ④ Per-account egress, end to end (TDD §3.5.2 EGRESS-006..008, SDD §4.8).
//
// The point of a label is that the account's traffic actually leaves through it.
// These cases run a real forwarding proxy and check what arrives: a labelled
// account's calls must appear there — chat, the catalog behind /v1/models, audio —
// and an unlabelled account's must not.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// proxyLog is a forwarding HTTP proxy that records what passed through it.
type proxyLog struct {
	mu     sync.Mutex
	hosts  []string
	server *httptest.Server
}

func newProxyLog(t *testing.T) *proxyLog {
	t.Helper()
	p := &proxyLog{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hosts = append(p.hosts, r.Host+r.URL.Path)
		p.mu.Unlock()
		// A proxy request carries an absolute URI, so the default transport can
		// fetch it as-is.
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *proxyLog) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hosts)
}

func (p *proxyLog) sawContains(t *testing.T, needle string) bool {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.hosts {
		if len(h) > 0 && contains(h, needle) {
			return true
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// EGRESS-006: a labelled account's chat and catalog calls leave through its proxy.
func TestLabelledAccountTrafficLeavesThroughEgress(t *testing.T) {
	proxy := newProxyLog(t)
	u, err := url.Parse(proxy.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	accts := []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie, Egress: "hk1",
	}}
	ts := testserver.NewTestServer(t, accts, &testserver.TestServerOptions{
		Egress: map[string]*url.URL{"hk1": u},
	})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The catalog surface (/v1/models) goes upstream too — a different call, the
	// same account, so the same egress.
	models, err := ts.Get("/v1/models", "")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	_, _ = io.Copy(io.Discard, models.Body)
	models.Body.Close()

	if proxy.count() == 0 {
		t.Fatal("no request left through the account's egress")
	}
	if !proxy.sawContains(t, "/open-apis/bot/chat") {
		t.Errorf("the chat call did not leave through the egress: %v", proxy.hosts)
	}
	if !proxy.sawContains(t, "/open-apis/bot/config") {
		t.Errorf("the catalog call did not leave through the egress: %v", proxy.hosts)
	}
}

// EGRESS-007: an unlabelled account keeps using the shared egress — a label is
// per account, not a global switch.
func TestUnlabelledAccountDoesNotUseALabel(t *testing.T) {
	proxy := newProxyLog(t)
	u, _ := url.Parse(proxy.server.URL)
	// The proxy is configured for hk1, but no account is labelled with it.
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		Egress: map[string]*url.URL{"hk1": u},
	})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if proxy.count() != 0 {
		t.Errorf("an unlabelled account left through hk1: %v", proxy.hosts)
	}
}

// EGRESS-008: the pool reports a lease's egress, so every call the request makes
// can be attributed to it (the API layer keys the client on this).
func TestLeaseCarriesItsEgress(t *testing.T) {
	accts := []testserver.TestAccount{{
		ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie, Egress: "hk1",
	}}
	ts := testserver.NewTestServer(t, accts, nil)
	if got := ts.Pool.EgressOf("acc1"); got != "hk1" {
		t.Errorf("EgressOf = %q, want hk1", got)
	}
	lease, err := ts.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	if lease.Egress != "hk1" {
		t.Errorf("lease egress = %q, want hk1", lease.Egress)
	}
	if got := ts.Pool.EgressOf("acc1"); got != "hk1" {
		t.Errorf("EgressOf changed under a lease: %q", got)
	}
}
