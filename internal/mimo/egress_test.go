package mimo

// Egress selection at the client level (SDD §4.8, TDD EGRESS-008): For(label)
// hands back a client that dials through that label's proxy while behaving
// identically in every other respect.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// TestForSelectsPerEgress: the empty and "direct" labels mean the shared client;
// a configured label yields a distinct client; an unknown label falls back
// unchanged (a missing endpoint is rejected at startup, not here).
func TestForSelectsPerEgress(t *testing.T) {
	proxy, _ := url.Parse("http://proxy.example:8080")
	c := New(Options{Egress: map[string]*url.URL{"hk1": proxy}})

	if c.For("") != c {
		t.Error(`For("") must return the shared client`)
	}
	if c.For(EgressDirect) != c {
		t.Error(`For("direct") must return the shared client`)
	}
	hk := c.For("hk1")
	if hk == c {
		t.Fatal(`For("hk1") returned the shared client`)
	}
	if hk.http == c.http {
		t.Error("the labelled client reuses the direct http client")
	}
	// Everything else is inherited.
	if hk.base != c.base || hk.userAgent != c.userAgent || hk.idleTimeout != c.idleTimeout {
		t.Error("the labelled client diverged from the shared configuration")
	}
	if c.For("unknown") != c {
		t.Error("an unconfigured label must leave the client as it is")
	}
	// The shared client's transport is untouched by building a labelled one.
	if c.http.Transport != nil {
		t.Error("the shared client's transport was replaced")
	}
}

// TestLabelledClientDialsThroughItsProxy: an end-to-end check that traffic from
// the labelled client really leaves through the proxy — and that the shared
// client does not.
func TestLabelledClientDialsThroughItsProxy(t *testing.T) {
	var hits int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxyURL, _ := url.Parse(proxy.URL)
	c := New(Options{Egress: map[string]*url.URL{"hk1": proxyURL}})

	// The shared client reaches the upstream directly.
	if resp, err := c.For("").http.Get(upstream.URL); err != nil {
		t.Fatalf("direct get: %v", err)
	} else {
		resp.Body.Close()
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("the shared client used the proxy (%d hits)", got)
	}

	resp, err := c.For("hk1").http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("proxied get: %v", err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("proxy hits = %d, want 1", got)
	}
}

// TestProxyTransportKeepsBaseSettings: the labelled transport is a clone of the
// base one — same pooling and timeouts — with only the proxy added, so a labelled
// account cannot behave differently by accident.
func TestProxyTransportKeepsBaseSettings(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 7
	base.ResponseHeaderTimeout = 3 * time.Second

	u, _ := url.Parse("http://proxy.example:8080")
	got, ok := proxyTransport(base, u).(*http.Transport)
	if !ok {
		t.Fatal("proxyTransport did not return an *http.Transport")
	}
	if got.MaxIdleConnsPerHost != 7 || got.ResponseHeaderTimeout != 3*time.Second {
		t.Error("the base transport's settings were lost")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://aistudio.xiaomimimo.com/x", nil)
	via, err := got.Proxy(req)
	if err != nil || via == nil || via.Host != "proxy.example:8080" {
		t.Errorf("Proxy(req) = %v, %v; want the configured proxy", via, err)
	}
	// The base transport must be left alone: it is shared with the direct egress.
	// (Its Proxy is DefaultTransport's ProxyFromEnvironment, so compare the
	// functions rather than testing for nil.)
	if reflect.ValueOf(base.Proxy).Pointer() == reflect.ValueOf(got.Proxy).Pointer() {
		t.Error("the labelled transport reuses the base Proxy function; it was modified in place")
	}
	if got == base {
		t.Error("proxyTransport returned the base transport itself")
	}
}
