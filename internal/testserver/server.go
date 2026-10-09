package testserver

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/api"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/session"
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
// pool, real session store, real accounts file (temp dir), real api.Server
// (TDD §2.2).
type TestServer struct {
	Upstream *testutil.MockUpstream
	Pool     *pool.Pool
	Store    *accounts.Store
	Sessions *session.Store
	API      *api.Server
	Server   *httptest.Server
}

// TestServerOptions tunes NewTestServer; zero value = SDD defaults.
type TestServerOptions struct {
	APIKeys      []string
	MaxInflight  int
	QueueWait    time.Duration
	Keepalive    time.Duration
	IdleTimeout  time.Duration
	DefaultModel string
	ReplayCap    int
	Now          func() time.Time
	Probe        pool.ProbeFunc
	Renew        pool.RenewFunc
	Debug        bool          // MIMO2API_LOG_LEVEL=debug equivalent
	SessionWait  time.Duration // fork-policy bound (SDD §4.3.1, default 2s)
	ForkPolicy   string        // "wait" (default) or "fork"
	LogSink      io.Writer     // nil = discard-free default logger; set to capture trace lines

	// Audio surface (M18). Tests keep the poll interval tiny so the
	// generate → poll → download loop does not cost real seconds.
	AudioPoll      time.Duration
	TTSTimeout     time.Duration
	ASRTimeout     time.Duration
	SpeechMaxChars int
	DefaultVoice   string
}

// NewTestServer wires N scripted accounts into the full stack. Keepalive
// defaults small (10ms) so keepalive tests don't crawl.
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

	keepalive := opts.Keepalive
	if keepalive == 0 {
		keepalive = 10 * time.Millisecond
	}
	queueWait := opts.QueueWait
	if queueWait == 0 {
		queueWait = 2 * time.Second
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	p := pool.New(storeAccounts, pool.Options{
		MaxInflight: opts.MaxInflight,
		QueueWait:   queueWait,
		Now:         nowFn,
	}, opts.Probe, opts.Renew)

	sessions := session.New(session.Options{Now: nowFn})

	client := mimo.New(mimo.Options{
		Base:        up.URL(),
		IdleTimeout: opts.IdleTimeout,
	})

	var logger *log.Logger
	if opts.LogSink != nil {
		logger = log.New(opts.LogSink, "", 0)
	}
	apiSrv := api.New(api.Options{
		APIKeys:      opts.APIKeys,
		Pool:         p,
		Accounts:     store,
		Sessions:     sessions,
		Client:       client,
		DefaultModel: opts.DefaultModel,
		ReplayCap:    opts.ReplayCap,
		Keepalive:    keepalive,
		Logger:       logger,
		Debug:        opts.Debug,

		SessionWait: opts.SessionWait,
		ForkPolicy:  opts.ForkPolicy,

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
		Sessions: sessions,
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
