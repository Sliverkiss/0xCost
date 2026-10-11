// ucprobe: live probe of the cross-region consent chain
// (/pass2/redirect → userinfo/start?bizType=UserCross → /fe/service/userCross
// → POST /pass2/userCrossAgreement/update → /pass2/userinfo/check?callback=…).
// See docs/login-chain-reverse.md §1 shape A. Manual redirect following only —
// sign/token/context ride on the intermediate Location headers.
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
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	phone, password := os.Args[1], os.Args[2]

	jar := map[string]string{}
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// plain client (follows nothing we don't ask)
	do := func(method, target string, form url.Values, extra map[string]string, follow bool) (*http.Response, string) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequestWithContext(ctx, method, target, body)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Accept-Language", "zh-CN")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		if ck := cookieStr(jar); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		client := &http.Client{Timeout: 30 * time.Second}
		if !follow {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Println("REQ-ERR:", err)
			os.Exit(1)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		for _, sc := range resp.Header.Values("Set-Cookie") {
			parts := strings.Split(sc, ";")
			kv := strings.SplitN(parts[0], "=", 2)
			if len(kv) == 2 && kv[1] != "" && kv[1] != "\"\"" {
				jar[kv[0]] = kv[1]
			}
		}
		return resp, string(b)
	}

	// ---- login: serviceLogin (phase0) + serviceLoginAuth2 ----
	resp0, b0 := do(http.MethodGet, "https://account.xiaomi.com/pass/serviceLogin?sid=xiaomichatbot&_json=true&_locale=zh_CN", nil, nil, false)
	fmt.Println("phase0:", resp0.Status)
	qs, sign0, cb := extract(b0, "qs"), extract(b0, "_sign"), extract(b0, "callback")
	fmt.Println("  qs:", trunc(qs, 40), "sign:", trunc(sign0, 30))

	form := url.Values{}
	form.Set("_json", "true")
	form.Set("callback", cb)
	form.Set("sid", "xiaomichatbot")
	form.Set("qs", qs)
	form.Set("_sign", sign0)
	form.Set("user", phone)
	sum := md5.Sum([]byte(password))
	form.Set("hash", fmt.Sprintf("%x", sum))
	form.Set("hash", strings.ToUpper(fmt.Sprintf("%x", sum)))
	form.Set("policyName", "miaccount")
	form.Set("captCode", "")
	form.Set("_locale", "zh_CN")

	respA, bA := do(http.MethodPost, "https://account.xiaomi.com/pass/serviceLoginAuth2", form,
		map[string]string{
			"Referer": "https://account.xiaomi.com/fe/service/login?sid=xiaomichatbot",
			"Origin":  "https://account.xiaomi.com",
		}, false)
	fmt.Println("auth2:", respA.Status)
	ntf := extract(bA, "notificationUrl")
	if ntf == "" {
		fmt.Println("NO notificationUrl. location:", extract(bA, "location"))
		fmt.Println(trunc(bA, 300))
		return
	}
	fmt.Println("ntf ok, jar:", keys(jar))

	// ---- P0: hop1 (pass2/redirect) — expect 302 to userinfo/start ----
	resp1, _ := do(http.MethodGet, ntf, nil, nil, false)
	loc1 := resp1.Header.Get("Location")
	fmt.Println("P0 hop1:", resp1.Status, trunc(loc1, 250))

	// ---- P1: hop2 (userinfo/start) — expect 302 to /fe/service/userCross WITH auth/callback/sign ----
	resp2, _ := do(http.MethodGet, loc1, nil, nil, false)
	loc2 := resp2.Header.Get("Location")
	fmt.Println("P1 hop2:", resp2.Status)
	fmt.Println("  FULL loc2:", loc2)
	u2, _ := url.Parse(loc2)
	q2 := u2.Query()
	fmt.Println("  hop2 path:", u2.Path)
	fmt.Println("  hop2 params:", keys2(q2))
	uid := q2.Get("userId")
	auth := q2.Get("auth")
	callback := q2.Get("callback")
	sign2 := q2.Get("sign")
	fmt.Println("  userId:", uid, "auth:", trunc(auth, 20), "callback:", trunc(callback, 60), "sign:", trunc(sign2, 30))

	if uid == "" || auth == "" {
		fmt.Println("!!! hop2 missing params — fetch the page and re-check")
		resp3, b3 := do(http.MethodGet, loc2, nil, nil, true)
		fmt.Println("page:", resp3.Status, len(b3))
		if u3, err := url.Parse(resp3.Request.URL.String()); err == nil {
			fmt.Println("final url:", trunc(u3.String(), 250))
		}
		return
	}

	// ---- P2: sendTicket (no captcha first) ----
	fullPhone := "+86" + phone
	st := url.Values{}
	st.Set("userId", uid)
	st.Set("phone", fullPhone)
	st.Set("icode", "")
	st.Set("recyclePhoneAllowed", "false")
	for _, ck := range []string{"passport_ph", "usercenter_ph", "passportsecurity_ph", "passportapi_ph", "passport_h5_ph", "passToken"} {
		st.Set(ck, jar[ck]) // empty passthrough like the SPA does
	}
	respS, bS := do(http.MethodPost, "https://account.xiaomi.com/pass2/repeatedPhone/sendTicket", st,
		map[string]string{"Referer": "https://account.xiaomi.com/fe/service/userCross?needClose=false&userId=" + uid}, false)
	fmt.Println("P2 sendTicket:", respS.Status)
	fmt.Println("  body:", trunc(strings.TrimPrefix(bS, "&&&START&&&"), 300))
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

func keys2(q url.Values) []string {
	var ks []string
	for k := range q {
		ks = append(ks, k)
	}
	return ks
}

func extract(body, field string) string {
	i := strings.Index(body, "\""+field+"\":")
	if i < 0 {
		return ""
	}
	rest := body[i+len(field)+3:]
	if strings.HasPrefix(rest, "\"") {
		end := strings.Index(rest[1:], "\"")
		if end < 0 {
			return ""
		}
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
