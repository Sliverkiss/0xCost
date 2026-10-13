package testserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/api"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testutil"
)

// TestAccount describes one scripted account for NewTestServer.
type TestAccount struct {
	ID     string
	PH     string
	Cookie string
	// Egress is the account's egress LABEL (SDD §4.8); empty means "direct". No
	// dialling is involved in tests — the label is what correlation keys on.
	Egress string
	Script []testutil.ScriptStep // initial upstream script (nil = default sse_basic)
}

// TestServer is a fully wired mimo2api stack against a testutil.MockUpstream: real
// pool, real accounts file (temp dir), real api.Server
// (TDD §2.2).
type TestServer struct {
	Upstream *testutil.MockUpstream
	Pool     *pool.Pool
	Store    *accounts.Store
	API      *api.Server
	Server   *httptest.Server
}

// TestServerOptions tunes NewTestServer; zero value = SDD defaults.
type TestServerOptions struct {
	APIKeys      []string
	MaxInflight  int
	QueueWait    time.Duration
	IdleTimeout  time.Duration
	DefaultModel string
	ReplayCap    int
	// MaxQueryChars mirrors MIMO2API_MAX_QUERY_CHARS: >0 overrides the
	// per-channel chat caps (ADR 0015).
	MaxQueryChars int
	// MaxImages/MaxImageBytes mirror MIMO2API_MAX_IMAGES / _MAX_IMAGE_BYTES
	// (IMG-*). Zero means the api.New defaults (10 images, 20 MiB each).
	MaxImages     int
	MaxImageBytes int
	// MaxImagePixels mirrors MIMO2API_MAX_IMAGE_PIXELS (the "low" decode bound).
	MaxImagePixels int
	// ImageAllowPrivate mirrors MIMO2API_IMAGE_ALLOW_PRIVATE. A test that fetches
	// an image from its own loopback listener sets it; the default is off, which is
	// what IMG-014 asserts.
	ImageAllowPrivate bool
	// RequestTimeout/LeaseTTL/SlotHold mirror the ADR 0019 knobs. Zero means the
	// api.New defaults (no total budget, 120s per turn, last-byte).
	RequestTimeout time.Duration
	LeaseTTL       time.Duration
	SlotHold       string
	// KeepaliveInterval mirrors the ADR 0034 SSE comment cadence: a streaming
	// test that wants to observe keepalives shortens it instead of waiting 15s.
	KeepaliveInterval time.Duration
	Now               func() time.Time
	Debug             bool      // MIMO2API_LOG_LEVEL=debug equivalent
	LogSink           io.Writer // nil = discard-free default logger; set to capture trace lines
	// Egress maps an egress label to a proxy, exactly as the real startup does
	// after reading MIMO2API_EGRESS_* (SDD §4.8). Tests point a label at an
	// httptest proxy and watch what leaves through it.
	Egress map[string]*url.URL

	// Audio surface (M18). Tests keep the poll interval tiny so the
	// generate → poll → download loop does not cost real seconds.
	AudioPoll      time.Duration
	TTSTimeout     time.Duration
	ASRTimeout     time.Duration
	SpeechMaxChars int
	DefaultVoice   string
}

// NewTestServer wires N scripted accounts into the full stack.
// defaults small (10ms) so timing tests don't crawl.
func NewTestServer(t *testing.T, accts []TestAccount, opts *TestServerOptions) *TestServer {
	t.Helper()
	if opts == nil {
		opts = &TestServerOptions{}
	}
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)

	var storeAccounts []accounts.Account
	for _, a := range accts {
		up.AddAccount(a.PH, a.Cookie)
		if len(a.Script) > 0 {
			up.Script(a.PH, a.Script...)
		}
		storeAccounts = append(storeAccounts, accounts.Account{
			ID:      a.ID,
			Egress:  a.Egress,
			Enabled: true,
			Session: &accounts.Session{Cookie: a.Cookie, PH: a.PH, FetchedAt: time.Now().Unix()},
		})
	}

	// Accounts file in a temp dir — NEVER the real data/accounts.json.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	f, _ := json.MarshalIndent(accounts.File{Accounts: storeAccounts}, "", "  ")
	if err := os.WriteFile(path, f, 0o600); err != nil {
		t.Fatalf("write temp accounts: %v", err)
	}
	store, err := accounts.Load(path)
	if err != nil {
		t.Fatalf("load temp accounts: %v", err)
	}

	queueWait := opts.QueueWait
	if queueWait == 0 {
		queueWait = 2 * time.Second
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	var logf func(format string, args ...any)
	if opts.LogSink != nil {
		sink := opts.LogSink
		logf = func(format string, args ...any) { fmt.Fprintf(sink, format+"\n", args...) }
	}

	p := pool.New(storeAccounts, pool.Options{
		MaxInflight: opts.MaxInflight,
		QueueWait:   queueWait,
		Now:         nowFn,
		SlotTTL:     opts.LeaseTTL, // reaper TTL (ADR 0019)
		Logf:        logf,
	})

	client := mimo.New(mimo.Options{
		Egress:      opts.Egress,
		Base:        up.URL(),
		IdleTimeout: opts.IdleTimeout,
	})

	var logger *log.Logger
	if opts.LogSink != nil {
		logger = log.New(opts.LogSink, "", 0)
	}
	apiSrv := api.New(api.Options{
		APIKeys:           opts.APIKeys,
		Pool:              p,
		Accounts:          store,
		Client:            client,
		DefaultModel:      opts.DefaultModel,
		ReplayCap:         opts.ReplayCap,
		MaxQueryChars:     opts.MaxQueryChars,
		MaxImages:         opts.MaxImages,
		MaxImageBytes:     opts.MaxImageBytes,
		MaxImagePixels:    opts.MaxImagePixels,
		ImageAllowPrivate: opts.ImageAllowPrivate,
		// ADR 0019 knobs.
		RequestTimeout: opts.RequestTimeout,
		LeaseTTL:       opts.LeaseTTL,
		SlotHold:       opts.SlotHold,

		KeepaliveInterval: opts.KeepaliveInterval,
		Logger:            logger,
		Debug:             opts.Debug,

		AudioPoll:      opts.AudioPoll,
		TTSTimeout:     opts.TTSTimeout,
		ASRTimeout:     opts.ASRTimeout,
		SpeechMaxChars: opts.SpeechMaxChars,
		DefaultVoice:   opts.DefaultVoice,
	})

	hs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(hs.Close)

	return &TestServer{
		Upstream: up,
		Pool:     p,
		Store:    store,
		API:      apiSrv,
		Server:   hs,
	}
}

// URL returns the server base URL.
func (ts *TestServer) URL() string { return ts.Server.URL }

// Chat posts an OpenAI chat request body (JSON string) and returns the
// response. apiKey may be "".
func (ts *TestServer) Chat(body string, apiKey string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, ts.URL()+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return ts.Server.Client().Do(req)
}

// Post sends a JSON body to a path with an optional key (audio surfaces, which
// Chat does not cover).
// PostHeaders is Post with caller-supplied headers, for the header passthrough
// the tool layer depends on.
func (ts *TestServer) PostHeaders(path, apiKey, body string, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, ts.URL()+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return ts.Server.Client().Do(req)
}

func (ts *TestServer) Post(path, apiKey, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, ts.URL()+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return ts.Server.Client().Do(req)
}

// Get fetches a path with an optional key (models, healthz).
func (ts *TestServer) Get(path, apiKey string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, ts.URL()+path, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return ts.Server.Client().Do(req)
}
