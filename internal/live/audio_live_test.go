package live

// LIVE-TTS-001 / LIVE-ASR-001: the audio surface against the real Studio.
//
// One test covers both because the honest check is a closed loop: synthesize a
// sentence, then feed those exact bytes back for recognition and compare the
// transcript with what was spoken. The ASR finding is therefore evidence about
// real upstream audio, not about a fixture.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

const liveSentence = "你好，这是一次语音合成测试。"

func TestLiveAudioSurfaceRoundTrip(t *testing.T) {
	liveGate(t)
	h := newLiveHarness(t, noMutation, true)

	// ---- LIVE-TTS-001: synthesize ----
	start := time.Now()
	speech := h.do(t, http.MethodPost, "/v1/audio/speech", map[string]any{
		"model":           "mimo-v2.5-tts",
		"input":           liveSentence,
		"voice":           "冰糖",
		"response_format": "wav",
	})
	ttsElapsed := time.Since(start)
	if speech.Status != http.StatusOK {
		t.Fatalf("speech status = %d body=%s", speech.Status, truncate(string(speech.Body), 300))
	}
	contentType := speech.Headers.Get("Content-Type")
	riif := bytes.HasPrefix(speech.Body, []byte("RIFF"))
	lines := []string{
		"POST /v1/audio/speech {input: " + liveSentence + ", voice: 冰糖, response_format: wav}",
		"status=" + strconv.Itoa(speech.Status) + " content-type=" + contentType +
			" bytes=" + strconv.Itoa(len(speech.Body)) + " riff=" + boolStr(riif) +
			" elapsed=" + ttsElapsed.Round(time.Millisecond).String(),
	}
	if !riif || len(speech.Body) < 1024 {
		lines = append(lines, "VERDICT: no usable audio returned — do not treat TTS as working")
		recordFindings(t, "LIVE-TTS-001", "synthesize a sentence via /v1/audio/speech", lines)
		t.Fatal("synthesized audio is not a usable WAV")
	}
	dir := os.Getenv("MIMO2API_LIVE_AUDIO_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	out := filepath.Join(dir, "live_tts.wav")
	if err := os.WriteFile(out, speech.Body, 0o600); err != nil {
		t.Errorf("save audio: %v", err)
	} else {
		lines = append(lines, "saved="+out)
	}
	lines = append(lines, "VERDICT: real WAV produced by the upstream through the gateway")
	recordFindings(t, "LIVE-TTS-001", "synthesize a sentence via /v1/audio/speech", lines)

	// ---- LIVE-ASR-001: recognize the bytes we just synthesized ----
	start = time.Now()
	asr := liveMultipart(t, h, speech.Body)
	asrElapsed := time.Since(start)
	lines = []string{
		"POST /v1/audio/transcriptions (multipart, filename live_tts.wav, model mimo-v2.5-asr, language zh)",
		"status=" + strconv.Itoa(asr.Status) + " content-type=" + asr.Headers.Get("Content-Type") +
			" elapsed=" + asrElapsed.Round(time.Millisecond).String(),
		"body=" + truncate(string(asr.Body), 200),
	}
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(asr.Body, &got); err != nil {
		lines = append(lines, "VERDICT: response is not the documented {\"text\": ...} JSON — check the endpoint")
		recordFindings(t, "LIVE-ASR-001", "transcribe the synthesized audio", lines)
		t.Fatalf("decode transcript: %v (body %s)", err, truncate(string(asr.Body), 200))
	}
	same := normalizeCJK(got.Text) == normalizeCJK(liveSentence)
	lines = append(lines, "spoken="+liveSentence, "transcribed="+got.Text, "match="+boolStr(same))
	if same {
		lines = append(lines, "VERDICT: the spoken sentence came back verbatim — TTS and ASR are both live")
	} else {
		lines = append(lines, "VERDICT: transcript differs from the spoken sentence — report it, do not claim a clean round trip")
	}
	recordFindings(t, "LIVE-ASR-001", "transcribe the synthesized audio", lines)
}

// liveMultipart posts an audio file to the transcription endpoint.
func liveMultipart(t *testing.T, h *harness, audio []byte) httpResp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "live_tts.wav")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	for k, v := range map[string]string{"model": "mimo-v2.5-asr", "language": "zh", "response_format": "json"} {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("multipart field %s: %v", k, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/audio/transcriptions", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+liveKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return httpResp{Status: resp.StatusCode, Body: body, Headers: resp.Header}
}

// normalizeCJK drops punctuation and whitespace so a punctuation difference in
// the transcript does not read as a failed round trip.
func normalizeCJK(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
