package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// nowFunc is replaceable in tests; production uses the wall clock.
var nowFunc = time.Now

// Endpoint paths (docs/reverse/ENDPOINTS.md). The leading /chatglm is included.
const (
	PathGuestAccess   = "/chatglm/user-api/guest/access"
	PathRefresh       = "/chatglm/user-api/user/refresh"
	PathUserInfo      = "/chatglm/user-api/user/info"
	PathChatStatus    = "/chatglm/mainchat-api/guest/chat_status"
	PathChatStream    = "/chatglm/backend-api/assistant/stream"
	PathConvDelete    = "/chatglm/mainchat-api/conversation/delete"
	PathModels        = "/chatglm/agent-api/operation/detail?tag=available_models"
	PathAssistantInfo = "/chatglm/backend-api/assistant/info"
)

// Client is the upstream HTTP client. One instance is shared by the process;
// it is safe for concurrent use.
type Client struct {
	BaseURL    string
	HTTP       *http.Client
	StreamHTTP *http.Client
	// HeaderTimeout bounds the wait for SSE response headers; IdleTimeout
	// bounds the gap between frames once streaming.
	HeaderTimeout time.Duration
	IdleTimeout   time.Duration
}

// New builds a client with HTTP/2 enabled and no custom TLS or ALPN settings
// (a fingerprint the real browser would not produce).
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "https://chatglm.cn"
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		HTTP:          &http.Client{Timeout: 30 * time.Second, Transport: tr},
		StreamHTTP:    &http.Client{Transport: tr},
		HeaderTimeout: 60 * time.Second,
		IdleTimeout:   80 * time.Second,
	}
}

// IdentityTokens is the acquisition/refresh result.
type IdentityTokens struct {
	UID          string `json:"user_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IsGuest      bool   `json:"is_guest"`
	Mode         int    `json:"mode"`
}

// AcquireGuest creates a fresh anonymous identity. deviceID is the value the
// gateway minted for the X-Device-Id header (the JWT carries its own).
func (c *Client) AcquireGuest(ctx context.Context, deviceID string, ua UserAgent, xff string) (*IdentityTokens, error) {
	var out IdentityTokens
	if err := c.doJSON(ctx, http.MethodPost, PathGuestAccess, "", deviceID, ua, xff, kindJSON, map[string]any{}, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.UID == "" {
		return nil, &APIError{Kind: ErrServer, Status: 502, Message: "guest/access returned an incomplete identity"}
	}
	return &out, nil
}

// Refresh swaps the access token using the refresh token. The upstream rotates
// the access token; the returned RefreshToken may be empty when unchanged.
//
// The disposable guest pool never calls this: a cartridge serves one request
// well inside the 24 h access-token lifetime, so refreshing would only preserve
// a half-used identity. The method stays for a future logged-in account mode.
func (c *Client) Refresh(ctx context.Context, refreshToken, deviceID string, ua UserAgent, xff string) (*IdentityTokens, error) {
	var out IdentityTokens
	if err := c.doJSON(ctx, http.MethodPost, PathRefresh, refreshToken, deviceID, ua, xff, kindJSON, map[string]any{}, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, &APIError{Kind: ErrAuth, Status: 401, Message: "refresh returned no access token"}
	}
	return &out, nil
}

// UserInfo is the guest profile.
type UserInfo struct {
	ID       string `json:"_id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	IsGuest  bool   `json:"is_guest"`
}

// FetchUserInfo reads the identity profile (no budget cost).
func (c *Client) FetchUserInfo(ctx context.Context, accessToken, deviceID string, ua UserAgent, xff string) (*UserInfo, error) {
	var out UserInfo
	if err := c.doJSON(ctx, http.MethodGet, PathUserInfo, accessToken, deviceID, ua, xff, kindJSON, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChatStatus is the authoritative budget reading.
type ChatStatus struct {
	Total     int  `json:"total"`
	HasChance bool `json:"has_chance"`
}

// FetchChatStatus reads total messages sent and whether the identity still has
// a chance (no budget cost).
func (c *Client) FetchChatStatus(ctx context.Context, accessToken, deviceID string, ua UserAgent, xff string) (*ChatStatus, error) {
	var out ChatStatus
	if err := c.doJSON(ctx, http.MethodGet, PathChatStatus, accessToken, deviceID, ua, xff, kindJSON, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ModelInfo is one upstream model entry.
type ModelInfo struct {
	Description      string `json:"description"`
	DisplayName      string `json:"display_name"`
	SelectedModel    string `json:"selected_model"`
	ReasoningEfforts []struct {
		Effort      string `json:"effort"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
	} `json:"reasoning_efforts"`
}

// ModelCatalogue is the available_models payload.
type ModelCatalogue struct {
	Default map[string]string `json:"default"`
	Models  []ModelInfo       `json:"models"`
}

// FetchModels lists the models visible to this identity.
func (c *Client) FetchModels(ctx context.Context, accessToken, deviceID string, ua UserAgent, xff string) (*ModelCatalogue, error) {
	var out ModelCatalogue
	if err := c.doJSON(ctx, http.MethodGet, PathModels, accessToken, deviceID, ua, xff, kindJSON, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteConversation removes an upstream thread. Failures are returned to the
// caller, which treats cleanup as best effort.
func (c *Client) DeleteConversation(ctx context.Context, accessToken, deviceID string, ua UserAgent, xff, conversationID, assistantID string) error {
	if conversationID == "" {
		return nil
	}
	body := map[string]any{"conversation_id": conversationID}
	if assistantID != "" {
		body["assistant_id"] = assistantID
	}
	var out json.RawMessage
	return c.doJSON(ctx, http.MethodPost, PathConvDelete, accessToken, deviceID, ua, xff, kindJSON, body, &out)
}

// AssistantInfo describes the main assistant.
type AssistantInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FetchAssistantInfo reads the assistant profile.
func (c *Client) FetchAssistantInfo(ctx context.Context, accessToken, deviceID, assistantID string, ua UserAgent, xff string) (*AssistantInfo, error) {
	var out AssistantInfo
	path := PathAssistantInfo + "?assistant_id=" + assistantID
	if err := c.doJSON(ctx, http.MethodGet, path, accessToken, deviceID, ua, xff, kindJSON, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// doJSON performs a signed JSON call and decodes the envelope's result.
func (c *Client) doJSON(ctx context.Context, method, path, bearer, deviceID string, ua UserAgent, xff string, kind requestKind, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	SetCommonHeaders(req, ua, "")
	SetAccept(req, kind)
	SetDeviceID(req, deviceID)
	SetSignature(req)
	SetEgress(req, xff)
	if bearer != "" {
		SetBearer(req, bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ErrTransportResponse(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ErrTransportResponse(err)
	}
	if resp.StatusCode >= 400 {
		return Classify(resp.StatusCode, string(raw))
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{Kind: ErrServer, Status: 502, Message: "upstream returned a non-JSON response: " + truncate(string(raw), 200)}
	}
	if env.Status != 0 {
		ae := Classify(resp.StatusCode, string(raw))
		if ae == nil {
			ae = &APIError{Kind: ErrServer, Status: 502, Message: env.Message, Code: env.Status}
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &APIError{Kind: ErrServer, Status: 502, Message: "cannot decode upstream result: " + err.Error()}
	}
	return nil
}

// ChatRequest is the upstream stream body (docs/reverse/ENDPOINTS.md §2).
type ChatRequest struct {
	AssistantID    string        `json:"assistant_id"`
	ConversationID string        `json:"conversation_id"`
	ProjectID      string        `json:"project_id"`
	ChatType       string        `json:"chat_type"`
	Messages       []ChatMessage `json:"messages"`
	Model          string        `json:"model,omitempty"`
	MetaData       ChatMeta      `json:"meta_data"`
}

// ChatMessage is one upstream message: content must be an array of typed parts.
type ChatMessage struct {
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}

// ContentPart is a typed prompt part.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ChatMeta is the upstream metadata block.
type ChatMeta struct {
	Channel           string  `json:"channel"`
	ChatMode          string  `json:"chat_mode"`
	DraftID           string  `json:"draft_id"`
	IfPlusModel       bool    `json:"if_plus_model"`
	InputQuestionType string  `json:"input_question_type"`
	IsNetworking      bool    `json:"is_networking"`
	IsTest            bool    `json:"is_test"`
	Platform          string  `json:"platform"`
	QuoteLogID        string  `json:"quote_log_id"`
	Cogview           Cogview `json:"cogview"`
}

// Cogview is the image-generation sub-config the SPA always sends.
type Cogview struct {
	RMLabelWatermark bool `json:"rm_label_watermark"`
}

// NewChatRequest builds the verified request body shape.
func NewChatRequest(assistantID, text, model string, networking bool, chatMode string) ChatRequest {
	return ChatRequest{
		AssistantID:    assistantID,
		ConversationID: "",
		ProjectID:      "",
		ChatType:       "user_chat",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: []ContentPart{{Type: "text", Text: text}},
		}},
		Model: model,
		MetaData: ChatMeta{
			Channel:           "",
			ChatMode:          chatMode,
			DraftID:           "",
			IfPlusModel:       true,
			InputQuestionType: "xxxx",
			IsNetworking:      networking,
			IsTest:            false,
			Platform:          "pc",
			QuoteLogID:        "",
			Cogview:           Cogview{RMLabelWatermark: false},
		},
	}
}

// OpenChatStream issues the SSE chat request and returns the response body.
// The caller must close it. Non-2xx responses are classified; the 200-with-JSON
// quota errors are detected and returned as APIError by the caller via
// ClassifyResponse (the response body is not SSE in that case).
func (c *Client) OpenChatStream(ctx context.Context, accessToken, deviceID string, ua UserAgent, xff string, body ChatRequest) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode chat request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+PathChatStream, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SetCommonHeaders(req, ua, "")
	SetAccept(req, kindStream)
	SetDeviceID(req, deviceID)
	SetSignature(req)
	SetEgress(req, xff)
	SetBearer(req, accessToken)

	// StreamHTTP has no client-level timeout because a long answer must be
	// allowed to run; bound only the wait for response headers here so a
	// silently accepted request cannot pin an identity forever. Once headers
	// arrive the stream is governed by the idle watchdog in Stream, and closing
	// the body cancels the derived context so nothing is retained.
	parent := ctx
	var cancel context.CancelFunc
	if c.HeaderTimeout > 0 {
		ctx, cancel = context.WithCancel(ctx)
		req = req.WithContext(ctx)
	}
	hc := c.StreamHTTP
	if hc == nil {
		hc = c.HTTP
	}
	var headerTimer *time.Timer
	var headerExpired atomic.Bool
	if cancel != nil {
		headerTimer = time.AfterFunc(c.HeaderTimeout, func() {
			headerExpired.Store(true)
			cancel()
		})
	}
	resp, err := hc.Do(req)
	if headerTimer != nil {
		headerTimer.Stop()
	}
	if err != nil {
		if cancel != nil {
			cancel()
		}
		// Prefer the caller's context error (a client disconnect) over the
		// header deadline.
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if headerExpired.Load() {
			return nil, &APIError{Kind: ErrTransport, Status: 504,
				Message: fmt.Sprintf("upstream did not send response headers within %s", c.HeaderTimeout)}
		}
		return nil, ErrTransportResponse(err)
	}
	if cancel != nil {
		// Release the cancel func with the body. resp.Body is only read by the
		// caller through Stream, which closes it (or the idle watchdog does).
		resp.Body = &cancelingBody{ReadCloser: resp.Body, cancel: cancel}
	}
	if resp.StatusCode >= 400 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if ae := Classify(resp.StatusCode, string(rawErr)); ae != nil {
			return nil, ae
		}
		return nil, &APIError{Kind: ErrServer, Status: 502, Message: string(rawErr)}
	}
	return resp, nil
}

// cancelingBody cancels the derived request context when the response body is
// closed, so the header-timeout cancel func is always released.
type cancelingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelingBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// IsSSE reports whether the response carries an event stream. A 200 JSON
// envelope instead of SSE is the documented budget/concurrency failure mode.
func IsSSE(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// ReadJSONBody reads a non-SSE response and classifies it.
func ReadJSONBody(resp *http.Response) *APIError {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ErrTransportResponse(err)
	}
	if ae := Classify(resp.StatusCode, string(raw)); ae != nil {
		return ae
	}
	return &APIError{Kind: ErrServer, Status: 502, Message: "unexpected non-SSE response: " + truncate(string(raw), 200)}
}

// Stream is an open SSE response with an idle watchdog.
type Stream struct {
	resp      *http.Response
	lines     *lineReader
	idle      time.Duration
	idleTimer *time.Timer
	idleErr   atomic.Pointer[error]
}

// NewStream wraps an SSE response. idle bounds the gap between reads; <= 0
// disables the watchdog. When the watchdog fires the body is closed, which
// unblocks a pending read; Next then reports ErrIdle.
func NewStream(resp *http.Response, idle time.Duration) *Stream {
	s := &Stream{resp: resp, lines: &lineReader{r: resp.Body}, idle: idle}
	if idle > 0 {
		s.idleTimer = time.AfterFunc(idle, func() {
			err := error(ErrIdle)
			s.idleErr.Store(&err)
			resp.Body.Close()
		})
	}
	return s
}

// Close releases the stream and stops the watchdog. It is safe to call more
// than once and after the body was already closed by the watchdog.
func (s *Stream) Close() error {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	return s.resp.Body.Close()
}

// touch resets the idle watchdog after each successful read.
func (s *Stream) touch() {
	if s.idleTimer != nil {
		s.idleTimer.Reset(s.idle)
	}
}

// ErrIdle is returned when no bytes arrive within the idle window.
var ErrIdle = errors.New("upstream stream idle timeout")

// Next returns the next SSE data payload. Keepalives (HEARTBEAT), comments and
// blank lines are skipped. io.EOF marks the end of the stream, either from the
// transport or from an explicit data: [DONE].
func (s *Stream) Next() ([]byte, error) {
	for {
		line, err := s.lines.readLine()
		if err != nil {
			if p := s.idleErr.Load(); p != nil {
				return nil, *p
			}
			return nil, err
		}
		s.touch()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		switch payload {
		case "":
			continue
		case "HEARTBEAT":
			continue
		case "[DONE]":
			return nil, io.EOF
		}
		return []byte(payload), nil
	}
}

// lineReader reads newline-delimited lines, buffering partial reads.
type lineReader struct {
	r    io.Reader
	buf  []byte
	fill []byte
}

func (l *lineReader) readLine() (string, error) {
	if l.fill == nil {
		l.fill = make([]byte, 32*1024)
	}
	var readErr error
	for {
		// Always drain complete lines before acting on a read error: a single
		// Read may return the last bytes and io.EOF together, and the final
		// line must still be delivered.
		if i := bytes.IndexByte(l.buf, '\n'); i >= 0 {
			line := string(l.buf[:i])
			l.buf = l.buf[i+1:]
			return strings.TrimRight(line, "\r"), nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && len(l.buf) > 0 {
				line := string(l.buf)
				l.buf = nil
				return strings.TrimRight(line, "\r"), nil
			}
			return "", readErr
		}
		n, err := l.r.Read(l.fill)
		if n > 0 {
			l.buf = append(l.buf, l.fill[:n]...)
		}
		readErr = err
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
