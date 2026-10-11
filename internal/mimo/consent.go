package mimo

// Cross-region login chain (verified live 2026-10-09, see
// docs/login-chain-reverse.md): when auth2 answers code=0 with an empty
// location and a notificationUrl, the demand is not necessarily the
// new-device OTP flow. From a non-mainland-China egress IP the chain is:
//
//	GET  <notificationUrl>                       (302)
//	GET  /pass2/userinfo/start?bizType=UserCross (302)
//	GET  /fe/service/userCross?needClose=false&userId=…   (the SPA page)
//	POST /pass2/userCrossAgreement/update {userId,status:1,needClose:false}
//	     → {code:0, location:"/pass2/userinfo/check?callback=<authStart…>"}
//	GET  <that location> → /fe/service/identity/authStart?sid&context
//	→ identity verification steps (see otp.go)
//
// Agreements are one-shot per verification session: repeating the POST on a
// consumed chain (or on a plain OTP chain) answers code 21301 认证失败.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxChainHops bounds the manual redirect walk of a notificationUrl chain.
const maxChainHops = 8

// hopRecord is one request/redirect observed while following a chain by hand.
type hopRecord struct {
	Status   int
	URL      string
	Location string
}

// ErrCrossRegionConsent is returned when the login chain demands the
// cross-region personal-data-transfer agreement and auto-agree is disabled
// (env MIMO2API_CROSS_REGION_AGREE=false). The account owner must agree in a
// browser once from this egress IP; afterwards the demand is not repeated.
var ErrCrossRegionConsent = errors.New("login demands the cross-region personal-data-transfer agreement (MIMO2API_CROSS_REGION_AGREE=false)")

// absURL resolves a possibly-relative Location header against its source URL.
func absURL(base, ref string) string {
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// followChain GETs start with redirects followed BY HAND (never by the
// transport), so every intermediate Location is observable — the consent
// handoff and the authStart context ride on intermediate Locations, not on
// the final page URL. Cookies accumulate into jar.
func (c *Client) followChain(ctx context.Context, jar map[string]string, start string, maxHops int) ([]hopRecord, string, error) {
	var hops []hopRecord
	target := c.absPassportURL(start)
	for i := 0; i < maxHops; i++ {
		u, err := url.Parse(target)
		if err != nil {
			return hops, "", fmt.Errorf("chain hop %d: parse %q: %w", i, target, err)
		}
		if u.Hostname() == "" || !c.otpRedirectOK(u.Hostname()) {
			return hops, "", fmt.Errorf("chain hop %d: host %q not allowlisted", i, u.Hostname())
		}
		_, resp, err := c.ssoDo(ctx, jar, http.MethodGet, target, nil, nil)
		if err != nil {
			return hops, "", fmt.Errorf("chain hop %d: %w", i, err)
		}
		loc := resp.Header.Get("Location")
		hops = append(hops, hopRecord{Status: resp.StatusCode, URL: target, Location: loc})
		if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
			return hops, target, nil
		}
		target = absURL(target, loc)
	}
	return hops, target, fmt.Errorf("notification chain exceeded %d hops", maxHops)
}

// queryOf parses the query of a raw URL, tolerating junk.
func queryOf(raw string) url.Values {
	u, err := url.Parse(raw)
	if err != nil {
		return url.Values{}
	}
	return u.Query()
}

// isConsentChain reports whether the walked chain went through the
// cross-region consent page, and returns its userId when present.
func isConsentChain(hops []hopRecord, page string) (string, bool) {
	for _, h := range hops {
		q := queryOf(h.Location)
		if q.Get("bizType") == "UserCross" {
			return q.Get("userId"), true
		}
	}
	if strings.Contains(page, "/fe/service/userCross") {
		return queryOf(page).Get("userId"), true
	}
	return "", false
}

// authStartParams extracts sid/context from an authStart URL, following one
// level of ?callback= indirection: the consent flow answers with
// /pass2/userinfo/check?callback=<percent-encoded authStart URL>.
func authStartParams(raw string) (sid, context string) {
	cur := raw
	for depth := 0; depth < 3 && cur != ""; depth++ {
		q := queryOf(cur)
		if q.Get("context") != "" {
			s := q.Get("sid")
			if s == "" {
				s = "xiaomichatbot"
			}
			return s, q.Get("context")
		}
		cur = q.Get("callback")
	}
	return "", ""
}

// verificationTarget is where the identity verification steps must run.
type verificationTarget struct {
	PassBase string // scheme://host of the passport base
	SID      string
	Context  string
	Page     string // last URL of the chain (the page the browser shows)
	Consent  bool   // the cross-region agreement was traversed
	Jar      map[string]string
}

// startVerification walks the notificationUrl chain, agrees to the
// cross-region data-transfer demand when it appears AND agreeConsent is true,
// and returns the sid/context the /identity/* steps need. When agreeConsent is
// false the consent POST is never issued: the demand surfaces as
// ErrCrossRegionConsent (the operator override arrives via LoginWithConsent,
// which threads agreeConsent=true regardless of disableCrossRegionAgree).
func (c *Client) startVerification(ctx context.Context, notificationURL, sidFallback string, seedJar map[string]string, agreeConsent bool) (*verificationTarget, error) {
	jar := map[string]string{}
	for k, v := range seedJar {
		jar[k] = v
	}
	ntf := c.absPassportURL(notificationURL)
	u, err := url.Parse(ntf)
	if err != nil {
		return nil, fmt.Errorf("notification url parse: %w", err)
	}
	if u.Hostname() == "" || !c.otpRedirectOK(u.Hostname()) {
		return nil, fmt.Errorf("notification url host %q not allowlisted", u.Hostname())
	}
	passBase := u.Scheme + "://" + u.Host
	sid := u.Query().Get("sid")
	if sid == "" {
		sid = sidFallback
	}
	target := &verificationTarget{PassBase: passBase, SID: sid, Jar: jar}

	hops, page, err := c.followChain(ctx, jar, ntf, maxChainHops)
	if err != nil {
		return nil, fmt.Errorf("notification chain: %w", err)
	}
	target.Page = page

	// Legacy/simple shape: the notificationUrl itself (or its first hop) is
	// the identity entry carrying context.
	if s, cx := authStartParams(page); cx != "" {
		if s != "" {
			target.SID = s
		}
		target.Context = cx
		return target, nil
	}

	userID, consent := isConsentChain(hops, page)
	if !consent {
		return nil, fmt.Errorf("login chain landed on %s without an identity context (unsupported demand)", page)
	}
	if !agreeConsent {
		return nil, ErrCrossRegionConsent
	}
	target.Consent = true
	if userID == "" {
		if _, cx := authStartParams(page); cx != "" {
			target.Context = cx
			return target, nil
		}
		return nil, errors.New("cross-region consent page carried no userId")
	}

	// Agree: POST /pass2/userCrossAgreement/update (status 1 = 同意).
	form := url.Values{}
	form.Set("userId", userID)
	form.Set("status", "1")
	form.Set("needClose", "false")
	body, _, err := c.ssoDo(ctx, jar, http.MethodPost, passBase+"/pass2/userCrossAgreement/update", form,
		map[string]string{"X-Requested-With": "XMLHttpRequest", "Referer": page})
	if err != nil {
		return nil, fmt.Errorf("cross-region consent: %w", err)
	}
	agree, err := parseLooseJSON(body)
	if err != nil {
		return nil, fmt.Errorf("cross-region consent response: %w", err)
	}
	if err := codeCheck(agree, "cross-region consent"); err != nil {
		return nil, err
	}
	loc, _ := agree["location"].(string)
	if loc == "" {
		return nil, errors.New("cross-region consent accepted but no location returned")
	}

	// The location is /pass2/userinfo/check?callback=<authStart…>; walking it
	// plants the identity-session cookies.
	_, next, err := c.followChain(ctx, jar, loc, maxChainHops)
	if err != nil {
		return nil, fmt.Errorf("post-consent chain: %w", err)
	}
	target.Page = next
	s, cx := authStartParams(loc)
	if cx == "" {
		s, cx = authStartParams(next)
	}
	if cx == "" {
		return nil, errors.New("post-consent chain carried no identity context")
	}
	if s != "" {
		target.SID = s
	}
	target.Context = cx
	return target, nil
}
