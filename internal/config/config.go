// Package config defines the shared configuration model for chatglm2api, its
// defaults, env overrides and validation. Both the server and the acct CLI load
// it from the same file so pool semantics cannot drift between processes.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that accepts either a Go duration string
// ("2s", "10m") or an integer number of nanoseconds in JSON.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == "" {
		*d = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if strings.TrimSpace(v) == "" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		*d = Duration(parsed)
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid duration %s: %w", s, err)
	}
	*d = Duration(n)
	return nil
}

// MarshalJSON emits a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Config is the top-level config.json document.
type Config struct {
	Listen    string `json:"listen"`     // ":7988"
	APIKey    string `json:"api_key"`    // empty = no client auth
	StateFile string `json:"state_file"` // "./data/state.json"
	BaseURL   string `json:"base_url"`   // upstream origin, default https://chatglm.cn
	// LogRequests toggles per-request log lines.
	LogRequests bool `json:"log_requests"`

	Admin  Admin  `json:"admin"`
	Pool   Pool   `json:"pool"`
	Pace   Pace   `json:"pace"`
	Egress Egress `json:"egress"`
	Chat   Chat   `json:"chat"`
}

// Admin controls the operator endpoints used by the acct CLI.
type Admin struct {
	// Enabled exposes /admin/* mutations. Requires a non-empty api_key:
	// Normalize rejects the combination, so mutations cannot be unauthenticated.
	Enabled bool `json:"enabled"`
}

// Pool sizes and lifecycle knobs. The pool holds one-shot cartridges: every
// identity serves exactly one request and is then discarded, so the only size
// that matters is how many virgin identities are kept in stock.
type Pool struct {
	// WarmSize is the virgin stock target: prewarm at startup and eager restock
	// after every discard, default 20.
	WarmSize int `json:"warm_size"`
	// MaxSize is the hard cap on live identities, default 512.
	MaxSize int `json:"max_size"`
	// AcquireConcurrency bounds concurrent guest/access calls, default 4.
	AcquireConcurrency int `json:"acquire_concurrency"`
	// PersistInterval is the background state.json flush period, default 5s.
	PersistInterval Duration `json:"persist_interval"`
	// MaxRotate bounds identity rotations per client request, default 3. The
	// 10061 "busy" requeue on the same identity shares this attempt budget.
	MaxRotate int `json:"max_rotate"`
	// AcquireWait bounds how long a request waits for the paced restock when
	// the stock is empty before getting ErrNoIdentity, default 10s.
	AcquireWait Duration `json:"acquire_wait"`
	// BreakerThreshold consecutive upstream 5xx failures that trip the pool
	// circuit breaker, default 20; BreakerCooldown is the half-open delay.
	BreakerThreshold int      `json:"breaker_threshold"`
	BreakerCooldown  Duration `json:"breaker_cooldown"`
}

// Pace is the rate-shaping block. Zero values are replaced by conservative
// defaults so a partially written config never becomes aggressive. There is no
// per-identity start interval: an identity starts exactly once.
type Pace struct {
	GlobalQPS       float64  `json:"global_qps"`       // default 5
	AcquireQPS      float64  `json:"acquire_qps"`      // default 2
	AcquireInterval Duration `json:"acquire_interval"` // default 500ms
	AcquireJitter   Duration `json:"acquire_jitter"`   // default 250ms
}

// Egress controls the X-Forwarded-For value sent upstream.
type Egress struct {
	// Mode selects the X-Forwarded-For strategy:
	//   "private" (default) mints a random RFC1918 address per request — the
	//             upstream skips its per-IP guest gate for private ranges;
	//   "random"  mints a random public IPv4 (measured to be gated);
	//   "pool"    cycles through XFFPool;
	//   "fixed"   sends XFFFixed.
	Mode string `json:"mode"`
	// XFFFixed is used when Mode is "fixed" or when no random source is wanted.
	XFFFixed string `json:"xff_fixed"`
	// XFFPool is the rotation set for Mode "pool".
	XFFPool []string `json:"xff_pool"`
}

// Chat controls model mapping and multi-turn behaviour.
type Chat struct {
	// AssistantID is the main chat assistant (65940acf…), overridable.
	AssistantID string `json:"assistant_id"`
	// DefaultModel is the model id reported when the client sends "auto".
	DefaultModel string `json:"default_model"` // default glm-5.3-flash
	// Models is the advertised model catalogue; empty = built-in list.
	Models []ModelSpec `json:"models"`
	// Networking enables the upstream web-search tool by default.
	Networking bool `json:"networking"`
	// DeleteConversations removes upstream threads after each request so guest
	// identities do not accumulate server-side history.
	DeleteConversations bool `json:"delete_conversations"`
	// ReplayHistory sends the full OpenAI history as one typed prompt instead of
	// relying on a server-side conversation. It must stay true: every identity
	// serves exactly one upstream message and no conversation_id is reused, so
	// there is no server-side thread to fall back on. Normalize rejects false.
	ReplayHistory bool `json:"replay_history"`
	// FCMode selects how function calling is served. The upstream has no native
	// FC, so "prompt" (default) injects the toolkit XYML protocol into the prompt
	// and parses tool calls back out; "off" rejects requests carrying tools.
	FCMode FCMode `json:"fc_mode"`
	// StripThinkTags removes <think>…</think> blocks before prompt-FC parsing.
	StripThinkTags bool `json:"strip_think_tags"`
	// FirstFlushHoldback is how long the first SSE payloads are held back so a
	// transient pre-first-token failure can rotate invisibly to another
	// identity. Default 500ms; 0 disables the holdback.
	FirstFlushHoldback Duration `json:"first_flush_holdback"`
}

// FirstFlushHoldbackDefault is the streaming holdback grace: the first SSE
// payloads are buffered this long so a transient pre-first-token failure can
// rotate to another identity before the client sees anything.
const FirstFlushHoldbackDefault = 500 * time.Millisecond

// FCMode is the function-calling strategy.
type FCMode string

// Supported function-calling modes.
const (
	FCModePrompt FCMode = "prompt"
	FCModeOff    FCMode = "off"
)

// ModelSpec is one advertised model.
type ModelSpec struct {
	ID       string `json:"id"`
	Upstream string `json:"upstream,omitempty"` // optional "model" field
	Thinking bool   `json:"thinking,omitempty"` // requests chat_mode "zero"
}

// Default returns the documented defaults.
func Default() *Config {
	return &Config{
		Listen:      ":7988",
		StateFile:   "./data/state.json",
		BaseURL:     "https://chatglm.cn",
		LogRequests: true,
		Pool: Pool{
			WarmSize:           20,
			MaxSize:            512,
			AcquireConcurrency: 4,
			PersistInterval:    Duration(5 * time.Second),
			MaxRotate:          3,
			AcquireWait:        Duration(10 * time.Second),
			BreakerThreshold:   20,
			BreakerCooldown:    Duration(30 * time.Second),
		},
		Pace: Pace{
			GlobalQPS:       5,
			AcquireQPS:      2,
			AcquireInterval: Duration(500 * time.Millisecond),
			AcquireJitter:   Duration(250 * time.Millisecond),
		},
		Egress: Egress{Mode: "private"},
		Chat: Chat{
			AssistantID:         "65940acff94777010aa6b796",
			DefaultModel:        "glm-5.3-flash",
			Networking:          true,
			DeleteConversations: true,
			ReplayHistory:       true,
			FCMode:              FCModePrompt,
			StripThinkTags:      true,
			FirstFlushHoldback:  Duration(FirstFlushHoldbackDefault),
		},
	}
}

// DefaultModels is the built-in catalogue when chat.models is not configured.
func DefaultModels() []ModelSpec {
	return []ModelSpec{
		{ID: "glm-5.3-flash", Upstream: "glm-5.3-flash"},
		{ID: "glm-5.3", Upstream: "glm-5.3", Thinking: true},
		{ID: "glm-4", Upstream: "glm-4"},
		{ID: "auto", Upstream: "glm-5.3-flash"},
	}
}

// Load reads path (empty = defaults only), applies env overrides, then validates.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadOptional is Load but tolerates a missing file (defaults + env only).
func LoadOptional(path string) (*Config, error) {
	if path == "" {
		return Load("")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return Load("")
	}
	return Load(path)
}

func applyEnv(c *Config) {
	if v := os.Getenv("CGLM_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CGLM_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("CGLM_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("CGLM_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("CGLM_POOL_WARM_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.WarmSize = n
		}
	}
	if v := os.Getenv("CGLM_POOL_MAX_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.MaxSize = n
		}
	}
}

// Normalize applies defaults to zero fields and validates ranges.
func (c *Config) Normalize() error {
	d := Default()
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = d.Listen
	}
	if !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = d.BaseURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")

	if c.Pool.WarmSize < 0 {
		return fmt.Errorf("pool.warm_size must be >= 0")
	}
	// An explicit warm_size=0 is honoured (lazy start: no prewarm, the stock
	// fills on demand). An absent key keeps the Default() value of 20, because
	// Load unmarshals into Default().
	if c.Pool.MaxSize <= 0 {
		c.Pool.MaxSize = d.Pool.MaxSize
	}
	if c.Pool.MaxSize < c.Pool.WarmSize {
		return fmt.Errorf("pool.max_size (%d) must be >= pool.warm_size (%d)", c.Pool.MaxSize, c.Pool.WarmSize)
	}
	if c.Pool.AcquireConcurrency <= 0 {
		c.Pool.AcquireConcurrency = d.Pool.AcquireConcurrency
	}
	if c.Pool.PersistInterval.D() <= 0 {
		c.Pool.PersistInterval = d.Pool.PersistInterval
	}
	if c.Pool.MaxRotate <= 0 {
		c.Pool.MaxRotate = d.Pool.MaxRotate
	}
	if c.Pool.AcquireWait.D() <= 0 {
		c.Pool.AcquireWait = d.Pool.AcquireWait
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = d.Pool.BreakerThreshold
	}
	if c.Pool.BreakerCooldown.D() <= 0 {
		c.Pool.BreakerCooldown = d.Pool.BreakerCooldown
	}

	if c.Pace.GlobalQPS <= 0 {
		c.Pace.GlobalQPS = d.Pace.GlobalQPS
	}
	if c.Pace.AcquireQPS <= 0 {
		c.Pace.AcquireQPS = d.Pace.AcquireQPS
	}
	if c.Pace.AcquireInterval.D() <= 0 {
		c.Pace.AcquireInterval = d.Pace.AcquireInterval
	}
	if c.Pace.AcquireJitter.D() < 0 {
		c.Pace.AcquireJitter = 0
	}

	switch strings.ToLower(strings.TrimSpace(c.Egress.Mode)) {
	case "":
		c.Egress.Mode = "private"
	case "private", "random", "pool", "fixed":
		c.Egress.Mode = strings.ToLower(strings.TrimSpace(c.Egress.Mode))
	default:
		return fmt.Errorf("egress.mode: %q is not one of private/random/pool/fixed", c.Egress.Mode)
	}
	if c.Egress.Mode == "fixed" && !ValidIPv4(c.Egress.XFFFixed) {
		return fmt.Errorf("egress.mode=fixed requires a valid egress.xff_fixed IPv4")
	}
	if c.Egress.Mode == "pool" && len(c.Egress.XFFPool) == 0 {
		return fmt.Errorf("egress.mode=pool requires a non-empty egress.xff_pool")
	}
	for _, ip := range c.Egress.XFFPool {
		if !ValidIPv4(ip) {
			return fmt.Errorf("egress.xff_pool: %q is not a valid IPv4", ip)
		}
	}

	if c.Admin.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("admin.enabled=true requires a non-empty api_key")
	}

	if strings.TrimSpace(c.Chat.AssistantID) == "" {
		c.Chat.AssistantID = d.Chat.AssistantID
	}
	// The gateway is stateless by construction (one fresh upstream message per
	// identity, empty conversation_id). A config that disables history replay
	// would silently drop every earlier turn, so it is rejected instead of
	// ignored. Note that json.Unmarshal only sets the field when the key is
	// present, and Default() pre-sets true, so an absent key keeps true.
	if !c.Chat.ReplayHistory {
		return fmt.Errorf("chat.replay_history=false is not supported: the gateway is stateless and must flatten the full history into each prompt")
	}
	switch strings.ToLower(strings.TrimSpace(string(c.Chat.FCMode))) {
	case "":
		c.Chat.FCMode = d.Chat.FCMode
	case string(FCModePrompt):
		c.Chat.FCMode = FCModePrompt
	case string(FCModeOff):
		c.Chat.FCMode = FCModeOff
	default:
		return fmt.Errorf("chat.fc_mode: %q is not one of prompt/off", c.Chat.FCMode)
	}
	if strings.TrimSpace(c.Chat.DefaultModel) == "" {
		c.Chat.DefaultModel = d.Chat.DefaultModel
	}
	if c.Chat.FirstFlushHoldback.D() < 0 {
		return fmt.Errorf("chat.first_flush_holdback must be >= 0")
	}
	if len(c.Chat.Models) == 0 {
		c.Chat.Models = DefaultModels()
	}
	seen := map[string]bool{}
	for _, m := range c.Chat.Models {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("chat.models: model id must not be empty")
		}
		if seen[m.ID] {
			return fmt.Errorf("chat.models: duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
	}
	return nil
}

// ModelByID resolves a requested model id to its spec. "auto" and unknown ids
// fall back to the default model spec so OpenAI clients always get a response.
func (c *Config) ModelByID(id string) ModelSpec {
	id = strings.TrimSpace(id)
	for _, m := range c.Chat.Models {
		if m.ID == id {
			return m
		}
	}
	for _, m := range c.Chat.Models {
		if m.ID == c.Chat.DefaultModel {
			return m
		}
	}
	return ModelSpec{ID: c.Chat.DefaultModel, Upstream: c.Chat.DefaultModel}
}

// ValidIPv4 reports whether s is a dotted-quad IPv4 address.
func ValidIPv4(s string) bool {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
		if len(p) > 1 && p[0] == '0' {
			return false
		}
	}
	return true
}
