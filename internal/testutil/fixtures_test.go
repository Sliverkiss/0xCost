package testutil

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// tddFixtures is the exact fixture file list from docs/TDD.md §2.1. It is
// encoded here (not discovered) so a missing or renamed fixture fails the
// test instead of silently dropping out of the checks.
var tddFixtures = []string{
	"sse_basic.txt",
	"sse_crlf.txt",
	"sse_nospace.txt",
	"sse_thinking.txt",
	"sse_thinking_split.txt",
	"sse_thinking_unterminated.txt",
	"sse_multi_think.txt",
	"sse_multi_usage.txt",
	"sse_error.txt",
	"sse_sensitive.txt",
	"sse_empty.txt",
	"sse_fastchat.txt",
	"sse_stall.txt",
	"xssi_login_ok.txt",
	"xssi_login_captcha.txt",
	"xssi_login_otp.txt",
	"xssi_sts_ok.txt",
	"xssi_sts_70016.txt",
	"openai_stream_expected.txt",
}

// extraFixtures back exported testutil names that the TDD §2.1 table does not
// list (the live-captured code=0 OTP shape used by the OTP-001 tests).
var extraFixtures = []string{"xssi_login_otp_zero.txt"}

// thinkingFixtures are the recorded streams embedding `<think>`/`</think>`
// markers.
var thinkingFixtures = []string{
	"sse_thinking.txt",
	"sse_thinking_split.txt",
	"sse_thinking_unterminated.txt",
	"sse_multi_think.txt",
}

// readFixture reads a committed fixture straight from disk. fixtures_test.go
// lives in package testutil, so `go test ./internal/testutil` runs with the
// package directory as the working directory and the relative path resolves.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// TestFixturesExistNonEmpty asserts every table fixture is a real, non-empty
// committed file — and that the embedded loader yields the same bytes.
func TestFixturesExistNonEmpty(t *testing.T) {
	all := append(append([]string{}, tddFixtures...), extraFixtures...)
	for _, name := range all {
		b := readFixture(t, name)
		if len(b) == 0 {
			t.Errorf("fixture %s: empty", name)
			continue
		}
		embedded, err := testdataFS.ReadFile("testdata/" + name)
		if err != nil || string(embedded) != string(b) {
			t.Errorf("fixture %s: embedded copy != on-disk file (err=%v)", name, err)
		}
	}
}

// TestExportedFixturesMatchFiles guarantees the exported testutil names are
// backed by the committed files (no inline drift).
func TestExportedFixturesMatchFiles(t *testing.T) {
	pairs := map[string]string{
		"sse_basic.txt":                 FixtureSSEBasic,
		"sse_crlf.txt":                  FixtureSSECRLF,
		"sse_nospace.txt":               FixtureSSENoSpace,
		"sse_thinking.txt":              FixtureSSEThinking,
		"sse_thinking_split.txt":        FixtureSSEThinkingSplit,
		"sse_thinking_unterminated.txt": FixtureSSEThinkingUnterminated,
		"sse_multi_think.txt":           FixtureSSEMultiThink,
		"sse_multi_usage.txt":           FixtureSSEMultiUsage,
		"sse_error.txt":                 FixtureSSEError,
		"sse_too_long.txt":              FixtureSSETooLong,
		"sse_sensitive.txt":             FixtureSSESensitive,
		"sse_empty.txt":                 FixtureSSEEmpty,
		"sse_fastchat.txt":              FixtureSSEFastchat,
		"sse_stall.txt":                 FixtureSSEStall,
		"xssi_login_ok.txt":             FixtureXSSILoginOK,
		"xssi_login_captcha.txt":        FixtureXSSILoginCaptcha,
		"xssi_login_otp.txt":            FixtureXSSILoginOTP,
		"xssi_sts_ok.txt":               FixtureXSSISTSOk,
		"xssi_sts_70016.txt":            FixtureXSSISTS70016,
		"xssi_login_otp_zero.txt":       FixtureXSSILoginOTPZero,
	}
	for name, got := range pairs {
		if want := string(readFixture(t, name)); got != want {
			t.Errorf("%s: exported value does not match the file", name)
		}
	}
	// LoadFixture exercises the embedded loader (TDD §2.2).
	if LoadFixture(t, "sse_basic.txt") != FixtureSSEBasic {
		t.Errorf("LoadFixture(sse_basic.txt) != FixtureSSEBasic")
	}
}

// TestMustFixtureIsLoud proves a missing fixture panics with the file name
// rather than degrading to an empty string.
func TestMustFixtureIsLoud(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mustFixture must panic on a missing fixture")
		}
		if !strings.Contains(fmt.Sprint(r), "no_such_fixture.txt") {
			t.Errorf("panic message must name the file: %v", r)
		}
	}()
	_ = mustFixture("no_such_fixture.txt")
}

// TestSSEFixtureDialect pins the byte-level wire rules (SDD §2.5).
func TestSSEFixtureDialect(t *testing.T) {
	basic := string(readFixture(t, "sse_basic.txt"))
	for _, want := range []string{"event:dialogId", "event:message", "event:usage", "event:finish", "data:{"} {
		if !strings.Contains(basic, want) {
			t.Errorf("sse_basic.txt missing %q", want)
		}
	}
	if strings.Contains(basic, "event: ") || strings.Contains(basic, "data: ") {
		t.Errorf("sse_basic.txt must use `event:x`/`data:{...}` (no space after the colon)")
	}
	if !strings.HasSuffix(basic, `data:{"type":null,"content":"[DONE]","usage":null}`+"\n\n") {
		t.Errorf("sse_basic.txt must end on the finish [DONE] frame")
	}

	// sse_crlf.txt == sse_basic.txt with CRLF endings.
	crlf := string(readFixture(t, "sse_crlf.txt"))
	if !strings.Contains(crlf, "\r\n") {
		t.Errorf("sse_crlf.txt must contain \\r\\n")
	}
	if strings.Contains(strings.ReplaceAll(crlf, "\r\n", "\n"), "\r") {
		t.Errorf("sse_crlf.txt has a stray \\r")
	}
	if strings.ReplaceAll(crlf, "\r\n", "\n") != basic {
		t.Errorf("sse_crlf.txt must equal sse_basic.txt with CRLF line endings")
	}

	// sse_nospace.txt must have at least one `event:x` (no space after colon).
	ns := string(readFixture(t, "sse_nospace.txt"))
	found := false
	for i := 0; ; {
		j := strings.Index(ns[i:], "event:")
		if j < 0 {
			break
		}
		at := i + j + len("event:")
		if at < len(ns) && ns[at] != ' ' && ns[at] != '\r' && ns[at] != '\n' {
			found = true
			break
		}
		i = at
	}
	if !found {
		t.Errorf("sse_nospace.txt: no `event:` immediately followed by a non-space name")
	}

	// sse_stall.txt carries the sse_basic bytes.
	if string(readFixture(t, "sse_stall.txt")) != basic {
		t.Errorf("sse_stall.txt must carry the sse_basic.txt bytes")
	}

	// The OpenAI golden is a chunk sequence ending in [DONE].
	golden := string(readFixture(t, "openai_stream_expected.txt"))
	if !strings.HasPrefix(golden, "data: {") || !strings.Contains(golden, "data: [DONE]") {
		t.Errorf("openai_stream_expected.txt is not an OpenAI chunk sequence")
	}
	if !strings.Contains(golden, `"object":"chat.completion.chunk"`) {
		t.Errorf("openai_stream_expected.txt missing chat.completion.chunk frames")
	}
}

// decodedMessageContent concatenates the `content` field of every `message`
// event in a recorded stream, decoding the data JSON exactly as the SSE
// parser does (internal/mimo/sse.go).
func decodedMessageContent(t *testing.T, name, body string) string {
	t.Helper()
	var out strings.Builder
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimRight(frame, "\r\n")
		var event string
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimRight(line, "\r")
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				event = strings.TrimPrefix(v, " ")
				continue
			}
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				payload := strings.TrimPrefix(v, " ")
				if event != "message" {
					continue
				}
				var p struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal([]byte(payload), &p); err != nil {
					t.Fatalf("%s: message payload is not valid JSON: %v", name, err)
				}
				out.WriteString(p.Content)
			}
		}
	}
	return out.String()
}

// TestThinkingFixturesCarryRealNUL asserts the thinking fixtures really encode
// a NUL after `<think>`/`</think>`.
//
// On the JSON wire the NUL is the 6-byte escape `\u0000` (PROTOCOL.md §3); a
// raw 0x00 byte inside a JSON string is rejected by encoding/json
// ("invalid character '\x00' in string literal"), so a raw byte would make
// the fixture unparseable and break the SSE parser. The file therefore spells
// the escape, and it must decode to a *real* 0x00 byte immediately after the
// marker — never the two-character sequence `\0`.
func TestThinkingFixturesCarryRealNUL(t *testing.T) {
	for _, name := range thinkingFixtures {
		body := string(readFixture(t, name))

		// A raw 0x00 inside a JSON string is invalid; the wire must use the
		// `\u0000` escape and must never spell the two-character `\0`.
		if !strings.Contains(body, `\u0000`) {
			t.Errorf("%s: no `\\u0000` NUL escape on the wire", name)
		}
		if strings.Contains(body, `\0`) {
			t.Errorf("%s: two-character `\\0` where a NUL is meant is a bug", name)
		}
		// Where a marker is contiguous in the raw file (not split across
		// deltas), it must be immediately followed by the escape.
		for _, marker := range []string{"<think>", "</think>"} {
			if idx := strings.Index(body, marker); idx >= 0 {
				after := body[idx+len(marker):]
				if !strings.HasPrefix(after, `\u0000`) {
					t.Errorf("%s: raw %s not followed by \\u0000", name, marker)
				}
			}
		}

		// Decoded content (as the SSE parser sees it after JSON decode) must
		// carry a real 0x00 immediately after every marker occurrence.
		content := decodedMessageContent(t, name, body)
		markers := 0
		for _, marker := range []string{"<think>", "</think>"} {
			for i := 0; ; {
				j := strings.Index(content[i:], marker)
				if j < 0 {
					break
				}
				at := i + j + len(marker)
				if at >= len(content) || content[at] != 0x00 {
					t.Errorf("%s: decoded %s not followed by a real 0x00 byte", name, marker)
				} else {
					markers++
				}
				i = at
			}
		}
		if markers == 0 {
			t.Errorf("%s: decoded content carries no <think>NUL / </think>NUL marker", name)
		}
	}
}
