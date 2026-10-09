// Package api implements the OpenAI-compatible HTTP surface: endpoints
// (SDD §5.1), Bearer auth middleware (§5.1), request mapping (§5.2), and
// error translation (§7.4).
package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
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
	otp      *otpStore // new-device OTP challenges (SDD §2.2 amendment)

	apiKeys      [][]byte // parsed keys; empty slice = auth open (SDD §5.1)
	defaultModel string
	replayCap    int
	keepalive    time.Duration
	logger       *log.Logger
}

// Options configures a Server.
type Options struct {
	APIKeys      []string     // parsed list; empty = open auth
	Pool         *pool.Pool
	Accounts     *accounts.Store
	Sessions     *session.Store
	Client       *mimo.Client
	DefaultModel string       // "auto"/missing model resolution (SDD §5.2)
	ReplayCap    int          // max replayed user turns (SDD §5.3B, default 20)
	Keepalive    time.Duration // SSE keepalive interval (SDD §5.4, default 15s)
	Logger       *log.Logger
}

// New builds a Server. All collaborators must be wired (real or test
// doubles).
func New(opts Options) *Server {
	if opts.DefaultModel == "" {
		opts.DefaultModel = adapter.DefaultModels[0]
	}
	if opts.ReplayCap <= 0 {
		opts.ReplayCap = 20
	}
	if opts.Keepalive <= 0 {
		opts.Keepalive = 15 * time.Second
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
		pool:         opts.Pool,
		store:        opts.Accounts,
		sessions:     opts.Sessions,
		client:       opts.Client,
		otp:          newOTPStore(),
		apiKeys:      keys,
		defaultModel: opts.DefaultModel,
		replayCap:    opts.ReplayCap,
		keepalive:    opts.Keepalive,
		logger:       logger,
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
	// Admin OTP endpoints (SDD §2.2 amendment) — same Bearer auth.
	mux.HandleFunc("GET /v1/admin/otp/pending", s.auth(s.handleOTPPending))
	mux.HandleFunc("POST /v1/admin/otp/submit", s.auth(s.handleOTPSubmit))
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

// modelJSON is the OpenAI model object (SDD §5.1).
type modelJSON struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels lists the catalog in OpenAI list shape (TDD API-005).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.modelsList())
}

func (s *Server) modelsList() map[string]any {
	data := make([]modelJSON, 0, len(adapter.DefaultModels))
	for _, m := range adapter.DefaultModels {
		data = append(data, modelJSON{ID: m, Object: "model", Created: 1700000000, OwnedBy: "xiaomi"})
	}
	return map[string]any{"object": "list", "data": data}
}

// handleModelByID returns one model or 404 listing available ids (TDD
// API-006: known → 200; alias → 200 after normalization; unknown → 404).
func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	model, _, err := adapter.NormalizeModel(id, s.defaultModel, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{
			Message: fmt.Sprintf("model %q not found; available models: %s", id, strings.Join(adapter.DefaultModels, ", ")),
			Type:    "invalid_request_error",
		}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelJSON{ID: model, Object: "model", Created: 1700000000, OwnedBy: "xiaomi"})
}
