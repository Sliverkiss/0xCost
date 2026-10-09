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
	DefaultHost = "0.0.0.0"
	// DefaultPort is the listen port when neither MIMO2API_PORT nor the
	// platform's PORT is set (ADR 0018: the hosted deployment — Belmo —
	// publishes 3000).
	DefaultPort         = 3000
	DefaultUpstreamBase = "https://aistudio.xiaomimimo.com"
	// DefaultAccountsFile is relative on purpose: a hosted build has no
	// /data mount, and a relative path creates only what the process may
	// write. The container deployment sets MIMO2API_ACCOUNTS_FILE explicitly
	// (ADR 0018).
	DefaultAccountsFile = "data/accounts.json"
	DefaultDefaultModel = "mimo-v2.6-pro"
	DefaultTimezone     = "Asia/Shanghai"
	DefaultLogLevel     = "info"
	DefaultUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	DefaultSecChUA      = `"Google Chrome";v="153", "Chromium";v="153", "Not)A;Brand";v="24"`
)

// Slot hold policies (ADR 0019): when a request gives its in-flight slot back.
const (
	// SlotHoldLastByte keeps the slot for the whole upstream generation — the
	// slot means "this account is working on this request".
	SlotHoldLastByte = "last-byte"
	// SlotHoldFirstByte releases the slot the moment the upstream starts
	// answering: the generation itself no longer occupies capacity, so a single
	// account can serve more conversations concurrently. Account risk, not code
	// risk — hence opt-in.
	SlotHoldFirstByte = "first-byte"
)

// parseSlotHold validates MIMO2API_SLOT_HOLD. An unknown value is an error
// naming the accepted ones: a typo must not silently pick a policy.
func parseSlotHold(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", SlotHoldLastByte:
		return SlotHoldLastByte, nil
	case SlotHoldFirstByte:
		return SlotHoldFirstByte, nil
	default:
		return "", fmt.Errorf("MIMO2API_SLOT_HOLD=%q: want %q or %q", v, SlotHoldLastByte, SlotHoldFirstByte)
	}
}

// Default durations per SDD §6.
const (
	// DefaultRequestTimeout is the whole client-facing budget for one chat
	// request (ADR 0019). It replaced a knob that nothing read, so the real
	// behaviour used to be "no deadline at all" — bounded only by a hardcoded
	// 30-minute HTTP client timeout. 120s covers a single measured turn
	// (6–40s, long ones ~100s) and sits inside the ~100s origin read timeout
	// of a CDN in front of us, so we answer with our own 504 and numbers
	// instead of letting an intermediary cut the connection (524).
	DefaultRequestTimeout = 120 * time.Second
	DefaultSSEIdleTimeout = 180 * time.Second
	// DefaultLeaseTTL bounds one upstream turn and, as a backstop, the in-flight
	// slot a turn holds (ADR 0019). A slot is capacity, not stickiness: it is
	// per request, and its TTL is what stops a wedged connection from pinning
	// an account for the 30 minutes the HTTP client used to allow.
	DefaultLeaseTTL = 120 * time.Second
	// DefaultSlotHold is when a request gives its in-flight slot back:
	// "last-byte" (the slot spans the generation) or "first-byte" (the slot is
	// released as soon as the upstream starts answering). Conservative by
	// default: first-byte raises per-account concurrency while a model is
	// generating, which is the account-safety question, not a code question.
	DefaultSlotHold = SlotHoldLastByte
	// DefaultSessionRefresh is the STS keepalive cadence (ADR 0014): 12 hours,
	// because a renewal re-mints the session cookie and never the credential —
	// the passToken's own 30-day clock is what a password login resets.
	DefaultSessionRefresh = 12 * time.Hour
	DefaultMaxInflight    = 2
	DefaultQueueWait      = 15 * time.Second

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
	// DefaultMaxQueryChars: 0 keeps the per-channel chat caps
	// (adapter.MaxQueryChars: 50,000 fastchat / 102,400 others — SDD §2.8,
	// measured per ADR 0015; the old "90k others" here was the pre-measurement
	// guess and contradicted the code).
	DefaultMaxQueryChars = 0
	DefaultReplayCap     = 20
	DefaultCooldown429   = 60 * time.Second
	DefaultCooldown5xx   = 30 * time.Second
	DefaultCooldownCap   = 10 * time.Minute
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
	leaseTTL             time.Duration
	slotHold             string
	sseIdleTimeout       time.Duration
	sessionRefresh       time.Duration
	maxInflight          int
	queueWait            time.Duration
	audioPoll            time.Duration
	catalogTTL           time.Duration
	ttsTimeout           time.Duration
	ttsMaxTimeout        time.Duration
	asrTimeout           time.Duration
	speechMaxChars       int
	maxQueryChars        int
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

	// redisHost/redisToken are the Upstash contract (DS_REDIS_HOST /
	// DS_REDIS_TOKEN, ADR 0018). Both empty means the accounts file backend.
	redisHost  string
	redisToken string
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
		leaseTTL:             DefaultLeaseTTL,
		slotHold:             DefaultSlotHold,
		sseIdleTimeout:       DefaultSSEIdleTimeout,
		sessionRefresh:       DefaultSessionRefresh,
		maxInflight:          DefaultMaxInflight,
		queueWait:            DefaultQueueWait,
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
		// Default OFF: starting a new-device challenge sends a real SMS to the
		// account owner, and a human has to enter the code anyway, so the
		// dispatch is an explicit operator decision (MIMO2API_OTP_AUTO=true).
		// A default of true meant any login attempt — including the startup
		// catalog warm-up — silently spent an SMS.
		otpAuto:          getenvBool("MIMO2API_OTP_AUTO", false),
		crossRegionAgree: getenvBool("MIMO2API_CROSS_REGION_AGREE", true),
		// Not MIMO2API_-prefixed on purpose: the hosted deployment sets the
		// same two variables as the reference deployment
		// (Sliverkiss/simple-chat), so one Upstash instance serves both.
		redisHost:  getenv("DS_REDIS_HOST", ""),
		redisToken: getenv("DS_REDIS_TOKEN", ""),
	}

	keys := ParseAPIKeys(os.Getenv("MIMO2API_API_KEYS"))
	c.apiKeys = keys
	if len(keys) == 0 {
		// Open mode (no keys): bind-local contract (SDD §5.1, CONF-003).
		c.host = "127.0.0.1"
	}

	var err error
	// Port: MIMO2API_PORT wins; otherwise the platform's own PORT variable
	// (Belmo/Railway/Heroku convention) is honoured so a hosted deploy works
	// without configuration; otherwise the default (ADR 0018).
	if c.port, err = portFromEnv(); err != nil {
		return nil, err
	}
	if c.requestTimeout, err = getenvMillis("MIMO2API_REQUEST_TIMEOUT_MS", DefaultRequestTimeout); err != nil {
		return nil, err
	}
	if c.leaseTTL, err = getenvSeconds("MIMO2API_LEASE_TTL_S", DefaultLeaseTTL); err != nil {
		return nil, err
	}
	if c.slotHold, err = parseSlotHold(getenv("MIMO2API_SLOT_HOLD", DefaultSlotHold)); err != nil {
		return nil, err
	}
	if c.sseIdleTimeout, err = getenvSeconds("MIMO2API_SSE_IDLE_TIMEOUT_S", DefaultSSEIdleTimeout); err != nil {
		return nil, err
	}
	if c.sessionRefresh, err = getenvHours("MIMO2API_SESSION_REFRESH_HOURS", DefaultSessionRefresh); err != nil {
		return nil, err
	}
	if c.maxInflight, err = getenvInt("MIMO2API_MAX_INFLIGHT_PER_ACCOUNT", DefaultMaxInflight); err != nil {
		return nil, err
	}
	if c.queueWait, err = getenvSeconds("MIMO2API_QUEUE_WAIT_S", DefaultQueueWait); err != nil {
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
	// 0 = keep the per-channel caps (50k fastchat / 90k others); a positive value
	// overrides both and is the operator's escape hatch for a client that sends
	// more context than the Studio's own UI would (ADR 0015).
	if c.maxQueryChars, err = getenvInt("MIMO2API_MAX_QUERY_CHARS", DefaultMaxQueryChars); err != nil {
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
func (c *Config) RedisHost() string             { return c.redisHost }
func (c *Config) RedisToken() string            { return c.redisToken }
func (c *Config) DefaultModel() string          { return c.defaultModel }
func (c *Config) RequestTimeout() time.Duration { return c.requestTimeout }

// LeaseTTL is the per-turn upstream deadline and the in-flight slot's TTL
// backstop (ADR 0019).
func (c *Config) LeaseTTL() time.Duration { return c.leaseTTL }

// SlotHold reports whether a request hands its slot back at the first upstream
// byte rather than after the last one (ADR 0019, MIMO2API_SLOT_HOLD).
func (c *Config) SlotHold() string { return c.slotHold }

// UpstreamTimeout is the HTTP client timeout for upstream calls. It is derived,
// never a bare constant: a single upstream turn can only legitimately outlive
// its own lease TTL, and the old hardcoded 30 minutes is what let one wedged
// connection hold an account slot for half an hour (ADR 0019). The margin
// covers connection setup, DNS and TLS inside the same budget.
func (c *Config) UpstreamTimeout() time.Duration { return c.leaseTTL + 30*time.Second }
func (c *Config) SSEIdleTimeout() time.Duration  { return c.sseIdleTimeout }
func (c *Config) SessionRefresh() time.Duration  { return c.sessionRefresh }
func (c *Config) MaxInflight() int               { return c.maxInflight }
func (c *Config) QueueWait() time.Duration       { return c.queueWait }
func (c *Config) AudioPoll() time.Duration       { return c.audioPoll }

// CatalogTTL is the per-account catalog cache lifetime (M19 §3).
func (c *Config) CatalogTTL() time.Duration { return c.catalogTTL }

func (c *Config) TTSTimeout() time.Duration { return c.ttsTimeout }

// TTSMaxTimeout caps the per-piece poll budget (see TTSTimeout).
func (c *Config) TTSMaxTimeout() time.Duration { return c.ttsMaxTimeout }
func (c *Config) ASRTimeout() time.Duration    { return c.asrTimeout }
func (c *Config) SpeechMaxChars() int          { return c.speechMaxChars }

// MaxQueryChars is the operator override for the chat request character cap;
// 0 means "use the per-channel defaults" (adapter.MaxQueryChars).
func (c *Config) MaxQueryChars() int               { return c.maxQueryChars }
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

// portFromEnv resolves the listen port: MIMO2API_PORT, else the platform's
// PORT, else DefaultPort. A malformed value fails loudly naming the variable
// it came from (CONF-005).
func portFromEnv() (int, error) {
	if v, ok := os.LookupEnv("MIMO2API_PORT"); ok && strings.TrimSpace(v) != "" {
		return getenvInt("MIMO2API_PORT", DefaultPort)
	}
	if v, ok := os.LookupEnv("PORT"); ok && strings.TrimSpace(v) != "" {
		return getenvInt("PORT", DefaultPort)
	}
	return DefaultPort, nil
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
