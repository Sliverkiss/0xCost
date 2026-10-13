// Package upstream 封装对 RicheeAI 上游（claw.richee.cn/claw-api）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
//
// 与 workbuddy2api 的差异（见 docs/PORT_NOTES.md）：
//   - 鉴权是单个 `richee-token: <uuid>` header（无 Authorization Bearer、无 refresh
//     token 轮换、无设备风控头）。
//   - 统一信封 {code,message,success,data,callSuccess}；业务成功 = success:true 或
//     code:"000000"/"0"；code 401（数字或字符串）= token 失效（池禁用 + 重登）。
//   - TLS 握手偶发超时 → doJSONWithRetry 在传输层做 3 次退避重试。
//   - 错误经 SSE 错误帧下发：data:{"error":{...}} + data:[DONE]。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"richee2api/internal/auth"
	"richee2api/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 业务 code 401：token 失效 → 禁用 + 重登
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截 → 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败 → 不罚账号，仍轮转
	ErrAccountFault                  // 账号级授权/配额故障 → 冷却轮换，不无限重试
	ErrModelBlocked                  // 210001「模型不存在或已被删除」→ (账号,模型) 负缓存避让
	ErrPromptTooLong                 // 上下文超限是请求的问题非账号的问题：不罚号、不轮转，末端透传原文
	ErrImageInvalid                  // 图片请求格式/数据无效 → 请求级错误：不罚号、不轮转
	ErrWafBlock                      // 403 + 非业务信封体（WAF 拦截页/空体）→ 账号软冷却 + 抖动退避
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrAccountFault:
		return "account_fault"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrWafBlock:
		return "waf_block"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// matchMode 描述错误分类 marker 的匹配通道（与参考实现同构：fold/exact/lower 三通道）。
type matchMode uint8

const (
	matchFold  matchMode = iota // 大小写不敏感 + 原文双通道
	matchExact                  // 大小写敏感字面包含
	matchLower                  // 小写化 body 上的包含匹配
)

// errorRule 一条错误分类规则：命中 patterns 中任一 marker 即归类为 kind。
type errorRule struct {
	kind     ErrKind
	mode     matchMode
	patterns []string
}

func matchPattern(p string, mode matchMode, body, lower string) bool {
	switch mode {
	case matchFold:
		return strings.Contains(lower, strings.ToLower(p)) || strings.Contains(body, p)
	case matchExact:
		return strings.Contains(body, p)
	case matchLower:
		return strings.Contains(lower, p)
	default:
		return false
	}
}

func (r errorRule) hit(body, lower string) bool {
	for _, p := range r.patterns {
		if matchPattern(p, r.mode, body, lower) {
			return true
		}
	}
	return false
}

// promptTooLongRule 上下文超限判定。
var promptTooLongRule = errorRule{kind: ErrPromptTooLong, mode: matchFold, patterns: []string{
	"prompt is too long", "context length exceeded", "maximum context length",
	"too many tokens",
}}

// isPromptTooLongStatus prompt 超限只在请求级 4xx 上判。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// sessionDeadTextRule token 失效的**文案**形态（msg 关键词，防信封变体）。
// code 判定走 hasExactBusinessCode（JSON 结构化精确匹配，杜绝 "401100" 子串误伤）。
var sessionDeadTextRule = errorRule{kind: ErrSessionDead, mode: matchFold, patterns: []string{
	"token invalid", "invalid token", "token expired", "token 无效", "token 已失效",
	"登录已过期", "未能读取到有效 token", "unauthorized",
}}

// modelBlockedCode SSE error 帧 business code：「模型不存在或已被删除」。
const modelBlockedCode = "210001"

// sessionDeadCodeValue token 失效的业务 code。
const sessionDeadCodeValue = "401"

// hasExactBusinessCode 报告 JSON body 的任一层 code 字段是否**精确等于** want
// （字符串或数字形态均可）。子串混淆（"401100"/"1401"）天然免疫。
func hasExactBusinessCode(body, want string) bool {
	var root any
	if json.Unmarshal([]byte(body), &root) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch node := value.(type) {
		case map[string]any:
			if code, ok := node["code"]; ok {
				if normalizeCodeString(code) == want {
					return true
				}
			}
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}

// normalizeCodeString 把 code 字段值（字符串/数字）归一为字符串。
func normalizeCodeString(v any) string {
	switch n := v.(type) {
	case string:
		return strings.TrimSpace(n)
	case float64:
		if n == float64(int64(n)) {
			return fmt.Sprintf("%d", int64(n))
		}
		return fmt.Sprintf("%v", n)
	default:
		return ""
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// RetryAfter 上游明示的等待时长（Retry-After 秒 / retry-after-ms /
	// x-ratelimit-reset 头解析）。零值 = 上游未明示，冷却时长回落调用方计算值。
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardRule 余额不足关键词（大小写不敏感 + 中文原文双通道）。
var hardRule = errorRule{kind: ErrHardCredit, mode: matchFold, patterns: []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}}

// softRateRule 限流/节流关键词。宁缺毋滥：只收明确指向「请求速率被节流」的措辞。
var softRateRule = errorRule{kind: ErrSoftRate, mode: matchFold, patterns: []string{
	"rate limit",
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit",
	"请求过于频繁", "限流",
}}

// accountFaultRule 账号级授权/配额故障关键词。
var accountFaultRule = errorRule{kind: ErrAccountFault, mode: matchFold, patterns: []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
	"账号已被封禁", "账号异常",
}}

// contentBlockedRule 内容策略拦截关键词。
var contentBlockedRule = errorRule{kind: ErrContentBlocked, mode: matchLower, patterns: []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
	"内容不规范", "涉及违规",
}}

// badParamsRule 请求体解析失败关键词。
var badParamsRule = errorRule{kind: ErrBadParams, mode: matchExact, patterns: []string{
	"Unmarshal chat params failed",
	`"code":11101`,
}}

// invalidImageRule 图片请求格式/数据无效的文案形态。
var invalidImageRule = errorRule{kind: ErrImageInvalid, mode: matchFold, patterns: []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}}

// alreadyCheckinRule 「今天已签到」幂等拒绝关键词。
var alreadyCheckinRule = errorRule{mode: matchFold, patterns: []string{"已签到", "已经签到", "already", "重复签到"}}

// modelBlockCode RicheeAI「模型不存在或已被删除」的业务 code（SSE 错误帧内）。
const modelBlockCode = "210001"

// ModelBlockReason 负缓存条目在 pool.modelCooldowns 里的 reason 前缀。
const ModelBlockReason = "210001 model not available"

// deadTokenCode token 失效的业务 code（数字或字符串形态均为 "401"）。
const deadTokenCode = "401"

// IsModelRateLimit 报告 body 是否携带模型级限流业务 code（RicheeAI 未观测到专用
// code，保留接口对齐参考实现；当前恒 false，由 softRateRule 文案兜底）。
func IsModelRateLimit(body string) bool { return false }

// ParseRateReset 从限流 body 解析「将在 … 重置」时间（保留接口口径；RicheeAI 未
// 观测到该文案，返回 false 由调用方退回有界退避）。
func ParseRateReset(body string) (time.Time, bool) {
	return time.Time{}, false
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果上限（超过视为上游异常值丢弃）。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流响应头解析上游明示的等待时长。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue
		}
		n, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if n <= 0 || n > retryAfterSanity {
			continue
		}
		return n, true
	}
	return 0, false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for _, r := range v {
		n = n*10 + int64(r-'0')
	}
	switch headerName {
	case "Retry-After":
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		return time.Duration(n) * time.Millisecond, true
	default:
		sec := n
		if len(v) >= 12 {
			sec = n / 1000
		}
		return time.Until(time.Unix(sec, 0)), true
	}
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  0. 业务 code 精确判定（JSON 结构化，免疫子串混淆）——
//     code 401 → ErrSessionDead（token 失效，禁用 + 重登）；
//     code 210001 → ErrModelBlocked（模型不存在，换模型/换号）。
//     最先判：两者语义最具体（终态/模型级），短冷却救不活失效 token。
//  1. sessionDeadTextRule —— token 失效文案变体（信封 msg 关键词）。
//  2. 402 —— 真正的计费余额耗尽状态码。
//  3. 429 —— 限流状态码兜底（先于 hardRule：状态码比关键词更权威）。
//  4. hardRule —— 非 429 响应携带计费关键词。
//  5. softRateRule —— 非 429 状态码携带限流文案。
//  6. promptTooLong —— 请求级语义，判在 404/5xx 与通用 4xx 兜底之前。
//  7. 404 / 5xx —— 与限流无关的常规分类。
//  8. 内容策略/参数错误/其他 4xx —— 通用兜底。
func Classify(status int, body string) ErrKind {
	// code 精确判定优先（JSON 结构化，"401100"/"1401" 不会误伤）。
	if hasExactBusinessCode(body, sessionDeadCodeValue) {
		return ErrSessionDead
	}
	if hasExactBusinessCode(body, modelBlockedCode) {
		return ErrModelBlocked
	}
	lower := strings.ToLower(body)
	// token 失效文案（无 code 形态的信封变体）先于限流/计费判定：
	// 误判为限流会让该死号留在池中反复被选中。
	if sessionDeadTextRule.hit(body, lower) {
		return ErrSessionDead
	}
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if hardRule.hit(body, lower) {
		return ErrHardCredit
	}
	if softRateRule.hit(body, lower) {
		return ErrSoftRate
	}
	if isPromptTooLongStatus(status) && promptTooLongRule.hit(body, lower) {
		return ErrPromptTooLong
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		if contentBlockedRule.hit(body, lower) {
			return ErrContentBlocked
		}
		if badParamsRule.hit(body, lower) {
			return ErrBadParams
		}
		// WAF 403（无业务信封的拦截形态）：判在通用 4xx 兜底之前——落 ErrClient
		// 兜底的代价是「只换号不罚」（连环 403 根因）。带信封的 403 已被上方各层捕获。
		if IsWafBlocked(status, body) {
			return ErrWafBlock
		}
		return ErrClient
	}
	// HTTP 200 + 业务失败信封形态（success:false + code 非成功码）：一般业务
	// 错误归 ErrClient（带分类的 *Error 路径在 doJSON 侧回落，此处为直接调用
	// Classify 的调用方兜底——幂等已签到等不罚号形态由 IsAlreadyCheckin 单独识别）。
	if isBusinessFailureEnvelope(body) {
		return ErrClient
	}
	return ErrNone
}

// isBusinessFailureEnvelope 报告 body 是否「HTTP 200 + 业务失败」形态：
// JSON 且显式携带 "success":false（成功信封 success:true 恒不命中）。
func isBusinessFailureEnvelope(body string) bool {
	var probe struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal([]byte(body), &probe) != nil {
		return false
	}
	return probe.Success != nil && !*probe.Success
}

// apiEnvelope RicheeAI 统一信封。
type apiEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// businessOK 报告信封业务是否成功：success:true 或 code "000000"/"0"。
func (e apiEnvelope) businessOK() bool {
	if e.Success {
		return true
	}
	switch e.Code {
	case "000000", "0", "":
		return !e.Success && e.Code != "" // code 空且 success false → 失败
	}
	return false
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 禁用。
	IdleTimeout time.Duration

	// UserAgent 出站 User-Agent 显式覆盖。
	UserAgent string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	PassthroughIP bool

	// Base 上游 base（默认 https://claw.richee.cn/claw-api）。
	Base string
}

// defaultBase 生产默认。
const defaultBase = "https://claw.richee.cn/claw-api"

// New 生产默认值。Transport 由 newTransport() 集中构造（连接层加固）。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr},
		Base:                 defaultBase,
		SanitizeFingerprints: true,
	}
}

// baseURL 生效的上游 base。
func (c *Client) baseURL() string {
	if c.Base != "" {
		return c.Base
	}
	return defaultBase
}

// chatHTTP 返回聊天专用 client；未设置时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务失败时返回带 body 片段的 *Error。
// body 读失败（连接中断/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return parseEnvelopeBody(resp.StatusCode, raw)
}

// parseEnvelopeBody 解析信封 body 为 data 或分类错误（doJSON 与 retry 共用）。
func parseEnvelopeBody(status int, raw []byte) (json.RawMessage, error) {
	body := string(raw)
	if status >= 400 {
		kind := Classify(status, body)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: status, Msg: truncate(body, 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(body, 120))
	}
	if !env.businessOK() {
		kind := Classify(status, body)
		if kind == ErrNone {
			kind = ErrClient
		}
		msg := env.Message
		if msg == "" {
			msg = env.Msg
		}
		return nil, &Error{Kind: kind, Status: status, Msg: fmt.Sprintf("code=%s msg=%s", env.Code, truncate(msg, 160))}
	}
	return env.Data, nil
}

// retryMaxAttempts 传输层重试次数（spec：TLS 握手偶发超时，3 次退避重试）。
const retryMaxAttempts = 3

// retryBackoff 重试间隔基数（100ms、200ms 指数退避）。
const retryBackoff = 100 * time.Millisecond

// shouldRetryTransportErr 判定传输层错误是否可重试（连接中断/握手超时/EOF 形态）。
// 业务错误（*Error）不重试——那是上游的确定性答复。
func shouldRetryTransportErr(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if errors.As(err, &ue) {
		return false
	}
	// 非业务错误：网络层失败（TLS 握手超时 / 连接重置 / EOF）均可重试。
	return true
}

// doJSONWithRetry doJSON 的带重试版本：传输层失败时退避重试，最多 3 次。
// RicheeAI TLS 握手偶发超时（spec 实测），所有短 RPC（credit/sign/gift/login）走此入口。
func (c *Client) doJSONWithRetry(req *http.Request) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt-1)) * retryBackoff)
		}
		// 重试需要可重放 body：GET/POST 短 RPC body 小，全量缓冲后重建请求。
		r2 := req.Clone(req.Context())
		if req.Body != nil {
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, fmt.Errorf("read request body: %w", err)
			}
			req.Body.Close()
			r2.Body = io.NopCloser(bytes.NewReader(raw))
			// 原请求 body 也恢复（调用方可能继续用）。
			req.Body = io.NopCloser(bytes.NewReader(raw))
		}
		resp, err := c.HTTP.Do(r2)
		if err != nil {
			lastErr = err
			if !shouldRetryTransportErr(err) {
				return nil, err
			}
			// 传输层失败：清空共享连接池的空闲连接（失败连接可能仍留在池里）。
			roundTripCloseIdle(c.HTTP.Transport)
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = fmt.Errorf("read body: %w", rerr)
			continue // 半截 body：传输层故障，重试
		}
		return parseEnvelopeBody(resp.StatusCode, raw)
	}
	return nil, lastErr
}

// baseJSON 构造带 richee-token 头的请求并发送（带重试）。
func (c *Client) baseJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.baseURL()+path, rdr)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a)
	return c.doJSONWithRetry(req)
}

// chatCompletionsPath chat 出站路径（spec 实测）。
const chatCompletionsPath = "/claw/v1/chat/completions"

// prepareBody 组装出站请求体（stream 强制 + thinking 注入 + tool 配对清理 + 脱敏）。
func (c *Client) prepareBody(body []byte, uid, conversationID string) []byte {
	body = PrepareBodyOpt(body, c.SanitizeFingerprints)
	return body
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context（客户端断连即中断
// 在途调用、释放租约）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := c.prepareBody(body, a.UID, meta.ConversationID)
	url := c.baseURL() + chatCompletionsPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a, clientIP, meta)
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] chat_stream acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			log.Printf("ERR: [upstream] chat_stream acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] chat_stream acct=%s: upstream %d %s body=%s",
			logfmt.Label(a.UID, a.Nickname), resp.StatusCode, kind, truncate(string(raw), 200))
		if kind == ErrNone {
			return nil, resp.StatusCode, raw, nil
		}
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		}
		return nil, resp.StatusCode, raw, ue
	}
	// RicheeAI 怪癖：chat 的业务错误也会走 HTTP 200 + JSON 信封（非 SSE 体）。
	// 判据：Content-Type 非 event-stream → 按错误信封解析（code 401 → token 失效等）。
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/event-stream") {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		var env apiEnvelope
		if json.Unmarshal(raw, &env) == nil && !env.businessOK() {
			kind := ErrNone
			if env.Code == deadTokenCode {
				kind = ErrSessionDead
			} else if env.Code == modelBlockedCode {
				kind = ErrModelBlocked
			} else {
				kind = Classify(resp.StatusCode, string(raw))
			}
			if kind == ErrNone {
				kind = ErrClient
			}
			log.Printf("WARN: [upstream] chat_stream acct=%s: upstream 200 business error body=%s",
				logfmt.Label(a.UID, a.Nickname), truncate(string(raw), 200))
			msg := env.Message
			if msg == "" {
				msg = env.Msg
			}
			ue := &Error{Kind: kind, Status: resp.StatusCode,
				Msg: fmt.Sprintf("code=%s msg=%s", env.Code, truncate(msg, 160))}
			if d, ok := ParseRetryAfter(resp.Header); ok {
				ue.RetryAfter = d
			}
			return nil, resp.StatusCode, raw, ue
		}
		// 200 + 成功信封但非流（理论形态）：按空流处理——包回 body 交给调用方。
		return io.NopCloser(bytes.NewReader(raw)), resp.StatusCode, nil, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}

// IsSessionDeadCode 报告 SSE error 帧内的业务 code 是否为 token 失效（"401"）。
// hint.go 的帧分类用（error.code 是字符串形态）。
func IsSessionDeadCode(code string) bool {
	return strings.TrimSpace(code) == deadTokenCode
}

// IsSessionDead 报告错误是否为 token 失效（业务 code 401 或失效文案）：
// 池据此禁用账号，恢复需要重新登录（login.sh SMS 流程）。
func IsSessionDead(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	if ue.Kind == ErrSessionDead {
		return true
	}
	// 防御：Kind 未判 session_dead 但 Msg 携带精确 401 信封 / 失效文案。
	return hasExactBusinessCode(ue.Msg, sessionDeadCodeValue) ||
		sessionDeadTextRule.hit(ue.Msg, strings.ToLower(ue.Msg))
}

// hasBusinessEnvelope 报告错误 body 是否携带业务信封形态（JSON 且含 code/msg/message 字段）。
// WAF 403 判定（IsWafBlocked）用「无业务信封」区分 WAF 拦截页（HTML/空体/纯文本）
// 与业务层 403（带信封，正常走既有分类）。
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`) ||
		strings.Contains(body, `"message":`) || strings.Contains(body, `"code": `) ||
		strings.Contains(body, `"msg": `) || strings.Contains(body, `"message": `)
}

// IsWafBlocked 报告 403 响应是否为 WAF 拦截形态：HTTP 403 且 body 无业务信封。
func IsWafBlocked(status int, body string) bool {
	return status == http.StatusForbidden && !hasBusinessEnvelope(body)
}
