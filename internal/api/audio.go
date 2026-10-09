package api

// Web-surface audio endpoints: OpenAI-compatible speech synthesis and
// transcription on top of the Studio's web API (M18, docs/web-surface-reverse.md
// §3/§4).
//
// Both flows are three-legged upstream (create/find a conversation entity →
// generate → poll → download), so unlike chat they hold an account lease for the
// whole operation and share the chat pipeline's error classification. The
// upstream's own `enableVoice` flag is a Studio UI switch, not an access check:
// it is false on our test account while TTS works, so it does not gate these
// endpoints.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yg/mimo2api/internal/mimo"
)

const (
	// maxAudioUploadBytes caps a transcription upload (OpenAI's own limit for
	// audio input is 25 MB; we enforce it locally instead of streaming a
	// multi-hundred-MB body into memory first).
	maxAudioUploadBytes = 25 << 20
	// maxSpeechRequestBytes bounds the JSON body of /v1/audio/speech. Scripts
	// are capped by SpeechMaxChars anyway; this stops a huge body earlier.
	maxSpeechRequestBytes = 1 << 20
	// botConfigTTL is how long the account catalog (models, voices, switches)
	// is reused. It changes on the operator's schedule, not per request.
	botConfigTTL = 10 * time.Minute
)

// openAIVoiceAliases are the voice names OpenAI clients hardcode. The Studio
// has its own eight-voice catalog with no equivalent set, so an alias maps to
// the account's default voice rather than failing: a client that always sends
// "alloy" still gets audio (documented in README).
var openAIVoiceAliases = map[string]bool{
	"alloy": true, "ash": true, "ballad": true, "coral": true, "echo": true,
	"fable": true, "nova": true, "onyx": true, "sage": true, "shimmer": true,
	"verse": true,
}

// unknownVoiceError is a request-level failure: the client named a voice the
// account's catalog does not have.
type unknownVoiceError struct {
	requested string
	available []string
}

func (e *unknownVoiceError) Error() string {
	return fmt.Sprintf("unknown voice %q; available: %s", e.requested, strings.Join(e.available, ", "))
}

// botConfigCache holds the last catalog fetched from an account.
type botConfigCache struct {
	mu  sync.Mutex
	cfg *mimo.BotConfig
	at  time.Time
}

// botConfig returns the account catalog, fetching at most once per TTL.
func (s *Server) botConfig(ctx context.Context, cookie, ph string) (*mimo.BotConfig, error) {
	s.botCfg.mu.Lock()
	if s.botCfg.cfg != nil && time.Since(s.botCfg.at) < botConfigTTL {
		cfg := s.botCfg.cfg
		s.botCfg.mu.Unlock()
		return cfg, nil
	}
	s.botCfg.mu.Unlock()

	cfg, err := s.client.FetchBotConfig(ctx, cookie, ph)
	if err != nil {
		return nil, err
	}
	s.botCfg.mu.Lock()
	s.botCfg.cfg, s.botCfg.at = cfg, time.Now()
	s.botCfg.mu.Unlock()
	return cfg, nil
}

// withAccount runs fn with an account lease and resolved credentials, applying
// the same account policy as the chat pipeline: an expired session is refreshed
// and the call retried once, a dead account is reported to the pool, and a 429
// feeds the cooldown ladder (SDD §7.1). Non-account failures (bad voice, bad
// input) are returned untouched for the caller to map.
func (s *Server) withAccount(ctx context.Context, fn func(cookie, ph, accountID string) error) error {
	lease, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()

	cookie, ph, err := s.creds(ctx, lease.ID)
	if err != nil {
		return err
	}
	err = fn(cookie, ph, lease.ID)
	if err == nil {
		s.pool.ReportSuccess(lease.ID)
		return nil
	}

	switch {
	case isAuthErr(err):
		// One transparent renewal, then give up (SDD §2.3).
		if rerr := s.refreshSession(ctx, lease.ID); rerr != nil {
			s.logger.Printf("accounts: renewal for %s failed (%v) during audio call", lease.ID, rerr)
			s.pool.ReportDisabled(lease.ID, rerr.Error(), false)
			return err
		}
		cookie, ph, cerr := s.creds(ctx, lease.ID)
		if cerr != nil {
			return cerr
		}
		if err = fn(cookie, ph, lease.ID); err == nil {
			s.pool.ReportSuccess(lease.ID)
			return nil
		}
	case isAccountDead(err):
		s.pool.ReportDisabled(lease.ID, err.Error(), isPermanentDead(err))
	case isUpstream429(err):
		s.pool.Report429(lease.ID)
	}
	return err
}

// handleSpeech implements POST /v1/audio/speech.
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSpeechRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed_request_body",
			"malformed request body", 0)
		return
	}
	var req struct {
		Model          string   `json:"model"`
		Input          string   `json:"input"`
		Voice          string   `json:"voice"`
		ResponseFormat string   `json:"response_format"`
		Speed          *float64 `json:"speed"`
		Instructions   string   `json:"instructions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed_request_body",
			"malformed request body", 0)
		return
	}
	input := strings.TrimSpace(req.Input)
	if input == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_input",
			"`input` is required", 0)
		return
	}
	if n := utf8.RuneCountInString(input); n > s.speechMaxChars {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input_too_long",
			fmt.Sprintf("`input` is %d characters; the limit is %d", n, s.speechMaxChars), 0)
		return
	}
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "wav"
	}
	if format != "wav" && format != "mp3" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_response_format",
			`the upstream supports "wav" and "mp3" only`, 0)
		return
	}
	if req.Speed != nil {
		// Accepted for OpenAI compatibility; the upstream has no speed control.
		s.debugf("audio: ignoring speed=%v (upstream has no rate control)", *req.Speed)
	}

	var result *mimo.TTSResult
	err = s.withAccount(r.Context(), func(cookie, ph, accountID string) error {
		cfg, cerr := s.botConfig(r.Context(), cookie, ph)
		if cerr != nil {
			return cerr
		}
		voice, verr := s.resolveVoice(cfg, req.Voice)
		if verr != nil {
			return verr
		}
		res, terr := s.synthesize(r.Context(), cookie, ph, accountID, mimo.TTSRequest{
			Text:        input,
			Instruction: req.Instructions,
			Voice:       voice,
			Format:      format,
			Model:       req.Model,
			PollEvery:   s.audioPoll,
			PollTimeout: s.ttsTimeout,
		})
		if terr != nil {
			return terr
		}
		result = res
		return nil
	})
	if err != nil {
		s.MapError(w, err)
		return
	}
	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(result.Audio)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Audio)
}

// synthesize runs EnsureConversation → Synthesize, recreating the conversation
// once when the upstream says it is gone (a cached id can outlive the row, and
// the upstream answers that case with a business error rather than 404).
func (s *Server) synthesize(ctx context.Context, cookie, ph, accountID string, req mimo.TTSRequest) (*mimo.TTSResult, error) {
	const kind = mimo.ConversationKindTTS
	conv, err := s.client.EnsureConversation(ctx, cookie, ph, s.audioConvs, accountID, kind)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Synthesize(ctx, cookie, ph, conv, req)
	if err == nil || !mimo.IsConversationMissing(err) {
		return res, err
	}
	s.audioConvs.Invalidate(accountID + "/" + kind)
	conv, err = s.client.EnsureConversation(ctx, cookie, ph, s.audioConvs, accountID, kind)
	if err != nil {
		return nil, err
	}
	return s.client.Synthesize(ctx, cookie, ph, conv, req)
}

// handleTranscriptions implements POST /v1/audio/transcriptions (multipart).
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed_request_body",
			"expected a multipart/form-data body with a `file` part", 0)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_file",
			"`file` is required", 0)
		return
	}
	defer file.Close()
	audio, err := io.ReadAll(io.LimitReader(file, maxAudioUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unreadable_file",
			"could not read the uploaded file", 0)
		return
	}
	if len(audio) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "empty_file",
			"the uploaded file is empty", 0)
		return
	}
	if len(audio) > maxAudioUploadBytes {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "file_too_large",
			fmt.Sprintf("the uploaded file exceeds %d bytes", maxAudioUploadBytes), 0)
		return
	}
	responseFormat := strings.ToLower(strings.TrimSpace(r.FormValue("response_format")))
	switch responseFormat {
	case "", "json", "text", "verbose_json":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_response_format",
			`supported values are "json", "text" and "verbose_json"`, 0)
		return
	}

	fileName := "audio.wav"
	if header != nil && strings.TrimSpace(header.Filename) != "" {
		fileName = header.Filename
	}
	language := strings.TrimSpace(r.FormValue("language"))
	model := strings.TrimSpace(r.FormValue("model"))

	var text string
	err = s.withAccount(r.Context(), func(cookie, ph, accountID string) error {
		const kind = mimo.ConversationKindASR
		conv, cerr := s.client.EnsureConversation(r.Context(), cookie, ph, s.audioConvs, accountID, kind)
		if cerr != nil {
			return cerr
		}
		req := mimo.ASRRequest{
			FileName:    fileName,
			Audio:       audio,
			Language:    language,
			Model:       model,
			PollEvery:   s.audioPoll,
			PollTimeout: s.asrTimeout,
		}
		out, terr := s.client.Transcribe(r.Context(), cookie, ph, conv, req)
		if terr != nil && mimo.IsConversationMissing(terr) {
			s.audioConvs.Invalidate(accountID + "/" + kind)
			conv, cerr = s.client.EnsureConversation(r.Context(), cookie, ph, s.audioConvs, accountID, kind)
			if cerr != nil {
				return cerr
			}
			out, terr = s.client.Transcribe(r.Context(), cookie, ph, conv, req)
		}
		if terr != nil {
			return terr
		}
		text = out
		return nil
	})
	if err != nil {
		s.MapError(w, err)
		return
	}

	switch responseFormat {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, text)
	case "verbose_json":
		// Only what the upstream actually tells us: no invented duration or
		// segment timings.
		payload := map[string]any{"text": text}
		if language != "" && language != "auto" {
			payload["language"] = language
		}
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, payload)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = encodeJSON(w, map[string]string{"text": text})
	}
}

// handleVoices implements GET /v1/audio/voices.
func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	type voiceDTO struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Description  string `json:"description,omitempty"`
		Avatar       string `json:"avatar,omitempty"`
		PreviewAudio string `json:"preview_audio,omitempty"`
	}
	var out []voiceDTO
	err := s.withAccount(r.Context(), func(cookie, ph, _ string) error {
		cfg, cerr := s.botConfig(r.Context(), cookie, ph)
		if cerr != nil {
			return cerr
		}
		out = make([]voiceDTO, 0, len(cfg.Voices))
		for _, v := range cfg.Voices {
			out = append(out, voiceDTO{
				ID: v.ID, Name: v.Name, Description: v.Description,
				Avatar: v.Avatar, PreviewAudio: v.PreviewAudio,
			})
		}
		return nil
	})
	if err != nil {
		s.MapError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = encodeJSON(w, map[string]any{"data": out})
}

// resolveVoice turns a client-supplied voice into an upstream voice id: the
// configured default, an OpenAI alias mapped to that default, or an exact
// Studio id/name. Anything else is a request error naming the alternatives
// instead of silently synthesizing in a voice the caller did not ask for.
func (s *Server) resolveVoice(cfg *mimo.BotConfig, requested string) (string, error) {
	def := s.defaultVoice
	if def == "" && cfg != nil && len(cfg.Voices) > 0 {
		def = cfg.Voices[0].ID
		if def == "" {
			def = cfg.Voices[0].Name
		}
	}
	req := strings.TrimSpace(requested)
	if req == "" || openAIVoiceAliases[strings.ToLower(req)] {
		if def == "" {
			return "", errors.New("no voice available: the account catalog is empty and MIMO2API_TTS_VOICE is unset")
		}
		return def, nil
	}
	if cfg != nil {
		for _, v := range cfg.Voices {
			if strings.EqualFold(v.ID, req) || strings.EqualFold(v.Name, req) {
				return v.ID, nil
			}
		}
	}
	var available []string
	if cfg != nil {
		for _, v := range cfg.Voices {
			name := v.ID
			if name == "" {
				name = v.Name
			}
			if name != "" {
				available = append(available, name)
			}
		}
	}
	sort.Strings(available)
	return "", &unknownVoiceError{requested: req, available: available}
}
