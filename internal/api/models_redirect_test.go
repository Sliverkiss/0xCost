package api_test

// Model redirects end to end (SDD §5.2, TDD MODELS-014/015, CHAT-REDIR-001,
// AUDIO-REDIR-001).
//
// The upstream supersedes model ids and says so in its own catalog
// (`redirectTo`); it then refuses the superseded id — outright on the audio
// surface, and with the busy frame on chat — so the deployment must not offer it
// and must not send it. Both halves are asserted here against the mock, whose
// catalog mirrors the live shape (18 of 25 live rows are superseded).

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// MODELS-014: the list advertises only what the upstream will run.
func TestModelsListOmitsSupersededIDs(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Get("/v1/models", "")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			RedirectTo string `json:"redirect_to"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range body.Data {
		ids[m.ID] = true
		if m.RedirectTo != "" {
			t.Errorf("%s is advertised with a redirect_to; superseded ids must not be listed", m.ID)
		}
	}
	for _, want := range []string{"mimo-v2.6-flash", "mimo-v2.6-pro", "mimo-v2.5-tts", "mimo-v2.5-asr"} {
		if !ids[want] {
			t.Errorf("%s is missing from the list", want)
		}
	}
	for _, gone := range []string{"mimo-v2-tts", "mimo-v2-flash", "clawm-alpha"} {
		if ids[gone] {
			t.Errorf("%s is superseded upstream and must not be advertised", gone)
		}
	}
	if !strings.Contains(string(raw), "mimo-v2.6-flash") {
		t.Errorf("unexpected payload: %s", raw)
	}
}

// MODELS-015: asking for a superseded id by name names its successor instead of
// 404ing on an id we demonstrably know about.
func TestModelByIDResolvesSupersededID(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Get("/v1/models/mimo-v2-flash", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var m struct {
		ID         string `json:"id"`
		RedirectTo string `json:"redirect_to"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if m.ID != "mimo-v2-flash" {
		t.Errorf("id = %q, want the id that was asked for", m.ID)
	}
	if m.RedirectTo != "mimo-v2.6-flash" {
		t.Errorf("redirect_to = %q, want mimo-v2.6-flash (%s)", m.RedirectTo, raw)
	}
}

// CHAT-REDIR-001: a chat request naming a superseded id reaches the upstream as
// its successor, while the client still sees the id it asked for.
func TestChatResolvesSupersededModel(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	// main.go warms this before serving; the request path itself must never read
	// a catalog (that could turn a chat request into a login).
	ts.API.RefreshModelRedirects(t.Context())
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2-flash", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d chat requests, want 1", len(reqs))
	}
	if reqs[0].Model != "mimo-v2.6-flash" {
		t.Errorf("upstream model = %q, want the successor mimo-v2.6-flash", reqs[0].Model)
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Model != "mimo-v2-flash" {
		t.Errorf("response model = %q, want the requested id echoed back", body.Model)
	}
}

// AUDIO-REDIR-001: the audio surface is where the upstream is strictest — a
// superseded modelCode is refused with a 400, which is what a live TTS call to
// mimo-v2-tts did before this resolution existed.
func TestSpeechResolvesSupersededModel(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.API.RefreshModelRedirects(t.Context())
	resp, err := ts.Post("/v1/audio/speech", "",
		`{"model":"mimo-v2-tts","input":"你好","voice":"冰糖","response_format":"wav"}`)
	if err != nil {
		t.Fatalf("speech: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, raw)
	}

	var sent string
	for _, r := range ts.Upstream.AudioRequests() {
		if !strings.Contains(r.Path, "/tts/v2/generate") {
			continue
		}
		var body struct {
			ModelConfig struct {
				ModelCode string `json:"modelCode"`
			} `json:"modelConfig"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("decode upstream body: %v (%s)", err, r.Body)
		}
		sent = body.ModelConfig.ModelCode
	}
	if sent == "" {
		t.Fatal("no TTS synthesis reached the upstream")
	}
	if sent != "mimo-v2.5-tts" {
		t.Errorf("upstream modelCode = %q, want the successor mimo-v2.5-tts", sent)
	}
}
