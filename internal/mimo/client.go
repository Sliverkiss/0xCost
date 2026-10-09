package mimo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// Client is the upstream MiMo AI Studio client (SDD §2.4). Construct via
// New; the zero value is unusable.
type Client struct {
	http          *http.Client
	base          string // upstream base URL, no trailing slash
	upstreamHost  string
	userAgent     string
	secChUA       string
	timezone      string
	passportHosts []string // login/sts location allowlist (default: account.xiaomi.com)
	idleTimeout   time.Duration
	// disableOTPAuto: when false (default), the code=0 new-device OTP
	// demand auto-starts a challenge (MIMO2API_OTP_AUTO=false disables).
	disableOTPAuto bool
	// disableCrossRegionAgree: when true, the cross-region
	// personal-data-transfer demand is surfaced instead of agreed to
	// (MIMO2API_CROSS_REGION_AGREE=false).
	disableCrossRegionAgree bool
}

// Options configures a Client. Base may be an httptest server URL in tests;
// PassportHosts lets tests point the allowlist at a MockPassport.
type Options struct {
	Base          string // upstream base (default https://aistudio.xiaomimimo.com)
	UserAgent     string // Chrome UA; must match SecChUA for risk control (SDD §2.4)
	SecChUA       string
	Timezone      string // x-timeZone header (default Asia/Shanghai)
	HTTP          *http.Client
	PassportHosts []string // override the passport host allowlist (tests)
	// IdleTimeout is the no-SSE-event watchdog (SDD §7.3; default 180s,
	// env MIMO2API_SSE_IDLE_TIMEOUT_S). Tests override it small.
	IdleTimeout time.Duration
	// DisableOTPAuto turns off auto-starting the new-device OTP challenge
	// on the code=0 notificationUrl demand (env MIMO2API_OTP_AUTO=false).
	DisableOTPAuto bool
	// DisableCrossRegionAgree turns off auto-agreeing to the cross-region
	// personal-data-transfer demand (env MIMO2API_CROSS_REGION_AGREE=false).
	// With it set, such a login fails with ErrCrossRegionConsent instead.
	DisableCrossRegionAgree bool
}

// New builds a client.
func New(opts Options) *Client {
	if opts.Base == "" {
		opts.Base = "https://aistudio.xiaomimimo.com"
	}
	for len(opts.Base) > 0 && opts.Base[len(opts.Base)-1] == '/' {
		opts.Base = opts.Base[:len(opts.Base)-1]
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	}
	if opts.SecChUA == "" {
		opts.SecChUA = `"Google Chrome";v="153", "Chromium";v="153", "Not)A;Brand";v="24"`
	}
	if opts.Timezone == "" {
		opts.Timezone = "Asia/Shanghai"
	}
	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	u, _ := url.Parse(opts.Base)
	hosts := opts.PassportHosts
	if len(hosts) == 0 {
		hosts = []string{"account.xiaomi.com"}
	}
	idle := opts.IdleTimeout
	if idle == 0 {
		idle = 180 * time.Second // SDD §7.3, env MIMO2API_SSE_IDLE_TIMEOUT_S
	}
	return &Client{
		http:                    hc,
		base:                    opts.Base,
		upstreamHost:            u.Hostname(),
		userAgent:               opts.UserAgent,
		secChUA:                 opts.SecChUA,
		timezone:                opts.Timezone,
		passportHosts:           hosts,
		idleTimeout:             idle,
		disableOTPAuto:          opts.DisableOTPAuto,
		disableCrossRegionAgree: opts.DisableCrossRegionAgree,
	}
}

// ErrIdleStall: the upstream stream produced no SSE event for the
// idle-timeout window (SDD §7.3). The API layer aborts the upstream,
// surfaces an error frame, and applies the 30s-cooldown ladder (§4.4).
var ErrIdleStall = errors.New("upstream stream stalled (no SSE event within idle timeout)")

// idleWatchedReader wraps the SSE body with a no-byte watchdog: if nothing
// is read for timeout, the upstream request is aborted (via cancel) and
// ErrIdleStall surfaces from the read. Any successful read resets the
// timer, so a normally paced stream never trips it (SDD §7.3).
type idleWatchedReader struct {
	r       io.Reader
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	stalled chan struct{}
	once    sync.Once
}

func newIdleWatchedReader(r io.Reader, cancel context.CancelFunc, timeout time.Duration) *idleWatchedReader {
	w := &idleWatchedReader{
		r:       r,
		cancel:  cancel,
		timeout: timeout,
		stalled: make(chan struct{}),
	}
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, func() {
			w.once.Do(func() {
				close(w.stalled)
				cancel() // abort the upstream request (SDD §7.3)
			})
		})
	}
	return w
}

func (w *idleWatchedReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	// The watchdog cancel surfaces here as "context canceled" from the
	// body — translate to ErrIdleStall so callers see the real cause.
	select {
	case <-w.stalled:
		return n, ErrIdleStall
	default:
	}
	if err == nil && w.timer != nil && n > 0 {
		w.timer.Reset(w.timeout)
	}
	return n, err
}

func (w *idleWatchedReader) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

// setBrowserHeaders injects the full browser header set shared by chat /
// validate calls (SDD §2.4; web_client.go setBrowserHeaders).
func (c *Client) setBrowserHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept-Language", "zh-CN")
	h.Set("x-timeZone", c.timezone)
	h.Set("Origin", c.base)
	h.Set("Referer", c.base+"/")
	h.Set("User-Agent", c.userAgent)
	h.Set("sec-ch-ua", c.secChUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
}

// modelConfig is the upstream modelConfig object. Temperature/TopP are
// pointers so they are omitted entirely when the caller gave none (TDD
// API-022).
type modelConfig struct {
	Model           string   `json:"model"`
	EnableThinking  bool     `json:"enableThinking"`
	WebSearchStatus string   `json:"webSearchStatus"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
}

// chatPayload is the upstream chat request body (SDD §2.4). multiMedias
// must serialize as [] — never null (web_client.go:121-125).
type chatPayload struct {
	MsgID            string      `json:"msgId"`
	ConversationID   string      `json:"conversationId"`
	Query            string      `json:"query"`
	IsEditedQuery    bool        `json:"isEditedQuery"`
	PreviousDialogID *string     `json:"previousDialogueId"`
	SceneType        any         `json:"sceneType"`
	Params           struct{}    `json:"params"`
	ModelConfig      modelConfig `json:"modelConfig"`
	MultiMedias      []any       `json:"multiMedias"`
}

// ChatRequest describes one upstream chat turn.
type ChatRequest struct {
	Query            string
	Model            string // normalized upstream model id
	Fastchat         bool   // route via /fastchat/open-apis/bot/chat (SDD §2.4)
	ConversationID   string // 32-hex; empty = mint fresh in Chat
	PreviousDialogID string // chaining anchor; empty = null (first turn)
	EnableThinking   bool
	Temperature      *float64
	TopP             *float64
	// OnPiece, when non-nil, receives content/reasoning pieces as they are
	// split (streaming passthrough). Returning an error aborts the upstream
	// request (client disconnect propagation, SDD §7.3).
	OnPiece func(piece adapter.Piece) error
	// OnFirstByte fires exactly once, when the first SSE event of any kind
	// arrives — the retry/commit boundary (SDD §7.2).
	OnFirstByte func()
	// OnDialogID, when non-nil, receives the turn's dialogId as soon as the
	// dialogId event arrives (mid-stream) — the API layer advances the
	// sticky entry even on client disconnect (SDD §7.3, TDD STICKY-010).
	OnDialogID func(dialogID string)
}

// ChatResult is the completed turn.
type ChatResult struct {
	DialogID  string // this turn's dialogId → next turn's previousDialogueId
	Reasoning string
	Content   string
	Usage     *adapter.MiMoUsage
}

// UpstreamError is a non-200 chat response. StatusCode drives pool health
// (429/5xx cooldown, 401 auth refresh, 451/461 disable) and API error
// mapping (SDD §7.4, TDD ERRMAP-006..008).
type UpstreamError struct {
	StatusCode int
	Body       string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.StatusCode, e.Body)
}

// ErrEmptyResponse: the stream finished with zero message deltas (SDD
// §7.1, TDD SSE-013). The caller retries once with fresh ids.
var ErrEmptyResponse = errors.New("upstream returned an empty response")

// AuthError wraps an error as auth-expired (mirrors pool.AuthError) so the
// retry layer can trigger a transparent refresh.
type authError struct{ err error }

func (authError) Error() string       { return "auth expired" }
func (a authError) Unwrap() error     { return a.err }
func (authError) IsAuthExpired() bool { return true }

// AuthError wraps err as an auth-expired signal.
func AuthError(err error) error { return authError{err} }

// IsAuthExpiredErr reports whether err signals a dead upstream session.
func IsAuthExpiredErr(err error) bool {
	var ae interface{ IsAuthExpired() bool }
	return errors.As(err, &ae) && ae.IsAuthExpired()
}

// Chat performs one upstream chat turn (SDD §2.4). The msgId is minted
// here and is fresh on every call — upstream dedupes by msgId, so retries
// re-enter Chat rather than resend (SDD §2.4). No retry logic lives here:
// account switch / auth refresh / empty-response retries are the API
// layer's job (SDD §7.1/§7.2).
func (c *Client) Chat(ctx context.Context, cookie, ph string, req ChatRequest) (*ChatResult, error) {
	if req.ConversationID == "" {
		req.ConversationID = adapter.RandomHex32()
	}
	var prev *string
	if req.PreviousDialogID != "" {
		s := req.PreviousDialogID
		prev = &s
	}
	payload := chatPayload{
		MsgID:            adapter.RandomHex32(),
		ConversationID:   req.ConversationID,
		Query:            req.Query,
		IsEditedQuery:    false,
		PreviousDialogID: prev,
		SceneType:        nil,
		ModelConfig: modelConfig{
			Model:           req.Model,
			EnableThinking:  req.EnableThinking,
			WebSearchStatus: "disabled",
			Temperature:     req.Temperature,
			TopP:            req.TopP,
		},
		MultiMedias: []any{}, // MUST be an array, never null (SDD §2.4)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// Channel routing: ultraspeed models must use the fastchat prefix
	// (SDD §2.4 — the normal channel rejects them with a model-name error).
	path := "/open-apis/bot/chat"
	if req.Fastchat {
		path = "/fastchat/open-apis/bot/chat"
	}
	target := fmt.Sprintf("%s%s?xiaomichatbot_ph=%s", c.base, path, url.QueryEscape(ph))

	// The stream runs under a cancellable child ctx so the idle watchdog
	// can abort the upstream request in-flight (SDD §7.3); parent (client
	// disconnect) cancellation propagates identically (§7.3).
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setBrowserHeaders(httpReq.Header)
	httpReq.Header.Set("Cookie", cookie)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		ue := &UpstreamError{StatusCode: resp.StatusCode, Body: string(errBody)}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, AuthError(ue)
		}
		return nil, ue
	}

	// Idle watchdog wraps the SSE body (SDD §7.3).
	stream := io.Reader(resp.Body)
	if c.idleTimeout > 0 {
		watch := newIdleWatchedReader(resp.Body, cancel, c.idleTimeout)
		defer watch.stop()
		stream = watch
	}

	st := newStreamer()
	st.onPiece = req.OnPiece
	st.onFirstByte = req.OnFirstByte
	st.onDialogID = req.OnDialogID
	if _, err := st.parseEvents(streamCtx, stream); err != nil {
		st.finish() // flush splitter state even on failure
		return nil, err
	}
	dialogID, reasoning, content, usage := st.finish()
	if !st.sawDeltas {
		return nil, ErrEmptyResponse
	}
	return &ChatResult{DialogID: dialogID, Reasoning: reasoning, Content: content, Usage: usage}, nil
}
