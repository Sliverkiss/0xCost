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
		"MIMO2API_SSE_IDLE_TIMEOUT_S", "MIMO2API_SESSION_REFRESH_HOURS",
		"MIMO2API_MAX_INFLIGHT_PER_ACCOUNT", "MIMO2API_QUEUE_WAIT_S",
		"MIMO2API_X_TIMEZONE", "MIMO2API_USER_AGENT",
		"MIMO2API_LOG_LEVEL", "MIMO2API_REPLAY_CAP", "MIMO2API_PROBE_INTERVAL_S",
		"MIMO2API_COOLDOWN_429_S", "MIMO2API_COOLDOWN_5XX_S", "MIMO2API_COOLDOWN_TRANSPORT_S",
		"MIMO2API_COOLDOWN_CAP_S", "MIMO2API_COOLDOWN_TRANSPORT_CAP_S",
		"MIMO2API_POOL_STATS_WINDOW_S",
		"MIMO2API_CATALOG_TTL_S", "MIMO2API_AUDIO_TTS_TIMEOUT_S", "MIMO2API_SPEECH_MAX_CHARS",
		"MIMO2API_MAX_QUERY_CHARS",
		"MIMO2API_OTP_AUTO", "MIMO2API_CROSS_REGION_AGREE",
		"MIMO2API_AUDIO_POLL_MS", "MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S", "MIMO2API_AUDIO_ASR_TIMEOUT_S",
		"MIMO2API_TTS_VOICE", "MIMO2API_SEC_CH_UA",
		// The hosted deployment's variables: PORT is the platform's own, the
		// DS_REDIS_* pair is Upstash (ADR 0018). None are MIMO2API_-prefixed.
		"PORT", "DS_REDIS_HOST", "DS_REDIS_TOKEN", "DS_REDIS_INSECURE",
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
	if c.Port() != DefaultPort || c.Port() != 3000 {
		t.Errorf("port = %d, want the hosted default 3000 (ADR 0018)", c.Port())
	}
	if c.MaxInflight() != 2 {
		t.Errorf("max inflight = %d", c.MaxInflight())
	}
	if c.QueueWait() != 15*time.Second {
		t.Errorf("queue wait = %v", c.QueueWait())
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
	// 12 h since ADR 0014: the keepalive re-mints the session cookie and never
	// the credential, so its cadence is about keeping the cookie warm, not about
	// the passToken's 30-day life (which only a password login resets).
	if c.SessionRefresh() != 12*time.Hour {
		t.Errorf("session refresh = %v, want 12h", c.SessionRefresh())
	}
	if c.MaxQueryChars() != 0 {
		t.Errorf("max query chars = %d, want 0 (per-channel defaults)", c.MaxQueryChars())
	}
	if c.DefaultModel() != "mimo-v2.6-pro" {
		t.Errorf("default model = %q", c.DefaultModel())
	}
	if c.ReplayCap() != 20 {
		t.Errorf("replay cap = %d", c.ReplayCap())
	}
	if c.Addr() != "127.0.0.1:3000" {
		t.Errorf("open-mode addr = %q (want host:port with the resolved port)", c.Addr())
	}
}

// CONF-014: MIMO2API_MAX_QUERY_CHARS is the operator override for the chat
// character guardrail (ADR 0015); unset/0 keeps the per-channel defaults.
func TestMaxQueryCharsOverrideEnv(t *testing.T) {
	testEnv(t, map[string]string{"MIMO2API_MAX_QUERY_CHARS": "250000"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxQueryChars() != 250000 {
		t.Errorf("max query chars = %d, want 250000", c.MaxQueryChars())
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
		"MIMO2API_MAX_INFLIGHT_PER_ACCOUNT": "5",
		"MIMO2API_QUEUE_WAIT_S":             "7",
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
		{"inflight", c.MaxInflight(), 5},
		{"queueWait", c.QueueWait(), 7 * time.Second},
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
		"MIMO2API_SESSION_REFRESH_HOURS":    "z",
		"MIMO2API_MAX_INFLIGHT_PER_ACCOUNT": "w",
		"MIMO2API_QUEUE_WAIT_S":             "v",
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

// CONF (amendment): MIMO2API_OTP_AUTO — default FALSE. Starting a challenge
// sends a real SMS to the account owner and only a human can finish it, so the
// dispatch is an explicit operator decision; the previous default of true meant
// any login attempt (the startup catalog warm-up included) silently spent one.
func TestOTPAutoDefaultFalse(t *testing.T) {
	testEnv(t, map[string]string{})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.OTPAuto() {
		t.Error("OTPAuto default = true, want false — an SMS must never be spent implicitly")
	}
	testEnv(t, map[string]string{"MIMO2API_OTP_AUTO": "true"})
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.OTPAuto() {
		t.Error("OTPAuto = false for explicit true, want true")
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

// CONFIG-DEBUG: MIMO2API_LOG_LEVEL was parsed but never consulted, so "debug"
// silently behaved like "info" and the routing trace was unreachable. Debug()
// is the accessor the server wires, and only the debug spelling (any case,
// padded) turns it on.
func TestLogLevelDebugAccessor(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  bool
	}{
		{level: "debug", want: true},
		{level: "DEBUG", want: true},
		{level: "  Debug  ", want: true},
		{level: "info", want: false},
		{level: "", want: false},
		{level: "verbose", want: false},
	} {
		testEnv(t, map[string]string{"MIMO2API_LOG_LEVEL": tc.level})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(%q): %v", tc.level, err)
		}
		if got := cfg.Debug(); got != tc.want {
			t.Errorf("Debug() with level %q = %v, want %v", tc.level, got, tc.want)
		}
		wantLevel := strings.TrimSpace(tc.level)
		if wantLevel == "" {
			wantLevel = DefaultLogLevel // empty env falls back to the default
		}
		if got := cfg.LogLevel(); got != wantLevel {
			t.Errorf("LogLevel() = %q, want %q", got, wantLevel)
		}
	}
}

// CONF-007: the cooldown ladders are operator-settable, and the transport class
// has its own base and cap (SDD §4.5.1) — the defaults are 60/30/15s with a 10m
// cap for 429/5xx and 5m for transport.
func TestConfigCooldownLadders(t *testing.T) {
	testEnv(t, nil) // clears and restores the pool-related environment
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.CooldownTransport(); got != DefaultCooldownTransport {
		t.Errorf("default transport cooldown = %v, want %v", got, DefaultCooldownTransport)
	}
	if got := cfg.CooldownTransportCap(); got != DefaultCooldownTransportCap {
		t.Errorf("default transport cap = %v, want %v", got, DefaultCooldownTransportCap)
	}
	if got := cfg.PoolStatsWindow(); got != DefaultPoolStatsWindow {
		t.Errorf("default risk window = %v, want %v", got, DefaultPoolStatsWindow)
	}
	testEnv(t, map[string]string{
		"MIMO2API_COOLDOWN_TRANSPORT_S":     "5",
		"MIMO2API_COOLDOWN_TRANSPORT_CAP_S": "90",
		"MIMO2API_COOLDOWN_429_S":           "45",
	})
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with overrides: %v", err)
	}
	if got := cfg.CooldownTransport(); got != 5*time.Second {
		t.Errorf("transport cooldown = %v, want 5s", got)
	}
	if got := cfg.CooldownTransportCap(); got != 90*time.Second {
		t.Errorf("transport cap = %v, want 90s", got)
	}
	if got := cfg.Cooldown429(); got != 45*time.Second {
		t.Errorf("429 cooldown = %v, want 45s", got)
	}
	testEnv(t, map[string]string{"MIMO2API_COOLDOWN_TRANSPORT_S": "not-a-number"})
	if _, err := Load(); err == nil {
		t.Error("Load accepted a non-numeric cooldown; it must fail loudly")
	}
}

// CONF-016 (ADR 0018): the listen port resolves as MIMO2API_PORT → the
// platform's PORT (Belmo/Railway/Heroku convention) → the default 3000. A
// malformed value fails loudly naming the variable it came from.
func TestPortResolution(t *testing.T) {
	t.Run("default 3000 with nothing set", func(t *testing.T) {
		testEnv(t, nil)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Port() != 3000 {
			t.Errorf("port = %d, want 3000", c.Port())
		}
	})

	t.Run("MIMO2API_PORT wins over PORT", func(t *testing.T) {
		testEnv(t, map[string]string{"PORT": "8080", "MIMO2API_PORT": "9999"})
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Port() != 9999 {
			t.Errorf("port = %d, want the explicit MIMO2API_PORT", c.Port())
		}
	})

	t.Run("PORT is honoured when MIMO2API_PORT is unset", func(t *testing.T) {
		testEnv(t, map[string]string{"PORT": "4567"})
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Port() != 4567 {
			t.Errorf("port = %d, want the platform's PORT", c.Port())
		}
	})

	t.Run("an empty PORT falls back to the default", func(t *testing.T) {
		testEnv(t, map[string]string{"PORT": "  "})
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Port() != 3000 {
			t.Errorf("port = %d, want the default for a blank PORT", c.Port())
		}
	})

	t.Run("a malformed PORT is refused by name", func(t *testing.T) {
		testEnv(t, map[string]string{"PORT": "http"})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PORT") {
			t.Fatalf("err = %v, want a message naming PORT", err)
		}
	})

	t.Run("a malformed MIMO2API_PORT is refused by name", func(t *testing.T) {
		testEnv(t, map[string]string{"MIMO2API_PORT": "x"})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MIMO2API_PORT") {
			t.Fatalf("err = %v, want a message naming MIMO2API_PORT", err)
		}
	})
}

// CONF-017 (ADR 0018): the Upstash pair is read unprefixed (so one Upstash
// instance serves the reference deployment too) and its absence keeps the
// file backend.
func TestRedisConfig(t *testing.T) {
	t.Run("absent pair means the file backend", func(t *testing.T) {
		testEnv(t, nil)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.RedisHost() != "" || c.RedisToken() != "" {
			t.Errorf("redis configured out of nowhere: %q", c.RedisHost())
		}
	})

	t.Run("the pair is read verbatim", func(t *testing.T) {
		testEnv(t, map[string]string{"DS_REDIS_HOST": "us1.upstash.io", "DS_REDIS_TOKEN": "tok"})
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.RedisHost() != "us1.upstash.io" || c.RedisToken() != "tok" {
			t.Errorf("redis = %q/%q", c.RedisHost(), c.RedisToken())
		}
	})
}

// CONF-018 (ADR 0018): the default accounts file is relative, so a hosted
// build with no /data mount can start; MIMO2API_ACCOUNTS_FILE still wins.
func TestAccountsFileDefaultIsRelative(t *testing.T) {
	testEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountsFile() != "data/accounts.json" {
		t.Errorf("accounts file = %q, want the relative default", c.AccountsFile())
	}
	testEnv(t, map[string]string{"MIMO2API_ACCOUNTS_FILE": "/data/accounts.json"})
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountsFile() != "/data/accounts.json" {
		t.Errorf("accounts file = %q, want the explicit path", c.AccountsFile())
	}
}

// CONF-019: every accessor answers from the parsed config — a table so a new
// knob with no accessor case is visible here rather than as a coverage cliff.
func TestAccessorsReflectParsedConfig(t *testing.T) {
	testEnv(t, map[string]string{
		"MIMO2API_HOST":                    "127.0.0.1",
		"MIMO2API_DEFAULT_MODEL":           "mimo-v2.6-flash",
		"MIMO2API_REQUEST_TIMEOUT_MS":      "1000",
		"MIMO2API_SSE_IDLE_TIMEOUT_S":      "10",
		"MIMO2API_CATALOG_TTL_S":           "20",
		"MIMO2API_AUDIO_POLL_MS":           "30",
		"MIMO2API_AUDIO_TTS_TIMEOUT_S":     "40",
		"MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S": "50",
		"MIMO2API_AUDIO_ASR_TIMEOUT_S":     "60",
		"MIMO2API_SPEECH_MAX_CHARS":        "700",
		"MIMO2API_TTS_VOICE":               "voice-1",
		"MIMO2API_SEC_CH_UA":               "\"Chromium\"",
		"MIMO2API_COOLDOWN_5XX_S":          "70",
		"MIMO2API_COOLDOWN_CAP_S":          "80",
		"MIMO2API_CROSS_REGION_AGREE":      "false",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"Host", c.Host(), "127.0.0.1"},
		{"DefaultModel", c.DefaultModel(), "mimo-v2.6-flash"},
		{"RequestTimeout", c.RequestTimeout(), time.Second},
		{"SSEIdleTimeout", c.SSEIdleTimeout(), 10 * time.Second},
		{"CatalogTTL", c.CatalogTTL(), 20 * time.Second},
		{"AudioPoll", c.AudioPoll(), 30 * time.Millisecond},
		{"TTSTimeout", c.TTSTimeout(), 40 * time.Second},
		{"TTSMaxTimeout", c.TTSMaxTimeout(), 50 * time.Second},
		{"ASRTimeout", c.ASRTimeout(), 60 * time.Second},
		{"SpeechMaxChars", c.SpeechMaxChars(), 700},
		{"TTSVoice", c.TTSVoice(), "voice-1"},
		{"SecChUA", c.SecChUA(), "\"Chromium\""},
		{"Cooldown5xx", c.Cooldown5xx(), 70 * time.Second},
		{"CooldownCap", c.CooldownCap(), 80 * time.Second},
		{"CrossRegionAgree", c.CrossRegionAgree(), false},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
