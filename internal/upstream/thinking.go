// thinking.go DeepSeek 思维链开启：出站请求体注入 thinking:{type:"enabled"} + 默认档位。
//
// RicheeAI 实测（2026-10-13）：chat 端点对不带 thinking 字段的请求默认开启思考
// （delta.reasoning_content 恒有值）。注入显式开关 + effort 档位仍按参考语义
// 保留（上游 OpenAI 兼容层接受该字段族），多轮一致性由
// backfillReasoningContent 保证 assistant 消息回带 reasoning_content。
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort 默认档兜底。
const defaultDeepSeekEffort = "high"

// lookupDefaultEffort 从模型声明表查默认档；nil/未收录 → 空串（回退硬编码）。
func lookupDefaultEffort(defaultEfforts map[string]string, model string) string {
	if len(defaultEfforts) == 0 || model == "" {
		return ""
	}
	return defaultEfforts[model]
}

// isDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写）。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// backfillReasoningContent DeepSeek 多轮一致性：每条 assistant 消息保证
// reasoning_content 字段且值为 string。已有 string 原样保留；reasoning 是
// 非空 string 且 rc 非 string → 复制 reasoning 值；两者皆无 → 补空串 ""。
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok {
		if typ, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			thinkingEnabled = true
		}
	}
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		rc, hasRC := msg["reasoning_content"].(string)
		if hasRC {
			// rc 已有 string → 不覆盖。
		} else if r, ok := msg["reasoning"].(string); ok {
			rc = r
			msg["reasoning_content"] = rc
		} else {
			rc = ""
			msg["reasoning_content"] = rc
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			continue
		}
		if rc != "" {
			msg["reasoning"] = rc
		} else {
			msg["reasoning"] = " "
		}
	}
}

// injectThinking 按 DeepSeek 思维链开关规则改写请求体。非 deepseek 零改动。
func injectThinking(obj map[string]any, defaultEffort string) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return
		}
		ensureDeepSeekEffort(obj, defaultEffort)
		return
	}
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj, defaultEffort)
}

// ensureDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底）。
func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	_, hasSnake := obj["reasoning_effort"]
	if hasSnake {
		return
	}
	_, hasCamel := obj["reasoningEffort"]
	if hasCamel {
		return
	}
	if defaultEffort == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
		}
		return
	}
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
	}
}
