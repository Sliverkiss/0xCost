// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游聊天按 SSE 吐流）
//  2. tool_choice 归一化（对象形态转字符串）
//  3. image_url 归一化（字符串形态转对象）
//  4. developer 角色归一为 system
//  5. tool_call↔tool 配对清理（孤儿剔除，防上游 400）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时跳过指纹脱敏（归一化照常）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（OpenAI 新客户端只发别名，
	// 上游只认 max_tokens——别名透传会被忽略后回落默认输出上限）。
	translateMaxCompletionTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}：
	// 上游据此在末帧返回 usage 用量。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	// tool 配对两步（见 tool_pairing.go）：先重排再清理。所有模型一律执行。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		obj["messages"] = msgs
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	model, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(nil, model))
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
// developer 是 OpenAI 新规范里 system 的别名（Codex/Cursor 等新客户端用它承载
// system 级指令），改写为 system 不丢语义。此归一化是「协议兼容」，
// 有意与 sanitize 参数解耦：即使 sanitize=false 也照常归一。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("[upstream] role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法：
// 字符串形态转 {"url": 原值}；已有对象原样保留。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// normalizeToolChoice 按上游约束改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// richeeEfforts RicheeAI 双模型的静态 effort 表（effort 降级管线数据源）。
// 上游无模型目录端点，档位能力按静态表声明；deepseek 系按 deepseek-v4 家族
// 通行的 low/high 档声明。
func richeeEfforts() map[string][]string {
	return map[string][]string{
		"deepseek-v4-flash": {"low", "high"},
		"deepseek-v4-pro":   {"low", "high"},
	}
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens。规则：显式 max_tokens 优先（别名只删）；别名非正数值
// （0/null/负数）不翻译；非数值别名（字符串等畸形）不翻译。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删
	if !has {
		return
	}
	if v, explicit := obj["max_tokens"]; explicit {
		// 显式 max_tokens 优先：别名只删不译。数值形态归一为 int64
		//（json.Unmarshal 解出 float64，出站统一整数口径）。
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			obj["max_tokens"] = int64(f)
		}
		return
	}
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}
