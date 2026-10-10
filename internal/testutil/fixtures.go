package testutil

import (
	"embed"
	"fmt"
	"strings"
	"testing"
)

// Recorded exact-byte fixtures live as committed files under testdata/
// (TDD §2.1). They are embedded so internal/mimo, internal/api and
// internal/testserver all read the same bytes regardless of the test
// binary's working directory; the files stay the source of truth, and a
// missing/renamed file is a go:embed build error or a loud load panic —
// never a silent empty string.
//
// Wire dialect (SDD §2.5): field colon WITHOUT a trailing space
// (`event:message`, `data:{...}`), frames separated by a blank line.
// Thinking markers are `<think>` / `</think>` followed by a NUL byte, which
// on the JSON wire is the 6-byte escape `\u0000` (PROTOCOL.md §3); a raw
// 0x00 inside a JSON string is rejected by encoding/json, so the fixture
// stores the escape and the decoded content carries the real NUL.

//go:embed testdata
var testdataFS embed.FS

// mustFixture loads testdata/<name>, panicking with the file name when it is
// missing — a fixture bug must fail loudly, never degrade to "".
func mustFixture(name string) string {
	b, err := testdataFS.ReadFile("testdata/" + name)
	if err != nil {
		panic(fmt.Sprintf("testutil: missing fixture testdata/%s: %v", name, err))
	}
	return string(b)
}

// LoadFixture reads testdata/<name>, failing the test (naming the file) on a
// missing or empty read (TDD §2.2).
func LoadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := testdataFS.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("testutil: missing fixture testdata/%s: %v", name, err)
	}
	if len(b) == 0 {
		t.Fatalf("testutil: empty fixture testdata/%s", name)
	}
	return string(b)
}

// SSE fixtures, backed by testdata/*.txt (TDD §2.1).
var (
	// FixtureSSEBasic: dialogId → 3 text deltas → usage → finish [DONE]
	// (TDD SSE-001, API-001/002).
	FixtureSSEBasic = mustFixture("sse_basic.txt")
	// FixtureSSEWebSearch: dialogId → doc → web_search (two results) →
	// message deltas → usage → finish — the real search frame shape captured
	// from the Studio (M19, TDD SSE-015/WS-004).
	FixtureSSEWebSearch = mustFixture("sse_websearch.txt")
	// FixtureSSECRLF: FixtureSSEBasic with \r\n line endings (TDD SSE-002).
	FixtureSSECRLF = mustFixture("sse_crlf.txt")
	// FixtureSSENoSpace: mixed `event: x` (space) and `event:x` (MiMo
	// dialect) variants — both parse identically (TDD SSE-003).
	FixtureSSENoSpace = mustFixture("sse_nospace.txt")
	// FixtureSSEThinking: single think block then plain text (TDD SSE-006,
	// API-010).
	FixtureSSEThinking = mustFixture("sse_thinking.txt")
	// FixtureSSEThinkingSplit: marker bytes straddling delta boundaries
	// (TDD SSE-007).
	FixtureSSEThinkingSplit = mustFixture("sse_thinking_split.txt")
	// FixtureSSEThinkingUnterminated: open marker never closed; everything
	// after it counts as reasoning (TDD SSE-009).
	FixtureSSEThinkingUnterminated = mustFixture("sse_thinking_unterminated.txt")
	// FixtureSSEMultiThink: two think blocks interleaved with text
	// (TDD SSE-011).
	FixtureSSEMultiThink = mustFixture("sse_multi_think.txt")
	// FixtureSSEMultiUsage: usage emitted 3×, the last one wins (TDD
	// SSE-012, API-004).
	FixtureSSEMultiUsage = mustFixture("sse_multi_usage.txt")
	// FixtureSSEError: two deltas then event:error mid-stream (TDD API-021,
	// SSE-005).
	FixtureSSEError = mustFixture("sse_error.txt")
	// FixtureSSETooLong: the platform's own too-long refusal, which arrives as a
	// mid-stream error event carrying English prose (ADR 0015). The gateway
	// translates it into a 400 the caller can act on.
	FixtureSSETooLong = mustFixture("sse_too_long.txt")
	// FixtureSSESensitive: content-moderation block (TDD ERRMAP-009).
	FixtureSSESensitive = mustFixture("sse_sensitive.txt")
	// FixtureSSEEmpty: dialogId → finish, zero message deltas (TDD SSE-013).
	FixtureSSEEmpty = mustFixture("sse_empty.txt")
	// FixtureSSEFastchat: fastchat-shaped stream with repeated usage events
	// (TDD SSE-012, LIVE-009 analogue).
	FixtureSSEFastchat = mustFixture("sse_fastchat.txt")
	// FixtureSSEStall: plain stream the mock serves with a deliberate
	// mid-stream delay; byte content is sse_basic.txt (TDD API-019,
	// SSE-014).
	FixtureSSEStall = mustFixture("sse_stall.txt")
)

// FixtureSSEDialogPrefix / FixtureSSEBodyHello decompose FixtureSSEBasic at
// its first message frame, kept for callers that build variants.
var FixtureSSEDialogPrefix, FixtureSSEBodyHello = cutBasic(FixtureSSEBasic)

// FixtureOpenAIStreamExpected is the recorded OpenAI chunk sequence that the
// client-facing stream must reproduce byte-for-byte once the per-request id and
// timestamp are normalized (TDD API-002, ADR 0034).
var FixtureOpenAIStreamExpected = mustFixture("openai_stream_expected.txt")

func cutBasic(s string) (prefix, body string) {
	i := strings.Index(s, "event:message")
	if i < 0 {
		panic("testutil: sse_basic.txt has no message frame")
	}
	return s[:i], s[i:]
}

// XSSI passport fixtures (TDD LOGIN-002..005, RENEW-002/003). Locations
// point at account.xiaomi.com; the MockPassport rewrites the host to itself.
var (
	// FixtureXSSILoginOK: code 0 + location + userId (TDD LOGIN-002/003).
	FixtureXSSILoginOK = mustFixture("xssi_login_ok.txt")
	// FixtureXSSILoginCaptcha: captcha demand (TDD LOGIN-004).
	FixtureXSSILoginCaptcha = mustFixture("xssi_login_captcha.txt")
	// FixtureXSSILoginOTP: new-device OTP demand (TDD LOGIN-005).
	FixtureXSSILoginOTP = mustFixture("xssi_login_otp.txt")
	// FixtureXSSISTSOk: code 0 + location (TDD RENEW-002).
	FixtureXSSISTSOk = mustFixture("xssi_sts_ok.txt")
	// FixtureXSSISTS70016: passToken dead (TDD RENEW-003).
	FixtureXSSISTS70016 = mustFixture("xssi_sts_70016.txt")
	// FixtureXSSILoginOTPZero: live-captured shape (2026-10-08 smoke test) —
	// code=0, securityStatus=128, EMPTY location, notificationUrl set. A
	// new-device OTP demand that arrives with code 0, not an error code (SDD
	// §2.2 amendment). The mock rewrites the notificationUrl host to itself.
	FixtureXSSILoginOTPZero = mustFixture("xssi_login_otp_zero.txt")
)
