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
// {"error":{"message","type","param","code"}}. param names the request field a
// parameter-level failure belongs to, the way OpenAI's own errors do
// ("param":"temperature"); it is null rather than invented for failures that are
// not about a single field.
type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	// Code is a pointer with no omitempty: OpenAI's error object always carries the
	// key (null when there is no code), and this project's rule is that a key OpenAI
	// sends as null is sent as null rather than omitted — the same treatment
	// message.refusal and choices[0].logprobs get. Without this, a client validating
	// against that schema had to branch on the key's absence (TDD OSTD-013).
	Code *string `json:"code"`
}

// codeOf maps an internal code onto the wire value: nil — JSON null — when the
// failure has no code of its own.
func codeOf(code string) *string {
	if code == "" {
		return nil
	}
	return &code
}

// errorParams maps the codes that ARE about one request field onto it. A code
// missing here deliberately reports param:null instead of guessing a field.
var errorParams = map[string]string{
	"unsupported_reasoning_effort":    "reasoning_effort",
	"unsupported_search_context_size": "web_search_options.search_context_size",
	"unsupported_image_detail":        "messages[].content[].image_url.detail",
	"unsupported_image_url":           "messages[].content[].image_url.url",
	"unsupported_image_type":          "messages[].content[].image_url.url",
	"image_fetch_failed":              "messages[].content[].image_url.url",
	"image_url_blocked":               "messages[].content[].image_url.url",
	"image_too_large":                 "messages[].content[].image_url.url",
	"too_many_images":                 "messages",
	"context_too_long":                "messages",
}

// paramForCode returns the field name to report for a code, or nil.
func paramForCode(code string) *string {
	if p, ok := errorParams[code]; ok {
		return &p
	}
	return nil
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
	_ = json.NewEncoder(w).Encode(errorBody{Error: apiError{
		Message: message, Type: typ, Param: paramForCode(code), Code: codeOf(code),
	}})
}

// Sentinel request-level errors (chat.go raises these before the upstream
// call; MapError translates them per SDD §7.4).
var (
	errMalformedRequest = errors.New("malformed request body")
	// errUnsupportedReasoningEffort: a reasoning_effort outside the OpenAI set
	// (none|minimal|low|medium|high).
	errUnsupportedReasoningEffort = errors.New("unsupported reasoning_effort")
	// errUnsupportedSearchContextSize: web_search_options.search_context_size
	// outside low|medium|high.
	errUnsupportedSearchContextSize = errors.New("unsupported search_context_size")
	// errImageURLBlocked: a client-supplied image URL pointing at loopback,
	// private or link-local space. A named 400 rather than a generic fetch
	// failure: "I refused to go there" and "I went there and it broke" are
	// different answers, and only one of them is the client's to fix.
	errImageURLBlocked = errors.New("image URL points at a blocked address")
	// errUnsupportedSearchStatus: a search switch outside disabled|auto|
	// enabled would be silently coerced upstream, hiding a client typo
	// (M19).
	errUnsupportedSearchStatus = errors.New("unsupported web_search_status")

	// Image input (IMG-*). Each one is a 400 with its own code: an image is the
	// only request field that can fail for a reason the client must fix (a URL
	// we cannot fetch, a file that is not an image, a file that is too big).
	errUnsupportedImageURL  = errors.New("unsupported image_url")
	errImageFetch           = errors.New("image fetch failed")
	errUnsupportedImageType = errors.New("unsupported image type")
	errImageTooLarge        = errors.New("image too large")
)

// unsupportedImageDetailError is an image_url.detail outside auto|low|high
// (IMG-010). A type because the message names the offending value.
type unsupportedImageDetailError struct {
	Detail string
}

func (e *unsupportedImageDetailError) Error() string {
	return fmt.Sprintf("image_url.detail %q is not one of: auto, low, high", e.Detail)
}

// asImageDetail extracts *unsupportedImageDetailError from a wrapped error.
func asImageDetail(err error) *unsupportedImageDetailError {
	var e *unsupportedImageDetailError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// tooManyImagesError is more image parts in one request than the client-facing
// cap (IMG-003). A type rather than a sentinel because the message has to carry
// the numbers.
type tooManyImagesError struct {
	Count int
	Max   int
}

func (e *tooManyImagesError) Error() string {
	return fmt.Sprintf("request carries %d images, over the cap of %d (MIMO2API_MAX_IMAGES)", e.Count, e.Max)
}

// asTooManyImages extracts *tooManyImagesError from a wrapped error.
func asTooManyImages(err error) *tooManyImagesError {
	var e *tooManyImagesError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

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
	// Lease is true when the silence window (MIMO2API_LEASE_TTL_S) was the bound
	// that fired rather than the wall clock. The two knobs are named in the
	// message, so the caller has to know which one bit.
	Lease bool
}

// Error names the knob the operator has to turn. Naming only the bound that
// actually fired matters: on the shipped default (REQUEST_TIMEOUT_MS=0) the bound
// is the silence window, and pointing at a disabled knob sends an operator to the
// wrong environment variable (ADR 0036).
func (e *requestTimeoutError) Error() string {
	knob := "MIMO2API_REQUEST_TIMEOUT_MS"
	if e.Lease {
		knob = "MIMO2API_LEASE_TTL_S"
	}
	where := e.Where
	if where == "" {
		where = "waiting for its first byte"
	}
	return fmt.Sprintf("the upstream stayed silent for the gateway's %s %s; "+
		"the gateway gave up on the request (raise %s if the platform is simply slow). "+
		"Retry, or send fewer messages per request", e.Budget.Round(time.Second), where, knob)
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
		// OpenAI's own code for this failure is `model_not_found`, so a client that
		// switches on it keeps working.
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", nf.Error(), 0)
	case errors.As(err, &nouser):
		// No sendable user message (SDD §7.4, TDD ERRMAP-002/API-014).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "no_user_message", nouser.Error(), 0)
	case errors.As(err, &tooMany):
		// Replay cap exceeded (SDD §5.3B, TDD API-015).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "too_many_turns", tooMany.Error(), 0)
	case errors.As(err, &filter):
		// sensitive_query / sensitive_title (SDD §7.4, TDD ERRMAP-009). The
		// platform's own event name and wording reach the client verbatim
		// (ADR 0033): a caller that sees only "the query was rejected" cannot tell
		// a moderation judgement from a broken request.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "content_filter",
			filter.Error(), 0)
	case errors.As(err, &tooLong):
		// Channel char cap (SDD §2.8, TDD ERRMAP-010/API-016). The message names
		// the measured size and the limit, so a caller can act on it.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "context_too_long", tooLong.Error(), 0)
	case errors.Is(err, errUnsupportedImageURL):
		// Name the real reason: the wrapped error says which rule was broken
		// ("data URL has no comma before its payload"), which is what a caller can
		// act on, instead of only restating the accepted shape.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_image_url", err.Error(), 0)
	case errors.Is(err, errImageURLBlocked):
		writeError(w, http.StatusBadRequest, "invalid_request_error", "image_url_blocked", err.Error(), 0)
	case errors.Is(err, errImageFetch):
		// The client pointed at an image we could not retrieve; the message
		// carries the reason (status, network, timeout) via the wrapped error.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "image_fetch_failed", err.Error(), 0)
	case errors.Is(err, errUnsupportedImageType):
		// There are three ways to land here — a URL that answered something that is
		// not an image, a data: URL whose declared subtype is not a plain image type,
		// and bytes that are not an image at all — and the wrapped error says which.
		// The canned "the fetched URL did not answer an image" was only ever true for
		// the first, so a client posting an inline image was told a fetch had failed.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_image_type", err.Error(), 0)
	case asImageDetail(err) != nil:
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_image_detail",
			asImageDetail(err).Error(), 0)
	case errors.Is(err, errImageTooLarge):
		writeError(w, http.StatusBadRequest, "invalid_request_error", "image_too_large", err.Error(), 0)
	case asTooManyImages(err) != nil:
		writeError(w, http.StatusBadRequest, "invalid_request_error", "too_many_images",
			asTooManyImages(err).Error(), 0)
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
	case errors.Is(err, errMalformedRequest):
		// Malformed JSON body (SDD §7.4, TDD ERRMAP-002). The code matches the audio
		// surface's (`audio.go`), because it is the same failure: one name for one
		// meaning, and never an empty code (ADR 0032).
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed_request_body",
			"malformed request body", 0)
	case errors.Is(err, mimo.ErrUpstreamModelRejected):
		// The upstream channel refuses this model id (docs/web-surface-reverse.md
		// §11.1). Surfaced as 400: the request cannot succeed as written, and
		// retrying it is pointless.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_not_available_on_channel",
			withUpstream("the upstream channel does not serve this model id", err), 0)
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
			withUpstream("upstream is busy; retry shortly", err), busyRetryAfterSeconds)
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

// withUpstream appends the platform's own wording, verbatim, to a sentence we
// wrote (ADR 0033). When the failure carried no upstream text the sentence stands
// alone, so callers never see a dangling colon.
func withUpstream(ours string, err error) string {
	if m := strings.TrimSpace(mimo.UpstreamMessage(err)); m != "" && !strings.Contains(ours, m) {
		return ours + ": " + m
	}
	return ours
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
