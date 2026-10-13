// Package headers 构造 RicheeAI 上游请求头。
//
// 鉴权是单个 richee-token 头（无 Bearer/refresh 双令牌）；其余头对齐
// RicheeAI 网页客户端的常规形态（JSON Accept / UA / Origin）。
package upstream

import (
	"net/http"
	"strings"

	"richee2api/internal/auth"
	"richee2api/internal/session"
)

// defaultUserAgent 出站 UA：对齐 RicheeAI 网页控制台的常见浏览器 UA 形态
// （官方 SKIP_GEETEST_ENABLED=true 客户端同样无设备指纹，UA 无风控语义）。
const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

// userAgent 返回当前出站 UA：Client.UserAgent 显式覆盖 > 默认。
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return defaultUserAgent
}

// CommonHeaders 设置所有 API 共享的请求头（richee-token 鉴权 + JSON + UA）。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if a != nil {
		if tok := a.TokenValue(); tok != "" {
			req.Header.Set("richee-token", tok)
		}
	}
}

// ChatMeta 一次 chat 出站的会话头族元数据。RicheeAI 无官方会话头族
// （无 X-Conversation-* 证据），此结构保留为网关侧聚合键的载体：
// ConversationID 用于 cache key 与粘性，透传优先不伪造。
type ChatMeta struct {
	ConversationID        string // body 提取的入站值，空则不发
	ConversationRequestID string // 网关侧轮级聚合主键
	TraceID               string // 入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属头。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// chat 流式 Accept 覆盖非流式默认。
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.injectClientIP(req, clientIP)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游。
// 按**参数传递**而非读共享字段：避免并发请求交叉污染对方 IP。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，
// 回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// AuthHeaders 登录族（SMS code）请求头：无 token（登录前）。
func (c *Client) AuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
}

// NewMessageID 生成消息级 ID（会话头族/幂等键用）。
// 委托 session 包，避免 id 生成逻辑两处漂移。
var NewMessageID = session.NewMessageID
