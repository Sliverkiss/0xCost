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
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// Client is the upstream MiMo AI Studio client (SDD §2.4). Construct via
// New; the zero value is unusable.
type Client struct {
	http *http.Client
	// egressHTTP: one http.Client per egress label, identical to http except for
	// the proxy it dials through (SDD §4.8). Read-only after New.
	egressHTTP    map[string]*http.Client
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
	// device is the account's device identity (passport cookies such as
	// deviceId/pass_ua/uLocale), replayed on every login/renewal hop of this
	// client so the upstream recognises the device instead of seeing new
	// hardware (docs/login-chain-reverse.md §5 item 1; device.go).
	device map[string]string
}

// WithDevice returns a shallow copy of the client that presents the given
// device-identity cookies. The account pool stores them per account, so the
// caller composes the client per call:
//
//	s.clientFor(acct.Egress).WithDevice(acct.Device).Renew(ctx, ...)
//
// An empty/nil device yields a client that presents none (the pre-2026-10-09
// behaviour). Never mutates the receiver.
func (c *Client) WithDevice(device map[string]string) *Client {
	if len(device) == 0 {
		return c
	}
	cp := *c
	cp.device = device
	return &cp
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
	// Egress maps an egress LABEL to the proxy its requests leave through
	// (SDD §4.8). Every call made through For(label) dials through it; the
	// empty label is the shared, unlabelled egress and uses HTTP as given.
	Egress map[string]*url.URL
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
	// One http.Client per egress label. They share every setting with the base
	// client and differ only in the proxy they dial through, so a labelled
	// account behaves identically except for where its traffic leaves from
	// (SDD §4.8). Built once: no proxy pool, no switching (owner decision).
	egressHTTP := map[string]*http.Client{}
	for label, proxy := range opts.Egress {
		if proxy == nil {
			continue
		}
		clone := *hc
		clone.Transport = proxyTransport(hc.Transport, proxy)
		egressHTTP[label] = &clone
	}
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
		egressHTTP:              egressHTTP,
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

// EgressDirect is the label of the shared, unlabelled egress.
const EgressDirect = "direct"

// For returns the client whose requests leave through an egress label. Every
// upstream call an account makes must go through its own egress — chat on both
// channels, audio, the catalog, renewal and the probe alike (REQ-16) — so callers
// pass the account's label rather than choosing a transport themselves.
//
// An unknown label returns the client unchanged *only* because a label without an
// endpoint is rejected at startup (internal/egress.Resolve): by the time a request
// is in flight there is nothing sensible left to do, and failing here would turn a
// configuration mistake into a per-request error storm.
func (c *Client) For(egress string) *Client {
	if egress == "" || egress == EgressDirect {
		return c
	}
	hc, ok := c.egressHTTP[egress]
	if !ok {
		return c
	}
	clone := *c
	clone.http = hc
	return &clone
}

// proxyTransport returns a transport that dials through proxy while keeping the
// base transport's settings (connection pooling, TLS, timeouts).
func proxyTransport(base http.RoundTripper, proxy *url.URL) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	var t *http.Transport
	if bt, ok := base.(*http.Transport); ok {
		t = bt.Clone()
	} else {
		t = http.DefaultTransport.(*http.Transport).Clone()
	}
	p := *proxy
	t.Proxy = http.ProxyURL(&p)
	return t
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
//
// Fidelity notes (docs/login-chain-reverse.md §3): the Studio's own HTTP layer
// adds Accept-Language and x-timeZone explicitly, and its axios instance adds
// X-Requested-With and the JSON Accept default. Dropping any of them is a
// fingerprint tell, so all of them are set here.
func (c *Client) setBrowserHeaders(h http.Header) {
	c.setPlatformXHRHeaders(h)
	h.Set("Content-Type", "application/json")
}

// setPlatformXHRHeaders is setBrowserHeaders without a Content-Type: a real
// axios GET carries no entity header, so a GET that sets one is a tell. Every
// aistudio-surface request uses this set (XHR from the SPA's own origin).
func (c *Client) setPlatformXHRHeaders(h http.Header) {
	h.Set("Accept", "application/json, text/plain, */*")
	// The SPA sets Accept-Language itself ("zh-CN"); Chrome's raw navigation
	// value ("zh-CN,zh;q=0.9") belongs to the passport surface only. Do not
	// conflate the two — an earlier change did, and the fidelity cases caught it.
	h.Set("Accept-Language", "zh-CN")
	h.Set("X-Requested-With", "XMLHttpRequest")
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
	Model          string `json:"model"`
	EnableThinking bool   `json:"enableThinking"`
	// WebSearchStatus is the upstream search switch (disabled|auto|enabled).
	// Defaults to disabled: search costs latency and tokens, so it is opt-in.
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
	// MultiMedias are the attachment handles to send with this turn (a parsed
	// resource's id and coordinates, docs/attachments-reverse.md). nil sends the
	// empty array every chat request carries (SDD §2.4). Attachments are
	// accounted separately from the query text, so they are the one input path
	// that does not spend the 102,400-character ceiling (ADR 0015).
	MultiMedias []any
	// WebSearchStatus is the upstream search switch; empty = "disabled".
	WebSearchStatus string
	Temperature     *float64
	TopP            *float64
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
	// OnEvent, when non-nil, reports each upstream event's name and payload
	// size as it arrives (debug tracing; M19).
	OnEvent func(name string, dataLen int, prefix string)
}

// ChatResult is the completed turn.
type ChatResult struct {
	DialogID  string // this turn's dialogId → next turn's previousDialogueId
	Reasoning string
	Content   string
	Usage     *adapter.MiMoUsage
	// Citations are the web-search results the upstream returned inline
	// (M19); empty when it searched nothing.
	Citations []adapter.Citation
	// CitationWarnings holds reasons a web_search payload was skipped —
	// surfaced at debug level so a silent drop cannot hide.
	CitationWarnings []string
}

// UpstreamError is a non-200 chat response. StatusCode drives pool health
// (429/5xx cooldown, 401 auth refresh, 451/461 disable) and API error
// mapping (SDD §7.4, TDD ERRMAP-006..008). Headers are kept for diagnostics
// only — a Retry-After is never echoed to the client (TDD ERRMAP-011 privacy
// invariant), but it is what an operator needs in the log and what LIVE-007
// inspects for a structured 429 deadline.
type UpstreamError struct {
	StatusCode int
	Body       string
	Headers    http.Header
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
// UserAgent is the browser identity this client sends (SDD §2.4). It is exported
// for the one other place that must look like the same browser: fetching an
// image URL the client handed us (internal/api/imageinput.go).
func (c *Client) UserAgent() string { return c.userAgent }

// AttachmentItem builds one entry of a chat request's `multiMedias` array for an
// uploaded attachment. The field set is the one the live platform accepts: the
// probe showed a stripped item (name/fileUrl/objectName/url only) is refused, so
// every field below is load-bearing (docs/attachments-reverse.md).
//
// mediaType is the channel's own word for what was uploaded — "image", "file",
// "audio", "video"; the sibling reverse-engineered client sends exactly these.
func AttachmentItem(mediaType, name string, size int, up *UploadedResource, parsed *ParsedResource) map[string]any {
	audioURL := ""
	if mediaType == "audio" {
		audioURL = up.ResourceURL
	}
	return map[string]any{
		"mediaType":          mediaType,
		"fileUrl":            up.ResourceURL,
		"compressedVideoUrl": "",
		"audioTrackUrl":      audioURL,
		"name":               name,
		"size":               size,
		"status":             "completed",
		"objectName":         up.ObjectName,
		"tokenUsage":         parsed.TokenUsage,
		"url":                parsed.ID,
	}
}

// mediaOrEmpty keeps multiMedias an ARRAY even when the caller sends no
// attachments: the upstream reads the field's type, and null breaks it
// (SDD §2.4).
func mediaOrEmpty(m []any) []any {
	if m == nil {
		return []any{}
	}
	return m
}

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
			WebSearchStatus: searchStatusOrDefault(req.WebSearchStatus),
			Temperature:     req.Temperature,
			TopP:            req.TopP,
		},
		MultiMedias: mediaOrEmpty(req.MultiMedias),
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
		ue := &UpstreamError{StatusCode: resp.StatusCode, Body: string(errBody), Headers: resp.Header.Clone()}
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
	st.onEvent = req.OnEvent
	if _, err := st.parseEvents(streamCtx, stream); err != nil {
		st.finish() // flush splitter state even on failure
		return nil, err
	}
	dialogID, reasoning, content, usage := st.finish()
	if !st.sawDeltas {
		return nil, ErrEmptyResponse
	}
	return &ChatResult{
		DialogID: dialogID, Reasoning: reasoning, Content: content,
		Usage: usage, Citations: st.citations, CitationWarnings: st.citationsLog,
	}, nil
}

// searchStatusOrDefault maps an empty search status to the disabled default
// (M19): search adds latency and tokens, so a caller must ask for it.
func searchStatusOrDefault(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "auto":
		return "auto"
	case "enabled":
		return "enabled"
	default:
		return "disabled"
	}
}
