// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
//
// 与 workbuddy2api 的差异（见 docs/PORT_NOTES.md）：
//   - 单 realm：无 cn:/global: 模型名前缀协议，模型名原样路由（resolveModel 恒
//     ("richee", 原串)，保留函数形态以兼容 handler 内部调用）。
//   - 模型列表来自 upstream 静态目录（RicheeAI 无模型目录端点）。
//   - WAF IP fail-fast 状态机保留（403 无信封形态仍可能出现在网关链路）。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"richee2api/internal/auth"
	"richee2api/internal/logfmt"
	"richee2api/internal/pool"
	"richee2api/internal/prompt"
	"richee2api/internal/session"
	"richee2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s

	// PromptMode "passthrough"（默认，透传客户端原始 system）/ "custom"（网关替换）/
	// "append"（两者并用）。
	PromptMode string
	// PromptText custom/append 模式下注入的系统提示词文本。
	PromptText string

	// AdminEnabled 运维管理端点开关（config admin.enabled，默认 false）。
	// 关闭时 /admin/* 一律 404（不向外暴露"这里存在管理面"）。
	AdminEnabled bool
}

// sessionDeadReason token 失效禁用的持久化 reason。
const sessionDeadReason = "token dead (business 401), re-login required"

// notFoundCooldown 上游 404 的固定短冷却时长。
// 404 是上游偶发路径缺失，不是"本账号在限流"，固定 60s 防雪崩即可，
// 不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// wafCooldownBase WAF 403 软冷却基数（60s 起；抖动 ±25% 后落 [45s,75s]）。
const wafCooldownBase = 60 * time.Second

// ServiceName 网关身份标识（/healthz service 字段 + X-Service 头双保险）。
const ServiceName = "richee2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough"
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))
	// 运维管理端点（默认关闭）。条件注册而非 handler 内 404：未注册的路由
	// 对探测与真 404 完全不可区分（不暴露管理面存在）。
	if cfg.AdminEnabled {
		h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.withAuth(h.adminAccountDisable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.withAuth(h.adminAccountEnable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/revive", h.withAuth(h.adminAccountRevive))
	}
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			// 常量时间比较：!= 短路时序随前缀长度变化，公网暴露下理论上可逐字节
			// 探测 key 前缀；ConstantTimeCompare 消除该信号。
			provided := strings.TrimPrefix(authz, "Bearer ")
			if !strings.HasPrefix(authz, "Bearer ") ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	realmServable := map[string]bool{
		"richee": h.cfg.Pool.ServableForRealm("richee"),
	}
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"realm_totals": map[string]map[string]int{
			"richee": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("richee")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	})
}

// countsMapFrom 把 CountsDetailed 五元组编码为 /status realm_totals 的字段对象。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// models 返回模型列表（RicheeAI 静态目录，无动态拉取）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// fmtCreditsPrefix 从目录 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把模型对象字段（ModelInfo）按「空值省略」写出规则合入
// /v1/models 条目。不覆盖 id/object/created/owned_by 基础字段。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 静态目录包装成 OpenAI 格式（含 context_length）。
// RicheeAI 单 realm 无前缀：id 原样输出。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, mi := range upstream.StaticModels() {
		entry := map[string]any{
			"id":       mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "richee",
		}
		entry["context_length"] = upstream.ContextWindowListing(mi.ID)
		if mo, ok := upstream.MaxOutputTokensListing(mi.ID); ok {
			entry["max_output_tokens"] = mo
		}
		entry = applyModelInfoFields(entry, mi)
		out = append(out, entry)
	}
	return out
}

// fetchDynamicModels 供 metrics enrichCredits 用：RicheeAI 静态目录快照。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	return upstream.StaticModels()
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 单 realm：模型名原样路由（无前缀协议）。
	_, bareModel := resolveModel(peek.Model)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号。
	sessKey := session.ExtractKey(body)
	stickyKey := sessKey
	if stickyKey == "" {
		stickyKey = session.StickyFallbackKey(body)
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		if uid, ok := h.cfg.Session.ResolveForModel(stickyKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键（改写前取——prompt.Rewrite 会动 messages）。
	turnKey := session.TurnKey(body)
	// gateway_hint 判定所需的请求形态（image_url part）。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// 会话头族 meta：RicheeAI 无官方会话头族要求，ConversationID 只用于
	// cache key 与粘性。轮级聚合键与参考实现同语义。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先，否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil {
				unbindSticky()
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, "")
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		heldUID = acct.UID

		// 客户端 IP 透传（仅 PassthroughIP 开启）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 r.Context()：客户端断连/请求取消立即中断在途上游调用并释放租约。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 网络层抖动：只换号，不喂熔断计数；连败兜底喂连败计数。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		if uerr != nil || status >= 400 {
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：降级中性提示词同请求内重试。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID)
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 上下文超限：请求的问题非账号的问题——立即透传原文，不罚号不轮转。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避）。
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 210001 负缓存清命：该账号该模型实测成功，立即解除避让。
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 粘性跟随最终成功号。
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
		if peek.Stream {
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}))
			if upstream.IsEmptyStreamError(sErr) {
				st.status = http.StatusBadGateway
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			}
			st.ttfb = stats.TTFB()
			toks, hasUsage := stats.Tokens()
			if hasUsage {
				st.toks = toks
			}
			st.hasUsage = hasUsage
			st.prompt = stats.PromptTokens()
			st.cacheHit, st.cacheMiss, st.cacheWr = stats.CacheTokens()
			if credit, ok := stats.Credit(); ok {
				st.credit = credit
				st.hasCredit = true
				// 成本账本：usage 带 credit 与 token 总数时记录实测单价。
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		if credit, total, ok := usageCreditTotal(resp); ok {
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		fillStatFromUsage(st, resp)
		return
	}
	// 末端错误透传（error-passthrough）：上游错误原文透传，不规范化成固定文案。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			msg = s // 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
		}
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
}

// promptTooLongMessage 透传 message：上游 body 原文；空 body 兜底可读短文案。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// rotateBackoff 轮转间指数退避 + 抖动：第 i 次轮转失败（continue 换号前）等待
// backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），ctx 取消返回 false。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 重置墙钟优先（RicheeAI 暂无该文案）；Retry-After 头次之；最后有界退避。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Time{}, "429 rate limit")
	case upstream.ErrSessionDead:
		// token 失效（业务 401）是确定性终态：chat 路径立即禁用（重登才能恢复）。
		// 连续计数形态只用于 scheduler 探活（偶发抖动容忍）。
		h.cfg.Pool.Disable(uid, sessionDeadReason)
	case upstream.ErrNotFound:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrModelBlocked:
		// 210001「模型不存在」：(账号, 模型) 负缓存避让，指数退避 TTL。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// ErrClient 喂连败计数（连败 N 次临时出池）；ErrContentBlocked/
		// ErrPromptTooLong/ErrImageInvalid/ErrBadParams 零动作（请求级问题）。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加 error.gateway_hint
// （hint 为空串时不带字段）。message 仍是上游原文透传。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 gateway_hint 判定上下文：请求裸模型名 + 是否带图 +
// 模型目录 supports_images 声明（静态目录只读，无上游调用）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range upstream.StaticModels() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
