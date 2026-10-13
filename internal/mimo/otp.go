package mimo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// OTPChallengeTTL: an OTP code is valid ~10 minutes; challenges expire with
// it (SDD §2.2 amendment).
const OTPChallengeTTL = 10 * time.Minute

// OTPChallenge is one started new-device verification (SDD §2.2 amendment,
// references/xm2api/lib/login.mjs startOTP). The code has been sent to the
// account owner's phone/email; SubmitOTP completes the login with it. The
// jar holds the verification-session cookies accumulated across the flow —
// the challenge is only usable together with them, in memory only.
type OTPChallenge struct {
	ID        string    // opaque handle for the admin submit endpoint
	Method    string    // "Phone" | "Email"
	Notify    string    // masked destination, e.g. "188****5616"
	SID       string    // passport sid for the final serviceLogin
	ExpiresAt time.Time // 10-minute window

	passBase string            // scheme://host of the passport base
	flag     int               // 4 = phone, 8 = email
	jar      map[string]string // verification-session cookies
}

// ErrOTPPending: Login hit the new-device OTP demand (code=0 +
// securityStatus=128 + empty location + notificationUrl) and auto-started
// the challenge. The OTP code arrives on the account owner's phone — the
// login cannot complete until SubmitOTP is called with it.
type ErrOTPPending struct {
	Challenge *OTPChallenge
	// Device is the identity the login chain harvested (deviceId/pass_ua/
	// uLocale) — persisted by the caller so the code the operator enters is
	// verified on the same device later attempts present.
	Device map[string]string
}

func (e *ErrOTPPending) Error() string {
	if e.Challenge != nil && e.Challenge.Notify != "" {
		return "new-device OTP verification required; code sent to " + e.Challenge.Notify
	}
	return "new-device OTP verification required"
}

// parseLooseJSON decodes a passport/identity response: XSSI prefix stripped
// when present, plain JSON otherwise (the reference parseJSON — the
// identity endpoints answer both dialects).
func parseLooseJSON(body []byte) (map[string]any, error) {
	s := strings.TrimSpace(string(body))
	s = strings.TrimSpace(strings.TrimPrefix(s, xssiPrefix))
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// scalarString renders a JSON string/number as a token string (userId is a
// number in real passport responses).
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), true
		}
		return strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}

// findTokens scans a whole JSON tree for passToken/userId (case-insensitive
// keys, string or number values) — the reference findTokens.
func findTokens(body []byte) (passToken, userID string) {
	root, err := parseLooseJSON(body)
	if err != nil {
		return "", ""
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := scalarString(x); ok && s != "" {
					switch strings.ToLower(k) {
					case "passtoken":
						if passToken == "" {
							passToken = s
						}
					case "userid":
						if userID == "" {
							userID = s
						}
					}
				}
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(root)
	return
}

// jarTokens harvests passToken/userId from the cookie jar.
func jarTokens(jar map[string]string) (passToken, userID string) {
	for k, v := range jar {
		switch strings.ToLower(k) {
		case "passtoken":
			if passToken == "" {
				passToken = v
			}
		case "userid":
			if userID == "" {
				userID = v
			}
		}
	}
	return
}

// cookieHeader renders the jar deterministically (sorted keys).
func cookieHeader(jar map[string]string) string {
	if len(jar) == 0 {
		return ""
	}
	keys := make([]string, 0, len(jar))
	for k := range jar {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(jar[k])
	}
	return b.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// otpClient is the manual-redirect HTTP client for the SSO flow (redirects
// are followed by hand so cookies land in our jar, never the transport's).
// Shares the transport with c.http.
func (c *Client) otpClient() *http.Client {
	return &http.Client{
		Transport: c.http.Transport,
		Timeout:   c.http.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ssoDo performs one SSO request with jar cookies in and Set-Cookie out.
// Returns the body and the (already-drained) response for header inspection.
func (c *Client) ssoDo(ctx context.Context, jar map[string]string, method, target string, form url.Values, extra map[string]string) ([]byte, *http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, err
	}
	// Same User-Agent as every other login hop: the identity JSP endpoints
	// answer fine for a browser UA (verified live 2026-10-09), and a
	// mismatch here is a fingerprint tell.
	// identity/* are same-origin XHRs from the account SPA: send what its axios
	// instance sends (passport.go), then let `extra` override Referer etc.
	if tu, perr := url.Parse(target); perr == nil {
		c.setXHRHeaders(req.Header, tu.Scheme+"://"+tu.Host, tu.Scheme+"://"+tu.Host+"/")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if ck := cookieHeader(jar); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := c.otpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	for k, v := range collectCookies(resp) {
		jar[k] = v
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return data, resp, nil
}

// otpRedirectOK guards redirect targets: xiaomi.com (apex or subdomain), the
// configured passport allowlist, or the configured upstream host — the
// post-verification landing chain ends at <upstream>/sts (live 2026-10-09).
func (c *Client) otpRedirectOK(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "xiaomi.com" || strings.HasSuffix(host, ".xiaomi.com") {
		return true
	}
	if c.upstreamHost != "" && host == strings.ToLower(c.upstreamHost) {
		return true
	}
	return hostAllowed(host, c.passportHosts)
}

// absPassportURL resolves maybe against the passport base — absolute URLs
// pass through; relative ones anchor to the first allowlisted host.
func (c *Client) absPassportURL(maybe string) string {
	s := strings.TrimSpace(maybe)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	host := "account.xiaomi.com"
	if len(c.passportHosts) > 0 {
		host = c.passportHosts[0]
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return "https://" + host + s
}

// codeCheck applies the reference rule: fail when code is present and is
// neither -1 nor 0.
func codeCheck(m map[string]any, what string) error {
	if m == nil {
		return nil
	}
	if code, ok := m["code"].(float64); ok && code != -1 && code != 0 {
		desc, _ := m["desc"].(string)
		if desc == "" {
			desc, _ = m["description"].(string)
		}
		return fmt.Errorf("%s failed: code=%d %s", what, int64(code), desc)
	}
	return nil
}

// startOTP opens a new-device verification session and sends the OTP code
// (SDD §2.2 amendment, steps 1-4):
//  1. walk the notificationUrl chain — a plain identity entry, or (from a
//     non-mainland egress IP) the cross-region consent page followed by the
//     authStart entry (consent.go; verified live 2026-10-09);
//  2. GET /identity/list — flag 4 = phone, 8 = email, masked notify;
//  3. GET /identity/auth/verify{Phone,Email}?_flag — trigger the session;
//  4. POST /identity/auth/send{Phone,Email}Ticket — X-Requested-With:
//     XMLHttpRequest is MANDATORY (without it email send always fails
//     66108, 2026-09-23 packet capture).
//
// The cross-region consent decision is threaded explicitly: startVerification
// performs the consent POST only when agreeConsent is true. Login/
// LoginWithConsent pass their effective decision here (consent.go).
func (c *Client) startOTP(ctx context.Context, notificationURL, sid string, seedJar map[string]string, agreeConsent bool) (*OTPChallenge, error) {
	tgt, err := c.startVerification(ctx, notificationURL, sid, seedJar, agreeConsent)
	if err != nil {
		return nil, err
	}
	jar := tgt.Jar
	passBase := tgt.PassBase
	sidParam := tgt.SID
	otpCtx := tgt.Context

	// Step 2: identity/list — verification method (flag 4=phone 8=email).
	listURL := passBase + "/identity/list?sid=" + url.QueryEscape(sidParam) +
		"&supportedMask=0&_locale=zh_CN&context=" + url.QueryEscape(otpCtx)
	body, _, err := c.ssoDo(ctx, jar, http.MethodGet, listURL, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("otp identity list: %w", err)
	}
	list, err := parseLooseJSON(body)
	if err != nil {
		return nil, fmt.Errorf("otp identity list parse: %w", err)
	}
	if list["flag"] == nil {
		if data, ok := list["data"].(map[string]any); ok {
			for k, v := range data {
				list[k] = v
			}
		}
	}
	flag := 4
	if f, ok := list["flag"].(float64); ok && int64(f) == 8 {
		flag = 8
	}
	method := "Phone"
	if flag == 8 {
		method = "Email"
	}
	notify := ""
	for _, k := range []string{"notify", "notifyPhone", "notifyEmail", "maskedPhone", "maskedEmail", "phone", "email", "tips", "description", "desc"} {
		if s, ok := list[k].(string); ok && s != "" {
			notify = s
			break
		}
	}

	// Step 3: trigger the verification session. The JSON answer carries the
	// masked destination ({"maskedPhone":"+86 182******16",...}) — the live
	// shape; /identity/list does not include it (verified 2026-10-09).
	trigURL := fmt.Sprintf("%s/identity/auth/verify%s?_flag=%d&_json=true", passBase, method, flag)
	body, _, err = c.ssoDo(ctx, jar, http.MethodGet, trigURL, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("otp verify trigger: %w", err)
	}
	if trig, perr := parseLooseJSON(body); perr == nil {
		if err := codeCheck(trig, "otp verify trigger"); err != nil {
			return nil, err
		}
		for _, k := range []string{"maskedPhone", "maskedEmail", "notify", "notifyPhone", "notifyEmail"} {
			if s, ok := trig[k].(string); ok && s != "" {
				notify = s
				break
			}
		}
	}

	// Step 4: send the code (X-Requested-With mandatory).
	sendURL := fmt.Sprintf("%s/identity/auth/send%sTicket?_dc=%d", passBase, method, time.Now().UnixMilli())
	form := url.Values{}
	form.Set("retry", "0")
	form.Set("icode", "")
	form.Set("_json", "true")
	body, _, err = c.ssoDo(ctx, jar, http.MethodPost, sendURL, form, map[string]string{"X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return nil, fmt.Errorf("otp send ticket: %w", err)
	}
	if sm, perr := parseLooseJSON(body); perr == nil {
		if err := codeCheck(sm, "otp code send"); err != nil {
			return nil, err
		}
	}

	return &OTPChallenge{
		ID:        "otp-" + adapter.RandomHex32(),
		Method:    method,
		Notify:    notify,
		SID:       sidParam,
		ExpiresAt: time.Now().Add(OTPChallengeTTL),
		passBase:  passBase,
		flag:      flag,
		jar:       jar,
	}, nil
}

// SubmitOTP completes the login with the received code (SDD §2.2 amendment,
// steps 5-8):
//  5. POST /identity/auth/verify{Method} — ticket + trust=true +
//     X-Requested-With;
//  6. GET the returned location — auth cookies into the jar;
//  7. GET /pass/serviceLogin — passToken/userId via findTokens /
//     collectTokens (≤2 redirect hops, xiaomi.com hosts only);
//  8. serviceLogin again with the authenticated jar → STS location →
//     stsExchange for the aistudio serviceToken session.
func (c *Client) SubmitOTP(ctx context.Context, ch *OTPChallenge, code string) (*Session, error) {
	if ch == nil {
		return nil, errors.New("otp challenge is nil")
	}
	if time.Now().After(ch.ExpiresAt) {
		return nil, errors.New("otp challenge expired (10-minute window)")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("otp code is empty")
	}

	// Step 5: submit the ticket (trust=true lowers re-trigger frequency).
	vURL := fmt.Sprintf("%s/identity/auth/verify%s?_dc=%d", ch.passBase, ch.Method, time.Now().UnixMilli())
	form := url.Values{}
	form.Set("_flag", strconv.Itoa(ch.flag))
	form.Set("ticket", code)
	form.Set("trust", "true")
	form.Set("_json", "true")
	body, _, err := c.ssoDo(ctx, ch.jar, http.MethodPost, vURL, form, map[string]string{"X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return nil, fmt.Errorf("otp verify submit: %w", err)
	}
	vm, _ := parseLooseJSON(body)
	loc, _ := vm["location"].(string)
	if loc == "" {
		if err := codeCheck(vm, "otp verify"); err != nil {
			return nil, err
		}
		return nil, errors.New("otp verify: response carried no location")
	}

	// Step 6: walk the landing chain to its end — the session cookies are
	// planted at the upstream STS hop, not at the first redirect.
	sess, err := c.stsExchangeWithJar(ctx, loc, ch.jar)
	if err != nil {
		// Passport variants whose landing location is a bare intermediate
		// still need the explicit serviceLogin hop.
		sess, err = c.otpServiceLoginSession(ctx, ch)
		if err != nil {
			return nil, err
		}
	}

	// Step 7: identity — prefer what the chain planted in the jar.
	passToken, userID := jarTokens(ch.jar) // (passToken, userID)
	if userID == "" {
		userID = sess.UserID
	}
	if passToken == "" {
		passToken = sess.PassToken
	}
	if userID == "" || passToken == "" {
		if pm, perr := parseLooseJSON(body); perr == nil {
			next, _ := pm["location"].(string)
			pt, uid := c.collectTokens(ctx, ch.jar, next)
			if passToken == "" {
				passToken = pt
			}
			if userID == "" {
				userID = uid
			}
		}
	}
	// passToken/userID are best-effort: they are only needed for renewal, and
	// the caller keeps the stored values when they are empty. The session
	// cookie is the deliverable — never throw a working login away over a
	// missing renewal token.
	sess.UserID = userID
	sess.PassToken = passToken
	return sess, nil
}

// otpServiceLoginSession is the fallback completion for passport variants
// that need the explicit serviceLogin hop: harvest passToken/userId, then
// exchange the offered STS location.
func (c *Client) otpServiceLoginSession(ctx context.Context, ch *OTPChallenge) (*Session, error) {
	slURL := ch.passBase + "/pass/serviceLogin?sid=" + url.QueryEscape(ch.SID) + "&_json=true&_locale=zh_CN"
	body, _, err := c.ssoDo(ctx, ch.jar, http.MethodGet, slURL, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("otp serviceLogin: %w", err)
	}
	passToken, userID := findTokens(body)
	if passToken == "" || userID == "" {
		pm, _ := parseLooseJSON(body)
		next, _ := pm["location"].(string)
		passToken, userID = c.collectTokens(ctx, ch.jar, next)
	}
	if passToken == "" || userID == "" {
		if pt, uid := jarTokens(ch.jar); pt != "" && uid != "" {
			passToken, userID = pt, uid
		}
	}
	body, _, err = c.ssoDo(ctx, ch.jar, http.MethodGet, slURL, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("otp sts serviceLogin: %w", err)
	}
	m2, err := parseLooseJSON(body)
	if err != nil {
		return nil, fmt.Errorf("otp sts serviceLogin parse: %w", err)
	}
	if err := codeCheck(m2, "otp sts serviceLogin"); err != nil {
		return nil, err
	}
	loc2, _ := m2["location"].(string)
	if loc2 == "" {
		return nil, errors.New("otp: no STS location after verification")
	}
	sess, err := c.stsExchangeWithJar(ctx, loc2, ch.jar)
	if err != nil {
		return nil, err
	}
	sess.UserID = userID
	sess.PassToken = passToken
	return sess, nil
}

// collectTokens follows the token-harvest redirect chain: at most 2 hops,
// each host must be xiaomi.com (or subdomain) / allowlisted; tokens are
// harvested from jar cookies, the response body tree, and URL query params.
func (c *Client) collectTokens(ctx context.Context, jar map[string]string, location string) (passToken, userID string) {
	next := c.absPassportURL(location)
	for hop := 0; hop < 2 && next != ""; hop++ {
		u, err := url.Parse(next)
		if err != nil || !c.otpRedirectOK(u.Hostname()) {
			break // open-redirect guard
		}
		body, resp, err := c.ssoDo(ctx, jar, http.MethodGet, next, nil, nil)
		if err != nil {
			break
		}
		if pt, uid := findTokens(body); pt != "" && uid != "" {
			return pt, uid
		}
		if pt, uid := jarTokens(jar); pt != "" && uid != "" {
			return pt, uid
		}
		q := u.Query()
		if q.Get("passToken") != "" && q.Get("userId") != "" {
			return q.Get("passToken"), q.Get("userId")
		}
		// Next hop: Location header or body JSON location.
		next = resp.Header.Get("Location")
		if next == "" {
			if m, perr := parseLooseJSON(body); perr == nil {
				next, _ = m["location"].(string)
			}
		}
		if next != "" && !strings.Contains(next, "://") {
			next = u.Scheme + "://" + u.Host + next
		}
	}
	return "", ""
}
