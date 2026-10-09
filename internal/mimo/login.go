package mimo

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// XSSI prefix on all passport JSON responses (SDD §2.2).
const xssiPrefix = "&&&START&&&"

// LoginContext is the parsed loginUrl parameter set required by
// serviceLoginAuth2 (SDD §2.2 step 1, TDD LOGIN-001).
type LoginContext struct {
	LoginURL     string
	Callback     string
	SID          string
	QS           string
	Sign         string
	ServiceParam string
	Group        string
}

// Session is a derived upstream session (mirrors accounts.Session shape,
// SDD §2.1/§2.3).
type Session struct {
	Cookie    string // full Cookie header value for upstream calls
	PH        string // xiaomichatbot_ph value
	UserID    string
	PassToken string
}

// parseXSSI strips the &&&START&&& prefix and decodes the JSON body. Bodies
// without the exact prefix (or with garbage before it) are rejected
// (TDD LOGIN-003).
func parseXSSI(body []byte) (map[string]any, error) {
	s := string(body)
	i := strings.Index(s, xssiPrefix)
	if i < 0 {
		return nil, fmt.Errorf("passport response missing %s prefix: %.120s", xssiPrefix, s)
	}
	if strings.TrimSpace(s[:i]) != "" {
		return nil, fmt.Errorf("garbage before %s prefix: %.60s", xssiPrefix, s[:i])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s[i+len(xssiPrefix):]), &m); err != nil {
		return nil, fmt.Errorf("passport response not JSON after prefix: %w", err)
	}
	return m, nil
}

// ErrAccountAction marks login outcomes that need a human: captcha,
// new-device OTP (SDD §2.2, TDD LOGIN-004/005). No auto-solve — the account
// is disabled with the reason. Cause carries the underlying sentinel (if any)
// so errors.Is keeps working through the "needs a human" wrapper.
type ErrAccountAction struct {
	Reason string
	URL    string
	Cause  error
}

func (e *ErrAccountAction) Error() string { return e.Reason }

// Unwrap exposes the underlying cause (e.g. ErrCrossRegionConsent).
func (e *ErrAccountAction) Unwrap() error { return e.Cause }

// hostAllowed checks a hostname against an exact-match allowlist.
func hostAllowed(host string, allow []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allow {
		if host == strings.ToLower(a) {
			return true
		}
	}
	return false
}

// parseLoginURL validates the loginUrl host against the passport allowlist
// and extracts callback/sid/qs/_sign/serviceParam/_group (TDD
// LOGIN-001/008).
func (c *Client) parseLoginURL(raw string) (*LoginContext, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("loginUrl parse: %w", err)
	}
	if u.Hostname() != "" && !hostAllowed(u.Hostname(), c.passportHosts) {
		return nil, fmt.Errorf("loginUrl host %q not in allowlist", u.Hostname())
	}
	q := u.Query()
	return &LoginContext{
		LoginURL:     raw,
		Callback:     q.Get("callback"),
		SID:          q.Get("sid"),
		QS:           q.Get("qs"),
		Sign:         q.Get("_sign"),
		ServiceParam: q.Get("serviceParam"),
		Group:        q.Get("_group"),
	}, nil
}

// fetchLoginContext probes GET /open-apis/user/mi/get and extracts the
// loginUrl parameters (SDD §2.2 step 1, TDD LOGIN-001).
func (c *Client) fetchLoginContext(ctx context.Context) (*LoginContext, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/open-apis/user/mi/get", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "zh-CN")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login context probe: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// The 401 body carries loginUrl as plain JSON (aistudio dialect).
	var probe struct {
		LoginURL string `json:"loginUrl"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.LoginURL != "" {
		return c.parseLoginURL(probe.LoginURL)
	}
	// Some error bodies are XSSI-prefixed (passport dialect).
	if m, err := parseXSSI(body); err == nil {
		if u, _ := m["loginUrl"].(string); u != "" {
			return c.parseLoginURL(u)
		}
	}
	return nil, errors.New("login context: no loginUrl in probe response")
}

// Login performs the full password login (SDD §2.2 steps 1–3):
// context probe → POST /pass/serviceLoginAuth2 (form-encoded, hash =
// MD5(password).hex().toUpperCase(), no salt) → follow location
// (host-allowlisted) → collect Set-Cookie into a session. Returns
// ErrAccountAction for captcha/OTP demands (TDD LOGIN-002..009).
func (c *Client) Login(ctx context.Context, phone, password string) (*Session, error) {
	lctx, err := c.fetchLoginContext(ctx)
	if err != nil {
		return nil, err
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

	pu, err := url.Parse(lctx.LoginURL)
	if err != nil {
		return nil, err
	}
	authURL := pu.Scheme + "://" + pu.Host + "/pass/serviceLoginAuth2"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "zh-CN")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serviceLoginAuth2: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	jar := collectCookies(resp)
	m, err := parseXSSI(body)
	if err != nil {
		return nil, err
	}
	code, _ := m["code"].(float64)
	if code != 0 {
		desc, _ := m["description"].(string)
		if cu, ok := m["captchaUrl"].(string); ok && cu != "" {
			return nil, &ErrAccountAction{Reason: "login requires slider captcha; manual re-login needed", URL: cu}
		}
		if nu, ok := m["notificationUrl"].(string); ok && nu != "" {
			return nil, &ErrAccountAction{Reason: "login requires new-device verification (OTP); manual re-login needed", URL: nu}
		}
		return nil, fmt.Errorf("login failed: code=%d %s", int64(code), desc)
	}
	loc, _ := m["location"].(string)
	if loc == "" {
		// Live-captured protocol gap (2026-10-08): the new-device OTP
		// demand also arrives with code=0 — securityStatus=128, empty
		// location, notificationUrl set. Auto-start the challenge; the
		// code is delivered to the account owner's phone, so completion
		// happens out-of-band via SubmitOTP (SDD §2.2 amendment).
		if nu, ok := m["notificationUrl"].(string); ok && nu != "" {
			if !c.disableOTPAuto {
				ch, cerr := c.StartOTP(ctx, nu, lctx.SID, jar)
				if cerr == nil {
					return nil, &ErrOTPPending{Challenge: ch}
				}
				if errors.Is(cerr, ErrCrossRegionConsent) {
					return nil, &ErrAccountAction{
						Reason: "login demands the cross-region data-transfer agreement: agree once in a browser from this egress IP, or set MIMO2API_CROSS_REGION_AGREE=true",
						URL:    nu,
						Cause:  cerr,
					}
				}
				return nil, &ErrAccountAction{Reason: "new-device OTP verification required (auto-start failed: " + cerr.Error() + ")", URL: nu, Cause: cerr}
			}
			return nil, &ErrAccountAction{Reason: "new-device OTP verification required", URL: nu}
		}
		return nil, errors.New("login succeeded but no location for STS exchange")
	}
	userID := ""
	if v, ok := m["userId"].(float64); ok {
		userID = fmt.Sprintf("%d", int64(v))
	} else if v, ok := m["userId"].(string); ok {
		userID = v
	}
	if userID == "" {
		userID = jar["userId"]
	}
	passToken := jar["passToken"]
	if userID == "" || passToken == "" {
		return nil, errors.New("login response missing userId/passToken — protocol drift")
	}

	sess, err := c.stsExchange(ctx, loc)
	if err != nil {
		return nil, err
	}
	sess.UserID = userID
	sess.PassToken = passToken
	return sess, nil
}

// stsExchange follows a one-time STS location (host must be the aistudio
// upstream, account.xiaomi.com, or another allowlisted passport host; TDD
// LOGIN-008) and assembles the working cookie string (SDD §2.2 step 3 /
// §2.3 step 3).
func (c *Client) stsExchange(ctx context.Context, location string) (*Session, error) {
	return c.stsExchangeWithJar(ctx, location, nil)
}

// stsExchangeWithJar walks the whole landing chain, seeding it with the
// cookies the caller already holds. Live shape (2026-10-09):
//
//	/identity/result/check → 302 → /pass/serviceLoginAuth2/end → 302 →
//	<upstream>/sts?sign=… → 307 → /open-apis/user/mi/get → 200
//
// and the xiaomichatbot_* session cookies are planted at the UPSTREAM hop —
// following only the first hop (the old behaviour) lands on an intermediate
// 302 and misses them entirely.
func (c *Client) stsExchangeWithJar(ctx context.Context, location string, seed map[string]string) (*Session, error) {
	// Collect into the caller's jar: the chain plants passToken/cUserId and
	// the caller (the OTP challenge) needs them afterwards. Copying into a
	// throwaway map is what kept the harvested passToken permanently empty.
	jar := seed
	if jar == nil {
		jar = map[string]string{}
	}
	hops, _, err := c.followChain(ctx, jar, location, maxChainHops)
	if err != nil {
		return nil, fmt.Errorf("sts exchange: %w", err)
	}
	if len(hops) == 0 {
		return nil, errors.New("sts exchange: empty chain")
	}
	token := unquoteCookie(jar["xiaomichatbot_serviceToken"])
	if token == "" {
		return nil, errors.New("sts response did not plant xiaomichatbot_serviceToken")
	}
	return c.sessionFromJar(jar, token), nil
}

// sessionFromJar composes the working upstream Cookie header from the chain's
// cookies, in the real browser's order and quoting (live capture 2026-10-09:
// cUserId; userId; xiaomichatbot_serviceToken/slh/ph quoted). The unquoted
// `serviceToken=` spelling is kept because the upstream accepts either form
// and older captures rely on it.
func (c *Client) sessionFromJar(jar map[string]string, token string) *Session {
	ph := unquoteCookie(jar["xiaomichatbot_ph"])
	slh := unquoteCookie(jar["xiaomichatbot_slh"])
	parts := []string{}
	if v := jar["cUserId"]; v != "" {
		parts = append(parts, "cUserId="+v)
	}
	if v := jar["userId"]; v != "" {
		parts = append(parts, "userId="+v)
	}
	parts = append(parts, "serviceToken="+token)
	parts = append(parts, `xiaomichatbot_serviceToken="`+token+`"`)
	if slh != "" {
		parts = append(parts, `xiaomichatbot_slh="`+slh+`"`)
	}
	if ph != "" {
		parts = append(parts, `xiaomichatbot_ph="`+ph+`"`)
	}
	return &Session{Cookie: strings.Join(parts, "; "), PH: ph}
}

// unquoteCookie strips the surrounding quotes passport cookies carry.
func unquoteCookie(v string) string {
	v = strings.TrimSpace(v)
	for len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return v
}

// collectCookies turns Set-Cookie headers into a name→value map.
func collectCookies(resp *http.Response) map[string]string {
	out := map[string]string{}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		parts := strings.SplitN(sc, ";", 2)
		if len(parts) == 0 {
			continue
		}
		head := parts[0]
		eq := strings.IndexByte(head, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(head[:eq])
		val := strings.TrimSpace(head[eq+1:])
		if val == "" || val == `""` {
			continue
		}
		out[name] = val
	}
	return out
}

// ErrPassTokenDead: code 70016 — the passToken can no longer mint sessions;
// the account needs a full re-login (SDD §2.3, TDD RENEW-003).
type ErrPassTokenDead struct{ Description string }

func (e *ErrPassTokenDead) Error() string {
	return "passToken expired (code 70016): " + e.Description
}

// renew refreshes the serviceToken from the passToken via STS (SDD §2.3):
// probe /open-apis/user/mi/get with userId only → extract loginUrl → GET
// loginUrl&_json=true with Cookie userId+passToken → follow location →
// fresh cookies (TDD RENEW-001/002).
func (c *Client) Renew(ctx context.Context, userID, passToken string) (*Session, error) {
	// Step 1: probe for the official loginUrl (SDD §2.3 step 1).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/open-apis/user/mi/get", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Cookie", "userId="+userID+";")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("renewal probe: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	loginURL := ""
	var probe struct {
		LoginURL string `json:"loginUrl"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		loginURL = probe.LoginURL
	}
	if loginURL == "" {
		// Fallback: construct the official renewal URL (session.js:149-153).
		cb := url.QueryEscape(c.base + "/sts?sign=%2FKuvzNMEPYoLWPMSl3fRtWQeN%2Bo%3D&followup=" + url.QueryEscape(c.base+"/"))
		loginURL = "https://account.xiaomi.com/pass/serviceLogin?callback=" + cb + "&sid=xiaomichatbot&_group=DEFAULT"
	}

	// Step 2: serviceLogin with _json=true and the passToken cookie.
	pu, err := url.Parse(loginURL)
	if err != nil {
		return nil, fmt.Errorf("renewal loginUrl parse: %w", err)
	}
	if !hostAllowed(pu.Hostname(), c.passportHosts) {
		return nil, fmt.Errorf("renewal loginUrl host %q not in allowlist", pu.Hostname())
	}
	q := pu.Query()
	q.Set("_json", "true")
	pu.RawQuery = q.Encode()
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, pu.String(), nil)
	if err != nil {
		return nil, err
	}
	req2.Header.Set("User-Agent", c.userAgent)
	req2.Header.Set("Cookie", "userId="+userID+"; passToken="+passToken)
	resp2, err := c.http.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("renewal serviceLogin: %w", err)
	}
	body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<20))
	resp2.Body.Close()
	m, err := parseXSSI(body2)
	if err != nil {
		return nil, err
	}
	code, _ := m["code"].(float64)
	if code != 0 {
		desc, _ := m["description"].(string)
		if int64(code) == 70016 {
			return nil, &ErrPassTokenDead{Description: desc}
		}
		return nil, fmt.Errorf("renewal failed: code=%d %s", int64(code), desc)
	}
	loc, _ := m["location"].(string)
	if loc == "" {
		return nil, errors.New("renewal response missing location")
	}

	// Step 3: follow the STS location for fresh cookies.
	sess, err := c.stsExchange(ctx, loc)
	if err != nil {
		return nil, err
	}
	if sess.UserID == "" {
		sess.UserID = userID
	}
	sess.PassToken = passToken
	return sess, nil
}

// Validate checks a session cookie against the upstream (SDD §2.3): 200 +
// business code==0 means healthy; anything else is invalid (TDD
// RENEW-009). Auth-expired outcomes are wrapped via AuthError so the pool
// probe can distinguish them from upstream sickness.
func (c *Client) Validate(ctx context.Context, cookie string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/open-apis/user/mi/get", nil)
	if err != nil {
		return err
	}
	c.setBrowserHeaders(req.Header)
	req.Header.Set("Cookie", cookie)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return AuthError(errors.New("session expired (401)"))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("validate: status %d", resp.StatusCode)
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err == nil && result.Code != 0 {
		return AuthError(fmt.Errorf("validate: business code %d", result.Code))
	}
	return nil

}

// FetchLoginContext is the exported diagnostic wrapper around
// fetchLoginContext for cmd/loginprobe.
func (c *Client) FetchLoginContext(ctx context.Context) (*LoginContext, error) {
	return c.fetchLoginContext(ctx)
}

// HTTPDo is the exported diagnostic wrapper exposing the underlying
// http.Client.Do for cmd/loginprobe raw-response dumps.
func (c *Client) HTTPDo(req *http.Request) (*http.Response, error) {
	return c.http.Do(req)
}

// HTTPDoManual performs a request WITHOUT following redirects (intermediate
// Set-Cookie must be captured by the caller); diagnostic wrapper for probes.
func (c *Client) HTTPDoManual(req *http.Request) (*http.Response, error) {
	manual := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: c.http.Timeout,
	}
	return manual.Do(req)
}
