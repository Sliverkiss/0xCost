// euiprobe: login via the EUI-encrypted form variant (mimo-web-api xiaomi-login.js
// equivalent) to test whether encrypted params avoid the UserCross risk gate.
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const rsaPubDERB64 = "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQCYEVrK/4Mahiv0pUJgTybx4J9P5dUT/Y0PuwMbk+gMU+jrZnBiXGv6/hCH1avIhoBcE535F8nJQQN3UavZdFkYidsoXuEnat3+eVTp3FslyhRwIBDF09v4vDhRtxFOT+R7uH7h/mzmyA2/+lfIMWGIrffXprYizbV76+YQKhoqFQIDAQAB"

const keyCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*"

const aesIV = "0102030405060708"

func randomKey16() string {
	b := make([]byte, 16)
	rand.Read(b)
	out := make([]byte, 16)
	for i := range out {
		out[i] = keyCharset[int(b[i])%len(keyCharset)]
	}
	return string(out)
}

func rsaEncryptBase64(plain string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(rsaPubDERB64)
	if err != nil {
		return "", err
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", err
	}
	rpub := pub.(*rsa.PublicKey)
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, rpub, []byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// aesEncryptPKCS7: AES-128-CBC with PKCS7 padding, base64 out.
func aesEncryptPKCS7(plain, key string) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append([]byte(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, []byte(aesIV)).CryptBlocks(ct, data)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func encryptParams(params map[string]string) (eui string, enc map[string]string, err error) {
	key := randomKey16()
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	fieldList := strings.Join(names, ",")
	kb64 := base64.StdEncoding.EncodeToString([]byte(key))
	rsaB64, err := rsaEncryptBase64(kb64)
	if err != nil {
		return "", nil, err
	}
	eui = rsaB64 + "." + base64.StdEncoding.EncodeToString([]byte(fieldList))
	enc = map[string]string{}
	for _, n := range names {
		v, err := aesEncryptPKCS7(params[n], key)
		if err != nil {
			return "", nil, err
		}
		enc[n] = v
	}
	return eui, enc, nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	phone, password := os.Args[1], os.Args[2]

	// 1. probe aistudio for loginUrl
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://aistudio.xiaomimimo.com/open-apis/user/mi/get", nil)
	if err != nil {
		panic(err)
	}
	probe.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	pc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	presp, err := pc.Do(probe)
	if err != nil {
		panic(err)
	}
	pb, _ := io.ReadAll(presp.Body)
	presp.Body.Close()
	var pj struct {
		LoginURL string `json:"loginUrl"`
	}
	json.Unmarshal(pb, &pj)
	if pj.LoginURL == "" {
		fmt.Println("NO loginUrl:", string(pb)[:200])
		return
	}
	lu, _ := url.Parse(pj.LoginURL)
	q := lu.Query()
	fmt.Println("loginUrl sid:", q.Get("sid"))

	// 2. EUI-encrypted serviceLoginAuth2
	eui, enc, err := encryptParams(map[string]string{"user": phone})
	if err != nil {
		panic(err)
	}
	sum := md5.Sum([]byte(password))
	form := url.Values{}
	form.Set("_json", "true")
	form.Set("callback", q.Get("callback"))
	form.Set("sid", q.Get("sid"))
	form.Set("qs", q.Get("qs"))
	form.Set("_sign", q.Get("_sign"))
	form.Set("serviceParam", q.Get("serviceParam"))
	form.Set("user", enc["user"])
	form.Set("hash", strings.ToUpper(hex.EncodeToString(sum[:])))
	form.Set("policyName", "miaccount")
	form.Set("captCode", "")
	form.Set("_group", q.Get("_group"))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://account.xiaomi.com/pass/serviceLoginAuth2", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Referer", "https://account.xiaomi.com/")
	req.Header.Set("EUI", eui)

	plain := &http.Client{Timeout: 30 * time.Second}
	resp, err := plain.Do(req)
	if err != nil {
		panic(err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	body := strings.TrimPrefix(string(b), "&&&START&&&")
	fmt.Println("STATUS:", resp.Status)
	fmt.Println("BODY:", trunc(body, 700))

	// passToken from cookies?
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "passToken=") {
			fmt.Println("HAS passToken cookie!")
		}
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
