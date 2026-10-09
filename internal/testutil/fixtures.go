package testutil

import (
	"encoding/json"
	"strings"

	"github.com/yg/mimo2api/internal/adapter"
)

// SSE fixtures recorded as Go string constants (TDD §2.1). Wire dialect is
// the MiMo one: field colon WITHOUT a trailing space (`event:message`),
// frames separated by blank lines (SDD §2.5). Thinking markers carry their
// trailing NUL byte ("<think>\0" / "</think>\0" markers, SDD §2.5).

// frame builds one SSE event frame in the MiMo dialect.
func frame(event, data string) string {
	out := ""
	if event != "" {
		out += "event:" + event + "\n"
	}
	return out + "data:" + data + "\n\n"
}

// jsonStr JSON-encodes a string (for embedding content with NULs/markers).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// FixtureSSEBasic: dialogId → 3 text deltas → usage → finish [DONE]
// (TDD SSE-001, API-001/002).
const FixtureSSEBasic = FixtureSSEDialogPrefix + FixtureSSEBodyHello

// FixtureSSEDialogPrefix: the dialogId frame shared by fixtures.
const FixtureSSEDialogPrefix = "event:dialogId\ndata:{\"type\":null,\"content\":\"dlg-0001\",\"usage\":null}\n\n"

// FixtureSSEBodyHello: 3 text deltas → usage → finish [DONE].
const FixtureSSEBodyHello = "event:message\ndata:{\"type\":\"text\",\"content\":\"Hel\",\"usage\":null}\n\n" +
	"event:message\ndata:{\"type\":\"text\",\"content\":\"lo w\",\"usage\":null}\n\n" +
	"event:message\ndata:{\"type\":\"text\",\"content\":\"orld\",\"usage\":null}\n\n" +
	"event:usage\ndata:{\"promptTokens\":2242,\"completionTokens\":78,\"totalTokens\":2320," +
	"\"nativeUsage\":{\"prompt_tokens\":2242,\"completion_tokens\":78,\"total_tokens\":2320," +
	"\"prompt_tokens_details\":{\"cached_tokens\":100},\"completion_tokens_details\":{\"reasoning_tokens\":32}}}\n\n" +
	"event:finish\ndata:{\"type\":null,\"content\":\"[DONE]\",\"usage\":null}\n\n"

// FixtureSSECRLF: FixtureSSEBasic with \r\n line endings (TDD SSE-002).
var FixtureSSECRLF = strings.ReplaceAll(FixtureSSEBasic, "\n", "\r\n")

// FixtureSSENoSpace: mixed `event: x` (space) and `event:x` (MiMo dialect)
// variants — both must parse identically (TDD SSE-003).
const FixtureSSENoSpace = "event: dialogId\r\n" +
	"data:{\"type\":null,\"content\":\"dlg-0001\",\"usage\":null}\r\n\r\n" +
	"event:message\ndata:{\"type\":\"text\",\"content\":\"Hi\",\"usage\":null}\n\n" +
	"event:usage\ndata:{\"promptTokens\":10,\"completionTokens\":2,\"totalTokens\":12}\n\n" +
	"event:finish\ndata:{\"type\":null,\"content\":\"[DONE]\",\"usage\":null}\n\n"

// Thinking-marker bytes with the protocol's trailing NUL (SDD §2.5).
var (
	thinkOpenNUL  = adapter.ThinkOpen + "\x00"
	thinkCloseNUL = adapter.ThinkClose + "\x00"
)

// FixtureSSEThinking: single think block then plain text (TDD SSE-006,
// API-010).
var FixtureSSEThinking = frame("dialogId", `{"type":null,"content":"dlg-0002","usage":null}`) +
	frame("message", `{"type":"text","content":`+jsonStr(thinkOpenNUL+"Let me check the math."+thinkCloseNUL)+`,"usage":null}`) +
	frame("message", `{"type":"text","content":"The answer is 4.","usage":null}`) +
	frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)

// FixtureSSEThinkingSplit: marker bytes straddling delta boundaries
// (TDD SSE-007): the open marker is split mid-tag across two deltas, and
// the close marker likewise.
var FixtureSSEThinkingSplit = func() string {
	op := thinkOpenNUL
	cl := thinkCloseNUL
	halfO := len(adapter.ThinkOpen) / 2
	halfC := len(adapter.ThinkClose) / 2
	return frame("dialogId", `{"type":null,"content":"dlg-0003","usage":null}`) +
		frame("message", `{"type":"text","content":`+jsonStr("before "+op[:halfO])+`,"usage":null}`) +
		frame("message", `{"type":"text","content":`+jsonStr(op[halfO:]+"reasoning here"+cl[:halfC])+`,"usage":null}`) +
		frame("message", `{"type":"text","content":`+jsonStr(cl[halfC:]+"after")+`,"usage":null}`) +
		frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)
}()

// FixtureSSEThinkingUnterminated: open marker never closed; everything
// after it counts as reasoning (TDD SSE-009).
var FixtureSSEThinkingUnterminated = frame("dialogId", `{"type":null,"content":"dlg-0004","usage":null}`) +
	frame("message", `{"type":"text","content":`+jsonStr("pure text "+thinkOpenNUL+"loose thought continues")+`,"usage":null}`) +
	frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)

// FixtureSSEMultiThink: two think blocks interleaved with text (TDD
// SSE-011).
var FixtureSSEMultiThink = frame("dialogId", `{"type":null,"content":"dlg-0005","usage":null}`) +
	frame("message", `{"type":"text","content":`+jsonStr(thinkOpenNUL+"first block"+thinkCloseNUL)+`,"usage":null}`) +
	frame("message", `{"type":"text","content":"middle text","usage":null}`) +
	frame("message", `{"type":"text","content":`+jsonStr(thinkOpenNUL+"second block"+thinkCloseNUL+"after text")+`,"usage":null}`) +
	frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)

// fixtureUsageLast: the last (winning) usage event of the multi-usage
// fixture — a raw string so nothing gets mangled.
const fixtureUsageLast = `{"promptTokens":30,"completionTokens":9,"totalTokens":39,"nativeUsage":{"prompt_tokens":30,"completion_tokens":9,"total_tokens":39,"prompt_tokens_details":{"cached_tokens":7},"completion_tokens_details":{"reasoning_tokens":3}}}`

// FixtureSSEMultiUsage: usage emitted 3×, the last one wins (TDD SSE-012,
// API-004).
var FixtureSSEMultiUsage = frame("dialogId", `{"type":null,"content":"dlg-0006","usage":null}`) +
	frame("message", `{"type":"text","content":"ok","usage":null}`) +
	frame("usage", `{"promptTokens":10,"completionTokens":1,"totalTokens":11}`) +
	frame("usage", `{"promptTokens":20,"completionTokens":5,"totalTokens":25}`) +
	frame("usage", fixtureUsageLast) +
	frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)

// FixtureSSEError: two deltas then event:error mid-stream (TDD API-021,
// SSE-005).
var FixtureSSEError = frame("dialogId", `{"type":null,"content":"dlg-0007","usage":null}`) +
	frame("message", `{"type":"text","content":"par","usage":null}`) +
	frame("message", `{"type":"text","content":"tial","usage":null}`) +
	frame("error", `{"type":null,"content":"upstream exploded","usage":null}`)

// FixtureSSESensitive: content-moderation block (TDD ERRMAP-009).
var FixtureSSESensitive = frame("sensitive_query", `{"type":null,"content":"topic not allowed","usage":null}`)

// FixtureSSEEmpty: dialogId → finish, zero message deltas (TDD SSE-013).
var FixtureSSEEmpty = frame("dialogId", `{"type":null,"content":"dlg-0008","usage":null}`) +
	frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)

// FixtureSSEFastchat: fastchat-shaped stream with repeated usage events
// (TDD SSE-012, LIVE-009 analogue).
var FixtureSSEFastchat = frame("dialogId", `{"type":null,"content":"dlg-0009","usage":null}`) +
	frame("message", `{"type":"text","content":"fast ","usage":null}`) +
	frame("message", `{"type":"text","content":"reply","usage":null}`) +
	frame("usage", `{"promptTokens":5,"completionTokens":5,"totalTokens":10}`) +
	frame("usage", `{"promptTokens":5,"completionTokens":8,"totalTokens":13}`) +
	frame("finish", `{"type":null,"content":"[DONE]","usage":null}`)

// FixtureSSEStall: plain stream the mock serves with a deliberate
// mid-stream delay (TDD API-019, SSE-014). Byte content is FixtureSSEBasic.
const FixtureSSEStall = FixtureSSEBasic

// XSSI passport fixtures (TDD LOGIN-002..005, RENEW-002/003). Locations
// point at the MockPassport itself; tests that need a live URL use the
// mock's own STS handler instead of these constants.
const xssiPrefix = "&&&START&&&"

// FixtureXSSILoginOK: code 0 + location + userId (TDD LOGIN-002/003).
const FixtureXSSILoginOK = xssiPrefix + `{"code":0,"description":"ok","location":"https://account.xiaomi.com/pass/sts?ticket=ticket1","userId":123456}` + "\n"

// FixtureXSSILoginCaptcha: captcha demand (TDD LOGIN-004).
const FixtureXSSILoginCaptcha = xssiPrefix + `{"code":87001,"description":"need captcha","captchaUrl":"https://account.xiaomi.com/captcha"}` + "\n"

// FixtureXSSILoginOTP: new-device OTP demand (TDD LOGIN-005).
const FixtureXSSILoginOTP = xssiPrefix + `{"code":87002,"description":"need device verification","notificationUrl":"https://account.xiaomi.com/device"}` + "\n"

// FixtureXSSISTSOk: code 0 + location (TDD RENEW-002).
const FixtureXSSISTSOk = xssiPrefix + `{"code":0,"description":"OK","location":"https://account.xiaomi.com/pass/sts?ticket=ticket9"}` + "\n"

// FixtureXSSISTS70016: passToken dead (TDD RENEW-003).
const FixtureXSSISTS70016 = xssiPrefix + `{"code":70016,"description":"login expired"}` + "\n"

// FixtureXSSILoginOTPZero: live-captured shape (2026-10-08 smoke test) —
// code=0, securityStatus=128, EMPTY location, notificationUrl set. A
// new-device OTP demand that arrives with code 0, not an error code (SDD
// §2.2 amendment). The mock rewrites the notificationUrl host to itself.
const FixtureXSSILoginOTPZero = xssiPrefix + `{"notificationUrl":"https://account.xiaomi.com/pass2/redirect?sid=xiaomichatbot&context=ctx-otp-1","result":"ok","code":0,"miDemo":0,"captchaUrl":null,"description":"成功","location":"","securityStatus":128,"pwd":0,"child":0,"desc":"成功"}` + "\n"
