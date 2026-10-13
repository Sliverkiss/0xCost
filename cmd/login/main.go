// login.go — RicheeAI SMS 验证码登录 CLI（两段子命令，login.sh 顺序驱动）。
//
//	login send <phone>            → POST /claw/auth/verify-code/send，stdout 打印结果行
//	login login <phone> <code>    → POST /claw/auth/login，成功后补查 user/current
//	                                 拿 nickName，stdout 打印完整凭证 JSON
//	                                 {"token","userId","nickName","phone"}（login.sh 落盘）
//	login geetest                 → POST /claw/auth/geetest/init（>3 次/日发送时的挑战参数）
//
// 交互模式（login.sh 无参数调用）：
//
//	login                        → 提示输入手机号 → send → 提示输入验证码 → login → 打印凭证
//
// 与 workbuddy2api 的差异（见 docs/PORT_NOTES.md）：OAuth 设备流 → SMS code 流；
// 无 realm 双域；geetest 字段可选（每号每天前 3 次发送跳过校验）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"richee2api/internal/auth"
	"richee2api/internal/upstream"
)

// exitFunc 供测试替换（默认 os.Exit）。
var exitFunc = os.Exit

// stdin/stdout 注入点（测试用）。
var stdin io.Reader = os.Stdin
var stdout io.Writer = os.Stdout

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

func main() {
	up := upstream.New()
	args := os.Args[1:]
	switch {
	case len(args) >= 2 && args[0] == "send":
		if err := up.SendVerifyCode(args[1], upstream.GeetestFields{}); err != nil {
			fatal("send code failed: %v", err)
		}
		fmt.Fprintln(stdout, "验证码已发送（请查收短信）")

	case len(args) >= 3 && args[0] == "login":
		printCredential(up, args[1], args[2])

	case len(args) == 1 && args[0] == "geetest":
		gi, err := up.GeetestInit()
		if err != nil {
			fatal("geetest init failed: %v", err)
		}
		raw, _ := json.Marshal(gi)
		fmt.Fprintln(stdout, string(raw))

	case len(args) == 0:
		interactive(up)

	default:
		fatal("usage: login [send <phone> | login <phone> <code> | geetest]")
	}
}

// interactive 交互模式：提示手机号 → 发码 → 提示验证码 → 登录 → 打印凭证。
func interactive(up *upstream.Client) {
	in := bufio.NewReader(stdin)
	phone, err := in.ReadString('\n')
	if err != nil && phone == "" {
		fatal("读取手机号失败（非交互模式请用: login send <phone>）")
	}
	phone = strings.TrimSpace(phone)
	if phone == "" {
		fatal("手机号不能为空")
	}
	if err := up.SendVerifyCode(phone, upstream.GeetestFields{}); err != nil {
		fatal("send code failed: %v（若提示需要人机验证，请明日再试或用 geetest 子命令）", err)
	}
	fmt.Fprintln(stdout, "验证码已发送，请查收短信")

	fmt.Fprint(os.Stderr, "输入验证码: ")
	code, err := in.ReadString('\n')
	if err != nil && code == "" {
		fatal("读取验证码失败")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		fatal("验证码不能为空")
	}
	printCredential(up, phone, code)
}

// printCredential 登录并补齐 uid/nickName（token-only 响应经 user/current 探测），
// stdout 打印完整凭证 JSON（login.sh 落盘 auths/richee-<uid>.json）。
func printCredential(up *upstream.Client, phone, code string) {
	a, err := up.LoginWithCode(phone, code)
	if err != nil {
		fatal("login failed: %v", err)
	}
	// uid/nickName 缺失时经 user/current 补齐（token 已有效）。
	if a.UID == "" || a.Nickname == "" {
		if u, uerr := up.UserCurrent(a); uerr == nil && u != nil {
			if a.UID == "" {
				a.UID = u.UserID
			}
			if a.Nickname == "" {
				a.Nickname = u.NickName
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"token":    a.Token,
		"userId":   a.UID,
		"nickName": a.Nickname,
		"phone":    a.Phone,
	})
	fmt.Fprintln(stdout, string(raw))
}

// SaveTo 把凭证落到 auths/richee-<uid>.json（幂等覆盖；uid 缺席时用 phone 派生）。
// 供 login.sh 之外的程序化调用与测试。
func SaveTo(dir string, a *auth.Auth) (string, error) {
	uid := a.UID
	if uid == "" {
		uid = "unknown-" + a.Phone
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := dir + "/richee-" + uid + ".json"
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return path, nil
}
