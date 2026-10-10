package mimo

// Client-level tests for the web-surface audio flows (M18). They run against
// the mock upstream, whose responses carry the real shapes captured from the
// Studio — including taskId as a JSON NUMBER, which is what the upID type
// exists for.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/testutil"
)

func audioClient(t *testing.T) (*Client, *testutil.MockUpstream) {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	up.AddAccount(testutil.TestPH, testutil.TestCookie)
	return New(Options{Base: up.URL()}), up
}

func fastPoll() (time.Duration, time.Duration) { return time.Millisecond, 3 * time.Second }

// AUDIO-001: synthesis walks generate → poll → download and returns real bytes.
func TestSynthesizeSuccess(t *testing.T) {
	c, up := audioClient(t)
	every, timeout := fastPoll()
	res, err := c.Synthesize(context.Background(), testutil.TestCookie, testutil.TestPH, "conv-1", TTSRequest{
		Text: "你好，这是一次语音合成测试。", Voice: "冰糖", Format: "wav",
		PollEvery: every, PollTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !bytes.HasPrefix(res.Audio, []byte("RIFF")) {
		t.Fatalf("audio does not start with RIFF: %q", res.Audio[:min(16, len(res.Audio))])
	}
	if res.ContentType != "audio/wav" {
		t.Errorf("content type = %q, want audio/wav", res.ContentType)
	}
	if res.TaskID != "16063666" {
		t.Errorf("task id = %q, want the numeric taskId read back as a string", res.TaskID)
	}
	paths := audioPaths(up)
	want := []string{"/open-apis/tts/v2/generate", "/open-apis/tts/generateStatus", "/open-apis/tts/generateStatus", "/mock-cdn/tts_out.wav"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("upstream path order = %v, want %v", paths, want)
	}
	if res.AudioURL == "" {
		t.Error("audio url not reported")
	}
}

// AUDIO-002: a failed upstream task is a distinct error, not a generic one.
func TestSynthesizeFailedTask(t *testing.T) {
	c, up := audioClient(t)
	up.SetAudioStatusScript("tts", "generating", "failed")
	every, timeout := fastPoll()
	_, err := c.Synthesize(context.Background(), testutil.TestCookie, testutil.TestPH, "conv-1", TTSRequest{
		Text: "hello", PollEvery: every, PollTimeout: timeout,
	})
	if !errors.Is(err, ErrAudioFailed) {
		t.Fatalf("err = %v, want ErrAudioFailed", err)
	}
	if !strings.Contains(err.Error(), "BLOCKED") {
		t.Errorf("err = %q, want the audit status included", err)
	}
}

// AUDIO-003: a task that never leaves "generating" times out with its own error.
func TestSynthesizeTimeout(t *testing.T) {
	c, up := audioClient(t)
	up.SetAudioStatusScript("tts", "generating")
	_, err := c.Synthesize(context.Background(), testutil.TestCookie, testutil.TestPH, "conv-1", TTSRequest{
		Text: "hello", PollEvery: time.Millisecond, PollTimeout: 5 * time.Millisecond,
	})
	if !errors.Is(err, ErrAudioTimeout) {
		t.Fatalf("err = %v, want ErrAudioTimeout", err)
	}
}

// AUDIO-004: guard rails on the request shape.
func TestSynthesizeRejectsEmptyInput(t *testing.T) {
	c, _ := audioClient(t)
	if _, err := c.Synthesize(context.Background(), testutil.TestCookie, testutil.TestPH, "conv-1", TTSRequest{Text: "  "}); err == nil {
		t.Fatal("empty text must be rejected before the upstream call")
	}
	if _, err := c.Synthesize(context.Background(), testutil.TestCookie, testutil.TestPH, "", TTSRequest{Text: "hi"}); err == nil {
		t.Fatal("empty conversationId must be rejected")
	}
}

// AUDIO-005: transcription uploads the bytes (the mock verifies content-md5
// against the body), then recognizes the resource and returns the text.
func TestTranscribeSuccess(t *testing.T) {
	c, up := audioClient(t)
	audio := up.WAVBytes()
	every, timeout := fastPoll()
	text, err := c.Transcribe(context.Background(), testutil.TestCookie, testutil.TestPH, "conv-asr", ASRRequest{
		FileName: "tts_out.wav", Audio: audio, Language: "zh",
		PollEvery: every, PollTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "你好，这是一次语音合成测试。" {
		t.Errorf("text = %q, want the transcript", text)
	}
	if got := up.LastUpload(); !bytes.Equal(got, audio) {
		t.Errorf("uploaded %d bytes, want the %d-byte file", len(got), len(audio))
	}
	paths := audioPaths(up)
	wantFirst := []string{"/open-apis/resource/genUploadInfo", "/mock-upload/audio.wav", "/open-apis/asr/recognize"}
	if len(paths) < 3 || strings.Join(paths[:3], ",") != strings.Join(wantFirst, ",") {
		t.Errorf("upstream path order = %v, want it to start with %v", paths, wantFirst)
	}
	if paths[len(paths)-1] != "/open-apis/asr/recognizeStatus" {
		t.Errorf("last call = %q, want the status poll", paths[len(paths)-1])
	}
	// The ASR upload hits the same signed storage with the same header set as the
	// attachment upload (both go through the SPA's XHR branch, ADR 0035).
	puts := 0
	for _, req := range up.AudioRequests() {
		if strings.HasPrefix(req.Path, "/mock-upload/") {
			puts++
			assertStoragePutHeaders(t, req.Header)
		}
	}
	if puts != 1 {
		t.Errorf("storage PUTs = %d, want 1", puts)
	}
}

// AUDIO-007: conversation reuse — one create per (account, kind), recreate after
// the upstream says the row is gone.
func TestEnsureConversationCachesAndRecreates(t *testing.T) {
	c, up := audioClient(t)
	cache := NewConversationCache(time.Minute)
	ctx := context.Background()
	first, err := c.EnsureConversation(ctx, testutil.TestCookie, testutil.TestPH, cache, "acc1", ConversationKindTTS)
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	second, err := c.EnsureConversation(ctx, testutil.TestCookie, testutil.TestPH, cache, "acc1", ConversationKindTTS)
	if err != nil {
		t.Fatalf("EnsureConversation (cached): %v", err)
	}
	if first != second {
		t.Errorf("conversation changed: %q then %q (cache miss)", first, second)
	}
	if n := countPath(up, "/open-apis/chat/conversation/save"); n != 1 {
		t.Errorf("conversation/save calls = %d, want 1 (cached afterwards)", n)
	}
	// A different kind is a different upstream row.
	other, err := c.EnsureConversation(ctx, testutil.TestCookie, testutil.TestPH, cache, "acc1", ConversationKindASR)
	if err != nil {
		t.Fatalf("EnsureConversation (asr): %v", err)
	}
	if other == first {
		t.Error("TTS and ASR conversations must not be shared")
	}
	// Upstream said it is gone: the next call must create a new one.
	cache.Invalidate("acc1/" + ConversationKindTTS)
	third, err := c.EnsureConversation(ctx, testutil.TestCookie, testutil.TestPH, cache, "acc1", ConversationKindTTS)
	if err != nil {
		t.Fatalf("EnsureConversation (after invalidate): %v", err)
	}
	if third == first {
		t.Error("invalidated conversation was reused")
	}
}

// AUDIO-008: a business error from conversation/save surfaces with its message
// (this is the "conversation not exist" path the API layer retries once).
func TestCreateConversationBizError(t *testing.T) {
	c, up := audioClient(t)
	up.SetConversationSaveErrors("conversation not exist")
	_, err := c.EnsureConversation(context.Background(), testutil.TestCookie, testutil.TestPH, NewConversationCache(time.Minute), "acc1", ConversationKindTTS)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsConversationMissing(err) {
		t.Errorf("err = %v, want IsConversationMissing", err)
	}
	var be *BizError
	if !errors.As(err, &be) || be.Code != 400 {
		t.Errorf("err = %v, want a BizError with the upstream code", err)
	}
}

// AUDIO-009: the catalog parse uses the real field names (modelConfigListNg for
// the 24-entry list, ttsVoices, ext) and drops schema-loose voice rows.
func TestFetchBotConfigParsesRealShape(t *testing.T) {
	c, _ := audioClient(t)
	cfg, err := c.FetchBotConfig(context.Background(), testutil.TestCookie, testutil.TestPH)
	if err != nil {
		t.Fatalf("FetchBotConfig: %v", err)
	}
	// The fixture mirrors the live payload, superseded rows included, so the count
	// is the fixture's own and the redirect field must survive the parse — a
	// fixture without `redirectTo` describes a service that does not exist and
	// hides the resolution the request paths depend on (model redirects).
	if len(cfg.Models) != 7 {
		t.Errorf("models = %d, want the modelConfigListNg entries", len(cfg.Models))
	}
	redirects := map[string]string{}
	for _, m := range cfg.Models {
		if m.RedirectTo != "" {
			redirects[m.Model] = m.RedirectTo
		}
	}
	if redirects["mimo-v2-flash"] != "mimo-v2.6-flash" {
		t.Errorf("the parse lost the superseded ids: %v", redirects)
	}
	// ultraspeed is a chat row without an Ng row in the live payload, so the two
	// lists are parsed separately and the row keeps its own capability defaults.
	var ultra *ChatModel
	for i, r := range cfg.ChatModels {
		if r.Model == "mimo-v2.6-pro-ultraspeed-studio" {
			ultra = &cfg.ChatModels[i]
		}
	}
	if ultra == nil {
		t.Fatal("ultraspeed chat row missing from the parsed config")
	}
	if ultra.ThinkingDefaultOn == nil || !*ultra.ThinkingDefaultOn {
		t.Errorf("ultraspeed thinkingDefaultOn = %v, want true", ultra.ThinkingDefaultOn)
	}
	if ultra.WebSearchDefaultStatus == nil || *ultra.WebSearchDefaultStatus != "auto" {
		t.Errorf("ultraspeed webSearchDefaultStatus = %v, want auto", ultra.WebSearchDefaultStatus)
	}
	if len(cfg.Voices) != 3 {
		t.Errorf("voices = %d, want 3 (the id-less row is filtered)", len(cfg.Voices))
	}
	if cfg.Voices[0].ID != "冰糖" || cfg.Voices[0].PreviewAudio == "" {
		t.Errorf("first voice = %+v, want 冰糖 with its preview", cfg.Voices[0])
	}
	if !cfg.Ext.EnableUltraspeed || cfg.Ext.EnableVoice {
		t.Errorf("ext = %+v, want enableUltraspeed=true and enableVoice=false (the live account state)", cfg.Ext)
	}
}

// AUDIO-010: the upstream's ids arrive as numbers on some endpoints and strings
// on others; upID must accept both without an unmarshal error.
func TestUpIDAcceptsNumberAndString(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: `{"taskId":16063666}`, want: "16063666"},
		{in: `{"taskId":"16063666"}`, want: "16063666"},
		{in: `{"taskId":null}`, want: ""},
		{in: `{}`, want: ""},
	} {
		var out struct {
			TaskID upID `json:"taskId"`
		}
		if err := json.Unmarshal([]byte(tc.in), &out); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if out.TaskID.String() != tc.want {
			t.Errorf("%s: got %q, want %q", tc.in, out.TaskID, tc.want)
		}
	}
}

// AUDIO-011: AudioContentType covers the two formats the upstream produces.
func TestAudioContentType(t *testing.T) {
	if got := AudioContentType("wav"); got != "audio/wav" {
		t.Errorf("wav -> %q", got)
	}
	if got := AudioContentType("MP3"); got != "audio/mpeg" {
		t.Errorf("mp3 -> %q", got)
	}
	if got := AudioContentType("opus"); got != "application/octet-stream" {
		t.Errorf("unknown -> %q", got)
	}
}

// audioPaths lists the recorded audio/catalog call paths in order.
func audioPaths(up *testutil.MockUpstream) []string {
	var out []string
	for _, r := range up.AudioRequests() {
		out = append(out, r.Path)
	}
	return out
}

func countPath(up *testutil.MockUpstream, path string) int {
	n := 0
	for _, p := range audioPaths(up) {
		if p == path {
			n++
		}
	}
	return n
}
