package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrKind classifies an upstream/exchange failure for rotation and cooling.
type ErrKind int

const (
	// ErrNone is a zero value (no classification).
	ErrNone ErrKind = iota
	// ErrBudget means the identity has no messages left (lifetime cap).
	ErrBudget
	// ErrBusy means another stream is in flight for the same identity.
	ErrBusy
	// ErrAuth means the token/signature was rejected; refresh then retire.
	ErrAuth
	// ErrLoginRequired means the endpoint is login-only for guests.
	ErrLoginRequired
	// ErrRateLimited means upstream refused for risk-control reasons.
	ErrRateLimited
	// ErrServer is an upstream 5xx or malformed response.
	ErrServer
	// ErrTransport is a network/timeout failure.
	ErrTransport
	// ErrBadRequest means the request itself is malformed (never retry).
	ErrBadRequest
	// ErrUpstream is an error frame inside a stream.
	ErrUpstream
	// ErrIPGate is the per-XFF gate rejection (same 10061 code, quota message).
	ErrIPGate
)

// String renders the kind for logs.
func (k ErrKind) String() string {
	switch k {
	case ErrBudget:
		return "budget_exhausted"
	case ErrBusy:
		return "identity_busy"
	case ErrAuth:
		return "auth_rejected"
	case ErrLoginRequired:
		return "login_required"
	case ErrRateLimited:
		return "rate_limited"
	case ErrServer:
		return "upstream_5xx"
	case ErrTransport:
		return "transport"
	case ErrBadRequest:
		return "bad_request"
	case ErrUpstream:
		return "upstream_error_frame"
	case ErrIPGate:
		return "ip_gate"
	}
	return "none"
}

// APIError is a classified failure. Message is the upstream text (Chinese for
// guest quota errors) and is safe to surface to clients.
type APIError struct {
	Kind       ErrKind
	Status     int // HTTP status to report to the client
	Message    string
	Code       int // upstream business code when present
	RetryAfter time.Duration
	// IdentityError marks failures that should penalise (or retire) the
	// identity rather than rotate transparently.
	IdentityError bool
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s (status=%d code=%d): %s", e.Kind, e.Status, e.Code, e.Message)
}

// Is lets callers match on kinds with errors.Is.
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	if !ok {
		return false
	}
	return e.Kind == t.Kind
}

// Envelope is the non-streaming response envelope:
// {"status":0,"message":"success","result":…,"rid":"…"}.
type Envelope struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
	RID     string          `json:"rid"`
	Code    int             `json:"code"`
}

// Classify maps an upstream HTTP response to an APIError. The guest quota
// errors share code 10061 with the concurrency error, so classification
// branches on the message text (docs/reverse/ANON-SESSION.md §6).
func Classify(status int, body string) *APIError {
	code, message := envelopeCodeMessage(body)
	combined := strings.ToLower(body)
	e := &APIError{Status: status, Code: code, Message: message}
	if e.Message == "" {
		e.Message = strings.TrimSpace(body)
	}

	switch {
	case status == 200 && strings.Contains(body, "您已多次体验过对话"):
		e.Kind = ErrBudget
		e.IdentityError = true
		e.Status = 429
		return e
	case code == 10061 && strings.Contains(body, "请等待其他对话生成完毕"):
		e.Kind = ErrBusy
		e.Status = 429
		return e
	case code == 10061 && (strings.Contains(body, "当前访客资源不足") || status == 429):
		// The message is shared by a per-IP gate and an exhausted identity
		// (docs/reverse/ENDPOINTS.md §Rate/quota). Both are identity-scoped:
		// the handler cools the identity and rotates, while the next health
		// tick reconciles chat_status, which retires an identity that is
		// genuinely out of budget. IdentityError marks it as worth rotating
		// rather than a request-shaped failure.
		e.Kind = ErrIPGate
		e.Status = 429
		e.IdentityError = true
		return e
	case code == 10061:
		// Any other 10061 text is still the guest-quota code (the reverse
		// engineering only pins two of its messages): classify it with the
		// quota family instead of a generic risk-control rate limit.
		e.Kind = ErrIPGate
		e.Status = 429
		e.IdentityError = true
		return e
	case code == 40103:
		e.Kind = ErrLoginRequired
		e.Status = 401
		return e
	case code == 40001 || (status == 400 && strings.Contains(combined, "bad request")):
		e.Kind = ErrAuth
		e.IdentityError = true
		e.Status = 400
		return e
	case status == 400:
		e.Kind = ErrBadRequest
		e.Status = 400
		return e
	case status == 401:
		e.Kind = ErrAuth
		e.IdentityError = true
		e.Status = 401
		return e
	case status == 429:
		e.Kind = ErrRateLimited
		e.Status = 429
		return e
	case status >= 500:
		e.Kind = ErrServer
		e.Status = 502
		return e
	case status >= 400:
		e.Kind = ErrBadRequest
		return e
	}
	return nil
}

// envelopeCodeMessage extracts the business status/code and message from the
// standard envelope. It tolerates both SSE-JSON bodies and HTML error pages.
func envelopeCodeMessage(body string) (int, string) {
	var env Envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return 0, ""
	}
	code := env.Code
	if code == 0 {
		code = env.Status
	}
	return code, strings.TrimSpace(env.Message)
}

// ErrTransportResponse is a convenience constructor for transport failures.
func ErrTransportResponse(err error) *APIError {
	return &APIError{Kind: ErrTransport, Status: 502, Message: err.Error()}
}

// AsAPIError extracts an *APIError from err.
func AsAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}
