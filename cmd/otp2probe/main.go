package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	// step0+1: get auth2 response using sid from login probe
	sid := "xiaomichatbot"
	if v := os.Getenv("SID"); v != "" {
		sid = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	jar := map[string]string{}
	do := func(name, method, target string, form url.Values) string {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequestWithContext(ctx, method, target, body)
		req.Header.Set("User-Agent", "MiClaw/1.0")
		req.Header.Set("Accept", "application/json, text/html")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if ck := cookieStr(jar); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		follow := &http.Client{Timeout: 30 * time.Second}
		resp, err := follow.Do(req)
		if err != nil {
			fmt.Printf("### %s ERR: %v\n", name, err)
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
		fmt.Printf("### %s: %s\n%s\n\n", name, resp.Status, trunc(string(b), 400))
		return string(b)
	}

	p0 := do("phase0-serviceLogin", http.MethodGet,
		"https://account.xiaomi.com/pass/serviceLogin?sid="+url.QueryEscape(sid)+"&_json=true&_locale=zh_CN", nil)

	// parse qs/_sign/callback
	qs := extract(p0, "qs")
	sign := extract(p0, "_sign")
	callback := extract(p0, "callback")

	form := url.Values{}
	form.Set("sid", sid)
	form.Set("callback", callback)
	if strings.TrimSpace(qs) != "" {
		form.Set("qs", qs)
	} else {
		form.Set("qs", "?sid="+sid+"&_json=true")
	}
	form.Set("user", os.Args[1])
	sum := md5.Sum([]byte(os.Args[2]))
	form.Set("hash", strings.ToUpper(hex.EncodeToString(sum[:])))
	form.Set("_json", "true")
	form.Set("_locale", "zh_CN")
	if strings.TrimSpace(sign) != "" {
		form.Set("_sign", sign)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://account.xiaomi.com/pass/serviceLoginAuth2", strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", "MiClaw/1.0")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://account.xiaomi.com/fe/service/login?sid="+url.QueryEscape(sid))
	req.Header.Set("Origin", "https://account.xiaomi.com")
	plain := &http.Client{Timeout: 30 * time.Second}
	resp, err := plain.Do(req)
	if err != nil {
		fmt.Println("auth2 ERR:", err)
		os.Exit(1)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	authBody := string(b)
	fmt.Println("### auth2:", trunc(authBody, 300))

	ntf := extract(authBody, "notificationUrl")
	if ntf == "" {
		fmt.Println("NO NTF — direct login? location:", extract(authBody, "location"))
		return
	}

	// OTP flow with fresh jar + deviceId/sdkVersion cookies
	loginJar := map[string]string{}
	for k, v := range jar {
		loginJar[k] = v
	}
	otpJar := map[string]string{
		"deviceId":   strings.ToUpper(newSessionID()),
		"sdkVersion": "3.9",
	}
	_ = otpJar
	fmt.Println("jar at OTP start: FRESH (deviceId+sdkVersion)")
	jar = otpJar // switch to the fresh OTP jar

	// reuse same main jar for OTP (cpa uses a fresh jar + explicit deviceId cookie)
	ntu, _ := url.Parse(ntf)
	sidParam := sid // use the SERVICE sid (xiaomichatbot), not the passport sid
	otpCtx := ntu.Query().Get("context")
	fmt.Println("OTP using sid:", sidParam, "context:", trunc(otpCtx, 50))

	{
		// trace redirect chain with fresh jar, printing each hop URL
		next := ntf
		for hop := 0; hop < 6 && next != ""; hop++ {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
			req.Header.Set("User-Agent", "MiClaw/1.0")
			req.Header.Set("Accept", "application/json, text/html")
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
			if ck := cookieStr(jar); ck != "" {
				req.Header.Set("Cookie", ck)
			}
			nr := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := nr.Do(req)
			if err != nil {
				fmt.Println("chain err:", err)
				break
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			for _, sc := range resp.Header.Values("Set-Cookie") {
				parts := strings.Split(sc, ";")
				kv := strings.SplitN(parts[0], "=", 2)
				if len(kv) == 2 {
					jar[kv[0]] = kv[1]
				}
			}
			fmt.Printf("### chain%d: %s loc=%s len=%d\n", hop, resp.Status, trunc(resp.Header.Get("Location"), 250), len(b))
			loc := resp.Header.Get("Location")
			if loc == "" {
				break
			}
			if !strings.HasPrefix(loc, "http") {
				loc = "https://account.xiaomi.com" + loc
			}
			next = loc
		}
	}
	do("otp1-open", http.MethodGet, ntf, nil)
	do("otp2-list", http.MethodGet,
		"https://account.xiaomi.com/identity/list?sid="+url.QueryEscape(sidParam)+"&supportedMask=0&_locale=zh_CN&context="+url.QueryEscape(otpCtx), nil)
	do("otp3-verify", http.MethodGet,
		"https://account.xiaomi.com/identity/auth/verifyPhone?_flag=4&_json=true", nil)
	sendForm := url.Values{}
	sendForm.Set("retry", "0")
	sendForm.Set("icode", "")
	sendForm.Set("_json", "true")
	do("otp4-send", http.MethodPost,
		"https://account.xiaomi.com/identity/auth/sendPhoneTicket?_dc="+fmt.Sprint(time.Now().UnixMilli()), sendForm)
	// ---- Retry auth2 with the jar that has now VISITED the UserCross chain ----
	{
		req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://account.xiaomi.com/pass/serviceLoginAuth2", strings.NewReader(form.Encode()))
		req2.Header.Set("User-Agent", "MiClaw/1.0")
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req2.Header.Set("Referer", "https://account.xiaomi.com/fe/service/login?sid="+url.QueryEscape(sid))
		req2.Header.Set("Origin", "https://account.xiaomi.com")
		// loginJar carried passToken from auth2 Set-Cookie; the chain was visited with otpJar...
		// use the merged jar
		merged := map[string]string{}
		for k, v := range loginJar {
			merged[k] = v
		}
		for k, v := range jar {
			merged[k] = v
		}
		if ck := cookieStr(merged); ck != "" {
			req2.Header.Set("Cookie", ck)
		}
		p := &http.Client{Timeout: 30 * time.Second}
		resp2, err := p.Do(req2)
		if err == nil {
			b2, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<20))
			resp2.Body.Close()
			fmt.Println("### auth2-RETRY:", trunc(string(b2), 400))
		}
	}

}

func cookieStr(jar map[string]string) string {
	var ps []string
	for k, v := range jar {
		ps = append(ps, k+"="+v)
	}
	return strings.Join(ps, "; ")
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
		v := rest[1 : 1+end]
		if strings.HasPrefix(v, "\\u") {
			return ""
		}
		return v
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
