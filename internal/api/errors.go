package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// encodeJSON writes v as a JSON response body.
func encodeJSON(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// apiError is the OpenAI-shaped error body (SDD §7.4):
// {"error":{"message","type","code"}}.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type errorBody struct {
	Error apiError `json:"error"`
}

// writeError emits the OpenAI error shape with the given status. retryAfter
// (seconds) adds a Retry-After header when > 0 — local recovery deadlines
// only, never account identity (TDD ERRMAP-011).
func writeError(w http.ResponseWriter, status int, typ, code, message string, retryAfter int) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{Message: message, Type: typ, Code: code}})
}

// Sentinel request-level errors (chat.go raises these before the upstream
// call; MapError translates them per SDD §7.4).
var (
	errContextTooLong     = errors.New("query exceeds channel character cap")
	errUnsupportedContent = errors.New("unsupported content part")
	errMalformedRequest   = errors.New("malformed request body")
	// errUnsupportedSearchStatus: a search switch outside disabled|auto|
	// enabled would be silently coerced upstream, hiding a client typo
	// (M19).
	errUnsupportedSearchStatus = errors.New("unsupported web_search_status")
)

// MapError translates a chat-pipeline error into the §7.4 HTTP response.
// Call only BEFORE the first byte reaches the client; post-first-byte
// stream failures take the SSE error-frame path instead (TDD API-021,
// ERRMAP-012).
func (s *Server) MapError(w http.ResponseWriter, err error) {
	var uv *unknownVoiceError
	var (
		parked  *pool.ErrParked
		busy    *pool.ErrBusy
		nf      *adapter.ErrModelNotFound
		nouser  adapter.ErrNoUserMessage
		tooMany *adapter.ErrTooManyTurns
		filter  *mimo.ErrContentFilter
		upErr   *mimo.UpstreamError
	)
	switch {
	case errors.As(err, &parked):
		// All accounts cooling down (SDD §7.4, TDD ERRMAP-003).
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "pool_parked",
			"all accounts are cooling down; retry later", parked.RetryAfterSeconds())
	case errors.As(err, &busy):
		// Queue wait exceeded (SDD §7.4, TDD ERRMAP-004).
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "pool_busy",
			"all account slots busy; retry later", busy.RetryAfterSeconds())
	case errors.Is(err, pool.ErrNoAccounts):
		// Empty pool / all disabled (SDD §7.4, TDD ERRMAP-005).
		writeError(w, http.StatusServiceUnavailable, "api_error", "no_accounts",
			"no accounts available", 0)
	case errors.As(err, &nf):
		// Unknown model → 404 naming available ids (SDD §5.2, TDD API-013).
		writeError(w, http.StatusNotFound, "invalid_request_error", "", nf.Error(), 0)
	case errors.As(err, &nouser):
		// No sendable user message (SDD §7.4, TDD ERRMAP-002/API-014).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "", nouser.Error(), 0)
	case errors.As(err, &tooMany):
		// Replay cap exceeded (SDD §5.3B, TDD API-015).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "", tooMany.Error(), 0)
	case errors.As(err, &filter):
		// sensitive_query / sensitive_title (SDD §7.4, TDD ERRMAP-009).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "content_filter",
			"upstream content filter rejected the query", 0)
	case errors.Is(err, errContextTooLong):
		// Channel char cap (SDD §2.8, TDD ERRMAP-010/API-016).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "context_too_long",
			"query exceeds the model channel's character limit", 0)
	case errors.Is(err, errUnsupportedSearchStatus):
		// Search switch (M19, TDD WS-002).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_search_status",
			"web_search_status must be one of: disabled, auto, enabled", 0)
	case errors.Is(err, errUnsupportedContent):
		// Image content parts (SDD §5.2, TDD API-012).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "",
			"image content parts are not supported; use text parts", 0)
	case errors.Is(err, errMalformedRequest):
		// Malformed JSON body (SDD §7.4, TDD ERRMAP-002).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "",
			"malformed request body", 0)
	case errors.Is(err, mimo.ErrUpstreamModelRejected):
		// The upstream channel refuses this model id (docs/web-surface-reverse.md
		// §11.1). Surfaced as 400: the request cannot succeed as written, and
		// retrying it is pointless.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_not_available_on_channel",
			"upstream rejected the model name for this channel: "+err.Error(), 0)
	case errors.As(err, &uv):
		// The client named a voice this account does not have (audio.go).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown_voice", uv.Error(), 0)
	case errors.Is(err, mimo.ErrAudioTimeout):
		writeError(w, http.StatusGatewayTimeout, "api_error", "upstream_audio_timeout",
			"the upstream audio task did not finish in time: "+err.Error(), 0)
	case errors.Is(err, errAudioTextTooLong):
		// The upstream's own 1024-character cap (TTS-HARDENING): the caller's
		// text is the problem, so answer 400 rather than a 502.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input_too_long",
			"the upstream refuses speech longer than 1024 characters per request; "+err.Error(), 0)
	case errors.Is(err, mimo.ErrAudioFailed):
		writeError(w, http.StatusBadGateway, "api_error", "upstream_audio_failed",
			"the upstream audio task failed: "+err.Error(), 0)
	case errors.Is(err, mimo.ErrUpstreamBusy):
		// Transient platform throttle that outlived the retry (SDD §7.1).
		// 503 + a short Retry-After, not a 502: the request is fine.
		writeError(w, http.StatusServiceUnavailable, "api_error", "upstream_busy",
			"upstream is busy; retry shortly", busyRetryAfterSeconds)
	case isAuthErr(err):
		// Upstream 401 surviving the refresh retry (SDD §7.4 row 8, TDD
		// ERRMAP-007/RENEW-006).
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "upstream_auth_expired",
			"upstream session expired and could not be refreshed", 0)
	case errors.As(err, &upErr):
		switch {
		case upErr.StatusCode == http.StatusTooManyRequests:
			// Upstream 429 surfaced (SDD §7.4, TDD ERRMAP-006).
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "upstream_rate_limited",
				"upstream rate limited the account", 0)
		default:
			// Upstream 5xx and everything else upstream-shaped (SDD §7.4,
			// TDD ERRMAP-008).
			writeError(w, http.StatusBadGateway, "api_error", "upstream_failure",
				fmt.Sprintf("upstream error %d", upErr.StatusCode), 0)
		}
	default:
		// Transport / stall / unexpected (SDD §7.4 row 9, TDD ERRMAP-008).
		writeError(w, http.StatusBadGateway, "api_error", "upstream_failure",
			"upstream failure: "+err.Error(), 0)
	}
}

// isAuthErr matches both mimo and pool auth-expired wrappers.
func isAuthErr(err error) bool {
	var ae interface{ IsAuthExpired() bool }
	return errors.As(err, &ae) && ae.IsAuthExpired()
}
