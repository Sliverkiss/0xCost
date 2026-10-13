// hint.go 网关错误附加说明字段（error.gateway_hint）的单一事实来源。
//
// 纪律：
//   - error.message 永远是上游 body 原文透传；gateway_hint 只做与 message
//     **并列**的网关视角补充说明，绝不替换/包装 message。
//   - 文案集中在本文件，按 ErrKind + 上下文映射，不散落 handler 的 if-else。
//   - 未覆盖形态返回空串 → 响应不带该字段（不编造）。
package upstream

import (
	"encoding/json"
	"net/http"
)

// GatewayHint 按错误形态返回网关视角的补充说明（error.gateway_hint 字段值）。
// msg 是上游错误 body 原文（或 SSE error 帧 payload）；kind 是权威分类。
// 返回空串 = 未覆盖形态，调用方不带字段。
func GatewayHint(kind ErrKind, msg string, ctx HintContext) string {
	// token 失效的流内形态（RicheeAI SSE error 帧 code 210001）：与
	// ErrSessionDead 同 hint（该形态已在 FrameKind 判定侧命中）。
	if isSessionDeadFrame(msg) {
		return "account token is dead at upstream; the account is disabled until re-login via login.sh"
	}
	switch kind {
	case ErrPromptTooLong:
		return "request context exceeds the model's limit; reduce history/message size"
	case ErrSessionDead:
		return "account token is dead at upstream; the account is disabled until re-login via login.sh"
	case ErrSoftRate:
		return "rate limited by upstream; retry after reset"
	case ErrHardCredit:
		return "account credits exhausted at upstream; waiting for daily check-in to restore"
	case ErrContentBlocked:
		return "request content was rejected by content policy; adjust the prompt and retry"
	default:
		// ErrNone/ErrNotFound/ErrServer/ErrBadParams/ErrClient 等未覆盖形态：无 hint。
		return ""
	}
}

// HintContext gateway_hint 判定所需的请求侧上下文。零值合法。
type HintContext struct {
	Model               string
	HasImage            bool
	ModelSupportsImages bool
	ModelInCatalog      bool
}

// noHealthyHint 本地调度类错误的固定 hint。
const noHealthyHint = "no healthy account available in pool; check /status or retry later"

// NoHealthyAccountHint 本地调度错误的 gateway_hint。
func NoHealthyAccountHint() string { return noHealthyHint }

// FrameHintFunc 返回 SSE error 帧的 gateway_hint 判定函数（Stream 的可选参数）。
func FrameHintFunc(ctxFn func() HintContext) func(string) string {
	return func(payload string) string {
		if payload == "" || payload == "[DONE]" {
			return ""
		}
		return GatewayHint(FrameKind(payload), payload, ctxFn())
	}
}

// FrameKind 从 SSE error 帧 payload 判定 ErrKind：token 失效流内形态
// （code 210001/401 家族）优先；其余取帧内 error.message 走 Classify
// （请求级 400 口径）。判不出 → ErrNone（无 hint）。
func FrameKind(payload string) ErrKind {
	var f struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &f) != nil {
		return ErrNone
	}
	if IsSessionDeadCode(f.Error.Code) {
		return ErrSessionDead
	}
	if f.Error.Message == "" {
		return ErrNone
	}
	return Classify(http.StatusBadRequest, f.Error.Message)
}

// isSessionDeadFrame 判定 error 帧 payload 是否携带 token 失效业务码。
func isSessionDeadFrame(payload string) bool {
	var f struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &f) != nil {
		return false
	}
	return IsSessionDeadCode(f.Error.Code)
}
