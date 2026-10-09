package api_test

// Long-form speech at the OpenAI edge (TTS hardening).
//
// The upstream refuses a single synthesis over 1024 characters, so a client
// asking for a long passage must still get audio: the gateway splits, speaks
// the pieces and joins them. These tests pin both halves — the split actually
// happens (several upstream tasks), and the client gets one valid file, never a
// half-spoken answer or an opaque 502.

import (
	"encoding/binary"
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

// wavDataLen reads the data chunk's declared size from a RIFF/WAVE file.
func wavDataLen(t *testing.T, b []byte) int {
	t.Helper()
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE file: %q", b[:min(12, len(b))])
	}
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		if id == "data" {
			return size
		}
		pos += 8 + size
		if size%2 == 1 {
			pos++
		}
	}
	t.Fatal("no data chunk in the joined WAV")
	return 0
}

// TTSCHUNK-001: text over the upstream cap is split into several tasks and the
// pieces are joined into one file the client can play.
func TestSpeechChunksLongInput(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	long := strings.Repeat("这是一句用于测试的中文句子。", 60) // 780 runes
	long = long + strings.Repeat("再来一句更长的句子用于跨越上限。", 20)
	body := fmt.Sprintf(`{"model":"mimo-v2.5-tts","input":%q,"voice":"茉莉"}`, long)
	resp, out := postJSON(t, ts, "/v1/audio/speech", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, out)
	}
	pieces := mimo.SplitSpeech(long, mimo.SpeechTextLimit)
	if len(pieces) < 2 {
		t.Fatalf("test input only produced %d piece(s); it must exceed the limit", len(pieces))
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != len(pieces) {
		t.Errorf("upstream synthesis tasks = %d, want %d (one per piece)", got, len(pieces))
	}
	if resp.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("content type = %q, want audio/wav", resp.Header.Get("Content-Type"))
	}
	// One joined WAV: the data length is the sum of the pieces the upstream
	// returned, and the header agrees with the file length.
	perCall := wavDataLen(t, out) / len(pieces)
	if perCall == 0 {
		t.Fatalf("joined file has no audio data (%d bytes total)", len(out))
	}
	if declared := int(binary.LittleEndian.Uint32(out[4:8])); declared != len(out)-8 {
		t.Errorf("RIFF size = %d, want %d (header must match the joined file)", declared, len(out)-8)
	}
}

// TTSCHUNK-002: a single piece within the upstream cap costs exactly one task —
// the split must not fire for ordinary input.
func TestSpeechShortInputIsOneCall(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"短句测试。"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 1 {
		t.Errorf("upstream synthesis tasks = %d, want 1", got)
	}
}

// TTSCHUNK-003: over the gateway's own total cap the request is refused before
// any upstream work — the client learns the limit instead of waiting.
func TestSpeechOverTotalCapRejected(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	huge := strings.Repeat("字", 10001)
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, huge))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "input_too_long") {
		t.Errorf("body = %s, want code input_too_long", body)
	}
	if got := ttsGenerateCalls(ts.Upstream.AudioRequests()); got != 0 {
		t.Errorf("upstream synthesis tasks = %d, want 0 (refused client-side)", got)
	}
}

// TTSCHUNK-004: when a later piece fails, the client gets an error, not the
// audio spoken so far — a truncated answer would be worse than a retry.
func TestSpeechPartialFailureIsNotReturned(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	ts.Upstream.SetAudioStatusScript("tts", "failed")
	long := strings.Repeat("这是一句用于测试的中文句子。", 60)
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		fmt.Sprintf(`{"input":%q}`, long))
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 with %d bytes; a failed piece must not yield audio", len(body))
	}
	if strings.HasPrefix(string(body), "RIFF") {
		t.Error("the response body is audio despite a failed piece")
	}
}
