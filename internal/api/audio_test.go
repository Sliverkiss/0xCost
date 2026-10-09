package api_test

// Endpoint tests for the web-surface audio surface (M18):
// POST /v1/audio/speech, POST /v1/audio/transcriptions, GET /v1/audio/voices.
//
// They run the real handler against the mock upstream whose payloads carry the
// captured Studio shapes, so a field-name or id-type mistake fails here rather
// than in production.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// audioServer builds a test server with sub-second polling so the
// generate → poll → download loop does not cost real seconds.
func audioServer(t *testing.T, accts []testserver.TestAccount, opts *testserver.TestServerOptions) *testserver.TestServer {
	t.Helper()
	if opts == nil {
		opts = &testserver.TestServerOptions{}
	}
	if opts.AudioPoll == 0 {
		opts.AudioPoll = time.Millisecond
	}
	return testserver.NewTestServer(t, accts, opts)
}

func postJSON(t *testing.T, ts *testserver.TestServer, path, body string) (*http.Response, []byte) {
	t.Helper()
	// ts.Chat always targets chat/completions, so issue the request directly.
	req, _ := http.NewRequest(http.MethodPost, ts.URL()+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	out, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	b, _ := io.ReadAll(out.Body)
	return out, b
}

func getPath(t *testing.T, ts *testserver.TestServer, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := ts.Get(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// AUDIO-TTS-001: a valid request returns real audio bytes with the right
// content type, and the upstream received the resolved voice and format.
func TestSpeechHappyPath(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	resp, body := postJSON(t, ts, "/v1/audio/speech",
		`{"model":"mimo-v2.5-tts","input":"你好，这是一次语音合成测试。","voice":"alloy","response_format":"mp3"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("content type = %q, want audio/mpeg", got)
	}
	if !bytes.HasPrefix(body, []byte("RIFF")) {
		t.Errorf("body does not start with RIFF: %q", body[:min(12, len(body))])
	}
	// The OpenAI alias must have been resolved to the account's default voice,
	// and the explicit mp3 format must have travelled upstream.
	var sentVoice, sentFormat string
	for _, r := range ts.Upstream.AudioRequests() {
		if r.Path != "/open-apis/tts/v2/generate" {
			continue
		}
		var payload struct {
			Content struct {
				Audio struct {
					Format string `json:"format"`
					Voice  string `json:"voice"`
				} `json:"audio"`
			} `json:"content"`
		}
		_ = json.Unmarshal(r.Body, &payload)
		sentVoice, sentFormat = payload.Content.Audio.Voice, payload.Content.Audio.Format
	}
	if sentVoice != "冰糖" {
		t.Errorf("upstream voice = %q, want the catalog default 冰糖 for the alias alloy", sentVoice)
	}
	if sentFormat != "mp3" {
		t.Errorf("upstream format = %q, want mp3", sentFormat)
	}
}

// AUDIO-TTS-002: an explicit Studio voice name is passed through.
func TestSpeechExplicitVoice(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	resp, body := postJSON(t, ts, "/v1/audio/speech", `{"input":"测试","voice":"茉莉"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var sentVoice string
	for _, r := range ts.Upstream.AudioRequests() {
		if r.Path != "/open-apis/tts/v2/generate" {
			continue
		}
		var payload struct {
			Content struct {
				Audio struct {
					Voice string `json:"voice"`
				} `json:"audio"`
			} `json:"content"`
		}
		_ = json.Unmarshal(r.Body, &payload)
		sentVoice = payload.Content.Audio.Voice
	}
	if sentVoice != "茉莉" {
		t.Errorf("upstream voice = %q, want 茉莉", sentVoice)
	}
}

// AUDIO-TTS-003: request validation.
func TestSpeechValidation(t *testing.T) {
	ts := audioServer(t, oneAccount(), &testserver.TestServerOptions{SpeechMaxChars: 10, AudioPoll: time.Millisecond})
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{"missing input", `{"input":"   "}`, "missing_input"},
		{"input too long", `{"input":"` + strings.Repeat("字", 11) + `"}`, "input_too_long"},
		{"bad format", `{"input":"hi","response_format":"opus"}`, "unsupported_response_format"},
		{"unknown voice", `{"input":"hi","voice":"不存在的音色"}`, "unknown_voice"},
		{"malformed json", `{"input":`, "malformed_request_body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postJSON(t, ts, "/v1/audio/speech", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.code) {
				t.Errorf("body = %s, want code %q", body, tc.code)
			}
		})
	}
	// Rejected requests must not synthesize or burn a conversation. Resolving an
	// unknown voice legitimately reads the catalog (bot/config) first, which is
	// why this asserts on the synthesis path rather than on all calls.
	for _, r := range ts.Upstream.AudioRequests() {
		if r.Path == "/open-apis/tts/v2/generate" || r.Path == "/open-apis/chat/conversation/save" {
			t.Errorf("a rejected request reached %s", r.Path)
		}
	}
}

// AUDIO-ASR-001: multipart transcription in all three response formats.
func TestTranscriptionsFormats(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	for _, tc := range []struct {
		name        string
		format      string
		contentType string
		check       func(t *testing.T, body []byte)
	}{
		{
			name: "json", format: "json", contentType: "application/json",
			check: func(t *testing.T, body []byte) {
				var out struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(body, &out); err != nil {
					t.Fatalf("not JSON: %v (%s)", err, body)
				}
				if out.Text != "你好，这是一次语音合成测试。" {
					t.Errorf("text = %q", out.Text)
				}
			},
		},
		{
			name: "text", format: "text", contentType: "text/plain; charset=utf-8",
			check: func(t *testing.T, body []byte) {
				if string(body) != "你好，这是一次语音合成测试。" {
					t.Errorf("body = %q, want the bare transcript", body)
				}
			},
		},
		{
			name: "verbose_json", format: "verbose_json", contentType: "application/json",
			check: func(t *testing.T, body []byte) {
				var out map[string]any
				if err := json.Unmarshal(body, &out); err != nil {
					t.Fatalf("not JSON: %v", err)
				}
				if out["text"] != "你好，这是一次语音合成测试。" {
					t.Errorf("text = %v", out["text"])
				}
				if out["language"] != "zh" {
					t.Errorf("language = %v, want the requested zh", out["language"])
				}
				if _, ok := out["duration"]; ok {
					t.Error("duration must not be invented: the upstream does not report it")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postMultipart(t, ts, map[string]string{
				"model":           "mimo-v2.5-asr",
				"language":        "zh",
				"response_format": tc.format,
			}, ts.Upstream.WAVBytes())
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Content-Type"); got != tc.contentType {
				t.Errorf("content type = %q, want %q", got, tc.contentType)
			}
			tc.check(t, body)
		})
	}
}

// AUDIO-ASR-002: upload validation.
func TestTranscriptionsValidation(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)

	// No file part at all.
	resp, body := postMultipart(t, ts, map[string]string{"model": "mimo-v2.5-asr"}, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "missing_file") {
		t.Errorf("no-file: status = %d body=%s, want 400 missing_file", resp.StatusCode, body)
	}
	// Empty file.
	resp, body = postMultipart(t, ts, map[string]string{}, []byte{})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "empty_file") {
		t.Errorf("empty: status = %d body=%s, want 400 empty_file", resp.StatusCode, body)
	}
	// Unsupported response_format.
	resp, body = postMultipart(t, ts, map[string]string{"response_format": "srt"}, ts.Upstream.WAVBytes())
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "unsupported_response_format") {
		t.Errorf("format: status = %d body=%s, want 400 unsupported_response_format", resp.StatusCode, body)
	}
	if n := len(ts.Upstream.AudioRequests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0 for invalid uploads", n)
	}
}

// AUDIO-ASR-003: a file over the 25 MB cap is refused before it is read whole.
func TestTranscriptionsTooLarge(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	big := make([]byte, (25<<20)+1024)
	resp, body := postMultipart(t, ts, map[string]string{}, big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%.200s, want 400", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "file_too_large") && !strings.Contains(string(body), "malformed_request_body") {
		t.Errorf("body = %.200s, want a size-related error code", body)
	}
}

// AUDIO-VOICES-001: the catalog comes from the account's bot/config, with the
// schema-loose row filtered out.
func TestVoicesEndpoint(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	resp, body := getPath(t, ts, "/v1/audio/voices")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var out struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Description  string `json:"description"`
			Avatar       string `json:"avatar"`
			PreviewAudio string `json:"preview_audio"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, body)
	}
	if len(out.Data) != 3 {
		t.Fatalf("voices = %d, want 3 usable entries", len(out.Data))
	}
	if out.Data[0].ID != "冰糖" || out.Data[0].Description != "活泼少女" || out.Data[0].PreviewAudio == "" {
		t.Errorf("first voice = %+v, want the 冰糖 entry with description and preview", out.Data[0])
	}
}

// AUDIO-AUTH-001: the audio endpoints sit behind the same Bearer auth as /v1/*.
func TestAudioEndpointsRequireAPIKey(t *testing.T) {
	ts := audioServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"secret-key"}})
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/audio/speech", `{"input":"hi"}`},
		{http.MethodPost, "/v1/audio/transcriptions", "x"},
		{http.MethodGet, "/v1/audio/voices", ""},
	} {
		req, rerr := http.NewRequest(tc.method, ts.URL()+tc.path, strings.NewReader(tc.body))
		if rerr != nil {
			t.Fatal(rerr)
		}
		resp, err := ts.Server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a key = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// postMultipart sends a transcription request with a `file` part (when audio is
// non-nil) plus the given form fields.
func postMultipart(t *testing.T, ts *testserver.TestServer, fields map[string]string, audio []byte) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if audio != nil {
		fw, err := mw.CreateFormFile("file", "audio.wav")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(audio); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL()+"/v1/audio/transcriptions", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// TestWAVBytesLookReal guards the fixture the audio assertions rest on.
func TestWAVBytesLookReal(t *testing.T) {
	up := testutil.NewMockUpstream()
	defer up.Close()
	wav := up.WAVBytes()
	if !bytes.HasPrefix(wav, []byte("RIFF")) || !bytes.Contains(wav[:16], []byte("WAVE")) {
		t.Fatalf("mock wav is not a RIFF/WAVE container: %q", wav[:min(16, len(wav))])
	}
}
