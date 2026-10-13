// Package scheduler 定时任务：签到（userSign）+ 礼包（gift）+ token 探活三类独立排程。
// 签到成功后重查积分，余额 > 0 的冷却账号自动解冻。
//
// 与 workbuddy2api 的差异（见 docs/PORT_NOTES.md）：
//   - 无 refresh/keepalive 排程：RicheeAI token 不过期（expireAt:null），
//     失效判定走业务 401（chat/签到路径的 NoteSessionDead）。
//   - 猫猫旅行/开学季/夜猫子/连登奖励链无 RicheeAI 对应端点，整族不移植
//     （SPEC 事实表只有 userSign/gift/subscription/inviteCode）。
package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"richee2api/internal/auth"
	"richee2api/internal/logfmt"
	"richee2api/internal/pool"
	"richee2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	// CheckinHours 签到时点（含积分回查 + 解冻），默认 [9, 21]。
	CheckinHours []int
	// GiftHours 礼包探测时点（新手礼包领取，幂等），默认 [10]。
	GiftHours []int
	// CheckinDisabled 显式关闭签到排程（schedule.checkin_enabled=false）。
	CheckinDisabled bool
	// GiftDisabled 显式关闭礼包排程（schedule.gift_enabled=false）。
	GiftDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// giftTried 礼包当日尝试记录：uid → 自然日（CST）。已尝试的账号当日不再
	// 重试（礼包是每号一次的幂等写，重复调用只刷上游 4xx 噪音）；进程重启即清零。
	mu        sync.Mutex
	giftTried map[string]string

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游。
	checkinMu sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.GiftHours) == 0 {
		cfg.GiftHours = []int{10}
	}
	return &Scheduler{cfg: cfg, giftTried: make(map[string]string)}
}

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 签到 / 积分查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskGift
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时，该时刻多类任务一并执行。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.GiftDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.GiftHours), taskGift})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 全部任务禁用：不空转，只等退出信号。
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	switch k {
	case taskCheckin:
		s.RunCheckinNow()
	case taskGift:
		s.RunGiftNow()
	}
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
func (s *Scheduler) RunCheckinNow() {
	if _, err := s.CheckinAll(); err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
	}
}

// CheckinAll 全量签到：daily-checkin → 查积分 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy。
//
// token 失效（业务 401）走 Pool.NoteSessionDead 的连续计数语义：
// 一次失败不立即杀号，连续 sessionDeadThreshold 次才禁用。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.TokenValue() == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// 签到返回错误（含"今天已签到"）也继续查积分：积分恢复即可解冻账号。
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsSessionDead(err) {
				// token 失效：连续计数（对齐 chat 路径的 NoteSessionDead 语义）。
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: checkin %s: 连续 %d 次 token 失效 — 禁用",
						logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
				oc.Status, oc.Detail = CheckinFail, "token dead: "+err.Error()
				failN++
				out = append(out, oc)
				continue
			}
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误。
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		} else {
			oc.Status = CheckinOK
		}
		// 查积分并解冻。
		remain, err := s.cfg.Upstream.UserResource(a)
		if err != nil {
			log.Printf("credit %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "credit: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, 0)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// cstZone 上游自然日口径：CST（Asia/Shanghai），固定 +8，不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// giftDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func giftDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunGiftNow 立即对池内所有可用账号执行新手礼包领取探测（幂等：canClaim 才调
// claim；已领过静默跳过）。禁用账号跳过；账号间限速 giftAccountDelay。
func (s *Scheduler) RunGiftNow() {
	s.runGift(context.Background())
}

// giftAccountDelay 账号间限速：避免上游风控。测试可置 0。
var giftAccountDelay = 800 * time.Millisecond

// sleepCtx 可取消的等待：ctx 取消立即返回 false，等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if ctx == nil {
		return true
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runGift 礼包领取遍历，随 ctx 取消立即退出。
func (s *Scheduler) runGift(ctx context.Context) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.TokenValue() == "" {
			continue
		}
		if !first {
			if !sleepCtx(ctx, giftAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再领
			}
		}
		first = false
		s.giftOne(a)
	}
}

// giftOne 单账号礼包领取：先查状态（canClaim 才写），幂等失败静默。
func (s *Scheduler) giftOne(a *auth.Auth) {
	if s.giftTriedToday(a.UID) {
		return // 当日已试过（成功或幂等拒绝），不重复打上游
	}
	gs, err := s.cfg.Upstream.GiftStatus(a)
	if err != nil {
		log.Printf("gift %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if gs == nil || !gs.CanClaim {
		return // 无可领/已领：正常态，静默
	}
	if _, err := s.cfg.Upstream.ClaimGift(a); err != nil {
		// 业务错误多为「已领取」幂等拒绝：静默记当日已试，不刷 WARN。
		log.Printf("gift %s: claim: %v", logfmt.Label(a.UID, a.Nickname), err)
	} else {
		log.Printf("gift %s: claim ok (+new user gift)", logfmt.Label(a.UID, a.Nickname))
	}
	s.markGiftTried(a.UID)
}

// giftTriedToday 该账号当日是否已尝试礼包领取。
func (s *Scheduler) giftTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.giftTried[uid] == giftDay(time.Now())
}

// markGiftTried 记录该账号当日已尝试礼包领取。
func (s *Scheduler) markGiftTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.giftTried[uid] = giftDay(time.Now())
}

// ProbeTokensNow 对全部非禁用账号做一次只读 token 探活（GET /claw/user/current）：
// 成功清 sessionDead 计数（误判复活路径），失败按 NoteSessionDead 连续计数。
// 供手动触发（cmd/smoke 同族的运维入口）与测试。
func (s *Scheduler) ProbeTokensNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.TokenValue() == "" {
			continue
		}
		if _, err := s.cfg.Upstream.UserCurrent(a); err != nil {
			log.Printf("probe %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			if upstream.IsSessionDead(err) {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: probe %s: 连续 %d 次 token 失效 — 禁用",
						logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 探活成功清误判计数
	}
}

// NextFireAt 导出 nextFire 的纯函数形态（测试/文档引用）。
func NextFireAt(now time.Time, hours []int) time.Time { return nextFire(now, hours) }
