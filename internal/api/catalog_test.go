package api_test

// /v1/models against the deployment's own Studio catalog (M19 §3).
//
// The endpoint must report what this deployment can actually serve, with
// per-id capabilities unioned across accounts, and it must never fail because
// an upstream catalog read failed: the static table is the floor.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// modelRow is the response shape under test (extension fields included).
type modelRow struct {
	ID           string `json:"id"`
	Object       string `json:"object"`
	OwnedBy      string `json:"owned_by"`
	Name         string `json:"name"`
	Capabilities struct {
		WebSearch int    `json:"web_search"`
		Thinking  bool   `json:"thinking"`
		Audio     string `json:"audio"`
	} `json:"capabilities"`
	PageType    string   `json:"page_type"`
	Tasks       []string `json:"tasks"`
	ContextSize string   `json:"context_size"`
	Source      string   `json:"source"`
}

func fetchModels(t *testing.T, ts *testserver.TestServer, path string) (int, []modelRow, string) {
	t.Helper()
	resp, err := ts.Get(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, string(body)
	}
	// The list endpoint wraps rows; the single-model endpoint returns one.
	var list struct {
		Object string     `json:"object"`
		Data   []modelRow `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err == nil && list.Object == "list" {
		return resp.StatusCode, list.Data, string(body)
	}
	var one modelRow
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatalf("models body is neither a list nor a model: %s", body)
	}
	return resp.StatusCode, []modelRow{one}, string(body)
}

func rowByID(rows []modelRow, id string) (modelRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return modelRow{}, false
}

// CAT-001: rows carry the capabilities the account reports — search level from
// the chat row's default, thinking from thinkingDefaultOn, audio/page_type/tasks
// from the page type.
func TestCAT001CapabilitiesFromAccountCatalog(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	status, rows, body := fetchModels(t, ts, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	flash, ok := rowByID(rows, "mimo-v2.6-flash")
	if !ok {
		t.Fatalf("mimo-v2.6-flash missing from %+v", rows)
	}
	if flash.Capabilities.WebSearch != 2 || !flash.Capabilities.Thinking {
		t.Errorf("flash capabilities = %+v, want web_search=2 thinking=true (webSearchDefaultStatus=auto, thinkingDefaultOn=true)", flash.Capabilities)
	}
	if flash.PageType != "chat" || len(flash.Tasks) != 1 || flash.Tasks[0] != "chat" {
		t.Errorf("flash page_type/tasks = %q/%v, want chat/[chat]", flash.PageType, flash.Tasks)
	}
	if flash.Capabilities.Audio != "none" {
		t.Errorf("flash audio = %q, want none", flash.Capabilities.Audio)
	}
	if flash.Source != "account" {
		t.Errorf("flash source = %q, want account", flash.Source)
	}

	// A chat model with no chat row: searchable, off by default, thinking off.
	if pro, ok := rowByID(rows, "mimo-v2.6-pro"); !ok {
		t.Error("mimo-v2.6-pro missing")
	} else if pro.Capabilities.WebSearch != 1 {
		t.Errorf("pro web_search = %d, want 1 (chat row with no stated search default)", pro.Capabilities.WebSearch)
	}

	// Audio models: no search surface, audio capability, audio task.
	if tts, ok := rowByID(rows, "mimo-v2.5-tts"); !ok {
		t.Error("mimo-v2.5-tts missing")
	} else {
		if tts.Capabilities.WebSearch != 0 {
			t.Errorf("tts web_search = %d, want 0 (no search surface)", tts.Capabilities.WebSearch)
		}
		if tts.Capabilities.Audio != "tts" || len(tts.Tasks) != 1 || tts.Tasks[0] != "tts" {
			t.Errorf("tts audio/tasks = %q/%v, want tts/[tts]", tts.Capabilities.Audio, tts.Tasks)
		}
	}
	if asr, ok := rowByID(rows, "mimo-v2.5-asr"); !ok {
		t.Error("mimo-v2.5-asr missing")
	} else if asr.Capabilities.Audio != "asr" {
		t.Errorf("asr audio = %q, want asr", asr.Capabilities.Audio)
	}

	// OpenAI shape bits stay intact.
	if flash.Object != "model" || flash.OwnedBy != "xiaomi-mimo" {
		t.Errorf("flash object/owned_by = %q/%q, want model/xiaomi-mimo", flash.Object, flash.OwnedBy)
	}
}

// CAT-002: two accounts reporting different capability defaults for the same id
// union to the maximum, and an id only one account reports still appears.
func TestCAT002UnionAcrossAccounts(t *testing.T) {
	accts := []testserver.TestAccount{
		{ID: "acc1", PH: testutil.TestPH, Cookie: testutil.TestCookie},
		{ID: "acc2", PH: "ph-acc2", Cookie: "cookie-acc2"},
	}
	ts := testserver.NewTestServer(t, accts, nil)
	// acc2 sees the same flash model but with search off by default, and knows
	// one model acc1 has never heard of.
	ts.Upstream.SetCatalogOverride("ph-acc2", []map[string]any{
		{"name": "MiMo-V2.6-Flash", "model": "mimo-v2.6-flash", "pageType": "chat"},
		{"name": "MiMo-V2.1-Omni", "model": "mimo-v2.1-omni", "pageType": "chat"},
	}, []map[string]any{
		{"model": "mimo-v2.6-flash", "webSearchDefaultStatus": "disabled", "thinkingDefaultOn": false},
		{"model": "mimo-v2.1-omni", "webSearchDefaultStatus": "auto", "thinkingDefaultOn": true},
	})

	status, rows, body := fetchModels(t, ts, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, body)
	}
	flash, ok := rowByID(rows, "mimo-v2.6-flash")
	if !ok {
		t.Fatalf("flash missing from %+v", rows)
	}
	// acc1 says auto (2), acc2 says disabled (1) → union keeps 2.
	if flash.Capabilities.WebSearch != 2 {
		t.Errorf("flash web_search = %d, want 2 (maximum across accounts)", flash.Capabilities.WebSearch)
	}
	if !flash.Capabilities.Thinking {
		t.Error("flash thinking = false, want true (true on any account wins)")
	}
	omni, ok := rowByID(rows, "mimo-v2.1-omni")
	if !ok {
		t.Fatalf("mimo-v2.1-omni missing: the union must include ids only one account reports")
	}
	if omni.Capabilities.WebSearch != 2 || !omni.Capabilities.Thinking {
		t.Errorf("omni capabilities = %+v, want web_search=2 thinking=true (from acc2)", omni.Capabilities)
	}
}

// CAT-003: when every account's catalog read fails the endpoint still answers
// 200 from the static floor, marked as such — a catalog read must never take
// the endpoint down.
func TestCAT003StaticFallback(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.SetBotConfigError("目录暂不可用")

	status, rows, body := fetchModels(t, ts, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 from the static floor", status, body)
	}
	if len(rows) == 0 {
		t.Fatal("no rows served")
	}
	for _, r := range rows {
		if r.Source != "static" {
			t.Errorf("row %s source = %q, want static (the account read failed)", r.ID, r.Source)
		}
		if r.Capabilities.WebSearch != 1 {
			t.Errorf("row %s web_search = %d, want the conservative 1 (searchable, off by default)", r.ID, r.Capabilities.WebSearch)
		}
		if r.Capabilities.Audio != "none" || r.PageType != "chat" {
			t.Errorf("row %s audio/page_type = %q/%q, want none/chat", r.ID, r.Capabilities.Audio, r.PageType)
		}
	}
	// The default alias must remain resolvable in this degraded mode.
	if _, ok := rowByID(rows, "mimo-v2.6-flash"); !ok {
		t.Errorf("static floor is missing the default model: %+v", rows)
	}
}

// CAT-004: the catalog is cached per account — repeated requests must not
// hammer the upstream.
func TestCAT004CatalogCachedPerAccount(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	if status, _, body := fetchModels(t, ts, "/v1/models"); status != http.StatusOK {
		t.Fatalf("first call status = %d body=%s", status, body)
	}
	first := ts.Upstream.AudioRequests()
	if len(first) == 0 {
		t.Fatal("no catalog request recorded on the first call")
	}
	if status, _, body := fetchModels(t, ts, "/v1/models"); status != http.StatusOK {
		t.Fatalf("second call status = %d body=%s", status, body)
	}
	if second := ts.Upstream.AudioRequests(); len(second) != len(first) {
		t.Errorf("catalog requests grew from %d to %d, want the TTL cache to absorb the repeat", len(first), len(second))
	}
}

// CAT-005: the single-model endpoint uses the same source, so an id the static
// table does not know still resolves; an unknown id keeps the 404 behaviour and
// lists what is available.
func TestCAT005ModelByIDUsesCatalog(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	ts.Upstream.SetCatalogOverride(testutil.TestPH, []map[string]any{
		{"name": "MiMo-V2.1-Omni", "model": "mimo-v2.1-omni", "pageType": "chat"},
	}, []map[string]any{
		{"model": "mimo-v2.1-omni", "webSearchDefaultStatus": "auto", "thinkingDefaultOn": true},
	})

	status, rows, body := fetchModels(t, ts, "/v1/models/mimo-v2.1-omni")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (id comes from the account catalog)", status, body)
	}
	if rows[0].ID != "mimo-v2.1-omni" || rows[0].Capabilities.WebSearch != 2 {
		t.Errorf("row = %+v, want the account's capabilities", rows[0])
	}

	status, _, body = fetchModels(t, ts, "/v1/models/definitely-not-a-model")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", status, body)
	}
	if !containsEvent(body, "mimo-v2.6-flash") {
		t.Errorf("404 body = %s, want it to list available ids", body)
	}
}
