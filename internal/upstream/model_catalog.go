// model_catalog.go RicheeAI 模型目录（静态种子表）。
//
// RicheeAI 的 chat 端点只收两个模型 id（SPEC 事实）：
//   - deepseek-v4-flash（服务端解析为 deepseek-v4-flash-0731）
//   - deepseek-v4-pro
//
// 无上游模型目录端点（无 /v3/config、无 enterprises/models 证据），
// 模型能力以静态表声明；/v1/models 输出以此为权威。
package upstream

// ModelInfo 模型信息（静态声明口径）。
type ModelInfo struct {
	ID                string
	Name              string
	ContextWindow     int64 // 最大输入 token
	MaxTokens         int64 // 最大输出 token
	Efforts           []string
	DefaultEffort     string
	SupportsImages    bool
	Description       string
	Credits           string // 积分倍率原文（仅展示）
	Tags              []string
	Vendor            string
	IsDefault         bool
	SupportsReasoning bool
	SupportsToolCall  bool
	OnlyReasoning     bool
	MaxAllowedSize    int64
	ReasoningEffort   string
	ReasoningSummary  string
}

// staticModels RicheeAI 静态种子表。
//
// ContextWindow/MaxTokens：RicheeAI 未在响应中声明上下文口径；按上游
// DeepSeek 家族通行的 128k 上下文 / 8k 输出声明（与 deepseek-v4 系公开口径
// 一致），客户端按 context_length 提前截断时以此为参考。flash 与 pro 的
// 差异是推理深度（flash 轻量快答、pro 深度推理），上下文口径相同。
// deepseek-v4-flash 服务端别名 0731 版：/v1/models 只透出网关侧 id。
var staticModels = []ModelInfo{
	{
		ID:                "deepseek-v4-flash",
		Name:              "DeepSeek V4 Flash",
		ContextWindow:     128 * 1024,
		MaxTokens:         8192,
		Description:       "DeepSeek V4 Flash：轻量快速档，服务端解析为 deepseek-v4-flash-0731",
		Credits:           "x0.05",
		Vendor:            "deepseek",
		IsDefault:         true,
		SupportsReasoning: true,
		SupportsToolCall:  true,
		Tags:              []string{"chat", "reasoning"},
	},
	{
		ID:                "deepseek-v4-pro",
		Name:              "DeepSeek V4 Pro",
		ContextWindow:     128 * 1024,
		MaxTokens:         8192,
		Description:       "DeepSeek V4 Pro：深度推理档",
		Credits:           "x0.29",
		Vendor:            "deepseek",
		SupportsReasoning: true,
		SupportsToolCall:  true,
		Tags:              []string{"chat", "reasoning", "pro"},
	},
}

// StaticModels 返回静态种子表副本（调用方可安全改写）。
func StaticModels() []ModelInfo {
	out := make([]ModelInfo, len(staticModels))
	copy(out, staticModels)
	return out
}

// ModelKnown 报告模型 id 是否在网关目录内（chat 出站前的 fail-fast 校验用）。
func ModelKnown(id string) bool {
	for _, m := range staticModels {
		if m.ID == id {
			return true
		}
	}
	return false
}

// ContextWindowListing 返回模型的 context_length（目录命中取声明值，
// 未收录回落 131072 的 OpenAI 惯例值，防客户端按缺省 0 截断）。
func ContextWindowListing(id string) int64 {
	for _, m := range staticModels {
		if m.ID == id {
			return m.ContextWindow
		}
	}
	return 131072
}

// MaxOutputTokensListing 返回模型的 max_output_tokens；未收录返回 ok=false。
func MaxOutputTokensListing(id string) (int64, bool) {
	for _, m := range staticModels {
		if m.ID == id {
			return m.MaxTokens, true
		}
	}
	return 0, false
}
