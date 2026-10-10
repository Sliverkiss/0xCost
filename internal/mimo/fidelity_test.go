package mimo

// Fidelity golden tests (docs/login-chain-reverse.md §3 + §4): the outbound
// request must look like the Studio SPA's own — same header set, same payload
// key set. A dropped header or key is a fingerprint tell even when the
// upstream happens to tolerate it, so these assertions are deliberately
// strict (exact values / exact key sets) rather than "contains".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// requiredBrowserHeaders maps each header the Studio SPA's fetch-based client
// plus the browser send on an `/open-apis/*` call to its exact expected value.
// Verified against the whole bundle (main.js + 452 chunks, 17.8 MB) in
// docs/web-surface-reverse.md §11.7.1: the SPA's client contributes
// Content-Type (POST with a JSON body), Accept-Language and x-timeZone; the
// browser contributes Accept: */*, Origin, Referer and the sec-* set.
func requiredBrowserHeaders(base string) map[string]string {
	return map[string]string{
		"Content-Type":       "application/json",
		"Accept":             "*/*",
		"Accept-Language":    "zh-CN",
		"Origin":             base,
		"Referer":            base + "/",
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
	}
}

// nonEmptyBrowserHeaders are present but host/UA-dependent, so only presence
// is asserted (they come from config: MIMO2API_USER_AGENT / SEC_CH_UA).
var nonEmptyBrowserHeaders = []string{"User-Agent", "sec-ch-ua", "x-timeZone"}

// forbiddenBrowserHeaders are headers this surface must NOT send: the SPA never
// sends them (bundle-verified), and an extra header is a fingerprint tell in
// itself — exactly the mistake the old axios assumption produced.
// X-Requested-With *is* mandatory on the identity surface (passport.go, 66108
// without it), which is why the two surfaces have separate header sets.
var forbiddenBrowserHeaders = []string{"X-Requested-With", "Authorization"}

// FIDELITY-001: every header the Studio SPA sends must be on our chat call.
func TestChatSendsFullBrowserHeaderSet(t *testing.T) {
	client, up := chatEnv(t)
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-2.6-pro",
	}); err != nil {
		t.Fatal(err)
	}
	reqs := up.RequestsFor(testPH)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	h := reqs[0].Headers
	for name, want := range requiredBrowserHeaders(up.URL()) {
		if got := h.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	for _, name := range nonEmptyBrowserHeaders {
		if h.Get(name) == "" {
			t.Errorf("header %s is missing", name)
		}
	}
	for _, name := range forbiddenBrowserHeaders {
		if got := h.Get(name); got != "" {
			t.Errorf("chat call sends %s = %q, which the Studio SPA never sends on this surface: an extra header is a tell", name, got)
		}
	}
	if h.Get("Cookie") == "" {
		t.Error("Cookie header missing on the chat call")
	}
}

// FIDELITY-002: the same set must ride on the session-validation probe (SDD
// §2.4) — an auth check that looks like a bare script is the easiest tell.
// MockUpstream does not record the validate probe, so this uses a raw
// recorder server and asserts against the captured request.
func TestValidateSendsFullBrowserHeaderSet(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"userId":123456}}`))
	}))
	defer srv.Close()

	client := New(Options{Base: srv.URL, Timezone: "Asia/Shanghai"})
	if err := client.Validate(context.Background(), testCookie); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("validate probe never reached the server")
	}
	for name, want := range requiredBrowserHeaders(srv.URL) {
		if v := got.Get(name); v != want {
			t.Errorf("validate header %s = %q, want %q", name, v, want)
		}
	}
	for _, name := range nonEmptyBrowserHeaders {
		if got.Get(name) == "" {
			t.Errorf("validate header %s is missing", name)
		}
	}
	for _, name := range forbiddenBrowserHeaders {
		if v := got.Get(name); v != "" {
			t.Errorf("validate probe sends %s = %q, which the Studio SPA never sends on this surface", name, v)
		}
	}
	if got.Get("Cookie") != testCookie {
		t.Errorf("validate cookie = %q, want the session cookie", got.Get("Cookie"))
	}
}

// FIDELITY-003: the chat payload must carry exactly the keys the SPA sends —
// no more, no fewer (docs/login-chain-reverse.md §3 quoted payload shape).
func TestChatPayloadKeySetIsExact(t *testing.T) {
	client, up := chatEnv(t)
	prev := "dlg-prev-0000000000000001"
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query:            "hi",
		Model:            "mimo-2.6-pro",
		ConversationID:   "conv-0000000000000000000000000001",
		PreviousDialogID: prev,
		EnableThinking:   true,
	}); err != nil {
		t.Fatal(err)
	}
	raw := up.RequestsFor(testPH)[0].RawBody
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload is not an object: %v (%s)", err, raw)
	}
	want := []string{ // sorted
		"conversationId", "isEditedQuery", "modelConfig", "msgId", "multiMedias",
		"params", "previousDialogueId", "query", "sceneType",
	}
	if got := sortedKeys(payload); !equalStrings(got, want) {
		t.Errorf("payload keys = %v, want %v", got, want)
	}
	// previousDialogueId must be the string anchor, not null.
	if got := string(payload["previousDialogueId"]); got != `"`+prev+`"` {
		t.Errorf("previousDialogueId = %s, want the chaining anchor", got)
	}
	// params is an object (payloadSceneParams), multiMedias an array.
	if got := strings.TrimSpace(string(payload["params"])); got != "{}" {
		t.Errorf("params = %s, want {}", got)
	}
	if got := strings.TrimSpace(string(payload["multiMedias"])); got != "[]" {
		t.Errorf("multiMedias = %s, want [] (never null)", got)
	}
	// modelConfig: model/enableThinking/webSearchStatus always; temperature and
	// topP only when the caller supplied them (TDD API-022).
	var mc map[string]json.RawMessage
	if err := json.Unmarshal(payload["modelConfig"], &mc); err != nil {
		t.Fatalf("modelConfig is not an object: %v", err)
	}
	if got := sortedKeys(mc); !equalStrings(got, []string{"enableThinking", "model", "webSearchStatus"}) {
		t.Errorf("modelConfig keys = %v, want [enableThinking model webSearchStatus]", got)
	}

	// With sampling knobs set, both appear.
	temp, topP := 0.3, 0.9
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-2.6-pro", Temperature: &temp, TopP: &topP,
	}); err != nil {
		t.Fatal(err)
	}
	raw2 := up.RequestsFor(testPH)[1].RawBody
	var payload2 map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw2), &payload2)
	var mc2 map[string]json.RawMessage
	_ = json.Unmarshal(payload2["modelConfig"], &mc2)
	if got := sortedKeys(mc2); !equalStrings(got, []string{"enableThinking", "model", "temperature", "topP", "webSearchStatus"}) {
		t.Errorf("modelConfig keys with sampling = %v, want temperature+topP included", got)
	}
	if string(mc2["temperature"]) != "0.3" || string(mc2["topP"]) != "0.9" {
		t.Errorf("temperature/topP = %s/%s, want 0.3/0.9", mc2["temperature"], mc2["topP"])
	}
}

// FIDELITY-004: a first turn must send previousDialogueId as null (the SPA's
// null anchor), never an empty string.
func TestFirstTurnSendsNullDialogAnchor(t *testing.T) {
	client, up := chatEnv(t)
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-2.6-pro",
	}); err != nil {
		t.Fatal(err)
	}
	raw := up.RequestsFor(testPH)[0].RawBody
	var payload map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &payload)
	if got := string(payload["previousDialogueId"]); got != "null" {
		t.Errorf("previousDialogueId = %s, want null on the first turn", got)
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
