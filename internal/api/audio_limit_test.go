package api_test

// Speech length at the OpenAI edge.
//
// The upstream caps one synthesis at SpeechTextLimit (1024) characters. The
// gateway used to work around that by splitting long input into pieces and
// joining the audio; measured live on 2026-10-09, a piece of ~1020 characters
// (what an 1110-character input splits into) makes the upstream fail the task
// itself after ~278 s ("upstream audio task failed (task ...)"), so raising the
// deadline does not help and the workaround only delays the error. The cap is
// now the contract: over-length input is refused before any upstream work, with
// the limit in the message, and a request at the limit still costs one task.
// See docs/decisions/0010-speech-length-is-a-refused-limit.md.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// ttsGenerateCalls counts the upstream synthesis tasks in a recorded list.
func ttsGenerateCalls(reqs []testutil.AudioRequest) int {
	n := 0
	for _, r := range reqs {
		if strings.Contains(r.Path, "/tts/v2/generate") {
			n++
		}
	}
	return n
}

// TTSLIMIT-001: input over the upstream cap is refused client-side, with the
// limit and the actual length in the message, and no upstream task is created —
// the client learns immediately instead of waiting out a doomed synthesis.
func TestSpeechOverUpstreamCapRejected(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	long := strings.Repeat("小米 MiMo 语音合成测试。", 74) // 1110 runes
	if n := len([]rune(long)); n <= mimo.SpeechTextLimit {
		t.Fatalf("test input is %d runes; it must exceed the %d cap", n, mimo.SpeechTextLimit)
	}
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, long))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "input_too_long") {
		t.Errorf("body = %s, want code input_too_long", body)
	}
	if !strings.Contains(string(body), fmt.Sprint(mimo.SpeechTextLimit)) {
		t.Errorf("body = %s, want the limit (%d) named in the message", body, mimo.SpeechTextLimit)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 0 {
		t.Errorf("upstream synthesis tasks = %d, want 0 (refused before any upstream work)", got)
	}
}

// TTSLIMIT-002: exactly at the cap is still accepted, and costs exactly one
// synthesis — the refusal must not creep one character in.
func TestSpeechAtUpstreamCapIsOneCall(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	atCap := strings.Repeat("字", mimo.SpeechTextLimit)
	if n := len([]rune(atCap)); n != mimo.SpeechTextLimit {
		t.Fatalf("fixture is %d runes, want exactly %d", n, mimo.SpeechTextLimit)
	}
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, atCap))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("content type = %q, want audio/wav", resp.Header.Get("Content-Type"))
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 1 {
		t.Errorf("upstream synthesis tasks = %d, want 1", got)
	}
}

// TTSLIMIT-003: a configured cap above the upstream limit does not raise it —
// the upstream cannot do more, so accepting the request would only turn a clear
// 400 into a slow upstream failure.
func TestSpeechConfiguredCapCannotExceedUpstream(t *testing.T) {
	ts := audioServer(t, oneAccount(), &testserver.TestServerOptions{SpeechMaxChars: 5000})
	over := strings.Repeat("字", mimo.SpeechTextLimit+1)
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, over))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 even with SpeechMaxChars=5000", resp.StatusCode, body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 0 {
		t.Errorf("upstream synthesis tasks = %d, want 0", got)
	}
}

// TTSLIMIT-004: a failed synthesis yields an error, never the audio produced so
// far — a truncated answer would be worse than a retry.
func TestSpeechFailureIsNotReturnedAsAudio(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	ts.Upstream.SetAudioStatusScript("tts", "failed")
	resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"这句会合成失败。"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 with %d bytes; a failed task must not yield audio", len(body))
	}
	if strings.HasPrefix(string(body), "RIFF") {
		t.Error("the response body is audio despite a failed task")
	}
}

// TTSLIMIT-005: an operator-lowered cap below the upstream limit still applies.
func TestSpeechConfiguredLowerCapApplies(t *testing.T) {
	ts := audioServer(t, oneAccount(), &testserver.TestServerOptions{SpeechMaxChars: 10})
	resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"这句话有十一个字了吧。"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 0 {
		t.Errorf("upstream synthesis tasks = %d, want 0", got)
	}
}
