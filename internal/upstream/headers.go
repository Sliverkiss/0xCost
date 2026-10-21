package upstream

import (
	"math/rand/v2"
	"net/http"

	"chatglm2api/internal/signer"
)

// UserAgent is one pinned browser fingerprint. Identities keep the same pair
// for their whole life so a guest never looks like it switched browsers.
type UserAgent struct {
	UA       string `json:"ua"`
	SecCHUA  string `json:"sec_ch_ua"`
	Platform string `json:"platform"`
	Mobile   string `json:"mobile"`
}

// userAgents is the fixed set the gateway pins identities to. The Chrome
// version in the UA string and Sec-CH-UA must agree; mixing them is a
// fingerprint mismatch the real SPA never produces.
var userAgents = []UserAgent{
	{
		UA:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
		SecCHUA:  `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		Platform: `"Windows"`, Mobile: "?0",
	},
	{
		UA:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		SecCHUA:  `"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="24"`,
		Platform: `"Windows"`, Mobile: "?0",
	},
	{
		UA:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
		SecCHUA:  `"Google Chrome";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		Platform: `"macOS"`, Mobile: "?0",
	},
	{
		UA:       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
		SecCHUA:  `"Google Chrome";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		Platform: `"Linux"`, Mobile: "?0",
	},
}

// PickUserAgent returns a random pinned fingerprint.
func PickUserAgent() UserAgent {
	return userAgents[rand.IntN(len(userAgents))]
}

// UserAgents returns a copy of the pinned set.
func UserAgents() []UserAgent {
	out := make([]UserAgent, len(userAgents))
	copy(out, userAgents)
	return out
}

// UserAgentFor returns the fingerprint at index i (wraps).
func UserAgentFor(i int) UserAgent {
	if len(userAgents) == 0 {
		return UserAgent{}
	}
	return userAgents[((i%len(userAgents))+len(userAgents))%len(userAgents)]
}

// DefaultUserAgent is the fallback when an identity carries no pinned pair.
func DefaultUserAgent() UserAgent { return userAgents[0] }

// requestKind distinguishes the Accept/streaming headers the SPA sends.
type requestKind int

const (
	kindJSON requestKind = iota
	kindStream
	kindPage
)

// SetCommonHeaders applies the header set shared by every API request
// (docs/reverse/SIGNING.md). Authorization is added by the caller when needed.
func SetCommonHeaders(req *http.Request, ua UserAgent, referer string) {
	if ua.UA == "" {
		ua = DefaultUserAgent()
	}
	if referer == "" {
		referer = "https://chatglm.cn/main/alltoolsdetail"
	}
	h := req.Header
	h.Set("Content-Type", "application/json;charset=utf-8")
	h.Set("App-Name", "chatglm")
	h.Set("X-App-Platform", "pc")
	h.Set("X-App-Version", "0.0.1")
	h.Set("X-App-fr", "default")
	h.Set("X-Lang", "zh")
	h.Set("X-Exp-Groups", "")
	h.Set("X-Device-Model", "")
	h.Set("X-Device-Brand", "")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Origin", "https://chatglm.cn")
	h.Set("Referer", referer)
	h.Set("User-Agent", ua.UA)
	if ua.SecCHUA != "" {
		h.Set("Sec-CH-UA", ua.SecCHUA)
	}
	if ua.Platform != "" {
		h.Set("Sec-CH-UA-Platform", ua.Platform)
	}
	if ua.Mobile != "" {
		h.Set("Sec-CH-UA-Mobile", ua.Mobile)
	}
}

// SetSignature adds a fresh X-Timestamp/X-Nonce/X-Sign triple.
func SetSignature(req *http.Request) signer.Signature {
	sig := signer.NowAt(nowFunc())
	req.Header.Set("X-Timestamp", sig.Timestamp)
	req.Header.Set("X-Nonce", sig.Nonce)
	req.Header.Set("X-Request-Id", signer.RandomHex32())
	req.Header.Set("X-Sign", sig.Sign)
	return sig
}

// SetDeviceID sets the per-identity device header.
func SetDeviceID(req *http.Request, deviceID string) {
	req.Header.Set("X-Device-Id", deviceID)
}

// SetBearer sets the Authorization header.
func SetBearer(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
}

// SetAccept configures the Accept family for JSON vs SSE calls.
func SetAccept(req *http.Request, kind requestKind) {
	switch kind {
	case kindStream:
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	default:
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
}

// SetEgress sets X-Forwarded-For when a value is configured.
func SetEgress(req *http.Request, xff string) {
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
}
