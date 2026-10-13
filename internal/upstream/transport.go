// transport.go 出站 Transport 构造的单一事实来源（连接层加固）：
// 真正禁 h2 / TLS 握手超时 / 短 keepalive 探测，参数集中定义可测试可调整。
//
// RicheeAI 特记（SPEC 事实）：TLS 握手从 Go 默认 transport 偶发超时——
// 这里除握手上限外，短 RPC 侧还有 doJSON 的 3 次退避重试（client.go）。
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// 连接层参数集中定义。
const (
	// dialTimeout TCP 连接建立上限：连不上就快速失败轮转换号。
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive 探测周期：NAT 黑洞里的死连接快速识别。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限：RicheeAI 实测偶发握手挂起，
	// 无此上限时只能干等 HTTP.Client.Timeout。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout 空闲连接池保留时长。
	idleConnTimeout = 30 * time.Second
	// responseHeaderTimeout 聊天 SSE 首字节前（响应头）硬上限。
	// 本常量只是构造默认，main 会按 config 覆盖。
	responseHeaderTimeout = 60 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost 连接池容量。
const (
	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

// newDialer 构造出站拨号器。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例）。
// 空 TLSNextProto（非 nil）真正禁 h2：nil 表示「让标准库注入默认 h2 映射」，
// ALPN 协商出 h2 后半死流复用表现为 "http2: timeout awaiting response headers"。
func newTransport() *http.Transport {
	dialer := newDialer()
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// closeIdler 实现该接口的 RoundTripper 支持清空空闲连接池。
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle 在传输层请求失败后清掉 rt 所属 Transport 的空闲连接池：
// 失败连接可能仍留在空闲池里，下一个请求会继续捡到它。best-effort。
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}
