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

// MODELS-LIVE-001: chat-capable ids from the account catalog are listed, the
// audio-only rows are not, and the static floor is never dropped.
func TestModelsIncludesLiveChatCatalog(t *testing.T) {
	ts := audioServer(t, oneAccount(), nil)
	ids := strings.Join(modelsIDs(t, ts), ",")
	for _, want := range []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed-studio", "clawm-alpha"} {
		if !strings.Contains(ids, want) {
			t.Errorf("catalog %s is missing chat model %q", ids, want)
		}
	}
	for _, unwanted := range []string{"mimo-v2-tts", "mimo-v2.5-asr"} {
		if strings.Contains(ids, unwanted) {
			t.Errorf("catalog %s must not advertise the non-chat row %q", ids, unwanted)
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
	if !strings.Contains(sink.String(), "live catalog unavailable") {
		t.Errorf("debug log = %q, want the fallback recorded", sink.String())
	}
}
