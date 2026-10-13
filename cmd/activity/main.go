// activity 一次性触发器：只跑一次 RunGiftNow + ProbeTokensNow（礼包领取 +
// token 只读探活），部署后验证 RicheeAI 活跃闭环用，不常驻。
//
// 用法：
//
//	go run ./cmd/activity            # 全池
//	go run ./cmd/activity <uid前缀>  # 单号模式
//
// 读取工作目录的 config.json（auth_dir / state_file / schedule），
// 加载 auths 后构建 pool + upstream，立即执行一次。
package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"richee2api/internal/auth"
	"richee2api/internal/config"
	"richee2api/internal/pool"
	"richee2api/internal/scheduler"
	"richee2api/internal/upstream"
)

// cfgFile 只取本工具需要的字段；Schedule 段直接用 internal/config.Schedule。
type cfgFile struct {
	AuthDir   string          `json:"auth_dir"`
	StateFile string          `json:"state_file"`
	Schedule  config.Schedule `json:"schedule"`
	Upstream  struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`
}

func main() {
	// 可选位置参数：uid 前缀过滤（单号模式）。
	var uidPrefix string
	if len(os.Args) > 1 {
		uidPrefix = os.Args[1]
	}
	raw, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	// 先置默认值再 Unmarshal：键缺席时字段原样保留默认。
	c := cfgFile{Schedule: config.DefaultSchedule()}
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if err := c.Schedule.Normalize(); err != nil {
		log.Fatalf("normalize schedule: %v", err)
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "data/state.json"
	}

	auths, err := auth.LoadDir(c.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	p := pool.New(c.StateFile)
	if uidPrefix != "" {
		// 单号模式：不做 SyncToDir（它会按筛选结果剔除池内其余账号并落盘，
		// 污染共享 state.json）。只 Add 命中账号。
		var hits int
		for _, a := range auths {
			if strings.HasPrefix(a.UID, uidPrefix) {
				p.Add(a)
				hits++
			}
		}
		if hits == 0 {
			log.Fatalf("no account matches uid prefix %q", uidPrefix)
		}
		log.Printf("single-account mode: %d account(s) match %q", hits, uidPrefix)
	} else {
		log.Printf("loaded %d account(s)", len(auths))
		p.SyncToDir(auths)
	}

	sch := scheduler.New(scheduler.Config{
		Pool:         p,
		Upstream:     newUpstream(&c),
		CheckinHours: c.Schedule.CheckinHours,
		GiftHours:    c.Schedule.GiftHours,
	})
	sch.RunGiftNow()
	log.Printf("gift run complete")
	sch.ProbeTokensNow()
	log.Printf("token probe complete")
}

// newUpstream 构造上游客户端（timeout 可配）。
func newUpstream(c *cfgFile) *upstream.Client {
	up := upstream.New()
	if c.Upstream.TimeoutSeconds > 0 {
		up.HTTP.Timeout = time.Duration(c.Upstream.TimeoutSeconds) * time.Second
	}
	return up
}
