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

// Default durations per SDD §6.
const (
	// DefaultRequestTimeout is the whole client-facing budget for one chat
	// request. **0 = no wall clock, and that is the default** (ADR 0036): a wall
	// clock is not a liveness test, silence is. A turn that keeps talking is
	// bounded by the client's own patience; one that goes quiet longer than the
	// lease silence window is cut. Set this only when
	// something in front of the gateway needs a hard ceiling to relay — a CDN
	// reading a silent origin response for ~100s will cut it before we can name
	// the cause.
	DefaultRequestTimeout = 0
	DefaultSSEIdleTimeout = 180 * time.Second
	// DefaultLeaseTTL is the upstream **silence** window for one turn (ADR 0019,
	// amended by ADR 0036). Every byte the upstream sends resets it, so a long
	// search that keeps fetching pages is never cut for taking long — only for
	// going quiet. The window is what stops a wedged connection from holding a
	// request for the 30 minutes the HTTP client used to allow.
	DefaultLeaseTTL = 120 * time.Second
	// DefaultRenewInterval is the session-renew job's cadence (owner ruling
	// 2026-10: hourly scheduled renewal instead of request-path renewal): the
	// job renews every enabled account's session and — when a credential is
	// dead — runs one password login, so the passToken's own 30-day clock
	// never comes close to expiring between rounds.
	DefaultRenewInterval = time.Hour

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
	DefaultTTSMaxTimeout = 900 * time.Second
	DefaultASRTimeout    = 120 * time.Second
	DefaultReplayCap     = 20
	// Image input (IMG-*): image parts travel through resource/parse, which
	// accounts for a file separately from the 102,400-char query budget
	// (ADR 0015). These two are guards, not measurements, and are documented as
	// such: 20 MB per image is OpenAI's own limit for vision inputs, and 10
	// images bounds one request's fan-out — every image costs three upstream
	// calls (genUploadInfo, PUT, resource/parse). The channel itself advertises
	// 100 MB per file and 50 files per message; raise these to use that.
	DefaultMaxImages     = 10
	DefaultMaxImageBytes = 20 << 20
	// DefaultMaxImagePixels bounds what one image may ask the process to decode
	// (only the "low" level decodes at all): 40 MP ≈ 160 MB of RGBA, far above any
	// real chat image and far below what a 16 MiB request can describe.
	DefaultMaxImagePixels = 40_000_000
	// DefaultPoolStatsWindow: the window inside which risk-control signals are
	// correlated across accounts on one egress (SDD §4.5.2).
	DefaultPoolStatsWindow = 5 * time.Minute
	DefaultProbeInterval   = 60 * time.Second
)

// Config is the immutable runtime configuration (CONF-006: no mutation
// surface; construct via Load only).
type Config struct {
	host              string
	port              int
	apiKeys           []string
	upstreamBase      string
	accountsFile      string
	defaultModel      string
	requestTimeout    time.Duration
	leaseTTL          time.Duration
	sseIdleTimeout    time.Duration
	renewInterval     time.Duration
	audioPoll         time.Duration
	catalogTTL        time.Duration
	ttsTimeout        time.Duration
	ttsMaxTimeout     time.Duration
	asrTimeout        time.Duration
	ttsVoice          string
	timezone          string
	userAgent         string
	secChUA           string
	replayCap         int
	maxImages         int
	maxImageBytes     int
	maxImagePixels    int
	imageAllowPrivate bool
	poolStatsWindow   time.Duration
	probeInterval     time.Duration
	logLevel          string
	otpAuto           bool
	crossRegionAgree  bool

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
		host:           getenv("MIMO2API_HOST", DefaultHost),
		port:           DefaultPort,
		upstreamBase:   getenv("MIMO2API_UPSTREAM_BASE", DefaultUpstreamBase),
		accountsFile:   getenv("MIMO2API_ACCOUNTS_FILE", DefaultAccountsFile),
		defaultModel:   getenv("MIMO2API_DEFAULT_MODEL", DefaultDefaultModel),
		timezone:       getenv("MIMO2API_X_TIMEZONE", DefaultTimezone),
		userAgent:      getenv("MIMO2API_USER_AGENT", DefaultUserAgent),
		secChUA:        getenv("MIMO2API_SEC_CH_UA", DefaultSecChUA),
		logLevel:       getenv("MIMO2API_LOG_LEVEL", DefaultLogLevel),
		requestTimeout: DefaultRequestTimeout,
		leaseTTL:       DefaultLeaseTTL,
		sseIdleTimeout: DefaultSSEIdleTimeout,
		renewInterval:  DefaultRenewInterval,
		audioPoll:      DefaultAudioPoll,
		catalogTTL:     DefaultCatalogTTL,
		ttsTimeout:     DefaultTTSTimeout,
		ttsMaxTimeout:  DefaultTTSMaxTimeout,
		asrTimeout:     DefaultASRTimeout,
		ttsVoice:       getenv("MIMO2API_TTS_VOICE", ""),
		replayCap:      DefaultReplayCap,
		probeInterval:  DefaultProbeInterval,
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
	if c.sseIdleTimeout, err = getenvSeconds("MIMO2API_SSE_IDLE_TIMEOUT_S", DefaultSSEIdleTimeout); err != nil {
		return nil, err
	}
	if c.renewInterval, err = getenvMinutes("MIMO2API_RENEW_INTERVAL", DefaultRenewInterval); err != nil {
		return nil, err
	}
	if c.replayCap, err = getenvInt("MIMO2API_REPLAY_CAP", DefaultReplayCap); err != nil {
		return nil, err
	}
	if c.maxImages, err = getenvInt("MIMO2API_MAX_IMAGES", DefaultMaxImages); err != nil {
		return nil, err
	}
	if c.maxImagePixels, err = getenvInt("MIMO2API_MAX_IMAGE_PIXELS", DefaultMaxImagePixels); err != nil {
		return nil, err
	}
	// Off by default and deliberately explicit: a client-supplied image URL is
	// assumed hostile, and a public gateway answering "fetch this URL for me" must
	// not be usable to probe the host's own network.
	c.imageAllowPrivate = getenvBool("MIMO2API_IMAGE_ALLOW_PRIVATE", false)
	if c.maxImageBytes, err = getenvInt("MIMO2API_MAX_IMAGE_BYTES", DefaultMaxImageBytes); err != nil {
		return nil, err
	}
	if c.probeInterval, err = getenvSeconds("MIMO2API_PROBE_INTERVAL_S", DefaultProbeInterval); err != nil {
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

	// Explicit non-loopback host with empty keys still forces local bind — safe
	// on a laptop, and instantly diagnosable there ("listening on 127.0.0.1").
	if len(c.apiKeys) == 0 && c.host != "127.0.0.1" && c.host != "localhost" {
		c.host = "127.0.0.1"
	}
	// On a hosted platform that is a trap instead: the proxy connects from
	// outside the container, so a loopback bind is unreachable and the platform
	// answers its own "service starting up" page forever — the deploy looks like
	// a build that never finishes (found on Belmo, ADR 0021). PORT is the
	// platform's own signal (MIMO2API_PORT was not set), which makes this the one
	// case where the fallback cannot help anyone: refuse loudly instead.
	if len(c.apiKeys) == 0 && platformSuppliesPort() {
		return nil, fmt.Errorf("MIMO2API_API_KEYS is required when the platform supplies PORT (got PORT=%d): "+
			"without a key the gateway would bind 127.0.0.1 behind the platform's proxy, where nothing can reach it "+
			"(every request a 503 and no hint in the logs). Set MIMO2API_API_KEYS to the key your clients use", c.port)
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

// UpstreamTimeout is the HTTP client timeout for upstream calls. It must never
// be what decides a turn is dead — the turn's **silence window** decides that
// (ADR 0036) — so with no operator wall clock configured the client carries no
// timeout at all: a live turn is bounded by the client's own patience, a dead
// one by silence, and a wedged socket by the little that silence leaves open.
// (The old derived value, leaseTTL+30s, would have become the de-facto wall
// clock the moment the lease stopped being one.) When
// MIMO2API_REQUEST_TIMEOUT_MS *is* set, the client outlives it by a margin so
// that our own named 504 wins the race against a bare i/o timeout.
func (c *Config) UpstreamTimeout() time.Duration {
	if c.requestTimeout > 0 {
		return c.requestTimeout + 30*time.Second
	}
	return 0
}
func (c *Config) SSEIdleTimeout() time.Duration { return c.sseIdleTimeout }

// RenewInterval is the session-renew job's cadence (owner ruling 2026-10:
// hourly scheduled renewal; MIMO2API_RENEW_INTERVAL, minutes).
func (c *Config) RenewInterval() time.Duration { return c.renewInterval }
func (c *Config) AudioPoll() time.Duration     { return c.audioPoll }

// CatalogTTL is the per-account catalog cache lifetime (M19 §3).
func (c *Config) CatalogTTL() time.Duration { return c.catalogTTL }

func (c *Config) TTSTimeout() time.Duration { return c.ttsTimeout }

// TTSMaxTimeout caps the per-piece poll budget (see TTSTimeout).
func (c *Config) TTSMaxTimeout() time.Duration { return c.ttsMaxTimeout }
func (c *Config) ASRTimeout() time.Duration    { return c.asrTimeout }
func (c *Config) TTSVoice() string             { return c.ttsVoice }
func (c *Config) Timezone() string             { return c.timezone }
func (c *Config) UserAgent() string            { return c.userAgent }
func (c *Config) SecChUA() string              { return c.secChUA }
func (c *Config) ReplayCap() int               { return c.replayCap }
func (c *Config) MaxImages() int               { return c.maxImages }
func (c *Config) MaxImageBytes() int           { return c.maxImageBytes }
func (c *Config) MaxImagePixels() int          { return c.maxImagePixels }
func (c *Config) ImageAllowPrivate() bool      { return c.imageAllowPrivate }

// PoolStatsWindow is the risk-control correlation window (SDD §4.5.2).
func (c *Config) PoolStatsWindow() time.Duration { return c.poolStatsWindow }
func (c *Config) ProbeInterval() time.Duration   { return c.probeInterval }

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

func getenv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// platformSuppliesPort reports whether the listen port came from the platform's
// own PORT variable rather than MIMO2API_PORT — the signal that we are running
// under a PaaS router rather than on a laptop or a compose deployment (which
// sets MIMO2API_PORT and would otherwise look identical).
func platformSuppliesPort() bool {
	if v, ok := os.LookupEnv("MIMO2API_PORT"); ok && strings.TrimSpace(v) != "" {
		return false
	}
	v, ok := os.LookupEnv("PORT")
	return ok && strings.TrimSpace(v) != ""
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

// getenvMinutes parses a whole-number of minutes; used for the renew-job
// interval so "60" reads as 60 minutes, the unit the ruling names.
func getenvMinutes(name string, def time.Duration) (time.Duration, error) {
	n, err := getenvInt(name, int(def/time.Minute))
	return time.Duration(n) * time.Minute, err
}
