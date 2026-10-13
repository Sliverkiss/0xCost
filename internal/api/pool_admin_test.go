package api_test

// ③ The pool view (TDD §3.5.2 POOLVIEW-001..006, SDD §4.9).
//
// The aggregate /healthz contract is frozen — the Docker healthcheck parses it —
// so everything an operator needs was moved into an authenticated endpoint
// instead. These cases pin the invariant, the frozen shape, the auth, the
// masking, and the fact that no credential material crosses the boundary.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

func poolView(t *testing.T, ts *testserver.TestServer, key string) (int, map[string]any, string) {
	t.Helper()
	resp, err := ts.Get("/v1/admin/pool", key)
	if err != nil {
		t.Fatalf("get pool view: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, string(raw)
}

// POOLVIEW-003: the detail view is authenticated, like the other admin routes.
func TestPoolViewRequiresAuth(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k-test"}})
	if status, _, _ := poolView(t, ts, ""); status != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", status)
	}
	if status, _, _ := poolView(t, ts, "wrong-key"); status != http.StatusUnauthorized {
		t.Errorf("bad key: status = %d, want 401", status)
	}
	if status, _, _ := poolView(t, ts, "k-test"); status != http.StatusOK {
		t.Errorf("valid key: status = %d, want 200", status)
	}
}

// POOLVIEW-001: the accounting invariant holds in the view itself.
func TestPoolViewAccountingInvariant(t *testing.T) {
	two := []testserver.TestAccount{
		{ID: "acc1", PH: "ph-1", Cookie: testutil.TestCookie},
		{ID: "acc2", PH: "ph-2", Cookie: testutil.TestCookie},
	}
	ts := testserver.NewTestServer(t, two, &testserver.TestServerOptions{APIKeys: []string{"k-test"}})
	_ = ts.Pool
	ts.Pool.ReportDisabled("acc2", "dead passToken", true)

	status, body, _ := poolView(t, ts, "k-test")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	accounts, _ := body["accounts"].(map[string]any)
	total := int(accounts["total"].(float64))
	if got := int(accounts["available"].(float64)) + int(accounts["disabled"].(float64)); got != total {
		t.Errorf("states add to %d, want total %d", got, total)
	}
	if _, present := accounts["cooldown"]; present {
		t.Error("the cooldown aggregate is gone with the mechanism (owner ruling 2026-10); it must not appear")
	}
	if ok, _ := body["invariant_ok"].(bool); !ok {
		t.Error("invariant_ok = false")
	}
	// The summary block must mirror the pool itself — an all-zero block would
	// satisfy the invariant while telling the operator nothing.
	sum := ts.Pool.Summarize()
	if total != sum.Total || int(accounts["disabled"].(float64)) != sum.Disabled ||
		int(accounts["available"].(float64)) != sum.Available {
		t.Errorf("summary %v does not mirror the pool %+v", accounts, sum)
	}
	if int(accounts["disabled"].(float64)) != 1 {
		t.Errorf("disabled = %v, want 1", accounts["disabled"])
	}
}

// POOLVIEW-002: /healthz keeps its shape — the Docker healthcheck parses it.
func TestHealthzShapeUnchanged(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Get("/healthz", "")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Status   string `json:"status"`
		Accounts struct {
			Total     *int `json:"total"`
			Available *int `json:"available"`
			Disabled  *int `json:"disabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("healthz is not the documented shape: %v (%s)", err, raw)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Accounts.Total == nil || body.Accounts.Available == nil ||
		body.Accounts.Disabled == nil {
		t.Errorf("healthz lost one of the three aggregates: %s", raw)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		t.Errorf("healthz is not JSON: %s", raw)
	}
}

// POOLVIEW-004: no credential material crosses the boundary — not the cookie, not
// the ph, not a password, not an egress endpoint.
func TestPoolViewExposesNoCredentials(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k-test"}})
	ts.Pool.ReportRiskControl("acc1", "upstream 461", pool.RiskEvent{
		Model: "mimo-v2.6-pro", Channel: "chat", Status: 461, BodyDigest: "abc123",
	})
	status, _, raw := poolView(t, ts, "k-test")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, secret := range []string{
		testutil.TestCookie, testutil.TestPH, "pass_token", "password", "secret",
	} {
		if secret == "" {
			continue
		}
		if strings.Contains(raw, secret) {
			t.Errorf("the view leaks %q: %s", secret, raw)
		}
	}
	// The evidence that IS meant to be there.
	if !strings.Contains(raw, "abc123") {
		t.Error("the risk event's body digest is missing; the evidence must be readable")
	}
	if !strings.Contains(raw, "risk-control") && !strings.Contains(raw, "461") {
		t.Error("the risk event's classification is missing")
	}
}

// POOLVIEW-005: ids are masked, distinct per account, and stable across reads.
func TestPoolViewMasksIDsStably(t *testing.T) {
	two := []testserver.TestAccount{
		{ID: "acc1", PH: "ph-1", Cookie: testutil.TestCookie},
		{ID: "acc2", PH: "ph-2", Cookie: testutil.TestCookie},
	}
	ts := testserver.NewTestServer(t, two, &testserver.TestServerOptions{APIKeys: []string{"k-test"}})
	_, first, raw1 := poolView(t, ts, "k-test")
	_, second, _ := poolView(t, ts, "k-test")

	ids := func(body map[string]any) []string {
		var out []string
		rows, _ := body["pool"].([]any)
		for _, r := range rows {
			row, _ := r.(map[string]any)
			if id, _ := row["id"].(string); id != "" {
				out = append(out, id)
			}
		}
		return out
	}
	a, b := ids(first), ids(second)
	if len(a) != 2 {
		t.Fatalf("rows = %d, want 2", len(a))
	}
	if a[0] == a[1] {
		t.Errorf("two accounts share the mask %q", a[0])
	}
	if a[0] != b[0] || a[1] != b[1] {
		t.Errorf("masks are not stable across reads: %v then %v", a, b)
	}
	if strings.Contains(raw1, "acc1") || strings.Contains(raw1, "acc2") {
		t.Error("the raw account id appears in the view")
	}
}

// POOLVIEW-006: the fields an operator actually diagnoses with are present.
// A recorded 429 is diagnostics only (owner ruling 2026-10): the account stays
// available and the last_error block carries its status and timestamp.
func TestPoolViewCarriesDiagnosticFields(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"k-test"}})
	ts.Pool.Report429("acc1")
	_, body, raw := poolView(t, ts, "k-test")
	rows, _ := body["pool"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row["state"] != string(pool.StateAvailable) {
		t.Errorf("state = %v, want available (a 429 is diagnostics, not a state change)", row["state"])
	}
	if _, present := row["cooldown"]; present {
		t.Error("the per-account cooldown block is gone with the mechanism; it must not appear")
	}
	if _, present := row["failures"]; present {
		t.Error("the per-class failure counters are gone with the ladder; they must not appear")
	}
	last, ok := row["last_error"].(map[string]any)
	if !ok {
		t.Fatalf("last_error is missing: %s", raw)
	}
	if int(last["status"].(float64)) != 429 {
		t.Errorf("last_error status = %v, want 429", last["status"])
	}
	if _, ok := last["at"].(string); !ok {
		t.Errorf("last_error at is missing: %v", last["at"])
	}
	if row["egress"] != pool.DefaultEgress {
		t.Errorf("egress = %v, want %v", row["egress"], pool.DefaultEgress)
	}
	if _, ok := row["ewma_latency_ms"]; !ok {
		t.Error("ewma_latency_ms is missing")
	}
	if _, ok := row["in_flight"]; !ok {
		t.Error("in_flight is missing")
	}
	if _, ok := row["credential_age_s"]; !ok {
		t.Error("credential_age_s is missing")
	}
}
