// Package auth 解析 RicheeAI auth 文件（扁平 {"token","userId","phone","nickName"}），
// 提供 token 轮换后的原子写回。
//
// 与 workbuddy2api 的差异（见 docs/PORT_NOTES.md）：
//   - RicheeAI 凭证是单个 `richee-token` header 值（uuid v4），无 accessToken /
//     refreshToken 双令牌、无 OAuth 设备流、无 device_token 风控头。
//   - 上游 expireAt 为 null：token 不过期，只有服务端撤销（业务 401）才失效；
//     NeedsRefresh 恒 false，失效判定走业务码而非时间。
//   - 单 realm：无 cn/global 双域，Realm() 恒 "richee"（保留方法以兼容池/调度层）。
package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"richee2api/internal/logfmt"
)

// Auth 是归一化后的 RicheeAI 账号凭证。
type Auth struct {
	// mu 串行化 token 写与读：token 轮换（重新登录落新 token）期间其他包
	// 不得读到半更新状态。所有出站头构造经 TokenValue() 取值。
	mu sync.Mutex

	Token    string // richee-token header 值（uuid v4）
	UID      string // userId
	Phone    string
	Nickname string
	FilePath string // 来源文件；token 轮换后原子写回此处
}

// Lock 供同进程内其他包在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// TokenValue 加锁读取 Token（出站 richee-token 头一律经此取值，勿直读字段）。
func (a *Auth) TokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Token
}

// Realm 返回账号域标识。RicheeAI 单上游单域，恒 "richee"。
// 保留方法签名以兼容池 / 调度层的调用形态。
func (a *Auth) Realm() string { return "richee" }

// IsGlobal 报告账号是否属于 global 域。RicheeAI 无双域，恒 false。
func (a *Auth) IsGlobal() bool { return false }

// NeedsRefresh 报告 token 是否将过期。RicheeAI token 不过期（expireAt:null），
// 恒 false；失效判定走业务 401（池 disable + 重登），不走时间。
func (a *Auth) NeedsRefresh(within time.Duration) bool { return false }

// Parse 解析磁盘形态：{"token":"<uuid>","userId":"...","phone":"...","nickName":"..."}。
// token 必填（空白视为缺失）；userId/phone/nickName 可缺席（token-only 文件
// 由 user/current 探测补齐 uid/nickName）。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var f struct {
		Token    string `json:"token"`
		UserID   string `json:"userId"`
		Phone    string `json:"phone"`
		Nickname string `json:"nickName"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if strings.TrimSpace(f.Token) == "" {
		return nil, fmt.Errorf("parse_error: missing token")
	}
	return &Auth{
		Token:    f.Token,
		UID:      f.UserID,
		Phone:    f.Phone,
		Nickname: f.Nickname,
	}, nil
}

// SaveAtomic 以扁平形原子写回 FilePath（tmp + rename）。
// 全程持 a.mu：防止与 token 轮换并发，杜绝写回半更新。
// 防御：token 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.Token) == "" {
		return fmt.Errorf("save refused: empty token (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"token": a.Token,
	}
	if a.UID != "" {
		doc["userId"] = a.UID
	}
	if a.Phone != "" {
		doc["phone"] = a.Phone
	}
	if a.Nickname != "" {
		doc["nickName"] = a.Nickname
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：richee*.json）。
// 网关 LoadDir 与 cmd 运维工具（signin/credit/trial）共用此单一来源。
const AuthFileGlob = "richee*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
// 供 cmd 运维工具复用：只列文件、不解析（LoadDir 才有副作用）。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 richee*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 同 UID 出现在多个文件时打 WARN（含两文件路径），由「后载入者胜出」保持现状行为
// （去重发生在 pool.SyncToDir 的 UID upsert）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		out = append(out, a)
	}
	return out, nil
}
