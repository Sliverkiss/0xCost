// resolve_model.go 模型名解析（单 realm）。
package server

import "strings"

// resolveModel 解析模型名。RicheeAI 单 realm：前缀协议退役——"richee:" 前缀
// 剥离（容忍历史双域客户端），其余原样、realm 恒 "richee"。
// 导出为 ResolveModel（粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "richee", model
	}
	prefix := model[:idx]
	if prefix != "richee" {
		return "richee", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
