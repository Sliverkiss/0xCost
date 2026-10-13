// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
// RicheeAI 的 delta 带 reasoning_content（思维链）字段，两路（聚合/透传）
// 均保留。错误帧形态：data:{"error":{"message","type":"business_error","code":"210001"}}。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// errEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为「上游空流」。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// Aggregate 读取完整 SSE 流，聚合 delta.content / delta.reasoning_content 为
// 单个 OpenAI chat.completion 响应。分片/半行由 bufio.Reader.ReadString 处理；
// 遇到 "data: [DONE]" 结束。tool_calls 按流式 delta 形态（按 index 合并）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		toolSeq       int
		idIndex       = map[string]int{}
	)
	// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：
	// 空串不算「已取到正文」、也不占 latch 名额（OpenAI 风格 role-only 首帧是常态帧）。
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表。
	// index 缺失兼容（id 优先、lastIdx 兜底）：带 index → 按 index 累积；
	// 缺 index 带 id 且已见过 → 延续该 id 所在 index；缺 index 带 id 且是新的 →
	// 开新序号；缺 index 无 id → 追加到最近收到碎片的 index。
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCallsChunk := func(tcs []any) {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			} else if cid, _ := call["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					idx = mid
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1]
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeToolCallDelta(merged, call)
		}
	}
	// mergeMessageFields 把非 delta 的完整 message 内容并入聚合。
	mergeMessageFields := func(msg map[string]any) {
		if r2, ok := msg["role"].(string); ok && r2 != "" {
			role = r2
		}
		if txt, ok := msg["content"].(string); ok {
			appendContent(txt)
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			// RicheeAI 实测帧形：data:{...}（无空格）与 data: {...}（带空格）并存。
			payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if payload == "[DONE]" {
				// 上游显式结束：DONE 之后的任何数据一律忽略。
				sawDone = true
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 错误帧不是数据事件（无 choices）：不计数（聚合结果按空流报错，
					// 由 handler 映射 502；原文透传语义在 Stream 路径）。
					if _, isErr := chunk["error"]; !isErr {
						validEvents++
					}
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									mergeToolCallsChunk(tcs)
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）：整条并入。
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游 200 但无任何有效数据事件：报错，由 handler 映射为 502 upstream_parse。
		return nil, errEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// 流被截断时 tool_call 的 arguments 是残缺 JSON，不把脏参数交给客户端。
		if finishReason == "length" || !sawDone {
			calls = dropTruncatedToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = ensureUsageTotal(usage)
	}
	return resp, nil
}

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens
// 都在时补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// num64 把 JSON number（float64/int64/int 均可）归一为 float64；非数字返回 ok=false。
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖，function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除。这是 OpenAI
// 官方流的真实形态，累加型与覆盖型客户端在该形态下同时正确。
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if seen[idx] {
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			seen[idx] = true
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声，空 delta 键一律省略，usage 缺失 → null。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-richee2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}

// StreamHint 同 Stream，但上游 error 帧透出前把 hintFn(payload) 的返回值写入
// error.gateway_hint。hintFn 为 nil 或返回空串 → 原样透传（零改写）。
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index。
	toolCallSeen := map[int]bool{}

	// firstID 透传流的消息级 id 基准：同一条 SSE 消息所有帧共用一个真实 id。
	firstID := ""

	// writeRaw 原样写出一帧（绕过 normalizeFrame）并 flush。
	writeRaw := func(payload string) error {
		if hint := frameGatewayHint(hintFn, payload); hint != "" {
			payload = attachHintToErrorFrame(payload, hint)
		}
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		valid := 0
		if json.Unmarshal([]byte(payload), &obj) == nil {
			// 上游错误帧透传：带 error 键的帧原样写出（白名单会剥掉 error 字段，
			// 客户端就看不到上游 code/message）。
			if _, hasErr := obj["error"]; hasErr {
				if werr := writeRaw(payload); werr != nil {
					return 0, werr
				}
				return 1, nil
			}
			// token 失效的流内形态（RicheeAI 实测：data:{"error":{...,"code":"210001"}}）
			// 已在上方 error 分支原样透传，这里只处理正常帧。
			stripToolCallNames(obj, toolCallSeen)
			// id 续传：首帧非空真实 id 缓存；后续帧缺 id/空 id 用缓存值。
			if firstID == "" {
				if v, ok := obj["id"].(string); ok && v != "" {
					firstID = v
				}
			} else {
				if v, ok := obj["id"].(string); !ok || v == "" {
					obj["id"] = firstID
				}
			}
			if raw, err := json.Marshal(normalizeFrame(obj)); err == nil {
				payload = string(raw)
			}
			valid = 1
		}
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return 0, werr
		}
		if fl != nil {
			fl.Flush()
		}
		return valid, nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case isDoneFrame(trimmed):
			// 上游显式结束：DONE 之后的任何数据一律不再透传。
			break readLoop
		case strings.HasPrefix(trimmed, "data:"):
			n, werr := writeFrame(trimPayload(trimmed))
			validFrames += n
			if werr != nil {
				return werr
			}
		case trimmed != "":
			// 注释/其他行：原样透传
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	// 空流（0 有效帧）：先写一帧 error，再补 [DONE] 保证客户端能正常收尾。
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if validFrames == 0 {
		return errEmptyStream
	}
	return nil
}

// isDoneFrame 判定 [DONE] 帧（data:[DONE] / data: [DONE] 双形态）。
func isDoneFrame(line string) bool {
	rest := strings.TrimPrefix(line, "data:")
	rest = strings.TrimPrefix(rest, " ")
	return rest == "[DONE]"
}

// trimPayload 剥掉 data: 前缀与可选的一个空格，返回帧 payload。
func trimPayload(line string) string {
	return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
}

// frameGatewayHint 取 error 帧的 gateway_hint（hintFn 缺失/异常返回空串 → 不附加）。
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic 隔离：hint 判定是补充功能，任何实现缺陷不得击穿流透传主路径。
	defer func() { _ = recover() }()
	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame 在 error 帧的 error 对象上附加 gateway_hint 字段。
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}
