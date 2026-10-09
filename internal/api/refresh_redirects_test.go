package api_test

// The cleaning is dynamic, and these are the cases that say so: nothing here
// hardcodes a model name, and a retirement that lands between two refreshes is
// learned from the upstream's own refusal (ADR 0011, TDD REFRESH-001/002).

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// REFRESH-001: a superseded chat id that our map does not know yet is refused with
// the busy frame; the gateway treats that as information, rebuilds the map from
// the account catalog, and retries with the successor — the caller gets an answer
// instead of a 503.
func TestChatLearnsModelFromBusyRefusal(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	// Deliberately NOT warmed: the map is empty, exactly as it is in the window
	// between a retirement upstream and our next refresh.
	ts.Upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: sseErrorFrame("服务器繁忙，请稍后再试")})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2-flash", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	reqs := ts.Upstream.Requests()
	if len(reqs) < 2 {
		t.Fatalf("upstream saw %d requests, want the refusal plus a retry (%s)", len(reqs), raw)
	}
	if reqs[0].Model != "mimo-v2-flash" {
		t.Errorf("first request model = %q, want the id the caller asked for", reqs[0].Model)
	}
	if last := reqs[len(reqs)-1].Model; last != "mimo-v2.6-flash" {
		t.Errorf("retry model = %q, want the successor learned from the catalog", last)
	}
}

// REFRESH-002: same on the audio surface, where the refusal is explicit
// ("model: [x] not support") rather than the busy frame.
func TestSpeechLearnsModelFromRefusal(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.SetTTSErrorOnce(`{"code":400,"msg":"model: [mimo-v2-tts] not support"}`)

	resp, err := ts.Post("/v1/audio/speech", "",
		`{"model":"mimo-v2-tts","input":"你好","voice":"冰糖","response_format":"wav"}`)
	if err != nil {
		t.Fatalf("speech: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s (the refusal should have been learned from)", resp.StatusCode, raw)
	}

	var models []string
	for _, r := range ts.Upstream.AudioRequests() {
		if !strings.Contains(r.Path, "/tts/v2/generate") {
			continue
		}
		var body struct {
			ModelConfig struct {
				ModelCode string `json:"modelCode"`
			} `json:"modelConfig"`
		}
		if err := json.Unmarshal(r.Body, &body); err == nil && body.ModelConfig.ModelCode != "" {
			models = append(models, body.ModelConfig.ModelCode)
		}
	}
	if len(models) < 2 {
		t.Fatalf("syntheses seen = %v, want the refused attempt and the retry", models)
	}
	if models[0] != "mimo-v2-tts" {
		t.Errorf("first synthesis modelCode = %q, want the id the caller asked for", models[0])
	}
	if models[len(models)-1] != "mimo-v2.5-tts" {
		t.Errorf("retry modelCode = %q, want the successor learned from the catalog", models[len(models)-1])
	}
}
