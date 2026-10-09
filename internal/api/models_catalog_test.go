package api_test

// The model catalog endpoint serves what THIS deployment can actually talk to.
// The account's own Studio catalog is the authority (bot/config
// modelConfigListNg), with the static list kept as the floor: a catalog the
// upstream fails to answer must not take /v1/models down, because every
// OpenAI-compatible client calls it on startup.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
)

func modelsIDs(t *testing.T, ts *testserver.TestServer) []string {
	t.Helper()
	resp, body := getPath(t, ts, "/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// MODELS-LIVE-001: the account catalog is listed in full — audio rows included,
// since they are valid models for /v1/audio/* and carry their own capabilities —
// and the static floor is never dropped.
func TestModelsIncludesLiveChatCatalog(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	ids := strings.Join(modelsIDs(t, ts), ",")
	for _, want := range []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed-studio"} {
		if !strings.Contains(ids, want) {
			t.Errorf("catalog %s is missing chat model %q", ids, want)
		}
	}
	// Audio rows belong in the catalog (M19 §3): they are usable through the
	// audio endpoints and their capabilities say so (CAT-001 asserts the
	// capability values; this only checks they are present).
	for _, wantAudio := range []string{"mimo-v2.5-tts", "mimo-v2.5-asr"} {
		if !strings.Contains(ids, wantAudio) {
			t.Errorf("catalog %s is missing the audio row %q", ids, wantAudio)
		}
	}
	if !strings.Contains(ids, "mimo-v2.6-pro") {
		t.Errorf("static floor missing from %s", ids)
	}
}

// MODELS-LIVE-002: when the upstream catalog read fails the endpoint still
// answers 200 with the static list, and says so in the debug trace.
func TestModelsFallsBackWhenCatalogUnavailable(t *testing.T) {
	var sink strings.Builder
	ts := audioServer(t, oneAccount(), &testserver.TestServerOptions{Debug: true, LogSink: &sink})
	ts.Upstream.SetBotConfigError("系统异常，请稍后重试")

	ids := modelsIDs(t, ts)
	if len(ids) == 0 {
		t.Fatal("no models listed when the catalog read failed")
	}
	if !strings.Contains(strings.Join(ids, ","), "mimo-v2.6-pro") {
		t.Errorf("static fallback missing: %v", ids)
	}
	log := sink.String()
	if !strings.Contains(log, "catalog read failed") || !strings.Contains(log, "serving the static floor") {
		t.Errorf("debug log = %q, want the failed read and the fallback recorded", log)
	}
}

// MODELS-LIVE-003: a chat row that is absent from modelConfigListNg is still an
// account model — the id comes from the row and its capability defaults come
// from the same row, not from the static floor. Live shape (2026-10-09): only
// ultraspeed is like this, and the floor's values (no thinking, web search
// merely enabled) contradict the account's own row (thinking on by default,
// web search "auto").
func TestModelsKeepsChatRowMissingFromNgList(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	_, body := getPath(t, ts, "/v1/models")
	var out struct {
		Data []struct {
			ID           string `json:"id"`
			Source       string `json:"source"`
			Capabilities struct {
				WebSearch int  `json:"web_search"`
				Thinking  bool `json:"thinking"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	var found bool
	for _, m := range out.Data {
		if m.ID != "mimo-v2.6-pro-ultraspeed-studio" {
			continue
		}
		found = true
		if m.Source != "account" {
			t.Errorf("ultraspeed source = %q, want account (the account row carries it)", m.Source)
		}
		if !m.Capabilities.Thinking {
			t.Error("ultraspeed thinking = false, want true (row sets thinkingDefaultOn)")
		}
		if m.Capabilities.WebSearch != 2 {
			t.Errorf("ultraspeed web_search = %d, want 2 (row defaults it on)", m.Capabilities.WebSearch)
		}
	}
	if !found {
		t.Fatal("ultraspeed missing from the catalog entirely")
	}
}
