package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// testEnv clears all MIMO2API_* vars, applies kv for the test, and cleans up.
func testEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	vars := []string{
		"MIMO2API_HOST", "MIMO2API_PORT", "MIMO2API_API_KEYS", "MIMO2API_UPSTREAM_BASE",
		"MIMO2API_ACCOUNTS_FILE", "MIMO2API_DEFAULT_MODEL", "MIMO2API_REQUEST_TIMEOUT_MS",
		"MIMO2API_SSE_IDLE_TIMEOUT_S", "MIMO2API_SESSION_REFRESH_HOURS", "MIMO2API_STICKY_TTL_S",
		"MIMO2API_STICKY_MAX", "MIMO2API_MAX_INFLIGHT_PER_ACCOUNT", "MIMO2API_QUEUE_WAIT_S",
		"MIMO2API_KEEPALIVE_INTERVAL_MS", "MIMO2API_X_TIMEZONE", "MIMO2API_USER_AGENT",
		"MIMO2API_LOG_LEVEL", "MIMO2API_REPLAY_CAP", "MIMO2API_PROBE_INTERVAL_S",
	}
	for _, k := range vars {
		os.Unsetenv(k)
	}
	for k, v := range kv {
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k := range kv {
			os.Unsetenv(k)
		}
	})
}

// CONF-001: defaults with unset env.
func TestDefaults(t *testing.T) {
	testEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port() != 8080 {
		t.Errorf("port = %d", c.Port())
	}
	if c.StickyTTL() != 1800*time.Second {
		t.Errorf("sticky ttl = %v", c.StickyTTL())
	}
	if c.StickyMax() != 500 {
		t.Errorf("sticky max = %d", c.StickyMax())
	}
	if c.MaxInflight() != 2 {
		t.Errorf("max inflight = %d", c.MaxInflight())
	}
	if c.QueueWait() != 15*time.Second {
		t.Errorf("queue wait = %v", c.QueueWait())
	}
	if c.Keepalive() != 15000*time.Millisecond {
		t.Errorf("keepalive = %v", c.Keepalive())
	}
	if c.Timezone() != "Asia/Shanghai" {
		t.Errorf("timezone = %q", c.Timezone())
	}
	if c.UpstreamBase() != "https://aistudio.xiaomimimo.com" {
		t.Errorf("upstream = %q", c.UpstreamBase())
	}
	if c.RequestTimeout() != 300*time.Second {
		t.Errorf("request timeout = %v", c.RequestTimeout())
	}
	if c.SSEIdleTimeout() != 180*time.Second {
		t.Errorf("sse idle = %v", c.SSEIdleTimeout())
	}
	if c.SessionRefresh() != 6*time.Hour {
		t.Errorf("session refresh = %v", c.SessionRefresh())
	}
	if c.DefaultModel() != "mimo-v2.6-pro" {
		t.Errorf("default model = %q", c.DefaultModel())
	}
	if c.ReplayCap() != 20 {
		t.Errorf("replay cap = %d", c.ReplayCap())
	}
	if c.Addr() != "127.0.0.1:8080" {
		t.Errorf("open-mode addr = %q", c.Addr())
	}
}

// CONF-002: every var parsed and applied.
func TestEnvOverrides(t *testing.T) {
	testEnv(t, map[string]string{
		"MIMO2API_HOST":                     "0.0.0.0",
		"MIMO2API_PORT":                     "9000",
		"MIMO2API_API_KEYS":                 "k1",
		"MIMO2API_UPSTREAM_BASE":            "https://example.com",
		"MIMO2API_ACCOUNTS_FILE":            "/tmp/accts.json",
		"MIMO2API_DEFAULT_MODEL":            "mimo-v2.5-pro",
		"MIMO2API_REQUEST_TIMEOUT_MS":       "12345",
		"MIMO2API_SSE_IDLE_TIMEOUT_S":       "42",
		"MIMO2API_SESSION_REFRESH_HOURS":    "2",
		"MIMO2API_STICKY_TTL_S":             "600",
		"MIMO2API_STICKY_MAX":               "10",
		"MIMO2API_MAX_INFLIGHT_PER_ACCOUNT": "5",
		"MIMO2API_QUEUE_WAIT_S":             "7",
		"MIMO2API_KEEPALIVE_INTERVAL_MS":    "250",
		"MIMO2API_X_TIMEZONE":               "UTC",
		"MIMO2API_USER_AGENT":               "ua-test",
		"MIMO2API_LOG_LEVEL":                "debug",
		"MIMO2API_REPLAY_CAP":               "3",
		"MIMO2API_PROBE_INTERVAL_S":         "11",
		"MIMO2API_OTP_AUTO":                 "false",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want interface{}
	}{
		{"host", c.Host(), "0.0.0.0"},
		{"port", c.Port(), 9000},
		{"keys", len(c.APIKeys()), 1},
		{"upstream", c.UpstreamBase(), "https://example.com"},
		{"accounts", c.AccountsFile(), "/tmp/accts.json"},
		{"model", c.DefaultModel(), "mimo-v2.5-pro"},
		{"reqTimeout", c.RequestTimeout(), 12345 * time.Millisecond},
		{"sseIdle", c.SSEIdleTimeout(), 42 * time.Second},
		{"refresh", c.SessionRefresh(), 2 * time.Hour},
		{"ttl", c.StickyTTL(), 600 * time.Second},
		{"stickyMax", c.StickyMax(), 10},
		{"inflight", c.MaxInflight(), 5},
		{"queueWait", c.QueueWait(), 7 * time.Second},
		{"keepalive", c.Keepalive(), 250 * time.Millisecond},
		{"tz", c.Timezone(), "UTC"},
		{"ua", c.UserAgent(), "ua-test"},
		{"log", c.LogLevel(), "debug"},
		{"replay", c.ReplayCap(), 3},
		{"probe", c.ProbeInterval(), 11 * time.Second},
		{"otpAuto", c.OTPAuto(), false},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	if c.Addr() != "0.0.0.0:9000" {
		t.Errorf("addr = %q", c.Addr())
	}
}

// CONF-003: empty API_KEYS forces local bind even with explicit host.
func TestEmptyKeysForceLocalBind(t *testing.T) {
	testEnv(t, map[string]string{"MIMO2API_HOST": "0.0.0.0", "MIMO2API_API_KEYS": ""})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Host() != "127.0.0.1" {
		t.Errorf("host = %q, want forced 127.0.0.1", c.Host())
	}
}

// CONF-004: API key parsing.
func TestParseAPIKeys(t *testing.T) {
	got := ParseAPIKeys(" k1 , k2 ,,k3,")
	want := []string{"k1", "k2", "k3"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keys[%d] = %q", i, got[i])
		}
	}
	if ParseAPIKeys("  ,,") != nil {
		t.Errorf("all-empty input must yield no keys")
	}
}

// CONF-005: invalid numerics error with the var name.
func TestInvalidValues(t *testing.T) {
	for name, val := range map[string]string{
		"MIMO2API_PORT":                     "not-a-port",
		"MIMO2API_SSE_IDLE_TIMEOUT_S":       "x",
		"MIMO2API_KEEPALIVE_INTERVAL_MS":    "y",
		"MIMO2API_SESSION_REFRESH_HOURS":    "z",
		"MIMO2API_STICKY_MAX":               "q",
		"MIMO2API_MAX_INFLIGHT_PER_ACCOUNT": "w",
		"MIMO2API_QUEUE_WAIT_S":             "v",
		"MIMO2API_STICKY_TTL_S":             "u",
		"MIMO2API_REQUEST_TIMEOUT_MS":       "t",
		"MIMO2API_REPLAY_CAP":               "s",
		"MIMO2API_PROBE_INTERVAL_S":         "r",
	} {
		t.Run(name, func(t *testing.T) {
			testEnv(t, map[string]string{name: val, "MIMO2API_API_KEYS": "k"})
			_, err := Load()
			if err == nil {
				t.Errorf("%s=%q: expected error", name, val)
				return
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name the variable", err)
			}
		})
	}
}

// CONF-006: immutability is structural (no setters exist); accessor must not
// leak an aliased slice.
func TestImmutableAfterParse(t *testing.T) {
	testEnv(t, map[string]string{"MIMO2API_API_KEYS": "k1"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	keys := c.APIKeys()
	keys[0] = "mutated"
	if c.APIKeys()[0] != "k1" {
		t.Errorf("APIKeys() returned an aliased slice — mutation leaked")
	}
}

// CONF (amendment): MIMO2API_OTP_AUTO — default true; explicit false and
// garbage values disable auto-starting the new-device OTP challenge.
func TestOTPAutoDefaultTrue(t *testing.T) {
	testEnv(t, map[string]string{})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.OTPAuto() {
		t.Error("OTPAuto default = false, want true")
	}
	testEnv(t, map[string]string{"MIMO2API_OTP_AUTO": "0"})
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.OTPAuto() {
		t.Error("OTPAuto = true for 0, want false")
	}
	testEnv(t, map[string]string{"MIMO2API_OTP_AUTO": "garbage"})
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.OTPAuto() {
		t.Error("OTPAuto = true for garbage, want false")
	}
}
