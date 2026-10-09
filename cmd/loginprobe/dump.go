package main

// dumpRawLogin performs the same serviceLoginAuth2 POST as Login() but prints
// the raw passport response for protocol-drift diagnosis.

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/mimo"
)

func dumpRawLogin(ctx context.Context, c *mimo.Client, phone, password string) {
	lctx, err := c.FetchLoginContext(ctx)
	if err != nil {
		fmt.Println("CTX-ERR:", err)
		return
	}
	form := url.Values{}
	form.Set("_json", "true")
	form.Set("callback", lctx.Callback)
	form.Set("sid", lctx.SID)
	form.Set("qs", lctx.QS)
	form.Set("_sign", lctx.Sign)
	form.Set("serviceParam", lctx.ServiceParam)
	form.Set("user", phone)
	sum := md5.Sum([]byte(password))
	form.Set("hash", strings.ToUpper(fmt.Sprintf("%x", sum)))
	form.Set("policyName", "miaccount")
	form.Set("captCode", "")
	form.Set("_group", lctx.Group)

	pu, _ := url.Parse(lctx.LoginURL)
	authURL := pu.Scheme + "://" + pu.Host + "/pass/serviceLoginAuth2"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN")
	resp, err := c.HTTPDo(req)
	if err != nil {
		fmt.Println("POST-ERR:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println("STATUS:", resp.Status)
	for k, v := range resp.Header {
		if strings.EqualFold(k, "Set-Cookie") || strings.EqualFold(k, "Content-Type") {
			fmt.Println("HDR", k, v)
		}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Println("BODY:", string(body))
	_ = time.Now
}
