package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RecordedRequest is one captured upstream chat POST (TDD §2.2:
// MockUpstream records every request for assertions).
type RecordedRequest struct {
	Method         string
	Path           string // /open-apis/bot/chat or /fastchat/open-apis/bot/chat
	PH             string // xiaomichatbot_ph query param
	MsgID          string
	ConvID         string
	PrevDlg        string // previousDialogueId; "" when the payload sent null
	Query          string
	Model          string
	EnableThinking bool
	Temperature    *float64
	TopP           *float64
	Cookie         string
	Headers        http.Header
	RawBody        string
	Cancelled      bool // request context cancelled before completion
	At             time.Time
}

// ScriptStep is one scripted upstream response for an account.
type ScriptStep struct {
	// Body, when non-empty, is served as a text/event-stream response.
	Body string
	// StatusCode: served bare when Body is empty (429/401/451/461/5xx),
	// or accompanies the body when both are set.
	StatusCode int
	// StallBefore: sleep before writing anything (pre-first-byte stall).
	StallBefore time.Duration
	// StallMidStream + MidStreamSplit: split the body at this many bytes
	// and sleep between the halves (mid-stream stall, TDD API-019).
	StallMidStream time.Duration
	MidStreamSplit int
	// AbortAfterBody: close the connection after the body without a clean
	// end (mid-stream abort, TDD STICKY-009).
	AbortAfterBody bool
	// Headers appended to the response.
	Headers map[string]string
}

// MockUpstream is a programmable MiMo upstream: routes by account identity
// (distinct xiaomichatbot_ph per scripted account), serves SSE fixtures,
// and records every request (TDD §1.2, §2.2).
type MockUpstream struct {
	mu       sync.Mutex
	server   *httptest.Server
	accounts map[string]*mockAccount // ph → account
	byCookie map[string]*mockAccount // serviceToken value → account
	requests []*RecordedRequest
	loginURL string // 401-probe loginUrl override (OTP login tests)

	// Web-surface audio/catalog state (mock_audio.go).
	audioReqs    []AudioRequest
	ttsStatuses  []string
	asrStatuses  []string
	asrText      string
	wav          []byte
	uploaded     []byte
	saveErrors   []string
	botConfigErr string
}

// SetLoginURL overrides the loginUrl in the /open-apis/user/mi/get 401
// body — point it at a MockPassport so the full login/OTP flow runs against
// test doubles (SDD §2.2 amendment).
func (m *MockUpstream) SetLoginURL(u string) {
	m.mu.Lock()
	m.loginURL = u
	m.mu.Unlock()
}

type mockAccount struct {
	ph     string
	cookie string
	script []ScriptStep
}

// NewMockUpstream starts the mock. Call AddAccount for each scripted
// identity, then URL() for the client base.
func NewMockUpstream() *MockUpstream {
	m := &MockUpstream{
		accounts: map[string]*mockAccount{},
		byCookie: map[string]*mockAccount{},
		// Defaults mirror the real flows: one intermediate status then
		// success, the transcript a live run produced, and real WAV bytes.
		ttsStatuses: []string{"generating", "success"},
		asrStatuses: []string{"generating", "success"},
		asrText:     "你好，这是一次语音合成测试。",
		wav:         testWAV(),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

// AddAccount registers a scripted account identity. cookie is the full
// Cookie header value clients send; the mock routes on the serviceToken
// value inside it (or on ph).
func (m *MockUpstream) AddAccount(ph, cookie string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := &mockAccount{ph: ph, cookie: cookie}
	m.accounts[ph] = a
	m.byCookie[serviceTokenOf(cookie)] = a
}

// Script queues scripted responses for the account (consumed in order;
// the first step repeats when the script is exhausted — deterministic for
// multi-request tests).
func (m *MockUpstream) Script(ph string, steps ...ScriptStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.accounts[ph]; a != nil {
		a.script = append(a.script, steps...)
	}
}

// ResetScript clears queued steps for an account.
func (m *MockUpstream) ResetScript(ph string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.accounts[ph]; a != nil {
		a.script = nil
	}
}

// URL returns the mock's base URL for client construction.
func (m *MockUpstream) URL() string { return m.server.URL }

// Close shuts the server down.
func (m *MockUpstream) Close() { m.server.Close() }

// Requests returns all recorded chat requests in order.
func (m *MockUpstream) Requests() []RecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedRequest, len(m.requests))
	for i, r := range m.requests {
		out[i] = *r
	}
	return out
}

// RequestsFor returns the recorded requests whose ph matches.
func (m *MockUpstream) RequestsFor(ph string) []RecordedRequest {
	var out []RecordedRequest
	for _, r := range m.Requests() {
		if r.PH == ph {
			out = append(out, r)
		}
	}
	return out
}

// ChatHits counts recorded chat POSTs.
func (m *MockUpstream) ChatHits() int {
	n := 0
	for _, r := range m.Requests() {
		if strings.HasSuffix(r.Path, "/bot/chat") {
			n++
		}
	}
	return n
}

// serviceTokenOf extracts the serviceToken cookie value from a Cookie
// header.
func serviceTokenOf(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "serviceToken="); ok {
			return v
		}
	}
	return ""
}

// nextStep pops the next scripted step (first repeats when exhausted).
func (m *MockUpstream) nextStep(a *mockAccount) ScriptStep {
	if len(a.script) == 0 {
		return ScriptStep{Body: FixtureSSEBasic}
	}
	step := a.script[0]
	if len(a.script) > 1 {
		a.script = a.script[1:]
	}
	return step
}

// serve implements the mock routing.
func (m *MockUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/open-apis/user/mi/get" {
		m.serveValidate(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/bot/chat") {
		m.serveChat(w, r)
		return
	}
	if audioPath(r.URL.Path) {
		m.serveAudio(w, r)
		return
	}
	http.NotFound(w, r)
}

// serveValidate answers session-validation probes: a recognized cookie
// gets code 0; anything else gets 401 (TDD RENEW-009 handles business-code
// variants via Chat scripts, so this stays simple).
func (m *MockUpstream) serveValidate(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	_, known := m.byCookie[serviceTokenOf(r.Header.Get("Cookie"))]
	loginURL := m.loginURL
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if known {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"code":0,"data":{"userId":123456}}`)
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	if loginURL == "" {
		loginURL = "https://account.xiaomi.com/pass/serviceLogin?callback=cb&sid=xiaomichatbot"
	}
	io.WriteString(w, `{"code":401,"loginUrl":"`+loginURL+`"}`)
}

// serveChat records the request and serves the next scripted step.
func (m *MockUpstream) serveChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	r.Body.Close()

	var payload struct {
		MsgID            string  `json:"msgId"`
		ConvID           string  `json:"conversationId"`
		Query            string  `json:"query"`
		PreviousDialogID *string `json:"previousDialogueId"`
		ModelConfig      struct {
			Model          string   `json:"model"`
			EnableThinking bool     `json:"enableThinking"`
			Temperature    *float64 `json:"temperature"`
			TopP           *float64 `json:"topP"`
		} `json:"modelConfig"`
	}
	_ = json.Unmarshal(body, &payload)

	m.mu.Lock()
	ph := r.URL.Query().Get("xiaomichatbot_ph")
	a := m.accounts[ph]
	if a == nil {
		a = m.byCookie[serviceTokenOf(r.Header.Get("Cookie"))]
	}
	var step ScriptStep
	if a != nil {
		step = m.nextStep(a)
	}
	rec := &RecordedRequest{
		Method:         r.Method,
		Path:           r.URL.Path,
		PH:             ph,
		MsgID:          payload.MsgID,
		ConvID:         payload.ConvID,
		PrevDlg:        deref(payload.PreviousDialogID),
		Query:          payload.Query,
		Model:          payload.ModelConfig.Model,
		EnableThinking: payload.ModelConfig.EnableThinking,
		Temperature:    payload.ModelConfig.Temperature,
		TopP:           payload.ModelConfig.TopP,
		Cookie:         r.Header.Get("Cookie"),
		Headers:        r.Header.Clone(),
		RawBody:        string(body),
		At:             time.Now(),
	}
	m.requests = append(m.requests, rec)
	unknown := a == nil
	m.mu.Unlock()

	if unknown {
		http.Error(w, "unknown account", http.StatusUnauthorized)
		return
	}

	// Bare status responses (429/401/451/461/5xx).
	if step.Body == "" {
		code := step.StatusCode
		if code == 0 {
			code = http.StatusOK
		}
		for k, v := range step.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		return
	}

	// Pre-first-byte stall: watch for client disconnect (TDD API-024).
	if step.StallBefore > 0 {
		select {
		case <-time.After(step.StallBefore):
		case <-r.Context().Done():
			m.markCancelled(rec)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for k, v := range step.Headers {
		w.Header().Set(k, v)
	}
	status := step.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	if step.StallMidStream > 0 && step.MidStreamSplit > 0 && step.MidStreamSplit < len(step.Body) {
		io.WriteString(w, step.Body[:step.MidStreamSplit])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(step.StallMidStream):
		case <-r.Context().Done():
			m.markCancelled(rec)
			return
		}
		io.WriteString(w, step.Body[step.MidStreamSplit:])
	} else {
		io.WriteString(w, step.Body)
	}
	if step.AbortAfterBody {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close() // hard abort mid-stream (STICKY-009)
				return
			}
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *MockUpstream) markCancelled(rec *RecordedRequest) {
	m.mu.Lock()
	rec.Cancelled = true
	m.mu.Unlock()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// MockPassport mocks account.xiaomi.com + the STS follow: the login-context
// 401 with loginUrl, /pass/serviceLoginAuth2, the renewal
// /pass/serviceLogin, and the STS location handler planting Set-Cookie
// (TDD §2.2).
type MockPassport struct {
	mu       sync.Mutex
	server   *httptest.Server
	upstream string // mock aistudio base URL (for loginUrl callback)

	// Scriptable responses (XSSI fixtures by default).
	Auth2Body        string
	Auth2Status      int
	ServiceLoginBody string
	StsStatus        int

	// Recorded calls for assertions.
	Auth2Calls        []map[string]string // form values per call
	ServiceLoginCalls []string            // Cookie header per call
	StsCalls          []string            // locations followed

	// OTP identity flow (SDD §2.2 amendment). IdentityFlag: 4 = phone
	// (default), 8 = email. IdentityListBody: verbatim /identity/list
	// response (data-wrapped variants). OTPWrongCode: when true, the ticket
	// submit answers code 70002 (wrong/expired code) with no location.
	IdentityFlag     int
	IdentityNotify   string
	IdentityListBody string
	OTPWrongCode     bool
	IdentityCalls    []RecordedIdentityCall

	// OmitPassToken drops passToken/userId from the landing chain's cookies,
	// mirroring passport variants that plant the session but no renewal token.
	OmitPassToken bool

	// Cross-region consent chain (verified live 2026-10-09). ConsentChain
	// makes /pass2/redirect answer with the real 302 chain ending on
	// /fe/service/userCross; ConsentBody scripts the
	// /pass2/userCrossAgreement/update answer ("" = code 0 + location).
	ConsentChain bool
	ConsentBody  string
	ConsentCalls []map[string]string
}

// RecordedIdentityCall is one captured /identity/* request (OTP flow
// assertions: X-Requested-With header, form body, flag parsing).
type RecordedIdentityCall struct {
	Path    string // /identity/list, /identity/auth/verifyPhone, .../sendPhoneTicket, ...
	Method  string
	Query   string // raw query string
	Form    map[string]string
	Headers http.Header
}

// NewMockPassport starts the passport mock. upstreamURL is the mock
// aistudio base — loginUrl callbacks point there so the client's host
// checks pass while hitting test doubles.
func NewMockPassport(upstreamURL string) *MockPassport {
	p := &MockPassport{upstream: strings.TrimSuffix(upstreamURL, "/")}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	return p
}

// URL returns the passport mock's base URL (the account.xiaomi.com
// stand-in).
func (p *MockPassport) URL() string { return p.server.URL }

// Host returns the mock's hostname without port (tests inject it into the
// client's passport allowlist, which matches bare hostnames).
func (p *MockPassport) Host() string {
	s := p.URL()
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Close shuts the server down.
func (p *MockPassport) Close() { p.server.Close() }

// SetAuth2Response scripts the serviceLoginAuth2 response body (an XSSI
// fixture) and optional non-200 status.
func (p *MockPassport) SetAuth2Response(body string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Auth2Body = body
	p.Auth2Status = status
}

// SetServiceLoginResponse scripts the renewal serviceLogin response body.
func (p *MockPassport) SetServiceLoginResponse(body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ServiceLoginBody = body
}

// loginURL builds the loginUrl the aistudio 401 probe returns — pointing
// at THIS mock.
func (p *MockPassport) loginURL() string {
	return p.URL() + "/pass/serviceLogin?callback=" + p.upstream + "%2Fsts%3Fsign%3Dt%26followup%3D" + p.upstream + "%2F&sid=xiaomichatbot&qs=%3Fsid%3Dxiaomichatbot&_sign=sign123&serviceParam=%7B%7D&_group=DEFAULT"
}

// LoginURLFor returns that loginUrl (tests script the aistudio probe with
// it).
func (p *MockPassport) LoginURLFor() string { return p.loginURL() }

// serve routes the passport mock.
func (p *MockPassport) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/pass/serviceLoginAuth2":
		p.serveAuth2(w, r)
	case r.URL.Path == "/pass/serviceLogin":
		p.serveServiceLogin(w, r)
	case r.URL.Path == "/identity/result/check":
		http.Redirect(w, r, p.URL()+"/pass/serviceLoginAuth2/end?userId=123456&sid=xiaomichatbot&callback="+url.QueryEscape(p.upstream+"/sts"), http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/identity/"):
		p.serveIdentity(w, r)
	case r.URL.Path == "/pass2/userCrossAgreement/update":
		p.serveConsentAgree(w, r)
	case r.URL.Path == "/pass2/userinfo/start":
		p.serveConsentHop(w, r, p.URL()+"/fe/service/userCross?needClose=false&userId=123456")
	case r.URL.Path == "/pass2/userinfo/check":
		p.serveConsentHop(w, r, r.URL.Query().Get("callback"))
	case r.URL.Path == "/fe/service/userCross":
		p.serveConsentPage(w, r)
	case r.URL.Path == "/fe/service/identity/authStart":
		p.serveConsentPage(w, r)
	case r.URL.Path == "/pass/serviceLoginAuth2/end":
		http.SetCookie(w, &http.Cookie{Name: "identity_session", Value: "id-sess-1", Path: "/"})
		http.Redirect(w, r, p.URL()+"/sts?sign=mock", http.StatusFound)
	case r.URL.Path == "/pass2/redirect" && p.consentChain():
		p.serveConsentHop(w, r, p.URL()+"/pass2/userinfo/start?_locale=zh_CN&bizType=UserCross&callback=&sign=2%26V1_passport%26sig&token=tok-1&userId=123456")
	case strings.HasPrefix(r.URL.Path, "/pass2/"):
		p.serveNotification(w, r)
	case strings.HasSuffix(r.URL.Path, "/sts"):
		p.serveSTS(w, r)
	default:
		http.NotFound(w, r)
	}
}

// consentChain reports whether the consent chain is scripted (locked read).
func (p *MockPassport) consentChain() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ConsentChain
}

// SetConsentChain scripts the cross-region consent chain on /pass2/redirect.
func (p *MockPassport) SetConsentChain(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ConsentChain = on
}

// SetConsentBody scripts the /pass2/userCrossAgreement/update answer.
func (p *MockPassport) SetConsentBody(body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ConsentBody = body
}

// serveConsentHop is one 302 of the cross-region chain.
func (p *MockPassport) serveConsentHop(w http.ResponseWriter, r *http.Request, to string) {
	http.SetCookie(w, &http.Cookie{Name: "consentHop", Value: "hop-1", Path: "/"})
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusFound)
}

// serveConsentPage serves the SPA page the chain lands on (200, no
// Location — exactly what the real page does).
func (p *MockPassport) serveConsentPage(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "user_info_modification_session", Value: "mod-1", Path: "/"})
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, "<!doctype html><html id=\"root\"></html>")
}

// serveConsentAgree answers POST /pass2/userCrossAgreement/update; the
// default body is the live-captured shape (code 0 + a
// /pass2/userinfo/check location whose callback is the authStart URL).
func (p *MockPassport) serveConsentAgree(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	form := map[string]string{}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			form[k] = v[0]
		}
	}
	form["_referer"] = r.Header.Get("Referer")
	form["_xrw"] = r.Header.Get("X-Requested-With")
	p.ConsentCalls = append(p.ConsentCalls, form)
	body := p.ConsentBody
	p.mu.Unlock()
	if body == "" {
		cb := url.QueryEscape(p.URL() + "/fe/service/identity/authStart?sid=xiaomichatbot&context=ctx-uc-1")
		body = "&&&START&&&" + `{"code":0,"result":"ok","desc":"成功","location":"` +
			p.URL() + `/pass2/userinfo/check?callback=` + cb + `"}` + "\n"
	}
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, body)
}

// serveAuth2 parses the form post, records it, and serves the scripted
// response; the location inside is rewritten to this mock's STS endpoint.
func (p *MockPassport) serveAuth2(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	form := map[string]string{}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			form[k] = v[0]
		}
	}
	p.Auth2Calls = append(p.Auth2Calls, form)
	body := p.Auth2Body
	status := p.Auth2Status
	p.mu.Unlock()

	if body == "" {
		body = FixtureXSSILoginOK
	}
	if status == 0 {
		status = http.StatusOK
	}
	body = strings.ReplaceAll(body, "https://account.xiaomi.com/pass/sts", p.URL()+"/sts")
	// OTP fixtures carry the notificationUrl pointing at account.xiaomi.com —
	// rewrite to this mock so StartOTP runs against the double (SDD §2.2
	// amendment).
	body = strings.ReplaceAll(body, "https://account.xiaomi.com/pass2/redirect", p.URL()+"/pass2/redirect")
	w.Header().Set("Content-Type", "text/html")
	// Passport plants passToken + userId once the login is actually
	// completed — a code=0 verification demand (notificationUrl) does not
	// plant them, so a login that only reaches the OTP challenge has no
	// renewal token yet (live behaviour, 2026-10-09).
	if !strings.Contains(body, "notificationUrl") {
		http.SetCookie(w, &http.Cookie{Name: "passToken", Value: "rt-mock-1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "123456", Path: "/"})
	}
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// serveServiceLogin answers the renewal step-2 GET (loginUrl&_json=true).
func (p *MockPassport) serveServiceLogin(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.ServiceLoginCalls = append(p.ServiceLoginCalls, r.Header.Get("Cookie"))
	body := p.ServiceLoginBody
	p.mu.Unlock()
	if body == "" {
		body = FixtureXSSISTSOk
	}
	body = strings.ReplaceAll(body, "https://account.xiaomi.com/pass/sts", p.URL()+"/sts")
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, body)
}

// serveSTS plants the session cookies (SDD §2.2 step 3).
func (p *MockPassport) serveSTS(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.StsCalls = append(p.StsCalls, r.URL.String())
	status := p.StsStatus
	p.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "text/html")
	if !p.OmitPassToken {
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "123456", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "passToken", Value: "rt-mock-1", Path: "/"})
	}
	http.SetCookie(w, &http.Cookie{Name: "cUserId", Value: "cu-mock-1", Path: "/"})
	http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: "st-mock-1", Path: "/"})
	http.SetCookie(w, &http.Cookie{Name: "xiaomichatbot_serviceToken", Value: "st-mock-1", Path: "/"})
	// The upstream quotes these two (live capture); http.SetCookie would strip
	// the quotes, so write the headers verbatim.
	w.Header().Add("Set-Cookie", `xiaomichatbot_slh="slh-mock-1"; Path=/`)
	w.Header().Add("Set-Cookie", `xiaomichatbot_ph="cGgtbW9jaw=="; Path=/`)
	w.WriteHeader(status)
	io.WriteString(w, `{"code":0}`)
}

// serveNotification: step 1 of the OTP flow — GET the notificationUrl;
// plants a session cookie the later steps must echo back.
func (p *MockPassport) serveNotification(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "otpSession", Value: "otp-sess-1", Path: "/"})
	io.WriteString(w, `{"code":0}`)
}

// serveIdentity implements the /identity/* OTP endpoints (SDD §2.2
// amendment, reference login.mjs startOTP/otpSubmit):
//
//   - GET /identity/list — flag 4=phone / 8=email + masked notify;
//   - GET /identity/auth/verify{Phone,Email}?_flag — session trigger, code 0;
//   - POST /identity/auth/send{Phone,Email}Ticket — code 0 (the real one
//     fails email sends with 66108 without X-Requested-With; tests assert
//     the header instead);
//   - POST /identity/auth/verify{Phone,Email} (ticket submit) — location
//     redirect on success, code 70002 when OTPWrongCode is scripted.
func (p *MockPassport) serveIdentity(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	rec := RecordedIdentityCall{
		Path:    r.URL.Path,
		Method:  r.Method,
		Query:   r.URL.RawQuery,
		Form:    map[string]string{},
		Headers: r.Header.Clone(),
	}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			rec.Form[k] = v[0]
		}
	}
	p.IdentityCalls = append(p.IdentityCalls, rec)
	flag := p.IdentityFlag
	notify := p.IdentityNotify
	listBody := p.IdentityListBody
	wrongCode := p.OTPWrongCode
	p.mu.Unlock()

	if flag == 0 {
		flag = 4
	}
	if notify == "" {
		notify = "188****5616"
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/identity/list":
		if listBody != "" {
			io.WriteString(w, listBody)
			return
		}
		io.WriteString(w, `{"code":0,"flag":`+strconv.Itoa(flag)+`,"notify":`+strconv.Quote(notify)+`}`)
	case r.URL.Path == "/identity/auth/verifyPhone" || r.URL.Path == "/identity/auth/verifyEmail":
		if r.Method == http.MethodPost {
			// Ticket submit (step 5).
			if wrongCode {
				io.WriteString(w, `{"code":70002,"desc":"验证码不正确或已过期"}`)
				return
			}
			// Success: the live landing chain (2026-10-09) —
			// /identity/result/check → 302 → /pass/serviceLoginAuth2/end
			// → 302 → /sts (session cookies + passToken planted there).
			io.WriteString(w, `{"code":0,"location":"`+p.URL()+`/identity/result/check"}`)
			return
		}
		io.WriteString(w, `{"code":0}`)
	case r.URL.Path == "/identity/auth/sendPhoneTicket" || r.URL.Path == "/identity/auth/sendEmailTicket":
		// The real endpoint 66108s without X-Requested-With; assert the
		// header in tests rather than emulating the failure.
		io.WriteString(w, `{"code":0}`)
	case r.URL.Path == "/identity/auth/done":
		// Step 6 landing: plant the auth cookies (passToken + userId) —
		// what the real flow harvests after verify.
		http.SetCookie(w, &http.Cookie{Name: "passToken", Value: "rt-mock-1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "123456", Path: "/"})
		io.WriteString(w, `{"code":0}`)
	default:
		http.NotFound(w, r)
	}
}

// TestPH / TestCookie are shared test fixtures for a scripted account
// (base64 of "ph-mock").
const (
	TestPH     = "cGgtbW9jaw=="
	TestCookie = `userId=123456; serviceToken=st-mock-1; xiaomichatbot_serviceToken="st-mock-1"; xiaomichatbot_ph="cGgtbW9jaw=="`

	// TestPH2 / TestCookie2: a second scripted identity (base64 of
	// "ph-mock2") for multi-account tests (TDD NOSTICK-001, STICKY-008).
	TestPH2     = "cGgtbW9jazI="
	TestCookie2 = `userId=654321; serviceToken=st-mock-2; xiaomichatbot_serviceToken="st-mock-2"; xiaomichatbot_ph="cGgtbW9jazI="`
)
