package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// encodeJSON writes v as a JSON response body. Plain io.Writer: the chat path
// encodes into a buffer first so the in-flight slot can go back before anything
// is written to the client (ADR 0019).
func encodeJSON(w io.Writer, v any) error {
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
	errUnsupportedContent = errors.New("unsupported content part")
	errMalformedRequest   = errors.New("malformed request body")
	// errUnsupportedReasoningEffort: a reasoning_effort outside the OpenAI set
	// (none|minimal|low|medium|high).
	errUnsupportedReasoningEffort = errors.New("unsupported reasoning_effort")
	// errUnsupportedSearchContextSize: web_search_options.search_context_size
	// outside low|medium|high.
	errUnsupportedSearchContextSize = errors.New("unsupported search_context_size")
	// errUnsupportedSearchStatus: a search switch outside disabled|auto|
	// enabled would be silently coerced upstream, hiding a client typo
	// (M19).
	errUnsupportedSearchStatus = errors.New("unsupported web_search_status")
)

// queryTooLongError names the number that matters: a caller that only learns
// "the query is too long" cannot act, one that learns "92311 chars over a 90000
// limit on this model" can trim. The code stays `context_too_long` so clients
// that key on it keep working.
type queryTooLongError struct {
	Limit  int
	Actual int
	Model  string
	// Overhead is how much of Actual is preamble the client did not write —
	// the system prompt it sent plus any protocol prompt a tool layer injected
	// before us (ADR 0015). Zero for a plain user message.
	Overhead int
}

func (e *queryTooLongError) Error() string {
	suffix := ""
	if e.Overhead > 0 {
		suffix = fmt.Sprintf(" (of which %d chars are the injected/system preamble, %d are the message)",
			e.Overhead, e.Actual-e.Overhead)
	}
	return fmt.Sprintf("query is too long for this model channel: %d chars, limit %d (model %q);%s send fewer or shorter messages",
		e.Actual, e.Limit, e.Model, suffix)
}

// streamUnsupportedError says exactly what to change: this deployment serves
// non-streaming chat only (ADR 0016), and a client that asks for streaming gets
// a named failure rather than a silently different response shape. The tool
// layer that needed SSE is gone, and a non-streaming turn is one upstream request
// whose whole answer arrives at once.
type streamUnsupportedError struct{}

func (e *streamUnsupportedError) Error() string {
	return "streaming is disabled on this deployment; send \"stream\": false (the answer arrives as a single JSON response)"
}

// upstreamTooLong reports whether an upstream refusal is about the request
// being too long, across the wordings the platform uses: the chat channel
// answers `Sorry, the text you sent is too long!`, the fastchat channel
// `query is too long`, and the audio surface `长度不能超过`.
func upstreamTooLong(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	for _, marker := range []string{"too long", "query is too long", "context_too_long", "too_long"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return strings.Contains(s, "长度不能超过") || strings.Contains(s, "内容过长") ||
		strings.Contains(s, "文本过长")
}

// upstreamTooLongMessage is the client-facing translation, with the measured
// ceilings (ADR 0015): 50,000 characters on the ultraspeed/fastchat channel and
// 102,400 elsewhere, counted on the query text the platform receives — preamble
// included.
func upstreamTooLongMessage() string {
	return "the upstream refused this request as too long; send fewer or shorter messages " +
		"(measured ceilings: 50000 characters on the ultraspeed channel, 102400 on other models, " +
		"counted on the composed query text — a system prompt or injected tool-protocol prompt " +
		"uses the same budget)"
}

// MapError translates a chat-pipeline error into the §7.4 HTTP response.
// Call only BEFORE the first byte reaches the client; post-first-byte
// stream failures take the SSE error-frame path instead (TDD API-021,
// ERRMAP-012).
// requestTimeoutError: the gateway's own deadline fired (ADR 0019). It is not an
// upstream failure and not the account's fault, so it neither cools the account
// nor retries — the caller gets a 504 naming the knob that governs it.
type requestTimeoutError struct {
	Budget time.Duration
	Where  string
}

func (e *requestTimeoutError) Error() string {
	where := e.Where
	if where == "" {
		where = "waiting for the upstream"
	}
	return fmt.Sprintf("the upstream did not answer within the gateway's %s budget while %s; "+
		"the gateway aborted the request (MIMO2API_REQUEST_TIMEOUT_MS / MIMO2API_LEASE_TTL_S). "+
		"Retry, send fewer messages per request, or raise the budget", e.Budget.Round(time.Second), where)
}

func (s *Server) MapError(w http.ResponseWriter, err error) {
	var uv *unknownVoiceError
	var deadline *requestTimeoutError
	var (
		parked  *pool.ErrParked
		busy    *pool.ErrBusy
		nf      *adapter.ErrModelNotFound
		nouser  adapter.ErrNoUserMessage
		tooMany *adapter.ErrTooManyTurns
		filter  *mimo.ErrContentFilter
		upErr   *mimo.UpstreamError
		tooLong *queryTooLongError
		noSSE   *streamUnsupportedError
	)
	switch {
	case errors.As(err, &deadline):
		// Our own budget/TTL (ADR 0019): a gateway-side timeout, with numbers,
		// and no account penalty — the account was serving correctly.
		writeError(w, http.StatusGatewayTimeout, "api_error", "chat_timeout", deadline.Error(), 0)
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
	case errors.As(err, &noSSE):
		// A streaming request on a non-streaming deployment (ADR 0016). Named,
		// with the fix, because the client can act on it.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "stream_unsupported",
			noSSE.Error(), 0)
	case errors.As(err, &tooLong):
		// Channel char cap (SDD §2.8, TDD ERRMAP-010/API-016). The message names
		// the measured size and the limit, so a caller can act on it.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "context_too_long", tooLong.Error(), 0)
	case errors.Is(err, errUnsupportedReasoningEffort):
		// OpenAI's standard thinking knob, spelled wrong (OSTD-003).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_reasoning_effort",
			"reasoning_effort must be one of: none, minimal, low, medium, high", 0)
	case errors.Is(err, errUnsupportedSearchContextSize):
		// OpenAI's search_context_size, spelled wrong (OSTD-005).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_search_context_size",
			"web_search_options.search_context_size must be one of: low, medium, high", 0)
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
		// The status alone cannot explain an upstream refusal, and the body is
		// where the reason lives ("modelCode cannot be empty", an audit refusal, a
		// conversation/model mismatch). Debug level only: it is an upstream body,
		// not something to print in normal operation or return to a client.
		if s.debug && s.logger != nil {
			s.logger.Printf("upstream %d: %s", upErr.StatusCode, truncateForLog(upErr.Body, 400))
		}
		switch {
		case upErr.StatusCode == http.StatusTooManyRequests:
			// Upstream 429 surfaced (SDD §7.4, TDD ERRMAP-006).
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "upstream_rate_limited",
				"upstream rate limited the account", 0)
		case upstreamTooLong(upErr.Body):
			// The upstream refused the request as too long (ADR 0015): its own
			// wording is prose ("Sorry, the text you sent is too long!") in a 5xx
			// envelope, which a client cannot act on. Answer the client's mistake
			// with the client's status.
			writeError(w, http.StatusBadRequest, "invalid_request_error", "context_too_long",
				upstreamTooLongMessage(), 0)
		default:
			// Upstream 5xx and everything else upstream-shaped (SDD §7.4,
			// TDD ERRMAP-008).
			writeError(w, http.StatusBadGateway, "api_error", "upstream_failure",
				fmt.Sprintf("upstream error %d", upErr.StatusCode), 0)
		}
	default:
		if upstreamTooLong(err.Error()) {
			// Same refusal, arriving as a mid-stream `error` event (the shape the
			// platform actually uses for the regular channel).
			writeError(w, http.StatusBadRequest, "invalid_request_error", "context_too_long",
				upstreamTooLongMessage(), 0)
			return
		}
		// Transport / stall / unexpected (SDD §7.4 row 9, TDD ERRMAP-008).
		writeError(w, http.StatusBadGateway, "api_error", "upstream_failure",
			"upstream failure: "+err.Error(), 0)
	}
}

// truncateForLog bounds a string destined for a log line.
func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isAuthErr matches both mimo and pool auth-expired wrappers.
func isAuthErr(err error) bool {
	var ae interface{ IsAuthExpired() bool }
	return errors.As(err, &ae) && ae.IsAuthExpired()
}
