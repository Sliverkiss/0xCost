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
	DefaultMaxInflight    = 2
	DefaultQueueWait      = 15 * time.Second
	DefaultKeepalive      = 15 * time.Second
	DefaultReplayCap      = 20
	DefaultCooldown429    = 60 * time.Second
	DefaultCooldown5xx    = 30 * time.Second
	DefaultCooldownCap    = 10 * time.Minute
	DefaultProbeInterval  = 60 * time.Second
)

// Config is the immutable runtime configuration (CONF-006: no mutation
// surface; construct via Load only).
type Config struct {
	host             string
	port             int
	apiKeys          []string
	upstreamBase     string
	accountsFile     string
	defaultModel     string
	requestTimeout   time.Duration
	sseIdleTimeout   time.Duration
	sessionRefresh   time.Duration
	stickyTTL        time.Duration
	stickyMax        int
	maxInflight      int
	queueWait        time.Duration
	keepalive        time.Duration
	timezone         string
	userAgent        string
	secChUA          string
	replayCap        int
	cooldown429      time.Duration
	cooldown5xx      time.Duration
	cooldownCap      time.Duration
	probeInterval    time.Duration
	logLevel         string
	otpAuto          bool
	crossRegionAgree bool
}

// Load reads the MIMO2API_* environment. Invalid numeric values produce an
// error naming the variable (CONF-005). Empty API_KEYS forces a local bind
// (CONF-003). API keys are comma-separated, whitespace-trimmed, empty
// entries dropped (CONF-004).
func Load() (*Config, error) {
	c := &Config{
		host:             getenv("MIMO2API_HOST", DefaultHost),
		port:             DefaultPort,
		upstreamBase:     getenv("MIMO2API_UPSTREAM_BASE", DefaultUpstreamBase),
		accountsFile:     getenv("MIMO2API_ACCOUNTS_FILE", DefaultAccountsFile),
		defaultModel:     getenv("MIMO2API_DEFAULT_MODEL", DefaultDefaultModel),
		timezone:         getenv("MIMO2API_X_TIMEZONE", DefaultTimezone),
		userAgent:        getenv("MIMO2API_USER_AGENT", DefaultUserAgent),
		secChUA:          getenv("MIMO2API_SEC_CH_UA", DefaultSecChUA),
		logLevel:         getenv("MIMO2API_LOG_LEVEL", DefaultLogLevel),
		requestTimeout:   DefaultRequestTimeout,
		sseIdleTimeout:   DefaultSSEIdleTimeout,
		sessionRefresh:   DefaultSessionRefresh,
		stickyTTL:        DefaultStickyTTL,
		stickyMax:        DefaultStickyMax,
		maxInflight:      DefaultMaxInflight,
		queueWait:        DefaultQueueWait,
		keepalive:        DefaultKeepalive,
		replayCap:        DefaultReplayCap,
		cooldown429:      DefaultCooldown429,
		cooldown5xx:      DefaultCooldown5xx,
		cooldownCap:      DefaultCooldownCap,
		probeInterval:    DefaultProbeInterval,
		otpAuto:          getenvBool("MIMO2API_OTP_AUTO", true),
		crossRegionAgree: getenvBool("MIMO2API_CROSS_REGION_AGREE", true),
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
func (c *Config) MaxInflight() int              { return c.maxInflight }
func (c *Config) QueueWait() time.Duration      { return c.queueWait }
func (c *Config) Keepalive() time.Duration      { return c.keepalive }
func (c *Config) Timezone() string              { return c.timezone }
func (c *Config) UserAgent() string             { return c.userAgent }
func (c *Config) SecChUA() string               { return c.secChUA }
func (c *Config) ReplayCap() int                { return c.replayCap }
func (c *Config) Cooldown429() time.Duration    { return c.cooldown429 }
func (c *Config) Cooldown5xx() time.Duration    { return c.cooldown5xx }
func (c *Config) CooldownCap() time.Duration    { return c.cooldownCap }
func (c *Config) ProbeInterval() time.Duration  { return c.probeInterval }
func (c *Config) LogLevel() string              { return c.logLevel }

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
