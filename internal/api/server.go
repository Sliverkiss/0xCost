// Package api implements the OpenAI-compatible HTTP surface: endpoints
// (SDD §5.1), Bearer auth middleware (§5.1), request mapping (§5.2), and
// error translation (§7.4).
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/session"
)

// Server wires the whole stack behind one http.Handler (SDD §3).
type Server struct {
	pool     *pool.Pool
	store    *accounts.Store
	sessions *session.Store
	client   *mimo.Client
	otp      *otpStore      // new-device OTP challenges (SDD §2.2 amendment)
	recovery *recoveryState // asynchronous credential recovery (ADR 0014)

	apiKeys      [][]byte // parsed keys; empty slice = auth open (SDD §5.1)
	defaultModel string
	replayCap    int
	// maxQueryChars overrides the per-channel chat char cap when > 0
	// (MIMO2API_MAX_QUERY_CHARS, ADR 0015).
	maxQueryChars int
	keepalive     time.Duration
	logger        *log.Logger
	debug         bool
	sessionWait   time.Duration
	forkPolicy    string

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
		at time.Time
		m  map[string]string
	}
}

// modelRedirects returns the cached superseded-id map. It is memory-only and
// does NOT read the account catalog: the request paths call it, and making a chat
// request fetch a catalog would let it trigger a login — which is exactly what an
// existing regression case caught (a chat call produced a second OTP challenge).
// The map is warmed by RefreshModelRedirects at startup, by the janitor, and as a
// side effect of reading the model list; a cold map (or one account whose catalog
// is unreadable) simply leaves the requested id alone.
func (s *Server) modelRedirects() map[string]string {
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	return s.redirects.m
}

// RefreshModelRedirects rebuilds the map from the account catalogs, at most once
// per catalog TTL. It is the exported warm-up main.go calls at startup and on its
// janitor tick, so the request paths never pay for a catalog read.
func (s *Server) RefreshModelRedirects(ctx context.Context) {
	s.redirects.mu.Lock()
	if s.redirects.m != nil && time.Since(s.redirects.at) < s.catalogWindow() {
		s.redirects.mu.Unlock()
		return
	}
	s.redirects.mu.Unlock()

	m := s.redirectMapOf(s.catalogEntries(ctx))
	s.redirects.mu.Lock()
	s.redirects.m = m
	s.redirects.at = time.Now()
	s.redirects.mu.Unlock()
	if len(m) > 0 {
		s.debugf("models: %d superseded ids will be resolved before they reach the upstream", len(m))
	}
}

// relearnModel is what a refusal teaches us: the map we hold was out of date, so
// rebuild it (TTL bypassed) and resolve the id again. The floor keeps a burst of
// refusals — several requests naming a model retired a moment ago — from
// stampeding the catalog read.
func (s *Server) relearnModel(ctx context.Context, id string) string {
	s.redirects.mu.Lock()
	tooSoon := !s.redirects.at.IsZero() && time.Since(s.redirects.at) < redirectRelearnFloor
	s.redirects.mu.Unlock()
	if tooSoon {
		return adapter.ResolveRedirect(id, s.modelRedirects())
	}
	s.redirects.mu.Lock()
	s.redirects.at = time.Time{} // expire, so the refresh below cannot short-circuit
	s.redirects.mu.Unlock()
	s.RefreshModelRedirects(ctx)
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
// (MIMO2API_LOG_LEVEL=debug). Routing decisions — sticky continuation versus
// full replay — are the first thing an operator needs when a conversation
// appears to lose context, and far too noisy for the default level.
func (s *Server) debugf(format string, args ...any) {
	if !s.debug {
		return
	}
	s.logger.Printf("debug: "+format, args...)
}

// Options configures a Server.
type Options struct {
	APIKeys      []string // parsed list; empty = open auth
	Pool         *pool.Pool
	Accounts     *accounts.Store
	Sessions     *session.Store
	Client       *mimo.Client
	DefaultModel string // "auto"/missing model resolution (SDD §5.2)
	ReplayCap    int    // max replayed user turns (SDD §5.3B, default 20)
	// MaxQueryChars overrides the per-channel chat char cap when > 0
	// (MIMO2API_MAX_QUERY_CHARS, ADR 0015). 0 = adapter.MaxQueryChars per model.
	MaxQueryChars int
	Keepalive     time.Duration // SSE keepalive interval (SDD §5.4, default 15s)
	// SessionWait bounds how long a second streaming request waits for its own
	// session before forking (SDD §4.3.1, REQ-8, default 2s).
	SessionWait time.Duration
	// ForkPolicy is "wait" (default) or "fork" (SDD §4.3.1).
	ForkPolicy string
	Logger     *log.Logger
	Debug      bool // MIMO2API_LOG_LEVEL=debug: per-request routing trace

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
	if opts.SessionWait <= 0 {
		opts.SessionWait = 2 * time.Second
	}
	if opts.ForkPolicy == "" {
		opts.ForkPolicy = "wait"
	}
	if opts.DefaultModel == "" {
		opts.DefaultModel = adapter.DefaultModels[0]
	}
	if opts.ReplayCap <= 0 {
		opts.ReplayCap = 20
	}
	if opts.Keepalive <= 0 {
		opts.Keepalive = 15 * time.Second
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
		pool:           opts.Pool,
		store:          opts.Accounts,
		sessions:       opts.Sessions,
		sessionWait:    opts.SessionWait,
		forkPolicy:     opts.ForkPolicy,
		client:         opts.Client,
		otp:            newOTPStore(),
		recovery:       newRecoveryState(),
		apiKeys:        keys,
		defaultModel:   opts.DefaultModel,
		replayCap:      opts.ReplayCap,
		maxQueryChars:  opts.MaxQueryChars,
		keepalive:      opts.Keepalive,
		logger:         logger,
		debug:          opts.Debug,
		audioConvs:     mimo.NewConversationCache(0),
		audioPoll:      opts.AudioPoll,
		catalogTTL:     opts.CatalogTTL,
		ttsTimeout:     opts.TTSTimeout,
		ttsMaxTimeout:  opts.TTSMaxTimeout,
		asrTimeout:     opts.ASRTimeout,
		speechMaxChars: opts.SpeechMaxChars,
		defaultVoice:   opts.DefaultVoice,
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
			// Session state is scoped to the calling credential (spec REQ-3,
			// INV-2). Only a one-way handle is carried, never the key itself.
			r = r.WithContext(context.WithValue(r.Context(), credentialCtxKey{}, credentialHandle(token)))
		}
		next(w, r)
	}
}

// forkWait returns how long a second streaming request waits for its own
// session before forking: the configured bound, or zero when the policy is
// "fork" (SDD §4.3.1, REQ-8).
func (s *Server) forkWait() time.Duration {
	if s.forkPolicy == "fork" {
		return 0
	}
	return s.sessionWait
}

// credentialCtxKey is the request-context key for the calling credential's
// non-reversible handle.
type credentialCtxKey struct{}

// credentialHandle derives the opaque credential identity used to scope session
// keys. Hashed so a session key can never be inverted into an API key (INV-3);
// a deployment without auth has a single, empty scope.
func credentialHandle(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("mimo2api-credential|" + token))
	return hex.EncodeToString(sum[:16])
}

// credentialOf returns the calling credential's handle, empty when the
// deployment runs without auth.
func credentialOf(r *http.Request) string {
	h, _ := r.Context().Value(credentialCtxKey{}).(string)
	return h
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
			Cooldown  int `json:"cooldown"`
			Disabled  int `json:"disabled"`
		} `json:"accounts"`
	}{
		Status: "ok",
	}
	body.Accounts.Total = sum.Total
	body.Accounts.Available = sum.Available
	body.Accounts.Cooldown = sum.Cooldown
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
	// Source says where the row came from: "account" (the deployment's own
	// Studio catalog) or "static" (the built-in fallback table, used only
	// when the account read failed). Account wins on conflict.
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
// The rows come from the account's own Studio catalog (bot/config), unioned
// across accounts, with the static table as a floor and fallback: an upstream
// hiccup must never take the endpoint down (M19 §3).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.modelsList(r.Context()))
}

func (s *Server) modelsList(ctx context.Context) map[string]any {
	all := s.catalogEntries(ctx)
	s.storeRedirects(s.redirectMapOf(all))
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
	source := adapter.SourceAccnt
	if e.Static {
		source = adapter.SourceFallo
	}
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
		Source:      source,
	}
}

// catalogEntries builds the deployment-wide catalog: the static floor merged
// with every account's own view, capabilities taken at their maximum per id
// (M19 §3).
func (s *Server) catalogEntries(ctx context.Context) []adapter.CatalogEntry {
	entries := make([]adapter.CatalogEntry, 0, len(adapter.DefaultModels)+24)
	for _, m := range adapter.DefaultModels {
		// Fallback values: a chat model that is searchable but not on by
		// default. The account catalog overrides these when it is readable.
		entries = append(entries, adapter.CatalogEntry{
			ID: m, Page: adapter.PageChat, Web: 1, Audio: adapter.AudioNone, Static: true,
		})
	}

	ids := s.pool.IDs()
	cfgByID := s.accountCatalogs(ctx, ids)
	for _, id := range ids {
		if cfg, ok := cfgByID[id]; ok {
			entries = append(entries, accountCatalogEntries(cfg)...)
		}
	}
	if len(cfgByID) == 0 {
		s.debugf("models: no account catalog available (%d accounts); serving the static floor", len(ids))
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
// matching Ng row, so an Ng-only walk loses the model and the response falls
// back to the static floor for it — reporting the floor's capabilities
// (no thinking, search merely enabled) over the account's own (thinking on,
// search "auto"). A chat row is by definition a chat model, which supplies the
// page type the row itself does not carry.
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
		var think bool
		var wsDefault string
		if row.Model != "" {
			if row.ThinkingDefaultOn != nil {
				think = *row.ThinkingDefaultOn
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
	s.storeRedirects(s.redirectMapOf(entries))
	for _, e := range entries {
		// Same source as the list: a model the account reports is known even
		// when the static table has never heard of it (M19 §3).
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
	model, _, err := adapter.NormalizeModel(id, s.defaultModel, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{
			Message: fmt.Sprintf("model %q not found; available models: %s", id, strings.Join(modelIDs(adapter.Usable(entries)), ", ")),
			Type:    "invalid_request_error",
		}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelJSON{ID: model, Object: "model", Created: 1700000000, OwnedBy: "xiaomi"})
}

// storeRedirects caches the superseded-id map.
func (s *Server) storeRedirects(m map[string]string) {
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	s.redirects.m, s.redirects.at = m, time.Now()
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
