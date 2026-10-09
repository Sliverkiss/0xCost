package testutil

// Web-surface audio + catalog routes for the mock upstream.
//
// Every response below is the shape captured from the real Studio
// (docs/web-surface-reverse.md §3/§4/§7), envelope included:
//   - taskId is a JSON NUMBER while msgId is a STRING (the real asymmetry that
//     breaks a naive string field — see internal/mimo/audio.go upID)
//   - bot/config carries BOTH modelConfigList (15 chat entries) and
//     modelConfigListNg (24 all-model entries); the gateway reads the latter
//   - ttsVoices entries are {id,name,description,avatar,previewAudio}
//   - audit objects are nested (inputAudit/outputAudit), not flat
// A mock that only fills the fields a test reads hides exactly these traps.

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AudioRequest is one recorded audio/catalog call.
type AudioRequest struct {
	Method string
	Path   string
	PH     string
	Body   []byte
	Header http.Header
	At     time.Time
}

// audioPath reports whether a path belongs to the audio/catalog surface.
func audioPath(p string) bool {
	for _, prefix := range []string{"/open-apis/tts/", "/open-apis/asr/", "/open-apis/resource/", "/mock-upload/", "/mock-cdn/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return p == "/open-apis/chat/conversation/save" || p == "/open-apis/bot/config"
}

// SetAudioStatusScript overrides the status sequence a TTS/ASR task walks
// through. The last element repeats, so {"success"} completes on the first poll
// and {"generating","success"} exercises the polling loop.
func (m *MockUpstream) SetAudioStatusScript(kind string, statuses ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if kind == "tts" {
		m.ttsStatuses = statuses
		return
	}
	m.asrStatuses = statuses
}

// SetBotConfigError makes bot/config fail with a business error (empty string
// clears it), which is how the model-catalog fallback path is exercised.
func (m *MockUpstream) SetBotConfigError(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.botConfigErr = msg
}

// SetCatalogOverride replaces one account's bot/config model lists, keyed by
// ph (each account has its own catalog). Used to prove the /v1/models union
// takes capabilities at their maximum across accounts.
func (m *MockUpstream) SetCatalogOverride(ph string, ng, chat []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.catalogOverrides == nil {
		m.catalogOverrides = map[string]catalogOverride{}
	}
	m.catalogOverrides[ph] = catalogOverride{ng: ng, chat: chat}
}

// catalogOverride is one account's scripted model lists.
type catalogOverride struct {
	ng   []map[string]any
	chat []map[string]any
}

// SetAudioText overrides the transcript the ASR status endpoint returns.
func (m *MockUpstream) SetAudioText(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asrText = text
}

// SetConversationSaveErrors makes the next n calls to conversation/save fail
// with a business error (used to prove the recreate-once path).
func (m *MockUpstream) SetConversationSaveErrors(msgs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveErrors = msgs
}

// AudioRequests returns every recorded audio/catalog call in order.
func (m *MockUpstream) AudioRequests() []AudioRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AudioRequest, len(m.audioReqs))
	copy(out, m.audioReqs)
	return out
}

// LastUpload returns the bytes of the most recent PUT upload (nil if none).
func (m *MockUpstream) LastUpload() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uploaded
}

// WAVBytes is the payload the mock serves as synthesized audio: a real RIFF
// header so a client can assert on the container, not just the length.
func (m *MockUpstream) WAVBytes() []byte { return m.wav }

func (m *MockUpstream) recordAudio(r *http.Request, body []byte) {
	m.audioReqs = append(m.audioReqs, AudioRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		PH:     r.URL.Query().Get("xiaomichatbot_ph"),
		Body:   body,
		Header: r.Header.Clone(),
		At:     time.Now(),
	})
}

// nextAudioStatus pops the scripted status sequence for kind.
func (m *MockUpstream) nextAudioStatus(kind string) string {
	seq := m.ttsStatuses
	if kind == "asr" {
		seq = m.asrStatuses
	}
	if len(seq) == 0 {
		return "success"
	}
	st := seq[0]
	if len(seq) > 1 {
		if kind == "asr" {
			m.asrStatuses = seq[1:]
		} else {
			m.ttsStatuses = seq[1:]
		}
	}
	return st
}

func writeJSONEnvelope(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	raw, _ := json.Marshal(data)
	fmt.Fprintf(w, `{"code":%d,"msg":%q,"data":%s}`, code, msg, raw)
}

// serveAudio answers the audio/catalog surface.
func (m *MockUpstream) serveAudio(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	r.Body.Close()

	m.mu.Lock()
	m.recordAudio(r, body)
	asrText := m.asrText
	m.mu.Unlock()

	switch {
	case r.URL.Path == "/open-apis/chat/conversation/save":
		m.mu.Lock()
		var fail string
		if len(m.saveErrors) > 0 {
			fail = m.saveErrors[0]
			m.saveErrors = m.saveErrors[1:]
		}
		m.mu.Unlock()
		if fail != "" {
			writeJSONEnvelope(w, 400, fail, nil)
			return
		}
		var req struct {
			ConversationID string `json:"conversationId"`
			Title          string `json:"title"`
			Type           string `json:"type"`
		}
		_ = json.Unmarshal(body, &req)
		writeJSONEnvelope(w, 0, "成功", map[string]any{
			"id":             9655605,
			"creator":        "2652042633",
			"createTime":     "2026-10-09 12:48:56",
			"updater":        "2652042633",
			"updateTime":     "2026-10-09 12:48:56",
			"title":          req.Title,
			"conversationId": req.ConversationID,
			"deleteFlag":     0,
			"type":           req.Type,
		})

	case r.URL.Path == "/open-apis/bot/config":
		m.mu.Lock()
		cfgErr := m.botConfigErr
		ov, hasOv := m.catalogOverrides[r.URL.Query().Get("xiaomichatbot_ph")]
		m.mu.Unlock()
		if cfgErr != "" {
			writeJSONEnvelope(w, 400, cfgErr, nil)
			return
		}
		cfg := mockBotConfig()
		if hasOv {
			cfg["modelConfigListNg"] = ov.ng
			cfg["modelConfigList"] = ov.chat
		}
		writeJSONEnvelope(w, 0, "成功", cfg)

	case r.URL.Path == "/open-apis/tts/v2/generate":
		var req struct {
			ConversationID string `json:"conversationId"`
			MsgID          string `json:"msgId"`
		}
		_ = json.Unmarshal(body, &req)
		writeJSONEnvelope(w, 0, "成功", map[string]any{
			"taskId":         16063666, // NUMBER, exactly as upstream sends it
			"conversationId": req.ConversationID,
			"msgId":          req.MsgID,
			"status":         "generating",
		})

	case r.URL.Path == "/open-apis/tts/generateStatus":
		status := m.statusLocked("tts")
		data := map[string]any{"taskId": 16063666, "status": status}
		switch status {
		case "success":
			data["audioUrl"] = m.URL() + "/mock-cdn/tts_out.wav"
			data["audit"] = map[string]any{"status": "PASS", "blocked": false}
		case "failed":
			data["audit"] = map[string]any{"status": "BLOCKED", "blocked": true}
		default:
			data["audit"] = map[string]any{"status": "PROCESSING", "blocked": false}
		}
		writeJSONEnvelope(w, 0, "成功", data)

	case r.URL.Path == "/open-apis/resource/genUploadInfo":
		writeJSONEnvelope(w, 0, "成功", map[string]any{
			"resourceId":  "439c596b8e9b0c109ae0251f2b86006e",
			"uploadUrl":   m.URL() + "/mock-upload/audio.wav",
			"resourceUrl": m.URL() + "/mock-cdn/audio.wav",
			"objectName":  "multimedia/2652042633/audio.wav",
		})

	case strings.HasPrefix(r.URL.Path, "/mock-upload/"):
		// The real upload URL is signed object storage; it verifies content-md5.
		sum := md5.Sum(body)
		if got, want := r.Header.Get("content-md5"), hex.EncodeToString(sum[:]); got != "" && got != want {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.uploaded = body
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case strings.HasPrefix(r.URL.Path, "/mock-cdn/"):
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(m.WAVBytes())

	case r.URL.Path == "/open-apis/asr/recognize":
		var req struct {
			ConversationID string `json:"conversationId"`
			AudioURL       string `json:"audioUrl"`
		}
		_ = json.Unmarshal(body, &req)
		writeJSONEnvelope(w, 0, "成功", map[string]any{
			"taskId":         16063666,
			"conversationId": req.ConversationID,
			"msgId":          "1791521293085",
			"status":         "generating",
		})

	case r.URL.Path == "/open-apis/asr/recognizeStatus":
		status := m.statusLocked("asr")
		data := map[string]any{
			"taskId":         16063666,
			"conversationId": "d1890025eee98cb9a5092c1a21cd8ab2",
			"msgId":          "1791521293085",
			"status":         status,
			"audit": map[string]any{
				"inputAudit":  map[string]any{"status": "PROCESSING"},
				"outputAudit": map[string]any{"status": "FINISHED", "resultCode": true},
			},
		}
		if status == "success" {
			data["text"] = asrText
		}
		writeJSONEnvelope(w, 0, "成功", data)

	default:
		http.NotFound(w, r)
	}
}

// statusLocked pops the next scripted status for kind.
func (m *MockUpstream) statusLocked(kind string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextAudioStatus(kind)
}

// mockBotConfig mirrors the live bot/config payload closely enough that the
// parser is exercised on real field names: both model lists, the eight voices
// and the account switches.
func mockBotConfig() map[string]any {
	return map[string]any{
		"modelConfigList": []map[string]any{
			{
				"name": "MiMo-V2.6-Flash", "model": "mimo-v2.6-flash",
				"temperature": 0.8, "topP": 0.95, "thinkingDefaultOn": true,
				"webSearchDefaultStatus": "auto", "cnIntro": "高性价比日常全模态模型",
			},
			{"name": "MiMo-V2.6-Pro", "model": "mimo-v2.6-pro", "temperature": 0.8, "topP": 0.95},
		},
		"modelConfigListNg": []map[string]any{
			{"name": "MiMo-V2.6-Flash", "model": "mimo-v2.6-flash", "pageType": "chat"},
			{"name": "MiMo-V2.6-Pro", "model": "mimo-v2.6-pro", "pageType": "chat"},
			{"name": "MiMo-V2.6-Pro UltraSpeed", "model": "mimo-v2.6-pro-ultraspeed-studio", "pageType": "chat"},
			{"name": "MiMo-TTS", "model": "mimo-v2-tts", "pageType": "tts"},
			{"name": "MiMo-ASR", "model": "mimo-v2.5-asr", "pageType": "asr"},
			{"name": "Claw-M", "model": "clawm-alpha", "pageType": "chat", "redirectTo": ""},
		},
		"ttsVoices": []map[string]any{
			{"id": "冰糖", "name": "冰糖", "description": "活泼少女",
				"avatar":       "https://aistudio-cdn.xiaomimimo.com/xiaomimimo-static/tts/avatar-ng/bingtang.webp",
				"previewAudio": "https://aistudio-cdn.xiaomimimo.com/xiaomimimo-static/tts/audio/bingtang.wav"},
			{"id": "Mia", "name": "Mia", "description": "Lively girl",
				"avatar": "https://aistudio-cdn.xiaomimimo.com/xiaomimimo-static/tts/avatar-ng/mia.webp"},
			{"id": "茉莉", "name": "茉莉", "description": "知性女声",
				"avatar": "https://aistudio-cdn.xiaomimimo.com/xiaomimimo-static/tts/avatar-ng/moli.webp"},
			// Schema-loose entry the Studio itself filters out.
			{"description": "no id, no name"},
		},
		"ext": map[string]any{
			"enableCustomVoice": true, "enableClaw": false, "isForeignVersion": true,
			"enableVoice": false, "enableV26": true, "enableUltraspeed": true,
		},
	}
}

// testWAV builds a minimal but valid RIFF/WAVE file (44-byte header + 200
// bytes of silence) so download-path assertions can check the container.
func testWAV() []byte {
	const sampleRate, channels, bits = 8000, 1, 8
	dataLen := 200
	buf := make([]byte, 0, 44+dataLen)
	u32 := func(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	u16 := func(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
	buf = append(buf, "RIFF"...)
	buf = append(buf, u32(uint32(36+dataLen))...)
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = append(buf, u32(16)...)
	buf = append(buf, u16(1)...) // PCM
	buf = append(buf, u16(channels)...)
	buf = append(buf, u32(sampleRate)...)
	buf = append(buf, u32(uint32(sampleRate*channels*bits/8))...)
	buf = append(buf, u16(uint16(channels*bits/8))...)
	buf = append(buf, u16(bits)...)
	buf = append(buf, "data"...)
	buf = append(buf, u32(uint32(dataLen))...)
	return append(buf, make([]byte, dataLen)...)
}
