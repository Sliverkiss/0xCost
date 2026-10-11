package mimo

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// This file implements the web-surface audio capabilities reverse-engineered
// in docs/web-surface-reverse.md §2–§4 and §7: conversation-backed TTS,
// multipart-free ASR (upload + recognize), and the voice catalog.
//
// Every call needs the session cookie AND the ?xiaomichatbot_ph= query
// parameter; a cookie alone is answered with 401 (reverse doc §1).

// Default audio models and endpoints (reverse doc §3/§4).
const (
	AudioModelTTS = "mimo-v2.5-tts"
	AudioModelASR = "mimo-v2.5-asr"

	// Conversation kinds are the upstream pageType values a conversation
	// entity carries; TTS and ASR conversations are distinct rows upstream.
	ConversationKindChat = "chat"
	ConversationKindTTS  = "tts"
	ConversationKindASR  = "asr"

	// DefaultTTSInstruction is the first (instruction) message: the upstream
	// requires exactly two messages, the second one being the spoken text
	// (reverse doc §3).
	DefaultTTSInstruction = "请用自然、亲切的语气朗读下面的文字。"
)

// BizError is a business-level upstream failure: HTTP 200 with
// code != 0 in the envelope (e.g. {"code":400,"msg":"conversation not exist"}).
type BizError struct {
	Code int
	Msg  string
}

func (e *BizError) Error() string { return fmt.Sprintf("upstream code %d: %s", e.Code, e.Msg) }

// IsConversationMissing reports whether err means the conversation entity
// does not exist upstream — the caller recreates one and retries once.
func IsConversationMissing(err error) bool {
	var be *BizError
	if !errors.As(err, &be) {
		return false
	}
	return strings.Contains(be.Msg, "conversation not exist") || strings.Contains(be.Msg, "会话不存在")
}

// IsUnknownModel reports the upstream's response to an unknown modelCode
// (reverse doc §3: an unresolvable model is answered with "modelCode cannot
// be empty" rather than a model-name error).
func IsUnknownModel(err error) bool {
	var be *BizError
	return errors.As(err, &be) && strings.Contains(be.Msg, "modelCode cannot be empty")
}

// IsModelRefused reports the upstream refusing a modelCode it was sent: either
// the explicit refusal ("model: [x] not support") or the misleading message it
// uses for an id it cannot resolve at all ("modelCode cannot be empty"). Both are
// what a retired id produces when the retirement happened after our map of
// superseded ids was built, so the caller can re-learn and retry (ADR 0011).
func IsModelRefused(err error) bool {
	refused := func(s string) bool {
		return strings.Contains(s, "not support") || strings.Contains(s, "modelCode cannot be empty")
	}
	var be *BizError
	if errors.As(err, &be) && refused(be.Msg) {
		return true
	}
	var ue *UpstreamError
	return errors.As(err, &ue) && refused(ue.Body)
}

// Errors surfaced by the audio flows.
var (
	// ErrAudioFailed: the upstream task reported status=failed.
	ErrAudioFailed = errors.New("upstream audio task failed")
	// ErrAudioTimeout: the task never reached a terminal status in time.
	ErrAudioTimeout = errors.New("upstream audio task timed out")
)

// envelope is the upstream JSON envelope: {"code":0,"msg":"成功","data":{...}}.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// callJSON performs one JSON request against the upstream: browser
// fingerprint headers (SDD §2.4), session cookie, ph query param, envelope
// unwrap. HTTP failures map to *UpstreamError (401 additionally wrapped as
// auth-expired, mirroring Chat); business codes map to *BizError.
func (c *Client) callJSON(ctx context.Context, cookie, ph, method, path string, query url.Values, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("xiaomichatbot_ph", ph)
	target := fmt.Sprintf("%s%s?%s", c.base, path, query.Encode())

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	c.setBrowserHeaders(req.Header)
	req.Header.Set("Cookie", cookie)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		ue := &UpstreamError{StatusCode: resp.StatusCode, Body: string(raw), Headers: resp.Header.Clone()}
		if resp.StatusCode == http.StatusUnauthorized {
			return AuthError(ue)
		}
		return ue
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s %s: bad envelope: %w (body %.200s)", method, path, err, raw)
	}
	if env.Code != 0 {
		return &BizError{Code: env.Code, Msg: env.Msg}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("%s %s: bad data: %w", method, path, err)
	}
	return nil
}

// msgID mints the millisecond-timestamp string the audio endpoints expect in
// `msgId` (reverse doc §3/§4: e.g. "1791521173154").
func msgID() string { return strconv.FormatInt(time.Now().UnixMilli(), 10) }

// ---- conversations (reverse doc §2) ----

// conversationSaveResponse is the upstream conversation/save payload.
type conversationSaveResponse struct {
	ID             int64  `json:"id"`
	ConversationID string `json:"conversationId"`
	Type           string `json:"type"`
	Title          string `json:"title"`
}

// createConversation saves a NEW conversation entity. Omitting the numeric
// `id` is what makes the upstream create rather than update; the
// conversationId is client-minted 32-hex (reverse doc §2).
func (c *Client) createConversation(ctx context.Context, cookie, ph, kind string) (string, error) {
	id := adapter.RandomHex32()
	body := map[string]any{
		"conversationId": id,
		"title":          "mimo2api " + kind,
		"type":           kind,
	}
	var out conversationSaveResponse
	if err := c.callJSON(ctx, cookie, ph, http.MethodPost,
		"/open-apis/chat/conversation/save", nil, body, &out); err != nil {
		return "", err
	}
	if out.ConversationID == "" {
		return "", fmt.Errorf("conversation/save returned no conversationId for kind %q", kind)
	}
	return out.ConversationID, nil
}

// ConversationCache caches conversationId per (account, kind). TTS/ASR are
// backed by conversation rows upstream, so the gateway reuses one per account
// instead of creating a row per request. Entries expire after TTL and are
// invalidated when the upstream reports the conversation missing.
type ConversationCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]convEntry
}

type convEntry struct {
	id string
	at time.Time
}

// NewConversationCache builds a cache with the given TTL (default 30m).
func NewConversationCache(ttl time.Duration) *ConversationCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &ConversationCache{ttl: ttl, now: time.Now, entries: map[string]convEntry{}}
}

// Get returns a live conversationId for key.
func (cc *ConversationCache) Get(key string) (string, bool) {
	if cc == nil {
		return "", false
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	e, ok := cc.entries[key]
	if !ok || cc.now().Sub(e.at) > cc.ttl {
		if ok {
			delete(cc.entries, key)
		}
		return "", false
	}
	return e.id, true
}

// Put stores a conversationId for key.
func (cc *ConversationCache) Put(key, id string) {
	if cc == nil || id == "" {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.entries[key] = convEntry{id: id, at: cc.now()}
}

// Invalidate drops the entry for key (upstream said it is gone).
func (cc *ConversationCache) Invalidate(key string) {
	if cc == nil {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.entries, key)
}

// EnsureConversation returns a conversationId of the given kind, creating and
// caching one when needed. cache/accountID may be empty to opt out of
// caching (tests, one-shot callers).
func (c *Client) EnsureConversation(ctx context.Context, cookie, ph string, cache *ConversationCache, accountID, kind string) (string, error) {
	key := accountID + "/" + kind
	if id, ok := cache.Get(key); ok {
		return id, nil
	}
	id, err := c.createConversation(ctx, cookie, ph, kind)
	if err != nil {
		return "", err
	}
	cache.Put(key, id)
	return id, nil
}

// ---- TTS (reverse doc §3) ----

// TTSRequest is one synthesis request.
type TTSRequest struct {
	Text        string // spoken text (second upstream message)
	Instruction string // optional style instruction (first message)
	Voice       string // upstream voice name (e.g. 冰糖); empty = caller default
	Format      string // wav | mp3 (empty = wav)
	Model       string // empty = AudioModelTTS
	PollTimeout time.Duration
	PollEvery   time.Duration
}

// TTSResult is a completed synthesis.
type TTSResult struct {
	Audio       []byte
	ContentType string
	AudioURL    string
	TaskID      string
}

// AudioContentType maps an upstream audio format to a MIME type.
func AudioContentType(format string) string {
	switch strings.ToLower(format) {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

// normalizeTTSBrackets rewrites round/curly/Asian brackets to square ones —
// the shape the Studio sends (bracket groups are the model's control tags).
func normalizeTTSBrackets(s string) string {
	r := strings.NewReplacer(
		"(", "[", "（", "[", ")", "]", "）", "]",
		"【", "[", "】", "]", "「", "[", "」", "]",
		"{", "[", "}", "]",
	)
	return r.Replace(s)
}

// upID is an upstream identifier that arrives in whichever JSON shape the
// endpoint feels like using: the audio flows return `taskId` as a NUMBER
// ({"taskId":16063666}) while `msgId` is a STRING, and both are used as opaque
// values in query parameters. Declaring the field as a string makes encoding/json
// reject the number ("cannot unmarshal number into Go struct field ... of type
// string") and the whole call fails against the real upstream, so accept both.
type upID string

func (v *upID) UnmarshalJSON(b []byte) error {
	switch s := strings.TrimSpace(string(b)); s {
	case "", "null", `""`:
		*v = ""
	default:
		*v = upID(strings.Trim(s, `"`))
	}
	return nil
}

func (v upID) String() string { return string(v) }

type ttsGenerateResponse struct {
	TaskID         upID   `json:"taskId"`
	ConversationID string `json:"conversationId"`
	MsgID          string `json:"msgId"`
	Status         string `json:"status"`
}

type audioTaskStatus struct {
	Status   string `json:"status"`
	Text     string `json:"text"`
	AudioURL string `json:"audioUrl"`
	Audit    struct {
		Status  string `json:"status"`
		Blocked bool   `json:"blocked"`
	} `json:"audit"`
}

// Synthesize runs generate → poll → download against the upstream
// conversation `conv` (create it with EnsureConversation).
func (c *Client) Synthesize(ctx context.Context, cookie, ph, conv string, req TTSRequest) (*TTSResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return nil, errors.New("tts: empty text")
	}
	if conv == "" {
		return nil, errors.New("tts: empty conversationId")
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "wav"
	}
	model := req.Model
	if model == "" {
		model = AudioModelTTS
	}
	instr := req.Instruction
	if strings.TrimSpace(instr) == "" {
		instr = DefaultTTSInstruction
	}
	body := map[string]any{
		"conversationId": conv,
		"msgId":          msgID(),
		"content": map[string]any{
			// Exactly two messages; the SECOND one is the spoken text
			// (reverse doc §3 — a single message is rejected/failed).
			"messages": []map[string]string{
				{"role": "user", "content": instr},
				{"role": "assistant", "content": normalizeTTSBrackets(req.Text)},
			},
			"audio": map[string]any{"format": format, "voice": req.Voice},
		},
		"modelConfig": map[string]any{"modelCode": model},
	}
	var gen ttsGenerateResponse
	if err := c.callJSON(ctx, cookie, ph, http.MethodPost,
		"/open-apis/tts/v2/generate", nil, body, &gen); err != nil {
		return nil, err
	}
	taskID := gen.TaskID.String()
	if taskID == "" {
		return nil, fmt.Errorf("tts: generate returned no taskId (status %q)", gen.Status)
	}
	st, err := c.pollAudioTask(ctx, cookie, ph, "/open-apis/tts/generateStatus", taskID, req.PollEvery, req.PollTimeout)
	if err != nil {
		return nil, err
	}
	if st.AudioURL == "" {
		return nil, fmt.Errorf("tts: task %s succeeded without audioUrl", taskID)
	}
	audio, err := c.download(ctx, st.AudioURL)
	if err != nil {
		return nil, err
	}
	return &TTSResult{Audio: audio, ContentType: AudioContentType(format), AudioURL: st.AudioURL, TaskID: taskID}, nil
}

// pollAudioTask polls a TTS/ASR status endpoint until a terminal status.
func (c *Client) pollAudioTask(ctx context.Context, cookie, ph, path, taskID string, every, timeout time.Duration) (*audioTaskStatus, error) {
	if every <= 0 {
		every = time.Second
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
		var st audioTaskStatus
		if err := c.callJSON(ctx, cookie, ph, http.MethodGet, path, url.Values{"taskId": {taskID}}, nil, &st); err != nil {
			return nil, err
		}
		switch st.Status {
		case "success":
			return &st, nil
		case "failed":
			return nil, fmt.Errorf("%w (task %s, audit %s)", ErrAudioFailed, taskID, st.Audit.Status)
		}
		// "generating" (and an audit still PROCESSING) keeps polling.
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (task %s, last status %q)", ErrAudioTimeout, taskID, st.Status)
		}
	}
}

// download fetches the generated audio. The URL is a signed CDN link, so the
// upstream session cookie is deliberately NOT attached (it belongs to the
// Studio host and must not leak to a third-party host).
func (c *Client) download(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audio download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("audio download: upstream %d: %.200s", resp.StatusCode, body)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("audio download: read: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("audio download: empty body")
	}
	return data, nil
}

// ---- ASR (reverse doc §4) ----

// ASRRequest is one transcription request.
type ASRRequest struct {
	FileName    string
	Audio       []byte
	Language    string // auto | zh | en (empty = auto)
	Model       string // empty = AudioModelASR
	PollTimeout time.Duration
	PollEvery   time.Duration
}

type genUploadInfoResponse struct {
	UploadURL   string `json:"uploadUrl"`
	ResourceURL string `json:"resourceUrl"`
	ObjectName  string `json:"objectName"`
}

type asrRecognizeResponse struct {
	TaskID         upID   `json:"taskId"`
	ConversationID string `json:"conversationId"`
	Status         string `json:"status"`
	Text           string `json:"text"`
}

// Transcribe uploads the audio and returns the recognized text.
func (c *Client) Transcribe(ctx context.Context, cookie, ph, conv string, req ASRRequest) (string, error) {
	if len(req.Audio) == 0 {
		return "", errors.New("asr: empty audio")
	}
	if conv == "" {
		return "", errors.New("asr: empty conversationId")
	}
	model := req.Model
	if model == "" {
		model = AudioModelASR
	}
	lang := req.Language
	if lang == "" {
		lang = "auto"
	}
	resourceURL, err := c.uploadAudio(ctx, cookie, ph, req.FileName, req.Audio)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"conversationId": conv,
		"msgId":          msgID(),
		"audioUrl":       resourceURL,
		"language":       lang,
		"modelConfig":    map[string]any{"modelCode": model},
	}
	var rec asrRecognizeResponse
	if err := c.callJSON(ctx, cookie, ph, http.MethodPost,
		"/open-apis/asr/recognize", nil, body, &rec); err != nil {
		return "", err
	}
	taskID := rec.TaskID.String()
	if taskID == "" {
		return "", fmt.Errorf("asr: recognize returned no taskId (status %q, text %q)", rec.Status, rec.Text)
	}
	if rec.Text != "" && rec.Status == "success" {
		return rec.Text, nil
	}
	st, err := c.pollAudioTask(ctx, cookie, ph, "/open-apis/asr/recognizeStatus", taskID, req.PollEvery, req.PollTimeout)
	if err != nil {
		return "", err
	}
	return st.Text, nil
}

// uploadAudio performs genUploadInfo + PUT (reverse doc §4). The returned
// resourceUrl is what the recognize call references.
func (c *Client) uploadAudio(ctx context.Context, cookie, ph, fileName string, audio []byte) (string, error) {
	if strings.TrimSpace(fileName) == "" {
		fileName = "audio.wav"
	}
	sum := md5.Sum(audio)
	digest := hex.EncodeToString(sum[:])
	var info genUploadInfoResponse
	if err := c.callJSON(ctx, cookie, ph, http.MethodPost,
		"/open-apis/resource/genUploadInfo", nil,
		map[string]any{"fileName": fileName, "fileContentMd5": digest}, &info); err != nil {
		return "", err
	}
	if info.UploadURL == "" || info.ResourceURL == "" {
		return "", errors.New("asr: genUploadInfo returned no uploadUrl/resourceUrl")
	}
	// The upload URL is a signed object-storage link on another host: the SPA's
	// XHR branch sends no credential there, so the browser header set rides along
	// without a Cookie (ADR 0035).
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, info.UploadURL, bytes.NewReader(audio))
	if err != nil {
		return "", err
	}
	c.setAistudioStorageHeaders(put.Header)
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Header.Set("content-md5", digest)
	put.ContentLength = int64(len(audio))
	resp, err := c.http.Do(put)
	if err != nil {
		return "", fmt.Errorf("asr upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("asr upload: upstream %d: %.200s", resp.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return info.ResourceURL, nil
}

// ---- catalog (reverse doc §7) ----

// Voice is one TTS voice from the Studio catalog.
type Voice struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Avatar       string `json:"avatar"`
	PreviewAudio string `json:"previewAudio,omitempty"`
}

// BotModel is one entry of the upstream model catalog.
type BotModel struct {
	Model      string `json:"model"`
	Name       string `json:"name"`
	PageType   string `json:"pageType"`
	RedirectTo string `json:"redirectTo,omitempty"`
}

// ChatModel is one row of bot/config `modelConfigList` — the chat-capable
// catalog entries, which is where the capability DEFAULTS live
// (`modelConfigListNg` carries only ids, names and page types). Pointers keep
// "the upstream did not say" distinguishable from a real false (M19 §3).
type ChatModel struct {
	Model                  string  `json:"model"`
	Name                   string  `json:"name"`
	ThinkingDefaultOn      *bool   `json:"thinkingDefaultOn"`
	WebSearchDefaultStatus *string `json:"webSearchDefaultStatus"`
	ShowWebSearch          *bool   `json:"showWebSearch"`
	ShowThinking           *bool   `json:"showThinking"`
	NToken                 string  `json:"nToken"`
}

// BotExt are the account-level capability switches.
type BotExt struct {
	EnableVoice       bool `json:"enableVoice"`
	EnableCustomVoice bool `json:"enableCustomVoice"`
	EnableClaw        bool `json:"enableClaw"`
	EnableUltraspeed  bool `json:"enableUltraspeed"`
	IsForeignVersion  bool `json:"isForeignVersion"`
	EnableV26         bool `json:"enableV26"`
}

// BotConfig is the Studio's per-account configuration (reverse doc §7).
type BotConfig struct {
	Models []BotModel `json:"modelConfigListNg"`
	// ChatModels is modelConfigList: the chat rows with capability defaults.
	ChatModels []ChatModel `json:"modelConfigList"`
	Voices     []Voice     `json:"ttsVoices"`
	Ext        BotExt      `json:"ext"`
}

// FetchBotConfig reads the account configuration (needs cookie + ph).
func (c *Client) FetchBotConfig(ctx context.Context, cookie, ph string) (*BotConfig, error) {
	var cfg BotConfig
	if err := c.callJSON(ctx, cookie, ph, http.MethodGet, "/open-apis/bot/config", nil, nil, &cfg); err != nil {
		return nil, err
	}
	// Drop entries without an id: the upstream list is schema-loose and the
	// Studio filters the same way client-side.
	voices := cfg.Voices[:0]
	for _, v := range cfg.Voices {
		if v.ID != "" || v.Name != "" {
			voices = append(voices, v)
		}
	}
	cfg.Voices = voices
	return &cfg, nil
}
