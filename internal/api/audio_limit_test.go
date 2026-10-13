package api_test

// Speech length at the OpenAI edge.
//
// Owner ruling 2026-10-14, verbatim: "字数预检砍了，超了上游会自己报错透传" —
// there is no local precheck anywhere on the speech path. The upstream owns the
// 1024-character synthesis cap (SpeechTextLimit is its mirror, ADR 0010) and
// answers over-length input with its own refusal ("播报内容长度不能超过1024，
// 当前长度为：1500"); classifyAudioUpstream translates that refusal into the
// client-facing 400 input_too_long carrying the upstream's evidence. These
// tests pin the passthrough: over-length input still costs exactly one
// synthesis task, and the answer is the upstream's, not ours.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/mimo"
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

// TTSLIMIT-001 (ruling): input over the upstream cap is forwarded as sent; the
// upstream refuses it, and its own wording ("播报内容长度不能超过1024…") passes
// through to the client as a 400 input_too_long. Exactly one synthesis task —
// the cap is judged upstream, nowhere else.
func TestSpeechOverUpstreamCapRefusedByUpstream(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	long := strings.Repeat("小米 MiMo 语音合成测试。", 74) // 1110 runes
	if n := len([]rune(long)); n <= mimo.SpeechTextLimit {
		t.Fatalf("test input is %d runes; it must exceed the %d cap", n, mimo.SpeechTextLimit)
	}
	// The upstream's own over-length refusal, exactly as the Studio sends it.
	ts.Upstream.SetAudioHTTPStatus(http.StatusBadRequest,
		`{"code":400,"msg":"播报内容长度不能超过1024，当前长度为：1110"}`)
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, long))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 (the upstream's own refusal)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "input_too_long") {
		t.Errorf("body = %s, want code input_too_long", body)
	}
	if !strings.Contains(string(body), "播报内容长度不能超过1024") {
		t.Errorf("body = %s, want the upstream's own sentence passed through", body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 1 {
		t.Errorf("upstream synthesis tasks = %d, want 1 (forwarded; the upstream decides)", got)
	}
	// Restore normal service so t.Cleanup-state stays sane for other checks.
	ts.Upstream.SetAudioHTTPStatus(0, "")
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

// TTSLIMIT-003 (ruling, "字数预检砍了"): there is no operator cap to raise
// above the upstream's any more — the request is forwarded regardless of
// length, and the answer is always the upstream's own. This pins that the
// 5000-character request below produces one task and the upstream refusal, not
// a local rejection.
func TestSpeechConfiguredCapIsGoneForwardedAndRefused(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	over := strings.Repeat("字", mimo.SpeechTextLimit+1)
	ts.Upstream.SetAudioHTTPStatus(http.StatusBadRequest,
		`{"code":400,"msg":"播报内容长度不能超过1024，当前长度为：1025"}`)
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, over))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 (the upstream's own refusal)", resp.StatusCode, body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 1 {
		t.Errorf("upstream synthesis tasks = %d, want 1", got)
	}
	ts.Upstream.SetAudioHTTPStatus(0, "")
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

// TTSLIMIT-005 (ruling): no operator-lowered cap either — a short input the
// old MIMO2API_SPEECH_MAX_CHARS would have rejected locally is forwarded and
// served. The knob is gone with the precheck.
func TestSpeechNoOperatorLowerCap(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"这句话有十一个字了吧。"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (forwarded; nothing rejects locally)", resp.StatusCode, body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 1 {
		t.Errorf("upstream synthesis tasks = %d, want 1", got)
	}
}
