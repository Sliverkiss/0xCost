// Package config parses mimo2api's environment-variable configuration once
// at startup; the result is immutable thereafter (SDD §3, §6).
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults per SDD §6 config surface.
const (
	DefaultHost         = "0.0.0.0"
	DefaultPort         = 8080
	DefaultUpstreamBase = "https://aistudio.xiaomimimo.com"
	DefaultAccountsFile = "/data/accounts.json"
	DefaultDefaultModel = "mimo-v2.6-pro"
	DefaultTimezone     = "Asia/Shanghai"
	DefaultLogLevel     = "info"
	DefaultUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	DefaultSecChUA      = `"Google Chrome";v="153", "Chromium";v="153", "Not)A;Brand";v="24"`
)

// Default durations per SDD §6.
const (
	DefaultRequestTimeout = 300 * time.Second
	DefaultSSEIdleTimeout = 180 * time.Second
	DefaultSessionRefresh = 6 * time.Hour
	DefaultStickyTTL      = 30 * time.Minute
	DefaultStickyMax      = 500
	// DefaultSessionWait bounds how long a second streaming request waits for
	// its own session before forking (ADR 0009 decision 5).
	DefaultSessionWait = 2 * time.Second
	DefaultMaxInflight = 2
	DefaultQueueWait   = 15 * time.Second
	DefaultKeepalive   = 15 * time.Second

	// Audio surface (M18): the Studio's own poll cadence is 1s and its task
	// deadlines are tens of seconds, so these are the honest defaults.
	DefaultAudioPoll = time.Second
	// DefaultCatalogTTL is how long an account's Studio catalog is cached
	// (M19 §3).
	DefaultCatalogTTL = 10 * time.Minute
	// DefaultTTSTimeout is the BASE poll budget for one synthesis piece; the
	// effective budget grows with the text (see TTSTimeoutBudget) because the
	// upstream synthesises roughly in real time — a 1024-character passage
	// needs minutes, not the 60s that suits a one-line prompt.
	DefaultTTSTimeout = 60 * time.Second
	// DefaultTTSPerRuneBudget is the added budget per spoken character. Live
	// timings: 128 runes ≈ 12.8s, 384 ≈ 17.8s, 700 > 220s — the upstream's
	// cost is superlinear and load-dependent, so the budget is generous.
	DefaultTTSPerRuneBudget = 500 * time.Millisecond
	// DefaultTTSMaxTimeout caps one piece's budget.
	DefaultTTSMaxTimeout  = 900 * time.Second
	DefaultASRTimeout     = 120 * time.Second
	DefaultSpeechMaxChars = 1024
	DefaultReplayCap      = 20
	DefaultCooldown429    = 60 * time.Second
	DefaultCooldown5xx    = 30 * time.Second
	DefaultCooldownCap    = 10 * time.Minute
	// DefaultCooldownTransport: the transport class has its own ladder (SDD
	// §4.5.1) — a dead connection or a stall is less likely to be the account's
	// fault than an upstream 5xx, so it cools for less and escalates less far.
	DefaultCooldownTransport    = 15 * time.Second
	DefaultCooldownTransportCap = 5 * time.Minute
	// DefaultPoolStatsWindow: the window inside which risk-control signals are
	// correlated across accounts on one egress (SDD §4.5.2).
	DefaultPoolStatsWindow = 5 * time.Minute
	DefaultProbeInterval   = 60 * time.Second
)

// Config is the immutable runtime configuration (CONF-006: no mutation
// surface; construct via Load only).
type Config struct {
	host                 string
	port                 int
	apiKeys              []string
	upstreamBase         string
	accountsFile         string
	defaultModel         string
	requestTimeout       time.Duration
	sseIdleTimeout       time.Duration
	sessionRefresh       time.Duration
	stickyTTL            time.Duration
	stickyMax            int
	sessionWait          time.Duration
	sessionFork          string
	maxInflight          int
	queueWait            time.Duration
	keepalive            time.Duration
	audioPoll            time.Duration
	catalogTTL           time.Duration
	ttsTimeout           time.Duration
	ttsMaxTimeout        time.Duration
	asrTimeout           time.Duration
	speechMaxChars       int
	ttsVoice             string
	timezone             string
	userAgent            string
	secChUA              string
	replayCap            int
	cooldown429          time.Duration
	cooldownTransport    time.Duration
	cooldownTransportCap time.Duration
	poolStatsWindow      time.Duration
	cooldown5xx          time.Duration
	cooldownCap          time.Duration
	probeInterval        time.Duration
	logLevel             string
	otpAuto              bool
	crossRegionAgree     bool
}

// Load reads the MIMO2API_* environment. Invalid numeric values produce an
// error naming the variable (CONF-005). Empty API_KEYS forces a local bind
// (CONF-003). API keys are comma-separated, whitespace-trimmed, empty
// entries dropped (CONF-004).
func Load() (*Config, error) {
	c := &Config{
		host:                 getenv("MIMO2API_HOST", DefaultHost),
		port:                 DefaultPort,
		upstreamBase:         getenv("MIMO2API_UPSTREAM_BASE", DefaultUpstreamBase),
		accountsFile:         getenv("MIMO2API_ACCOUNTS_FILE", DefaultAccountsFile),
		defaultModel:         getenv("MIMO2API_DEFAULT_MODEL", DefaultDefaultModel),
		timezone:             getenv("MIMO2API_X_TIMEZONE", DefaultTimezone),
		userAgent:            getenv("MIMO2API_USER_AGENT", DefaultUserAgent),
		secChUA:              getenv("MIMO2API_SEC_CH_UA", DefaultSecChUA),
		logLevel:             getenv("MIMO2API_LOG_LEVEL", DefaultLogLevel),
		requestTimeout:       DefaultRequestTimeout,
		sseIdleTimeout:       DefaultSSEIdleTimeout,
		sessionRefresh:       DefaultSessionRefresh,
		stickyTTL:            DefaultStickyTTL,
		stickyMax:            DefaultStickyMax,
		sessionWait:          DefaultSessionWait,
		maxInflight:          DefaultMaxInflight,
		queueWait:            DefaultQueueWait,
		keepalive:            DefaultKeepalive,
		audioPoll:            DefaultAudioPoll,
		catalogTTL:           DefaultCatalogTTL,
		ttsTimeout:           DefaultTTSTimeout,
		ttsMaxTimeout:        DefaultTTSMaxTimeout,
		asrTimeout:           DefaultASRTimeout,
		speechMaxChars:       DefaultSpeechMaxChars,
		ttsVoice:             getenv("MIMO2API_TTS_VOICE", ""),
		replayCap:            DefaultReplayCap,
		cooldown429:          DefaultCooldown429,
		cooldown5xx:          DefaultCooldown5xx,
		cooldownTransport:    DefaultCooldownTransport,
		cooldownCap:          DefaultCooldownCap,
		cooldownTransportCap: DefaultCooldownTransportCap,
		probeInterval:        DefaultProbeInterval,
		otpAuto:              getenvBool("MIMO2API_OTP_AUTO", true),
		crossRegionAgree:     getenvBool("MIMO2API_CROSS_REGION_AGREE", true),
	}

	keys := ParseAPIKeys(os.Getenv("MIMO2API_API_KEYS"))
	c.apiKeys = keys
	if len(keys) == 0 {
		// Open mode (no keys): bind-local contract (SDD §5.1, CONF-003).
		c.host = "127.0.0.1"
	}

	var err error
	if c.port, err = getenvInt("MIMO2API_PORT", DefaultPort); err != nil {
		return nil, err
	}
	if c.requestTimeout, err = getenvMillis("MIMO2API_REQUEST_TIMEOUT_MS", DefaultRequestTimeout); err != nil {
		return nil, err
	}
	if c.sseIdleTimeout, err = getenvSeconds("MIMO2API_SSE_IDLE_TIMEOUT_S", DefaultSSEIdleTimeout); err != nil {
		return nil, err
	}
	if c.sessionRefresh, err = getenvHours("MIMO2API_SESSION_REFRESH_HOURS", DefaultSessionRefresh); err != nil {
		return nil, err
	}
	if c.stickyTTL, err = getenvSeconds("MIMO2API_STICKY_TTL_S", DefaultStickyTTL); err != nil {
		return nil, err
	}
	if c.sessionWait, err = getenvMillis("MIMO2API_SESSION_WAIT_MS", DefaultSessionWait); err != nil {
		return nil, err
	}
	if c.sessionFork, err = getenvEnum("MIMO2API_SESSION_FORK_POLICY", "wait", "wait", "fork"); err != nil {
		return nil, err
	}
	if c.stickyMax, err = getenvInt("MIMO2API_STICKY_MAX", DefaultStickyMax); err != nil {
		return nil, err
	}
	if c.maxInflight, err = getenvInt("MIMO2API_MAX_INFLIGHT_PER_ACCOUNT", DefaultMaxInflight); err != nil {
		return nil, err
	}
	if c.queueWait, err = getenvSeconds("MIMO2API_QUEUE_WAIT_S", DefaultQueueWait); err != nil {
		return nil, err
	}
	if c.keepalive, err = getenvMillis("MIMO2API_KEEPALIVE_INTERVAL_MS", DefaultKeepalive); err != nil {
		return nil, err
	}
	if c.replayCap, err = getenvInt("MIMO2API_REPLAY_CAP", DefaultReplayCap); err != nil {
		return nil, err
	}
	if c.probeInterval, err = getenvSeconds("MIMO2API_PROBE_INTERVAL_S", DefaultProbeInterval); err != nil {
		return nil, err
	}
	if c.cooldown429, err = getenvSeconds("MIMO2API_COOLDOWN_429_S", DefaultCooldown429); err != nil {
		return nil, err
	}
	if c.cooldown5xx, err = getenvSeconds("MIMO2API_COOLDOWN_5XX_S", DefaultCooldown5xx); err != nil {
		return nil, err
	}
	if c.cooldownTransport, err = getenvSeconds("MIMO2API_COOLDOWN_TRANSPORT_S", DefaultCooldownTransport); err != nil {
		return nil, err
	}
	if c.cooldownCap, err = getenvSeconds("MIMO2API_COOLDOWN_CAP_S", DefaultCooldownCap); err != nil {
		return nil, err
	}
	if c.cooldownTransportCap, err = getenvSeconds("MIMO2API_COOLDOWN_TRANSPORT_CAP_S", DefaultCooldownTransportCap); err != nil {
		return nil, err
	}
	if c.poolStatsWindow, err = getenvSeconds("MIMO2API_POOL_STATS_WINDOW_S", DefaultPoolStatsWindow); err != nil {
		return nil, err
	}
	if c.catalogTTL, err = getenvSeconds("MIMO2API_CATALOG_TTL_S", DefaultCatalogTTL); err != nil {
		return nil, err
	}
	if c.audioPoll, err = getenvMillis("MIMO2API_AUDIO_POLL_MS", DefaultAudioPoll); err != nil {
		return nil, err
	}
	if c.ttsTimeout, err = getenvSeconds("MIMO2API_AUDIO_TTS_TIMEOUT_S", DefaultTTSTimeout); err != nil {
		return nil, err
	}
	if c.ttsMaxTimeout, err = getenvSeconds("MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S", DefaultTTSMaxTimeout); err != nil {
		return nil, err
	}
	if c.asrTimeout, err = getenvSeconds("MIMO2API_AUDIO_ASR_TIMEOUT_S", DefaultASRTimeout); err != nil {
		return nil, err
	}
	if c.speechMaxChars, err = getenvInt("MIMO2API_SPEECH_MAX_CHARS", DefaultSpeechMaxChars); err != nil {
		return nil, err
	}

	// Explicit non-loopback host with empty keys still forces local bind.
	if len(c.apiKeys) == 0 && c.host != "127.0.0.1" && c.host != "localhost" {
		c.host = "127.0.0.1"
	}

	if u, err := url.Parse(c.upstreamBase); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("MIMO2API_UPSTREAM_BASE: invalid URL %q", c.upstreamBase)
	}
	return c, nil
}

// Accessors — read-only (CONF-006).

func (c *Config) Host() string                  { return c.host }
func (c *Config) Port() int                     { return c.port }
func (c *Config) APIKeys() []string             { return append([]string(nil), c.apiKeys...) }
func (c *Config) UpstreamBase() string          { return c.upstreamBase }
func (c *Config) AccountsFile() string          { return c.accountsFile }
func (c *Config) DefaultModel() string          { return c.defaultModel }
func (c *Config) RequestTimeout() time.Duration { return c.requestTimeout }
func (c *Config) SSEIdleTimeout() time.Duration { return c.sseIdleTimeout }
func (c *Config) SessionRefresh() time.Duration { return c.sessionRefresh }
func (c *Config) StickyTTL() time.Duration      { return c.stickyTTL }
func (c *Config) StickyMax() int                { return c.stickyMax }

// SessionWait is the bound a second streaming request waits for its session
// before forking (SDD §4.3.1, REQ-8).
func (c *Config) SessionWait() time.Duration { return c.sessionWait }

// SessionForkPolicy is "wait" (default) or "fork": whether a busy session is
// waited for or forked immediately.
func (c *Config) SessionForkPolicy() string { return c.sessionFork }
func (c *Config) MaxInflight() int          { return c.maxInflight }
func (c *Config) QueueWait() time.Duration  { return c.queueWait }
func (c *Config) Keepalive() time.Duration  { return c.keepalive }
func (c *Config) AudioPoll() time.Duration  { return c.audioPoll }

// CatalogTTL is the per-account catalog cache lifetime (M19 §3).
func (c *Config) CatalogTTL() time.Duration { return c.catalogTTL }

func (c *Config) TTSTimeout() time.Duration { return c.ttsTimeout }

// TTSMaxTimeout caps the per-piece poll budget (see TTSTimeout).
func (c *Config) TTSMaxTimeout() time.Duration     { return c.ttsMaxTimeout }
func (c *Config) ASRTimeout() time.Duration        { return c.asrTimeout }
func (c *Config) SpeechMaxChars() int              { return c.speechMaxChars }
func (c *Config) TTSVoice() string                 { return c.ttsVoice }
func (c *Config) Timezone() string                 { return c.timezone }
func (c *Config) UserAgent() string                { return c.userAgent }
func (c *Config) SecChUA() string                  { return c.secChUA }
func (c *Config) ReplayCap() int                   { return c.replayCap }
func (c *Config) Cooldown429() time.Duration       { return c.cooldown429 }
func (c *Config) Cooldown5xx() time.Duration       { return c.cooldown5xx }
func (c *Config) CooldownCap() time.Duration       { return c.cooldownCap }
func (c *Config) CooldownTransport() time.Duration { return c.cooldownTransport }
func (c *Config) CooldownTransportCap() time.Duration {
	return c.cooldownTransportCap
}

// PoolStatsWindow is the risk-control correlation window (SDD §4.5.2).
func (c *Config) PoolStatsWindow() time.Duration { return c.poolStatsWindow }
func (c *Config) ProbeInterval() time.Duration   { return c.probeInterval }
func (c *Config) LogLevel() string               { return c.logLevel }

// Debug reports whether the operator asked for the verbose tracing level.
// Anything other than "debug" (case-insensitive) keeps the default quiet
// output; an unknown level is not an error, it is simply not debug.
func (c *Config) Debug() bool {
	return strings.EqualFold(strings.TrimSpace(c.logLevel), "debug")
}
func (c *Config) OTPAuto() bool { return c.otpAuto }

// CrossRegionAgree reports whether the client may agree to the cross-region
// personal-data-transfer demand automatically (MIMO2API_CROSS_REGION_AGREE,
// default true). Only affects logins whose egress IP is outside mainland
// China.
func (c *Config) CrossRegionAgree() bool { return c.crossRegionAgree }

// getenvBool parses a "1/true/yes/on" (case-insensitive) boolean; anything
// else is false. Missing/empty → default.
func getenvBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Addr returns the listen address, e.g. "0.0.0.0:8080".
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.host, c.port)
}

// ParseAPIKeys splits a comma-separated key list: whitespace-trimmed, empty
// entries dropped (CONF-004).
func ParseAPIKeys(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// getenvEnum parses one of a fixed set of values (case-insensitive); a value
// outside the set is a startup error rather than a silent fallback, so a typo
// can never quietly change policy.
func getenvEnum(name, def string, allowed ...string) (string, error) {
	v := strings.ToLower(getenv(name, def))
	for _, a := range allowed {
		if v == strings.ToLower(a) {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s=%q is not one of %v", name, v, allowed)
}

func getenv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func getenvInt(name string, def int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", name, v)
	}
	return n, nil
}

func getenvMillis(name string, def time.Duration) (time.Duration, error) {
	n, err := getenvInt(name, int(def/time.Millisecond))
	return time.Duration(n) * time.Millisecond, err
}

func getenvSeconds(name string, def time.Duration) (time.Duration, error) {
	n, err := getenvInt(name, int(def/time.Second))
	return time.Duration(n) * time.Second, err
}

func getenvHours(name string, def time.Duration) (time.Duration, error) {
	n, err := getenvInt(name, int(def/time.Hour))
	return time.Duration(n) * time.Hour, err
}
