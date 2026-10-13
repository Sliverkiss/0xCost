// signin 一次性批量签到工具：遍历 ./auths/richee*.json 全部账号，
// 逐个调 userSign/create，顺手查积分。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"richee2api/internal/auth"
	"richee2api/internal/logfmt"
	"richee2api/internal/upstream"
)

type row struct {
	file     string
	uid      string
	nick     string
	status   string // OK | ALREADY | FAIL | LOAD_ERR | DEAD_TOKEN
	detail   string
	remain   int64
	hasQuota bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, err := auth.LoadAuthFiles(dir)
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	up := upstream.New()

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		r := row{file: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		err = up.DailyCheckin(a)
		r.status = checkinStatusOf(err)
		if err != nil {
			r.detail = short(err.Error())
		}
		switch r.status {
		case "OK":
			okN++
		case "ALREADY":
			alreadyN++
		default:
			failN++
		}
		// 顺手查积分（token 失效时也查——查询失败不计入签到结果）。
		if remain, qerr := up.UserResource(a); qerr == nil {
			r.remain, r.hasQuota = remain, true
		}
		rows = append(rows, r)
	}

	// 报告
	fmt.Printf("uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			logfmt.Truncate(r.uid, 36), logfmt.Truncate(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

// isDeadToken 报告 err 是否 token 失效（业务 401）。
func isDeadToken(err error) bool {
	return upstream.IsSessionDead(err)
}

// checkinStatusOf DailyCheckin 结果的归一化状态（纯函数，供 main 循环与测试直接断言）：
// 成功 → OK；幂等码（已签到）→ ALREADY；token 失效 → DEAD_TOKEN（需重登）；其余 → FAIL。
func checkinStatusOf(err error) string {
	if err == nil {
		return "OK"
	}
	if isDeadToken(err) {
		return "DEAD_TOKEN"
	}
	if upstream.IsAlreadyCheckin(err) {
		return "ALREADY"
	}
	return "FAIL"
}

// idempotentMarkers 幂等/不适用文案关键词。
var idempotentMarkers = []string{"已签到", "已经签到", "重复签到", "already"}

// bareMarkers 裸错误（非 *upstream.Error，网络/解析层）回退匹配用的中文文案子集。
// 英文短词在传输层错误文本里太常见，不得对裸错误启用。
var bareMarkers = []string{"已签到", "已经签到", "未开启", "未开放"}

// short 错误详情截断（表格单元格宽度）。
func short(s string) string {
	s = replaceNewlines(s)
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func replaceNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, ' ')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
