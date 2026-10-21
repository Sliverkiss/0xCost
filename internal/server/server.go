package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"chatglm2api/internal/config"
	"chatglm2api/internal/pool"
	"chatglm2api/internal/upstream"
)

// ServiceName identifies the gateway in health payloads.
const ServiceName = "chatglm2api"

// Egress chooses the X-Forwarded-For value for one upstream call.
type Egress interface {
	Next() string
}

// Config is the server's dependency set.
type Config struct {
	Config   *config.Config
	Pool     *pool.Pool
	Upstream *upstream.Client
	Egress   Egress
	// APIKey enables client authentication; empty disables it.
	APIKey string
	// ModelFetcher optionally refreshes the advertised model list from the
	// upstream catalogue. Nil keeps the configured list.
	ModelFetcher func(ctx context.Context) ([]string, error)
}

// Server is the HTTP handler.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	maxBody int64

	modelsMu  sync.RWMutex
	models    []string
	modelsAt  time.Time
	modelsTTL time.Duration
	// fetchMu serialises upstream catalogue refreshes so a burst of
	// /v1/models requests performs one lease + fetch, not one each.
	fetchMu sync.Mutex

	// startPacer enforces pace.global_qps across all chat starts.
	startPacer *pacer

	// wg tracks background maintenance (conversation cleanup) so a shutdown
	// can wait for in-flight deletes instead of racing the upstream client.
	wg sync.WaitGroup
}

// Wait blocks until background maintenance started by handlers has finished.
// It is intended for a bounded shutdown wait.
func (s *Server) Wait() { s.wg.Wait() }

// pacer is a token bucket for process-wide chat start rate limiting.
type pacer struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newPacer(qps float64, burst float64) *pacer {
	if qps <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &pacer{rate: qps, burst: burst, tokens: burst, last: time.Now()}
}

// Wait blocks until a token is available or the context ends.
func (p *pacer) Wait(ctx context.Context) bool {
	if p == nil {
		return true
	}
	for {
		p.mu.Lock()
		now := time.Now()
		p.tokens += now.Sub(p.last).Seconds() * p.rate
		if p.tokens > p.burst {
			p.tokens = p.burst
		}
		p.last = now
		if p.tokens >= 1 {
			p.tokens--
			p.mu.Unlock()
			return true
		}
		wait := time.Duration((1 - p.tokens) / p.rate * float64(time.Second))
		p.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false
		}
	}
}

// New builds the server and registers routes.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux(), maxBody: 8 << 20, modelsTTL: time.Hour}
	if cfg.Config != nil {
		s.startPacer = newPacer(cfg.Config.Pace.GlobalQPS, cfg.Config.Pace.GlobalQPS)
	}
	s.mux.HandleFunc("POST /v1/chat/completions", s.withAuth(s.handleChat))
	s.mux.HandleFunc("GET /v1/models", s.withAuth(s.handleModels))
	s.mux.HandleFunc("GET /v1/models/{id}", s.withAuth(s.handleModel))
	s.mux.HandleFunc("GET /status", s.withAuth(s.handleStatus))
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.cfg.APIKey
		if key == "" {
			next(w, r)
			return
		}
		authz := r.Header.Get("Authorization")
		provided := strings.TrimPrefix(authz, "Bearer ")
		if !strings.HasPrefix(authz, "Bearer ") ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// handleHealth reports liveness plus pool servability.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	if !s.cfg.Pool.Servable() {
		status = http.StatusServiceUnavailable
	}
	c := s.cfg.Pool.Counts()
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"service":   ServiceName,
		"status":    map[bool]string{true: "ok", false: "unavailable"}[status == http.StatusOK],
		"waiters":   c.Waiters,
		"stock":     c.Stock,
		"total":     c.Total,
		"in_flight": c.InFlight,
	})
}

// handleStatus returns the pool observability payload.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.cfg.Pool.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":      ServiceName,
		"pool":         st,
		"breaker_open": s.cfg.Pool.BreakerOpen(),
	})
}

// handleModels lists the advertised models.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ids := s.modelIDs(r.Context())
	now := time.Now().Unix()
	data := make([]modelObject, 0, len(ids))
	for _, id := range ids {
		data = append(data, modelObject{ID: id, Object: "model", Created: now, OwnedBy: "chatglm"})
	}
	writeJSON(w, http.StatusOK, modelsResponse{Object: "list", Data: data})
}

// handleModel returns one model or a 404 envelope.
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, m := range s.modelIDs(r.Context()) {
		if m == id {
			writeJSON(w, http.StatusOK, modelObject{ID: m, Object: "model", Created: time.Now().Unix(), OwnedBy: "chatglm"})
			return
		}
	}
	writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model "+id+" does not exist")
}

// modelIDs merges the configured catalogue with the upstream list, refreshing
// it at most once per TTL when a fetcher is configured. Concurrent callers
// share one refresh: the fetch leases a pooled identity and makes a remote
// call, so a stampede of /v1/models requests must not multiply it.
func (s *Server) modelIDs(ctx context.Context) []string {
	cfgIDs := make([]string, 0, len(s.cfg.Config.Chat.Models))
	seen := map[string]bool{}
	for _, m := range s.cfg.Config.Chat.Models {
		if !seen[m.ID] {
			cfgIDs = append(cfgIDs, m.ID)
			seen[m.ID] = true
		}
	}
	cached := s.cachedModels()
	if s.cfg.ModelFetcher != nil && (cached == nil || s.modelsStale()) {
		cached = s.refreshModels(ctx)
	}
	out := append([]string(nil), cfgIDs...)
	for _, id := range cached {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *Server) cachedModels() []string {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	return append([]string(nil), s.models...)
}

func (s *Server) modelsStale() bool {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	return s.models == nil || time.Since(s.modelsAt) > s.modelsTTL
}

// refreshModels runs at most one fetcher call at a time and returns the
// freshest cached list (the caller's own failure falls back to the cache).
func (s *Server) refreshModels(ctx context.Context) []string {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	// Another caller may have refreshed while we waited for the lock.
	if cached := s.cachedModels(); cached != nil && !s.modelsStale() {
		return cached
	}
	up, err := s.cfg.ModelFetcher(ctx)
	if err != nil || len(up) == 0 {
		return s.cachedModels()
	}
	s.modelsMu.Lock()
	s.models, s.modelsAt = up, time.Now()
	s.modelsMu.Unlock()
	return up
}

// Admin mutations. They are only registered when AdminEnabled is set.

// EnableAdmin registers the operator endpoints on the server mux. The server
// binary only calls this when config.admin.enabled is true and an API key is
// configured, so mutating routes are never exposed unauthenticated.
func (s *Server) EnableAdmin() {
	s.mux.HandleFunc("POST /admin/identities/acquire", s.withAuth(s.handleAdminAcquire))
	s.mux.HandleFunc("POST /admin/identities/{uid}/retire", s.withAuth(s.handleAdminRetire))
	s.mux.HandleFunc("GET /admin/identities/{uid}/status", s.withAuth(s.handleAdminProbe))
	s.mux.HandleFunc("POST /admin/pool/refill", s.withAuth(s.handleAdminRefill))
	s.mux.HandleFunc("GET /admin/export", s.withAuth(s.handleAdminExport))
}

func (s *Server) handleAdminAcquire(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id, err := s.cfg.Pool.AcquireOne(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "acquire_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": id.UID})
}

func (s *Server) handleAdminRetire(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_uid", "uid is required")
		return
	}
	if !s.cfg.Pool.Remove(uid) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "no such identity")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "removed": true})
}

// handleAdminProbe reads one identity's authoritative chat_status upstream.
// The call costs no message, so it is safe for operators; it is the live
// counterpart of the periodic health reconciliation.
func (s *Server) handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_uid", "uid is required")
		return
	}
	id := s.cfg.Pool.Inspect(uid)
	if id == nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "no such identity")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := s.cfg.Upstream.FetchChatStatus(ctx, id.AccessToken, id.DeviceID, id.UserAgent(), s.xff())
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "probe_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "total": st.Total, "has_chance": st.HasChance,
	})
}

func (s *Server) handleAdminRefill(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	attempted := s.cfg.Pool.Refill(ctx)
	st := s.cfg.Pool.Status()
	writeJSON(w, http.StatusOK, map[string]any{"attempted": attempted, "stock": st.Stock, "warm_size": st.WarmSize})
}

func (s *Server) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	raw, err := s.cfg.Pool.Export()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "export_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
