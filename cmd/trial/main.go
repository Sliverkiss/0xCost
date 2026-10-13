// trial 一次性批量礼包/试用探测：遍历 auths 下全部账号，
// 查询 gift/status（新用户礼包状态）并领取 canClaim=true 的礼包。
//
// 结果逐账号输出到 stdout：
//
//	uid | nick | status | detail
//	----+------+--------+-------
//	... | OK | gift claimed
//	... | ALREADY | 已领取过（幂等，不算失败）
//	... | FAIL | ...
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"richee2api/internal/auth"
	"richee2api/internal/logfmt"
	"richee2api/internal/upstream"
)

// trialStatus 单账号礼包领取结果状态。
type trialStatus string

const (
	trialOK      trialStatus = "OK"
	trialAlready trialStatus = "ALREADY"
	trialFailed  trialStatus = "FAIL"
)

type trialRow struct {
	uid    string
	nick   string
	status trialStatus
	detail string
}

// classifyTrial 归一化 ClaimGift 结果（纯函数，供 main 循环与测试直接断言）：
// err → FAIL；claimed → OK；否则（已领取幂等）→ ALREADY。
func classifyTrial(claimed bool, err error) (trialStatus, string) {
	switch {
	case err != nil:
		return trialFailed, err.Error()
	case claimed:
		return trialOK, "gift claimed"
	default:
		return trialAlready, "already claimed (idempotent)"
	}
}

func main() {
	authDir := "auths"
	if len(os.Args) > 1 {
		authDir = os.Args[1]
	}
	files, err := auth.LoadAuthFiles(authDir)
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", authDir)
		os.Exit(1)
	}
	up := upstream.New()
	var rows []trialRow
	for _, f := range files {
		r := trialRow{uid: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = trialFailed, "load: "+err.Error()
			rows = append(rows, r)
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = trialFailed, "parse: "+err.Error()
			rows = append(rows, r)
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// 先查状态：canClaim=true 才调 claim（幂等写，不轰炸上游）。
		gs, err := up.GiftStatus(a)
		if err != nil {
			r.status, r.detail = trialFailed, "status: "+err.Error()
			rows = append(rows, r)
			continue
		}
		if gs == nil || !gs.CanClaim {
			r.status, r.detail = trialAlready, "no claimable gift"
			rows = append(rows, r)
			continue
		}
		claimed, err := up.ClaimGift(a)
		r.status, r.detail = classifyTrial(claimed, err)
		rows = append(rows, r)
	}

	var okN, alreadyN, failN int
	fmt.Printf("uid                                  | nick        | status  | detail\n")
	fmt.Printf("-------------------------------------+-------------+---------+------------------------------\n")
	for _, r := range rows {
		fmt.Printf("%-36s | %-11s | %-7s | %s\n",
			logfmt.Truncate(r.uid, 36), logfmt.Truncate(r.nick, 11), r.status, r.detail)
		switch r.status {
		case trialOK:
			okN++
		case trialAlready:
			alreadyN++
		default:
			failN++
		}
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}
