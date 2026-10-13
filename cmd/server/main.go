// main.go richee2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"richee2api/internal/auth"
	"richee2api/internal/pool"
	"richee2api/internal/redisstore"
	"richee2api/internal/scheduler"
	"richee2api/internal/server"
	"richee2api/internal/session"
	"richee2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env。
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 停后台落盘 goroutine + 最后补一次落盘
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除

	// auths 目录热加载：新增凭证文件自动进池。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被限流时重分配。
			// 单 realm：前缀剥离对可用集合无过滤效果（恒 "richee"），保留形态
			// 以对齐 handler 侧 resolveModel 的调用链。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	up.Base = cfg.Upstream.Base
	// 短 RPC 总时长上限（checkin/gift/credit/login）。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	// 聊天 SSE 流中空闲上限。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.UserAgent = cfg.Upstream.UserAgent
	up.PassthroughIP = cfg.Upstream.PassthroughIP

	sch := scheduler.New(scheduler.Config{
		Pool:            p,
		Upstream:        up,
		CheckinHours:    cfg.Schedule.CheckinHours,
		GiftHours:       cfg.Schedule.GiftHours,
		CheckinDisabled: !cfg.Schedule.CheckinEnabled,
		GiftDisabled:    !cfg.Schedule.GiftEnabled,
	})
	if !cfg.Schedule.CheckinEnabled {
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	} else {
		log.Printf("签到已启用：%v 点（签到 + 积分查询解冻）", cfg.Schedule.CheckinHours)
	}
	if !cfg.Schedule.GiftEnabled {
		log.Printf("礼包探测已禁用（schedule.gift_enabled=false）")
	} else {
		log.Printf("礼包探测已启用：%v 点（新手礼包幂等领取）", cfg.Schedule.GiftHours)
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		AdminEnabled: cfg.Admin.Enabled,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("richee2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
