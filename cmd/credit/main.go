// credit.go — RicheeAI 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//	go run ./cmd/credit -pretty
//
// 输出结构:
//
//	{"service":"richee","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","ok","error?"}]}
//
// 数据源 /credit/info（RicheeAI 单套餐口径，见 upstream.ResourceSummary）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"richee2api/internal/auth"
	"richee2api/internal/upstream"
)

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("RICHEE2API_AUTH_DIR"); v != "" {
		authDir = v
	} else if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	up := upstream.New()
	accounts := collect(authDir, up)
	printAccounts(accounts, pretty)
}

// collect 遍历 auths 目录并查询每个账号的积分摘要。供测试注入 upstream 断言。
func collect(authDir string, up *upstream.Client) []accountResult {
	files, _ := auth.LoadAuthFiles(authDir)
	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		res := accountResult{UID: a.UID, Nickname: a.Nickname}
		if a.Token == "" {
			res.Error = "no token"
			accounts = append(accounts, res)
			continue
		}
		remain, used, size, _, err := up.ResourceSummary(a)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Used = &used
			res.Size = &size
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}
	return accounts
}

// printAccounts 汇总并输出结果：-pretty 走人类可读日报，否则 JSON。
func printAccounts(accounts []accountResult, pretty bool) {
	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	if pretty {
		printPretty(accounts, totalRemain, totalSize, okCount)
		return
	}
	out := map[string]any{
		"service": "richee",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报。
func printPretty(accounts []accountResult, totalRemain, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 RicheeAI 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
