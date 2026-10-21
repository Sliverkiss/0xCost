// Package waf solves the Aliyun "new-gen _waf_*" challenge used by
// chatglm.cn's /main/alltoolsdetail page.
//
// Findings (docs/reverse/WAF.md): the challenge sets no cookie; it runs an
// obfuscated inline script that derives a 192-character `refer__991` query
// token (prefix 2790552d-), then navigates to the original URL with that token
// appended. The token is server-validated, reusable and not bound to UA, IP or
// path. No /chatglm/** API endpoint is ever challenged, so this package is a
// fallback utility for flows that need the page itself.
//
// Rather than re-implementing the obfuscated cipher, the solver executes the
// unmodified vendor script inside goja (a pure Go ECMAScript engine) with a
// minimal DOM shim — the same approach as the Node reference solver in
// docs/reverse/tools, without a Node dependency.
package waf

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

//go:embed harness.js
var harnessJS string

// QueryKey is the query parameter carrying the challenge token.
const QueryKey = "refer__991"

// TokenPrefix is constant across issued tokens.
const TokenPrefix = "2790552d-"

// ErrNoNavigation is returned when the challenge script ran but never assigned
// a navigation target (usually an incomplete shim for a new challenge variant).
var ErrNoNavigation = errors.New("waf: challenge script produced no navigation target")

// ErrStillChallenged is returned when the page stayed challenged even after a
// token was appended.
var ErrStillChallenged = errors.New("waf: page still serves the challenge after solving")

var (
	renderDataRe = regexp.MustCompile(`<textarea id="renderData"[^>]*>([\s\S]*?)</textarea>`)
	inlineRe     = regexp.MustCompile(`(?i)<script([^>]*)>([\s\S]*?)</script>`)
	srcAttrRe    = regexp.MustCompile(`(?i)\bsrc\s*=`)
	challengeRe  = regexp.MustCompile(`<textarea id="renderData"|aliyun_waf_`)
)

// IsChallenge reports whether body is a WAF challenge document rather than the
// real SPA shell.
func IsChallenge(body string) bool { return challengeRe.MatchString(body) }

// Info is the parsed challenge document.
type Info struct {
	RenderData string // raw JSON blob from the renderData textarea
	Inline     []string
	External   []string
}

// Parse extracts the challenge scripts and renderData blob. It returns ok=false
// when html is not a challenge.
func Parse(html string) (Info, bool) {
	if !IsChallenge(html) {
		return Info{}, false
	}
	var info Info
	if m := renderDataRe.FindStringSubmatch(html); m != nil {
		info.RenderData = strings.TrimSpace(m[1])
	}
	for _, m := range inlineRe.FindAllStringSubmatch(html, -1) {
		attrs, body := m[1], m[2]
		if srcAttrRe.MatchString(attrs) {
			if src := attrValue(attrs, "src"); src != "" {
				info.External = append(info.External, src)
			}
			continue
		}
		if strings.TrimSpace(body) != "" {
			info.Inline = append(info.Inline, body)
		}
	}
	return info, true
}

func attrValue(attrs, name string) string {
	re := regexp.MustCompile(`(?i)\b` + name + `\s*=\s*["']([^"']+)["']`)
	if m := re.FindStringSubmatch(attrs); m != nil {
		return m[1]
	}
	return ""
}

// SolveToken executes the challenge scripts in html and returns the token from
// the captured navigation target. pageURL is the absolute URL that served the
// challenge; relative navigations are resolved against it.
func SolveToken(html, pageURL string, ua string) (string, error) {
	info, ok := Parse(html)
	if !ok {
		return "", errors.New("waf: document is not a challenge")
	}
	if strings.TrimSpace(info.RenderData) == "" {
		return "", errors.New("waf: challenge has no renderData blob")
	}

	vm := goja.New()
	var (
		mu       sync.Mutex
		target   string
		targetOK bool
	)
	setTarget := func(raw string) {
		resolved := resolveURL(pageURL, raw)
		mu.Lock()
		defer mu.Unlock()
		if !targetOK {
			target, targetOK = resolved, true
		}
	}

	_ = vm.Set("__opts", map[string]any{
		"url":        pageURL,
		"renderData": info.RenderData,
		"ua":         ua,
	})
	_ = vm.Set("__hostLog", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	_ = vm.Set("__hostStorage", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	_ = vm.Set("__hostNavigate", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			setTarget(call.Arguments[0].String())
		}
		return goja.Undefined()
	})
	_ = vm.Set("__atob", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return vm.ToValue("")
		}
		s, err := decodeBase64(call.Arguments[0].String())
		if err != nil {
			panic(vm.NewTypeError("atob: %v", err))
		}
		return vm.ToValue(s)
	})
	_ = vm.Set("__btoa", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return vm.ToValue("")
		}
		return vm.ToValue(encodeBase64(call.Arguments[0].String()))
	})
	_ = vm.Set("__hostUUID", func(call goja.FunctionCall) goja.Value {
		b := randomBytes(16)
		// RFC 4122 version 4 layout.
		out := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		return vm.ToValue(out)
	})
	_ = vm.Set("__hostRandomBytes", func(call goja.FunctionCall) goja.Value {
		n := 16
		if len(call.Arguments) > 0 {
			n = int(call.Arguments[0].ToInteger())
		}
		if n < 0 || n > 65536 {
			n = 16
		}
		return vm.ToValue(randomBytes(n))
	})
	timers := installTimers(vm)

	if _, err := vm.RunString(harnessJS); err != nil {
		return "", fmt.Errorf("waf: harness: %w", err)
	}
	for _, src := range info.Inline {
		// Challenge scripts routinely throw on incomplete DOM shims after they
		// have already emitted the navigation; errors are non-fatal by design.
		_, _ = vm.RunString(src)
	}
	timers.drain(vm, 200, 5*time.Second)

	mu.Lock()
	got, ok := target, targetOK
	mu.Unlock()
	if !ok || got == "" {
		return "", ErrNoNavigation
	}
	u, err := url.Parse(got)
	if err != nil {
		return "", fmt.Errorf("waf: parse navigation target: %w", err)
	}
	token := u.Query().Get(QueryKey)
	if token == "" {
		return "", fmt.Errorf("waf: navigation target carries no %s token", QueryKey)
	}
	return token, nil
}

func resolveURL(base, ref string) string {
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

// TokenURL appends the solved token to raw, preserving existing query values.
func TokenURL(raw, token string) string {
	u, err := url.Parse(raw)
	if err != nil {
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		return raw + sep + QueryKey + "=" + url.QueryEscape(token)
	}
	q := u.Query()
	q.Set(QueryKey, token)
	u.RawQuery = q.Encode()
	return u.String()
}

// BrowserHeaders is the header set used for page fetches. It matches the SPA's
// navigation requests; API calls use their own set in internal/upstream.
func BrowserHeaders(ua string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", ua)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	return h
}

// tokenAttempts bounds how many fresh tokens are tried before giving up. A
// token can be rejected when the server rotates its challenge between the
// solve and the verify fetch, so one retry is required, not optional.
const tokenAttempts = 2

// Solver caches one solved token for the process lifetime and re-solves when a
// page fetch says the token no longer works. Concurrent callers share one
// solve: the challenge endpoint is remote and the token is reusable, so
// running the cycle once per waiting goroutine is pure load.
type Solver struct {
	Client *http.Client
	UA     string

	mu     sync.Mutex
	token  string
	solved time.Time
	flight *solveFlight
}

// solveFlight is an in-progress solve that later callers join.
type solveFlight struct {
	done  chan struct{}
	token string
	err   error
}

// NewSolver builds a Solver with a sane HTTP client.
func NewSolver(client *http.Client, ua string) *Solver {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Solver{Client: client, UA: ua}
}

// EnsureToken returns a cached token or solves for one. pageURL defaults to the
// protected SPA route when empty.
func (s *Solver) EnsureToken(ctx context.Context, pageURL string) (string, error) {
	if pageURL == "" {
		pageURL = "https://chatglm.cn/main/alltoolsdetail"
	}
	s.mu.Lock()
	if s.token != "" {
		tok := s.token
		s.mu.Unlock()
		return tok, nil
	}
	if f := s.flight; f != nil {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.token, f.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f := &solveFlight{done: make(chan struct{})}
	s.flight = f
	s.mu.Unlock()

	f.token, f.err = s.solve(ctx, pageURL)
	s.mu.Lock()
	if f.err == nil && f.token != "" {
		s.token, s.solved = f.token, time.Now()
	}
	s.flight = nil
	s.mu.Unlock()
	close(f.done)
	return f.token, f.err
}

// solve runs the fetch → solve → verify cycle, retrying with a fresh token
// when the server rejects the one just derived.
func (s *Solver) solve(ctx context.Context, pageURL string) (string, error) {
	body, err := s.fetch(ctx, pageURL)
	if err != nil {
		return "", err
	}
	if !IsChallenge(body) {
		// Nothing to solve; there is no token to return and callers should
		// treat the page as accessible.
		return "", nil
	}
	for attempt := 0; attempt < tokenAttempts; attempt++ {
		token, err := SolveToken(body, pageURL, s.UA)
		if err != nil {
			return "", err
		}
		verify, err := s.fetch(ctx, TokenURL(pageURL, token))
		if err != nil {
			return "", err
		}
		if !IsChallenge(verify) {
			return token, nil
		}
		// The token was rejected: the server may have rotated its challenge.
		// Re-fetch the challenge page before solving again.
		if body, err = s.fetch(ctx, pageURL); err != nil {
			return "", err
		}
		if !IsChallenge(body) {
			return "", nil
		}
	}
	return "", ErrStillChallenged
}

// Invalidate drops the cached token.
func (s *Solver) Invalidate() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

func (s *Solver) fetch(ctx context.Context, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	for k, v := range BrowserHeaders(s.UA) {
		req.Header[k] = v
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
