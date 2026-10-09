package mimo

// Passport-side request fidelity (SDD §2.4, docs/login-chain-reverse.md §5.1).
//
// setBrowserHeaders covers the aistudio platform surface. The passport chain
// used to send three headers (Content-Type, User-Agent, Accept-Language) and no
// cookie context at all, which is not what any real client looks like: the
// account front end is plain Chrome loading an HTML page, then posting the
// login form. Two things follow and both are fingerprints:
//
//  1. the browser navigates to the login URL *first*. That GET is what the
//     passport uses to hand out the device anchor — live: `deviceId=wb_<uuid>`
//     (390d), `pass_ua=web`, `uLocale=zh_CN` and a `JSESSIONID` binding the
//     login attempt. Posting serviceLoginAuth2 with no cookies makes every
//     attempt a brand-new device, which is exactly why the new-device demand
//     never went away.
//  2. every hop carries Chrome's header set: Accept, Upgrade-Insecure-Requests,
//     the client hints (sec-ch-ua*) and the sec-fetch-* triplet, plus an Origin
//     and Referer naming the page the request came from.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const (
	// chromeAcceptHTML is the Accept a Chrome navigation/HTML-form request
	// sends (verbatim from a live capture of the account front end).
	chromeAcceptHTML = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
	// chromeAcceptJSON is the axios Accept the account SPA's XHRs carry.
	chromeAcceptJSON  = "application/json, text/plain, */*"
	chromeAcceptLang  = "zh-CN,zh;q=0.9"
	chromeAcceptLangQ = "zh-CN"
)

// setNavigationHeaders makes h look like Chrome navigating to a document:
// destination document, cross-site, top-level, with client hints.
func (c *Client) setNavigationHeaders(h http.Header, referer string) {
	h.Set("Accept", chromeAcceptHTML)
	h.Set("Accept-Language", chromeAcceptLang)
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("User-Agent", c.userAgent)
	h.Set("sec-ch-ua", c.secChUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("sec-fetch-dest", "document")
	h.Set("sec-fetch-mode", "navigate")
	h.Set("sec-fetch-site", "cross-site")
	h.Set("sec-fetch-user", "?1")
	if referer != "" {
		h.Set("Referer", referer)
	}
}

// setFormPostHeaders makes h look like an HTML form POST submitted from the
// passport login page: same-origin navigation, form encoding, Origin naming the
// page's own origin. This is the shape of the flow we implement (the legacy
// form endpoint serviceLoginAuth2); a client that instead XHRs the SPA API
// would use setXHRHeaders, so the two are kept apart on purpose.
func (c *Client) setFormPostHeaders(h http.Header, origin, referer string) {
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	c.setNavigationHeaders(h, referer)
	h.Set("sec-fetch-site", "same-origin")
	h.Set("Origin", origin)
}

// setXHRHeaders makes h look like the account SPA's same-origin XHR (axios):
// empty destination, CORS mode, JSON Accept, X-Requested-With.
func (c *Client) setXHRHeaders(h http.Header, origin, referer string) {
	h.Set("Accept", chromeAcceptJSON)
	h.Set("Accept-Language", chromeAcceptLang)
	h.Set("User-Agent", c.userAgent)
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("sec-ch-ua", c.secChUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
	if origin != "" {
		h.Set("Origin", origin)
	}
	if referer != "" {
		h.Set("Referer", referer)
	}
}

// primeLoginPage performs the navigation a browser does before it can post the
// login form: GET the login URL (which 302s from the legacy page to the account
// SPA) and keep whatever device cookies the passport hands out. The result is
// the seed for the flow jar, so serviceLoginAuth2 presents the same device the
// browser would.
//
// Best-effort by design: a failure returns what was collected so far, because a
// login that presents no device cookies is still better than no login at all —
// the caller simply ends up on the pre-fix path (a brand-new device, an OTP
// demand) instead of failing outright.
func (c *Client) primeLoginPage(ctx context.Context, loginURL string, seed map[string]string) map[string]string {
	jar := map[string]string{}
	for k, v := range seed {
		jar[k] = v
	}
	if loginURL == "" {
		return jar
	}
	target := loginURL
	for hop := 0; hop < 2; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return jar
		}
		c.setNavigationHeaders(req.Header, c.base+"/")
		if hop > 0 {
			req.Header.Set("Referer", loginURL)
		}
		// Present what we already know: a cookieless navigation is exactly what
		// makes the passport mint a brand-new deviceId (the live capture).
		if ck := cookieHeader(jar); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return jar
		}
		// Cookies only — the body of the login page carries no data we need.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<18))
		for k, v := range collectCookies(resp) {
			jar[k] = v
		}
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if loc == "" || resp.StatusCode < 300 || resp.StatusCode > 399 {
			return jar
		}
		next, err := resolvePassportLocation(target, loc, c.passportHosts)
		if err != nil {
			return jar
		}
		target = next
	}
	return jar
}

// resolvePassportLocation resolves a redirect target and enforces the same host
// allowlist the rest of the chain uses, so a hostile Location cannot send the
// client (and its cookies) somewhere else.
func resolvePassportLocation(base, loc string, allow []string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	lu, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	abs := bu.ResolveReference(lu)
	if !hostAllowed(abs.Hostname(), allow) {
		return "", fmt.Errorf("login page redirect host %q not in allowlist", abs.Hostname())
	}
	return abs.String(), nil
}

// mergeJars overlays b onto a (b wins) and returns a — the flow jar is built by
// folding the seed device identity, the primed page cookies and every response's
// Set-Cookie, later values overwriting earlier ones exactly like a browser's
// cookie store.
func mergeJars(a, b map[string]string) map[string]string {
	if a == nil {
		a = map[string]string{}
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}
