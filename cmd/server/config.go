// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"richee2api/internal/config"
	"richee2api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		SoftRate    string `json:"soft_rate"`     // "600s"，软限流冷却基数
		SoftRateMax string `json:"soft_rate_max"` // "2h"，软冷却指数退避封顶
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	// Admin 运维管理端点开关。默认关闭：管理能力默认不暴露。
	Admin struct {
		Enabled bool `json:"enabled"` // 默认 false
	} `json:"admin"`

	Upstream struct {
		// Base 上游 base 覆盖；空 = 内置默认 https://claw.richee.cn/claw-api。
		Base string `json:"base"`
		// TimeoutSeconds 短 RPC（checkin/gift/credit/login）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖。
		UserAgent string `json:"user_agent"`
		// PassthroughIP 是否透传客户端 IP 给上游；缺省 false。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		Mode string `json:"mode"` // "custom" / "append" / "passthrough"
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式
		Token string `json:"token"` // url 非完整连接串时组装用
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败降权阈值，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长，默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长上限钳制，默认 "2h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时 +0.5
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		ExpiringSoon       string  `json:"expiring_soon"`        // 快过期积分窗口，默认 "168h"
		// CostExploreInterval costTier 条件探索窗口，默认 "30m"；"0" 关停。
		CostExploreInterval string `json:"cost_explore_interval"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	DegradeCooldownDur     time.Duration `json:"-"`
	DegradeCooldownMaxD    time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	ExpiringSoonDur        time.Duration `json:"-"`
	CostExploreIntervalDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	// 排程段默认值由 internal/config 集中维护。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough"
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h"
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 R2A_* env 覆盖。
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
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// envLookup 环境变量优先序：RICHEE2API_*（本项目口径）> R2A_*（短名）
// > WB2A_*（参考项目习惯兼容）。
func envLookup(key string) string {
	if v := os.Getenv("RICHEE2API_" + key); v != "" {
		return v
	}
	if v := os.Getenv("R2A_" + key); v != "" {
		return v
	}
	return os.Getenv("WB2A_" + key)
}

func applyEnv(c *Config) {
	if v := envLookup("LISTEN"); v != "" {
		c.Listen = v
	}
	if v := envLookup("API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := envLookup("AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := envLookup("STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := envLookup("UPSTREAM_BASE"); v != "" {
		c.Upstream.Base = v
	}
	if v := envLookup("SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := envLookup("SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := envLookup("TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := envLookup("HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := envLookup("IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := envLookup("USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := envLookup("PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := envLookup("SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := envLookup("PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := envLookup("PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := envLookup("EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := envLookup("ADMIN_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.Enabled = b
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = "168h"
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
	}
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// fail-fast：admin.enabled=true 且 api_key 为空 = 未鉴权的 mutation 端点，拒绝启动。
	if c.Admin.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("admin.enabled=true 但 api_key 为空：请设置 api_key 或将 admin.enabled 置 false")
	}
	// 排程段归一由 internal/config 统一实现。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / append / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}
