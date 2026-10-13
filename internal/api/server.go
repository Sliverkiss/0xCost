// Package api implements the OpenAI-compatible HTTP surface: endpoints
// (SDD §5.1), Bearer auth middleware (§5.1), request mapping (§5.2), and
// error translation (§7.4).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// Server wires the whole stack behind one http.Handler (SDD §3).
type Server struct {
	pool  *pool.Pool
	store *accounts.Store

	client   *mimo.Client
	otp      *otpStore      // new-device OTP challenges (SDD §2.2 amendment)
	recovery *recoveryState // asynchronous credential recovery (ADR 0014)
	// logins is the per-account credential-resolution gate (BUGS-FOUND #2):
	// one login chain at a time per account, shared by creds and the
	// background recovery, so a burst cannot start a challenge per request.
	logins *loginGate

	apiKeys        [][]byte // parsed keys; empty slice = auth open (SDD §5.1)
	defaultModel   string
	replayCap      int
	maxImages      int
	maxImageBytes  int
	maxImagePixels int
	// imageClient fetches client-supplied image URLs. Its transport refuses
	// loopback/private/link-local destinations on every hop (imageinput.go), so a
	// caller cannot use the gateway as a probe into the host's own network.
	imageClient *http.Client
	// requestTimeout/leaseTTL/slotHold implement ADR 0019: a real client-facing
	// deadline, a per-turn upstream deadline, and when the in-flight slot goes
	// back to the pool.
	requestTimeout time.Duration
	leaseTTL       time.Duration
	slotHold       string
	// keepaliveInterval is how often a committed chat stream writes an SSE
	// comment while the upstream is still generating (ADR 0034). It is a timing
	// constant, not a deployment knob: the client's `stream` field decides
	// whether there is a stream at all, and nothing here changes the answer.
	keepaliveInterval time.Duration
	// maxQueryChars overrides the per-channel chat char cap when > 0
	// (MIMO2API_MAX_QUERY_CHARS, ADR 0015).
	maxQueryChars int

	logger *log.Logger
	debug  bool

	forkPolicy string

	// Web-surface audio + catalog (audio.go).
	audioConvs     *mimo.ConversationCache
	botCfg         botConfigCache
	catalogTTL     time.Duration
	audioPoll      time.Duration
	ttsTimeout     time.Duration
	ttsMaxTimeout  time.Duration
	asrTimeout     time.Duration
	speechMaxChars int
	defaultVoice   string

	// redirects caches the upstream's deprecated-model map (`redirectTo`) for
	// catalogTTL, so the request paths can resolve a superseded id without
	// touching the upstream per request (M19 §3, model redirects).
	redirects struct {
		mu sync.Mutex
		// refreshMu serializes catalog rebuilds (RefreshModelRedirects and the
		// relearn floor): without it, a refusal burst can fan out into several
		// concurrent catalog reads, each running creds per account (ADR 0011).
		refreshMu sync.Mutex
		at        time.Time
		m         map[string]string
		// ids is the set of model ids the account catalogs currently serve —
		// the single source of truth for chat's model gate. Read from this
		// snapshot, never fetched: a chat request that read a catalog could
		// trigger a login (and a second OTP challenge), which is exactly what
		// the request path must never do (ADR 0011).
		ids map[string]bool
		// thinks caches each id's declared thinking default (`thinkingDefaultOn`)
		// from the same snapshot: a request that says nothing about thinking must
		// be able to follow the model's own setting without the request path
		// reading a catalog (ADR 0011/0031).
		thinks map[string]bool
	}
}

// modelRedirects returns the cached superseded-id map. The whole snapshot —
// this map, the served-id set, and the thinking defaults — is memory-only and
// is never populated from the request paths: a chat request that fetched a
// catalog could trigger a login, which is exactly what an existing regression
// case caught (a chat call produced a second OTP challenge). The snapshot is
// warmed by RefreshModelRedirects at startup, by the janitor, and as a side
// effect of reading the model list; a cold map (or one account whose catalog
// is unreadable) simply leaves the requested id alone.
func (s *Server) modelRedirects() map[string]string {
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	return s.redirects.m
}

// catalogIDs returns the cached set of model ids the account catalogs serve —
// the id set chat's model gate validates against. Same contract as
// modelRedirects: snapshot only, never a fetch.
func (s *Server) catalogIDs() map[string]bool {
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	return s.redirects.ids
}

// RefreshModelRedirects rebuilds the cached catalog *hints* from the account
// catalogs — the superseded-id map, the served-id set, and each id's declared
// thinking default — at most once per catalog TTL. It is the exported warm-up
// main.go calls at startup and on its janitor tick, so the request paths never
// pay for a catalog read.
func (s *Server) RefreshModelRedirects(ctx context.Context) {
	s.redirects.refreshMu.Lock()
	defer s.redirects.refreshMu.Unlock()
	s.refreshModelRedirectsLocked(ctx)
}

// refreshModelRedirectsLocked does the rebuild; the caller holds refreshMu.
// That mutex is the anti-stampede gate: relearnModel zeroes `at` before
// calling here, so a burst of concurrent refusals would otherwise every one of
// them pass the TTL gate and pay for its own catalog read — a read that runs
// creds per account and can trigger a login (ADR 0011). The gate is re-checked
// under the mutex so the losers of the race wait for the winner's snapshot
// instead of fetching their own.
func (s *Server) refreshModelRedirectsLocked(ctx context.Context) {
	s.redirects.mu.Lock()
	if s.redirects.m != nil && time.Since(s.redirects.at) < s.catalogWindow() {
		s.redirects.mu.Unlock()
		return
	}
	s.redirects.mu.Unlock()

	entries := s.catalogEntries(ctx)
	if ctx.Err() != nil {
		// A cancelled context reads every account as unreadable; caching that
		// emptiness for a whole TTL would wipe a known-good snapshot. The map
		// itself is left alone, but `at` is re-stamped so the floor still holds
		// — otherwise every subsequent refusal would pay for another fetch
		// this same cancellation just aborted.
		s.redirects.mu.Lock()
		if s.redirects.m != nil {
			s.redirects.at = time.Now()
		}
		s.redirects.mu.Unlock()
		return
	}
	m := s.redirectMapOf(entries)
	ids := servedIDsOf(entries)
	thinks := thinkingDefaultsOf(entries)
	s.storeSnapshot(m, ids, thinks)
	if len(m) > 0 {
		s.debugf("models: %d superseded ids will be resolved before they reach the upstream", len(m))
	}
	if len(thinks) > 0 {
		s.debugf("models: %d ids declare their own thinking default; a request that says nothing follows it", len(thinks))
	}
}

// relearnModel is what a refusal teaches us: the map we hold was out of date, so
// rebuild it (TTL bypassed) and resolve the id again. The floor keeps a burst of
// refusals — several requests naming a model retired a moment ago — from
// stampeding the catalog read: the floor check, the TTL-bypassing expiry, and
// the rebuild itself all happen under refreshMu, so one refusal wins the race
// and the rest resolve against the snapshot it stored.
func (s *Server) relearnModel(ctx context.Context, id string) string {
	s.redirects.refreshMu.Lock()
	defer s.redirects.refreshMu.Unlock()
	s.redirects.mu.Lock()
	tooSoon := !s.redirects.at.IsZero() && time.Since(s.redirects.at) < redirectRelearnFloor
	if !tooSoon {
		s.redirects.at = time.Time{} // expire, so the refresh below cannot short-circuit
	}
	s.redirects.mu.Unlock()
	if !tooSoon {
		s.refreshModelRedirectsLocked(ctx)
	}
	resolved := adapter.ResolveRedirect(id, s.modelRedirects())
	if resolved != id {
		s.debugf("model: the upstream refused %s, which is superseded by %s; retrying with %s", id, resolved, resolved)
	} else {
		s.debugf("model: the upstream refused %s and the catalog knows no successor", id)
	}
	return resolved
}

// redirectRelearnFloor bounds how often a refusal may trigger a rebuild.
const redirectRelearnFloor = 5 * time.Second

// resolveModel maps a client's model id onto the id the upstream accepts. Both
// surfaces need it: the audio surface refuses a superseded id outright
// ("model: [x] not support") and chat answers it with the busy frame, while the
// Studio SPA never sends one (it resolves the same map client-side).
// resolveModel maps a client's model id onto the id the upstream accepts. Both
// surfaces need it: the audio surface refuses a superseded id outright
// ("model: [x] not support") and chat answers it with the busy frame, while the
// Studio SPA never sends one (it resolves the same map client-side).
func (s *Server) resolveModel(id string) string {
	resolved := adapter.ResolveRedirect(id, s.modelRedirects())
	if resolved != id {
		s.debugf("model: %s is superseded by %s upstream; sending %s", id, resolved, resolved)
	}
	return resolved
}

// debugf logs only when the server was built with Options.Debug
// (MIMO2API_LOG_LEVEL=debug). It carries the routing trace an operator needs
// when a request misbehaves — which account served it, how many queries the
// replay sent — and is far too noisy for the default level.
func (s *Server) debugf(format string, args ...any) {
	if !s.debug {
		return
	}
	s.logger.Printf("debug: "+format, args...)
}

// Slot hold policies (ADR 0019). The env-level names live in
// internal/config (MIMO2API_SLOT_HOLD); the API layer only needs to know which
// one is in force.
const (
	slotHoldLastByte  = "last-byte"
	slotHoldFirstByte = "first-byte"
)

// Options configures a Server.
type Options struct {
	APIKeys      []string // parsed list; empty = open auth
	Pool         *pool.Pool
	Accounts     *accounts.Store
	Client       *mimo.Client
	DefaultModel string // "auto"/missing model resolution (SDD §5.2)
	ReplayCap    int    // max replayed user turns (SDD §5.3B, default 20)
	// MaxImages bounds how many image parts one request may carry; MaxImageBytes
	// bounds one image's size (IMG-*, defaults 10 / 20 MiB). Both are
	// client-facing guards — every image costs three upstream calls.
	MaxImages     int
	MaxImageBytes int
	// MaxImagePixels bounds what one image may ask this process to decode
	// (MIMO2API_MAX_IMAGE_PIXELS, default 40 MP). Only the "low" level decodes, so
	// this is the bound that keeps a 200 KB PNG header from turning into gigabytes
	// of pixel buffer.
	MaxImagePixels int
	// ImageAllowPrivate lets the fetch reach loopback/private/link-local
	// destinations (MIMO2API_IMAGE_ALLOW_PRIVATE). Off by default: a
	// client-supplied URL is assumed hostile, and a public gateway must not double
	// as a probe into the host's own network.
	ImageAllowPrivate bool
	// RequestTimeout is the whole client-facing budget for one chat request
	// (ADR 0019, default 120s). 0 = no total budget (the per-turn lease TTL
	// still applies).
	RequestTimeout time.Duration
	// LeaseTTL bounds one upstream turn and, as a backstop, the in-flight slot
	// that turn holds (ADR 0019, default 120s).
	LeaseTTL time.Duration
	// SlotHold is when a request gives its in-flight slot back: "last-byte"
	// (default) or "first-byte" (ADR 0019).
	SlotHold string
	// MaxQueryChars overrides the per-channel chat char cap when > 0
	// (MIMO2API_MAX_QUERY_CHARS, ADR 0015). 0 = adapter.MaxQueryChars per model.
	MaxQueryChars int
	// KeepaliveInterval is the SSE comment cadence on a committed chat stream
	// (ADR 0034, default 15s). Tests shorten it; production never needs to.
	KeepaliveInterval time.Duration
	Logger            *log.Logger
	Debug             bool // MIMO2API_LOG_LEVEL=debug: per-request routing trace

	// Audio surface (M18): poll cadence and per-flow deadlines.
	AudioPoll time.Duration // TTS/ASR status poll interval (default 1s)
	// CatalogTTL is the per-account Studio catalog cache lifetime
	// (default 10m, M19 §3).
	CatalogTTL time.Duration
	TTSTimeout time.Duration // synthesis deadline for one piece (default 60s)
	// TTSMaxTimeout caps the per-piece budget when the text scales it up
	// (default 15m).
	TTSMaxTimeout  time.Duration
	ASRTimeout     time.Duration // transcription deadline (default 120s)
	SpeechMaxChars int           // /v1/audio/speech input cap (default 10000)
	DefaultVoice   string        // upstream voice id/name; empty = first in the catalog
}

// New builds a Server. All collaborators must be wired (real or test
// doubles).
func New(opts Options) *Server {
	// DefaultModel: no hardcoded value. An empty default is resolved against
	// the live catalog at request time (NormalizeModel falls back to the
	// catalog's first entry), and a configured default comes from config
	// (MIMO2API_DEFAULT_MODEL). The account catalog is the source of truth —
	// never a built-in list.
	if opts.ReplayCap <= 0 {
		opts.ReplayCap = 20
	}
	if opts.MaxImages <= 0 {
		opts.MaxImages = 10
	}
	if opts.MaxImageBytes <= 0 {
		opts.MaxImageBytes = 20 << 20
	}
	if opts.MaxImagePixels <= 0 {
		opts.MaxImagePixels = defaultMaxImagePixels
	}
	if opts.RequestTimeout < 0 {
		opts.RequestTimeout = 0
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 120 * time.Second
	}
	if opts.SlotHold == "" {
		opts.SlotHold = slotHoldLastByte
	}
	if opts.AudioPoll <= 0 {
		opts.AudioPoll = time.Second
	}
	if opts.TTSMaxTimeout <= 0 {
		opts.TTSMaxTimeout = 15 * time.Minute
	}
	if opts.TTSTimeout <= 0 {
		opts.TTSTimeout = 60 * time.Second
	}
	if opts.ASRTimeout <= 0 {
		opts.ASRTimeout = 120 * time.Second
	}
	if opts.SpeechMaxChars <= 0 {
		opts.SpeechMaxChars = 10000
	}
	// The upstream refuses one synthesis over SpeechTextLimit characters, so a
	// larger configured cap would only accept requests that cannot succeed and
	// turn a clear 400 into a slow upstream failure (ADR 0010).
	if opts.SpeechMaxChars > mimo.SpeechTextLimit {
		opts.SpeechMaxChars = mimo.SpeechTextLimit
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	var keys [][]byte
	for _, k := range opts.APIKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, []byte(k))
		}
	}
	return &Server{
		pool:              opts.Pool,
		imageClient:       newImageHTTPClient(opts.ImageAllowPrivate),
		maxImagePixels:    opts.MaxImagePixels,
		store:             opts.Accounts,
		client:            opts.Client,
		otp:               newOTPStore(),
		recovery:          newRecoveryState(),
		logins:            &loginGate{},
		apiKeys:           keys,
		defaultModel:      opts.DefaultModel,
		replayCap:         opts.ReplayCap,
		maxImages:         opts.MaxImages,
		maxImageBytes:     opts.MaxImageBytes,
		requestTimeout:    opts.RequestTimeout,
		leaseTTL:          opts.LeaseTTL,
		slotHold:          opts.SlotHold,
		maxQueryChars:     opts.MaxQueryChars,
		keepaliveInterval: opts.KeepaliveInterval,
		logger:            logger,
		debug:             opts.Debug,
		audioConvs:        mimo.NewConversationCache(0),
		audioPoll:         opts.AudioPoll,
		catalogTTL:        opts.CatalogTTL,
		ttsTimeout:        opts.TTSTimeout,
		ttsMaxTimeout:     opts.TTSMaxTimeout,
		asrTimeout:        opts.ASRTimeout,
		speechMaxChars:    opts.SpeechMaxChars,
		defaultVoice:      opts.DefaultVoice,
	}
}

// Handler returns the root handler with routing (Go 1.22+ method+pattern
// ServeMux, SDD §3) and auth middleware applied to /v1/*.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	mux.HandleFunc("GET /v1/models/{id}", s.auth(s.handleModelByID))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChat))
	// Web-surface audio (M18) — OpenAI-compatible shapes.
	mux.HandleFunc("POST /v1/audio/speech", s.auth(s.handleSpeech))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.auth(s.handleTranscriptions))
	mux.HandleFunc("GET /v1/audio/voices", s.auth(s.handleVoices))
	// Admin OTP endpoints (SDD §2.2 amendment) — same Bearer auth.
	// The pool view (SDD §4.9). /healthz stays as it is: the Docker healthcheck
	// parses it (REQ-13).
	mux.HandleFunc("GET /v1/admin/pool", s.auth(s.handlePoolView))
	mux.HandleFunc("GET /v1/admin/otp/pending", s.auth(s.handleOTPPending))
	mux.HandleFunc("POST /v1/admin/otp/submit", s.auth(s.handleOTPSubmit))
	// Cross-region consent override (docs/login-chain-reverse.md §1) — the
	// operator path out of the MIMO2API_CROSS_REGION_AGREE=false dead-end.
	mux.HandleFunc("POST /v1/admin/consent/agree", s.auth(s.handleConsentAgree))
	return mux
}

// auth wraps /v1/* handlers with the Bearer check (SDD §5.1). No keys
// configured → open (bind-local is enforced at config level, CONF-003).
// Constant-time comparison via crypto/subtle.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.apiKeys) > 0 {
			token, ok := parseBearer(r.Header.Get("Authorization"))
			if !ok || !s.validKey(token) {
				writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
					"missing or invalid API key", 0)
				return
			}
		}
		next(w, r)
	}
}

// parseBearer extracts the token from "Authorization: Bearer <key>".
func parseBearer(h string) (string, bool) {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// validKey compares the token against every configured key in constant
// time (crypto/subtle, SDD §5.1).
func (s *Server) validKey(token string) bool {
	t := []byte(token)
	for _, k := range s.apiKeys {
		if subtle.ConstantTimeCompare(t, k) == 1 {
			return true
		}
	}
	return false
}

// handleHealthz: liveness + pool summary — states only, no credentials
// (SDD §5.1, TDD API-018).
// clientFor returns the upstream client whose traffic leaves through an egress
// label. Every upstream call is made through it, so an account's egress is a
// property of the account rather than of the caller (REQ-16, SDD §4.8).
func (s *Server) clientFor(egress string) *mimo.Client { return s.client.For(egress) }

// clientForAccount is clientFor for callers that hold an account id rather than a
// lease (the OTP and consent admin paths).
func (s *Server) clientForAccount(accountID string) *mimo.Client {
	return s.client.For(s.pool.EgressOf(accountID))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	sum := s.pool.Summarize()
	body := struct {
		Status   string `json:"status"`
		Accounts struct {
			Total     int `json:"total"`
			Available int `json:"available"`
			Disabled  int `json:"disabled"`
		} `json:"accounts"`
	}{
		Status: "ok",
	}
	body.Accounts.Total = sum.Total
	body.Accounts.Available = sum.Available
	body.Accounts.Disabled = sum.Disabled
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// modelJSON is the OpenAI model object (SDD §5.1) plus this project's
// documented extension fields (M19 §3): capabilities, page_type, tasks.
type modelJSON struct {
	ID           string            `json:"id"`
	Object       string            `json:"object"`
	Created      int64             `json:"created"`
	OwnedBy      string            `json:"owned_by"`
	Name         string            `json:"name,omitempty"`
	Capabilities modelCapabilities `json:"capabilities"`
	PageType     string            `json:"page_type,omitempty"`
	Tasks        []string          `json:"tasks,omitempty"`
	// ContextSize is the upstream's own context label ("1M") when it states
	// one; absent when it does not, rather than guessed.
	ContextSize string `json:"context_size,omitempty"`
	// Source says where the row came from. There is only one source now — the
	// account's own Studio catalog — but the field stays in the wire shape so
	// existing clients keep parsing. A "static" row cannot exist: the built-in
	// fallback table is gone.
	Source string `json:"source"`
	// RedirectTo is set only when a client asked for a superseded id: it names
	// the model the upstream serves instead (the list itself never advertises a
	// superseded id).
	RedirectTo string `json:"redirect_to,omitempty"`
}

type modelCapabilities struct {
	// WebSearch: 0 = no search surface, 1 = searchable (off by default),
	// 2 = searchable and on by default.
	WebSearch int    `json:"web_search"`
	Thinking  bool   `json:"thinking"`
	Audio     string `json:"audio"`
}

// handleModels lists the catalog in OpenAI list shape (TDD API-005).
//
// The rows come from the accounts' own Studio catalogs (bot/config), unioned
// across accounts. There is no static fallback list: when no account catalog is
// readable the endpoint answers 200 with an empty list and says so in the debug
// trace — honest ("this deployment currently knows of no models") rather than
// invented rows for retired ids. Chat stays consistent with this: its model
// gate reads the same account catalog and passes ids through when the catalog
// is unreadable (the upstream is the better judge of its own names).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.modelsList(r.Context()))
}

func (s *Server) modelsList(ctx context.Context) map[string]any {
	all := s.catalogEntries(ctx)
	// Reading the model list is one of the two sanctioned catalog readers (the
	// other is RefreshModelRedirects), so it refreshes the whole snapshot —
	// superseded-id map, served-id set, thinking defaults — as a side effect.
	s.storeSnapshot(s.redirectMapOf(all), servedIDsOf(all), thinkingDefaultsOf(all))
	entries := adapter.Usable(all)
	data := make([]modelJSON, 0, len(entries))
	for _, e := range entries {
		data = append(data, modelEntryDTO(e))
	}
	return map[string]any{"object": "list", "data": data}
}

// modelEntryDTO renders one catalog entry, so the list and the single-model
// endpoint cannot drift apart (M19 §3).
func modelEntryDTO(e adapter.CatalogEntry) modelJSON {
	return modelJSON{
		ID:      e.ID,
		Object:  "model",
		Created: 1700000000,
		OwnedBy: "xiaomi-mimo",
		Name:    e.Name,
		Capabilities: modelCapabilities{
			WebSearch: e.Web,
			Thinking:  e.Think,
			Audio:     e.Audio,
		},
		PageType:    e.Page,
		Tasks:       adapter.Tasks(e.Page),
		ContextSize: e.NToken,
		Source:      adapter.SourceAccnt,
	}
}

// catalogEntries builds the deployment-wide catalog from the accounts' own
// views, capabilities taken at their maximum per id (M19 §3). There is no
// static floor on purpose: a hardcoded list goes stale the day the upstream
// retires a model, so when no account catalog is readable the catalog is
// empty — the account catalog is the single source of truth, and an
// unreadable one yields nothing rather than invented rows.
func (s *Server) catalogEntries(ctx context.Context) []adapter.CatalogEntry {
	entries := make([]adapter.CatalogEntry, 0, 24)

	ids := s.pool.IDs()
	cfgByID := s.accountCatalogs(ctx, ids)
	for _, id := range ids {
		if cfg, ok := cfgByID[id]; ok {
			entries = append(entries, accountCatalogEntries(cfg)...)
		}
	}
	if len(cfgByID) == 0 {
		s.debugf("models: no account catalog available (%d accounts); serving an empty list", len(ids))
	}
	return adapter.SortCatalog(adapter.MergeCatalog(entries))
}

// accountCatalogs reads every account's catalog concurrently, bounded so a
// large pool cannot stampede the upstream; per-account results are cached by
// botConfigFor for the TTL.
func (s *Server) accountCatalogs(ctx context.Context, ids []string) map[string]*mimo.BotConfig {
	const parallelism = 4
	var (
		mu  sync.Mutex
		out = make(map[string]*mimo.BotConfig, len(ids))
		wg  sync.WaitGroup
		sem = make(chan struct{}, parallelism)
	)
	for _, id := range ids {
		cookie, ph, err := s.creds(ctx, id)
		if err != nil {
			s.debugf("models: no credentials for account %s: %v", id, err)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id, cookie, ph string) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cfg, err := s.botConfigFor(fctx, id, cookie, ph)
			if err != nil {
				s.debugf("models: catalog read failed for account %s: %v", id, err)
				return
			}
			mu.Lock()
			out[id] = cfg
			mu.Unlock()
		}(id, cookie, ph)
	}
	wg.Wait()
	return out
}

// accountCatalogEntries maps one account's bot/config onto catalog entries:
// modelConfigListNg carries the ids and page types, and the chat rows
// (modelConfigList) carry the capability defaults. The two are joined by id.
//
// The id source is both lists, not just modelConfigListNg: the live payload
// (2026-10-09) carries a chat row for mimo-v2.6-pro-ultraspeed-studio without a
// matching Ng row, so an Ng-only walk loses the model entirely. A chat row is
// by definition a chat model, which supplies the page type the row itself does
// not carry.
func accountCatalogEntries(cfg *mimo.BotConfig) []adapter.CatalogEntry {
	rows := make(map[string]mimo.ChatModel, len(cfg.ChatModels))
	for _, r := range cfg.ChatModels {
		rows[r.Model] = r
	}
	ids := make([]string, 0, len(cfg.Models)+len(cfg.ChatModels))
	pages := make(map[string]string, len(cfg.Models)+len(cfg.ChatModels))
	names := make(map[string]string, len(cfg.Models))
	// redirects is the upstream's own deprecated-model map (`redirectTo`): the
	// ids it has superseded, and the id it wants instead.
	redirects := make(map[string]string, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.Model == "" {
			continue
		}
		if _, seen := pages[m.Model]; !seen {
			ids = append(ids, m.Model)
		}
		pages[m.Model] = m.PageType
		if m.Name != "" {
			names[m.Model] = m.Name
		}
		if m.RedirectTo != "" {
			redirects[m.Model] = m.RedirectTo
		}
	}
	for _, r := range cfg.ChatModels {
		if r.Model == "" {
			continue
		}
		if _, seen := pages[r.Model]; !seen {
			ids = append(ids, r.Model)
			pages[r.Model] = adapter.PageChat
		}
		if _, ok := names[r.Model]; !ok && r.Name != "" {
			names[r.Model] = r.Name
		}
	}

	out := make([]adapter.CatalogEntry, 0, len(ids))
	for _, id := range ids {
		page := pages[id]
		row := rows[id]
		var think, thinkKnown bool
		var wsDefault string
		if row.Model != "" {
			if row.ThinkingDefaultOn != nil {
				think, thinkKnown = *row.ThinkingDefaultOn, true
			}
			if row.WebSearchDefaultStatus != nil {
				wsDefault = *row.WebSearchDefaultStatus
			}
		}
		out = append(out, adapter.CatalogEntry{
			ID:         id,
			Name:       names[id],
			Page:       page,
			Web:        adapter.WebSearchLevel(page, wsDefault),
			Think:      think,
			ThinkKnown: thinkKnown,
			Audio:      adapter.AudioForPage(page),
			NToken:     row.NToken,
			RedirectTo: redirects[id],
		})
	}
	return out
}

// handleModelByID returns one model or 404 listing available ids (TDD
// API-006: known → 200; alias → 200 after normalization; unknown → 404).
func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries := s.catalogEntries(r.Context())
	s.storeSnapshot(s.redirectMapOf(entries), servedIDsOf(entries), thinkingDefaultsOf(entries))
	for _, e := range entries {
		// Same source as the list: a model the account reports is known
		// (M19 §3).
		if e.ID == id {
			dto := modelEntryDTO(e)
			if e.RedirectTo != "" {
				// Superseded: the list hides it, but a client that asks for it by
				// name is told what the upstream serves instead rather than
				// getting a 404 for an id we clearly know about.
				if target, _ := findEntry(entries, adapter.ResolveRedirect(e.ID, s.redirectMapOf(entries))); target.ID != "" {
					dto = modelEntryDTO(target)
					dto.ID = id
					dto.RedirectTo = target.ID
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(dto)
			return
		}
	}
	model, _, err := adapter.NormalizeModel(id, s.defaultModel, modelIDs(entries))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{
			Message: fmt.Sprintf("model %q not found; available models: %s", id, strings.Join(modelIDs(adapter.Usable(entries)), ", ")),
			Type:    "invalid_request_error",
			Param:   nil,
			Code:    codeOf("model_not_found"),
		}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelJSON{ID: model, Object: "model", Created: 1700000000, OwnedBy: "xiaomi"})
}

// resolveChatModel is the only model gate on the chat surface: the account
// catalog is the single source of truth. There is no static fallback list.
//
//   - Snapshot warmed (main.go calls RefreshModelRedirects before serving, and
//     the janitor keeps it current): the id must be in it (redirects already
//     resolved). Anything else is 404 listing what the deployment actually
//     serves.
//   - Snapshot cold — no account catalog readable, or a fresh start whose
//     warm-up read failed: the id passes through unchecked. The upstream is a
//     better judge of its own model names than any list we hold, and a failed
//     catalog read must not turn into invented 404s.
//
// It reads the cached snapshot only, never a catalog: fetching here would put
// a login (and its OTP challenge) on the chat request path (ADR 0011).
func (s *Server) resolveChatModel(requested string) (string, bool, error) {
	ids := s.catalogIDs()
	if len(ids) == 0 {
		// An empty pool cannot serve any model; report that instead of a
		// misleading "model not found".
		if len(s.pool.IDs()) == 0 {
			return "", false, pool.ErrNoAccounts
		}
		s.debugf("models: account catalog unavailable; passing %q through unchecked", requested)
		return requested, adapter.IsFastchatModel(requested), nil
	}
	return adapter.NormalizeModel(requested, s.defaultModel, idsSlice(ids))
}

// servedIDsOf builds the served-id set from an entry slice: every id the
// account catalogs carry that the upstream has not superseded. A superseded id
// is not served (the upstream refuses it), so it must not validate.
func servedIDsOf(entries []adapter.CatalogEntry) map[string]bool {
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.Deprecated() {
			m[e.ID] = true
		}
	}
	return m
}

// idsSlice lists a set's ids in sorted order, for NormalizeModel and the error
// message it builds.
func idsSlice(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// storeSnapshot caches the superseded-id map, the served-id set, and the
// thinking defaults from one catalog read.
func (s *Server) storeSnapshot(m map[string]string, ids map[string]bool, thinks map[string]bool) {
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	s.redirects.m, s.redirects.ids, s.redirects.thinks = m, ids, thinks
	s.redirects.at = time.Now()
}

// thinkingDefaultsOf maps the ids the account catalog itself described onto
// their declared thinking default. An entry with no declaration is left out:
// its zero must not be read as a declaration that thinking is off (ADR 0031).
func thinkingDefaultsOf(entries []adapter.CatalogEntry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		if e.ThinkKnown {
			m[e.ID] = e.Think
		}
	}
	return m
}

// thinkingDefaultFor is the thinking switch's default for a request that says
// nothing about thinking (ADR 0031). It is the platform's own declaration for the
// model (`thinkingDefaultOn` — the value the Studio UI starts from, true for every
// chat model on this account); an id the catalog does not describe gets the same
// answer, on, because that is what the platform declares for its chat models and
// what OpenAI's own reasoning models do — a client that wants it off sends the
// standard field, and there is deliberately no deployment-level knob to disagree
// with the provider. Read from the cached snapshot, never a fetch (ADR 0011).
func (s *Server) thinkingDefaultFor(model string) bool {
	s.redirects.mu.Lock()
	v, ok := s.redirects.thinks[model]
	s.redirects.mu.Unlock()
	if ok {
		return v
	}
	return true
}

// redirectMapOf builds the superseded-id map from an entry slice.
func (s *Server) redirectMapOf(entries []adapter.CatalogEntry) map[string]string {
	m := map[string]string{}
	for _, e := range entries {
		if e.RedirectTo != "" {
			m[e.ID] = e.RedirectTo
		}
	}
	return m
}

// findEntry returns the entry with an id, or a zero entry.
func findEntry(entries []adapter.CatalogEntry, id string) (adapter.CatalogEntry, bool) {
	for _, e := range entries {
		if e.ID == id {
			return e, true
		}
	}
	return adapter.CatalogEntry{}, false
}

// modelIDs lists catalog ids for an error message, capped so a 25-model
// catalog does not produce a wall of text.
func modelIDs(entries []adapter.CatalogEntry) []string {
	const max = 24
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if len(out) == max {
			out = append(out, "…")
			break
		}
		out = append(out, e.ID)
	}
	return out
}

// catalogWindow is how long an account's catalog (models, voices, switches) is
// served from cache; zero falls back to the documented default (M19 §3).
func (s *Server) catalogWindow() time.Duration {
	if s.catalogTTL > 0 {
		return s.catalogTTL
	}
	return DefaultCatalogTTL
}

// DefaultCatalogTTL is the catalog cache lifetime when nothing is configured.
const DefaultCatalogTTL = 10 * time.Minute
