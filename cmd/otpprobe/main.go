// otpprobe: step-by-step live dump of the passport new-device OTP flow,
// printing status + body at every hop to locate protocol drift.
package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/mimo"
)

func main() {
	c := mimo.New(mimo.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	jar := map[string]string{}
	do := func(name, method, target string, form url.Values, extra map[string]string) (string, string) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequestWithContext(ctx, method, target, body)
		req.Header.Set("User-Agent", "MiClaw/1.0")
		req.Header.Set("Accept-Language", "zh-CN")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		if ck := cookieStr(jar); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		manual := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
		_ = manual
		resp, err := c.HTTPDoManual(req)
		if err != nil {
			fmt.Printf("### %s: ERR %v\n", name, err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		for _, sc := range resp.Header.Values("Set-Cookie") {
			parts := strings.Split(sc, ";")
			kv := strings.SplitN(parts[0], "=", 2)
			if len(kv) == 2 {
				jar[kv[0]] = kv[1]
			}
		}
		fmt.Printf("### %s: %s\n%s\n\n", name, resp.Status, trunc(string(b), 600))
		return string(b), resp.Header.Get("Location")
	}

	lctx, err := c.FetchLoginContext(ctx)
	if err != nil {
		fmt.Println("CTX-ERR:", err)
		os.Exit(1)
	}
	pu, _ := url.Parse(lctx.LoginURL)
	passBase := pu.Scheme + "://" + pu.Host

	form := url.Values{}
	form.Set("_json", "true")
	form.Set("callback", lctx.Callback)
	form.Set("sid", lctx.SID)
	form.Set("qs", lctx.QS)
	form.Set("_sign", lctx.Sign)
	form.Set("serviceParam", lctx.ServiceParam)
	form.Set("user", os.Args[1])
	sum := md5.Sum([]byte(os.Args[2]))
	form.Set("hash", strings.ToUpper(fmt.Sprintf("%x", sum)))
	form.Set("policyName", "miaccount")
	form.Set("captCode", "")
	form.Set("_group", lctx.Group)

	do("step0-serviceLogin-GET", http.MethodGet, lctx.LoginURL+"&_json=true&_locale=zh_CN", nil, nil)
	authBody, _ := do("serviceLoginAuth2", http.MethodPost, passBase+"/pass/serviceLoginAuth2", form,
		map[string]string{
			"Referer": passBase + "/fe/service/login?sid=" + url.QueryEscape(lctx.SID),
			"Origin":  passBase,
		})

	ntf := extractStr(authBody, "notificationUrl")
	if ntf == "" {
		fmt.Println("NO notificationUrl — login result:", trunc(authBody, 300))
		return
	}
	ntu, _ := url.Parse(ntf)
	sid := ntu.Query().Get("sid")
	otpCtx := ntu.Query().Get("context")
	fmt.Println("NTF sid:", sid, "context:", trunc(otpCtx, 40))
	fmt.Println("jar keys:", keys(jar))

	// cpa-plugin style: FRESH jar, new deviceId, sdkVersion=3.9, Android UA,
	// X-Requested-With on every request.
	{
		otpJar := map[string]string{
			"deviceId":   strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", time.Now().UnixNano(), os.Getpid(), time.Now().UnixMicro(), 0x1234, 0x5678abcd)),
			"sdkVersion": "3.9",
		}
		doOTP := func(name, method, target string, form url.Values) string {
			var body io.Reader
			if form != nil {
				body = strings.NewReader(form.Encode())
			}
			req, _ := http.NewRequestWithContext(ctx, method, target, body)
			req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36 MiClaw/1.0")
			req.Header.Set("Accept", "application/json, text/html")
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			if form != nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			if ck := cookieStr(otpJar); ck != "" {
				req.Header.Set("Cookie", ck)
			}
			resp, err := c.HTTPDo(req)
			if err != nil {
				fmt.Printf("### %s: ERR %v\n", name, err)
				os.Exit(1)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			for _, sc := range resp.Header.Values("Set-Cookie") {
				parts := strings.Split(sc, ";")
				kv := strings.SplitN(parts[0], "=", 2)
				if len(kv) == 2 {
					otpJar[kv[0]] = kv[1]
				}
			}
			fmt.Printf("### OTP-%s: %s\n%s\n\n", name, resp.Status, trunc(string(b), 400))
			return string(b)
		}
		_ = doOTP

		ntu, _ := url.Parse(ntf)
		sidParam := ntu.Query().Get("sid")
		if sidParam == "" {
			sidParam = "xiaomichatbot"
		}
		otpCtxParam := ntu.Query().Get("context")

		doOTP("step1-open", http.MethodGet, ntf, nil)
		doOTP("step2-identity-list", http.MethodGet,
			"https://account.xiaomi.com/identity/list?sid="+url.QueryEscape(sidParam)+"&supportedMask=0&_locale=zh_CN&context="+url.QueryEscape(otpCtxParam), nil)
		doOTP("step3-verify-trigger", http.MethodGet,
			"https://account.xiaomi.com/identity/auth/verifyPhone?_flag=4&_json=true", nil)
		sendForm := url.Values{}
		sendForm.Set("retry", "0")
		sendForm.Set("icode", "")
		sendForm.Set("_json", "true")
		doOTP("step4-send-ticket", http.MethodPost,
			"https://account.xiaomi.com/identity/auth/sendPhoneTicket?_dc="+fmt.Sprint(time.Now().UnixMilli()), sendForm)
	}

}

func cookieStr(jar map[string]string) string {
	var ps []string
	for k, v := range jar {
		ps = append(ps, k+"="+v)
	}
	return strings.Join(ps, "; ")
}

func keys(jar map[string]string) []string {
	var ks []string
	for k := range jar {
		ks = append(ks, k)
	}
	return ks
}

func extractStr(body, field string) string {
	i := strings.Index(body, "\""+field+"\":")
	if i < 0 {
		return ""
	}
	rest := body[i+len(field)+3:]
	if strings.HasPrefix(rest, "\"") {
		end := strings.Index(rest[1:], "\"")
		return rest[1 : 1+end]
	}
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
