package mimo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/testutil"
)

const testPH = "cGgtbW9jaw=="

const testCookie = `userId=123456; serviceToken=st-mock-1; xiaomichatbot_serviceToken="st-mock-1"; xiaomichatbot_ph="cGgtbW9jaw=="`

// chatEnv boots a MockUpstream with one scripted account and a client
// pointed at it.
func chatEnv(t *testing.T) (*Client, *testutil.MockUpstream) {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	up.AddAccount(testPH, testCookie)
	client := New(Options{Base: up.URL()})
	return client, up
}

// Upstream payload shaping (SDD §2.4): msgId 32-hex, multiMedias []
// never null, browser header set, ph query param, cookie.
func TestChatPayloadShaping(t *testing.T) {
	client, up := chatEnv(t)
	prev := "dlg-prev-0000000000000001"
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query:            "hi",
		Model:            "mimo-128k",
		ConversationID:   "conv-0000000000000000000000000001",
		PreviousDialogID: prev,
		EnableThinking:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := up.RequestsFor(testPH)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if !isHex32(r.MsgID) {
		t.Errorf("msgId = %q, want 32-hex", r.MsgID)
	}
	if r.ConvID != "conv-0000000000000000000000000001" {
		t.Errorf("conversationId = %q", r.ConvID)
	}
	if r.PrevDlg != prev {
		t.Errorf("previousDialogueId = %q, want %q", r.PrevDlg, prev)
	}
	if r.Model != "mimo-128k" || !r.EnableThinking {
		t.Errorf("modelConfig = %s thinking=%v", r.Model, r.EnableThinking)
	}
	if !strings.Contains(r.RawBody, `"multiMedias":[]`) {
		t.Errorf("multiMedias must be [] (never null): %s", r.RawBody)
	}
	if !strings.Contains(r.RawBody, `"isEditedQuery":false`) {
		t.Errorf("isEditedQuery must be false: %s", r.RawBody)
	}
	if !strings.Contains(r.RawBody, `"webSearchStatus":"disabled"`) {
		t.Errorf("webSearchStatus must be disabled: %s", r.RawBody)
	}
	// Headers (SDD §2.4).
	h := r.Headers
	if h.Get("Cookie") != testCookie {
		t.Errorf("cookie = %q", h.Get("Cookie"))
	}
	if h.Get("Accept-Language") != "zh-CN" || h.Get("x-timeZone") == "" {
		t.Errorf("accept-language/x-timeZone missing: %v", h)
	}
	if h.Get("Origin") == "" || h.Get("Referer") == "" || h.Get("User-Agent") == "" {
		t.Errorf("origin/referer/UA missing: %v", h)
	}
	if h.Get("sec-ch-ua") == "" || h.Get("sec-fetch-site") == "" {
		t.Errorf("sec-ch-ua/sec-fetch set missing: %v", h)
	}
	if r.PH != testPH {
		t.Errorf("xiaomichatbot_ph query param = %q, want %q", r.PH, testPH)
	}
}

// isHex32 pins the 32-hex id shape without regexp.
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// First turn (no previousDialogueId) sends null.
func TestChatFirstTurnNullPrev(t *testing.T) {
	client, up := chatEnv(t)
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := up.RequestsFor(testPH)[0]
	if !strings.Contains(r.RawBody, `"previousDialogueId":null`) {
		t.Errorf("first turn must send null: %s", r.RawBody)
	}
	if r.PrevDlg != "" {
		t.Errorf("PrevDlg = %q, want empty-for-null", r.PrevDlg)
	}
}

// temperature/topP: present when given, omitted when not (TDD API-022).
func TestChatSamplingParams(t *testing.T) {
	client, up := chatEnv(t)
	hot := 0.9
	tp := 0.8
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo", Temperature: &hot, TopP: &tp,
	}); err != nil {
		t.Fatal(err)
	}
	r := up.RequestsFor(testPH)[0]
	if r.Temperature == nil || *r.Temperature != 0.9 {
		t.Errorf("temperature = %v, want 0.9", r.Temperature)
	}
	if r.TopP == nil || *r.TopP != 0.8 {
		t.Errorf("topP = %v, want 0.8", r.TopP)
	}

	// Absent: field omitted entirely from the JSON.
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo",
	}); err != nil {
		t.Fatal(err)
	}
	r2 := up.RequestsFor(testPH)[1]
	if strings.Contains(r2.RawBody, "temperature") || strings.Contains(r2.RawBody, "topP") {
		t.Errorf("sampling fields must be omitted when unset: %s", r2.RawBody)
	}
}

// Fastchat routing: ultraspeed models hit /fastchat/open-apis/bot/chat
// (SDD §2.4).
func TestChatFastchatRouting(t *testing.T) {
	client, up := chatEnv(t)
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "ultraspeed", Fastchat: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := up.RequestsFor(testPH)[0]
	if !strings.HasPrefix(r.Path, "/fastchat/") {
		t.Errorf("path = %q, want /fastchat/ prefix", r.Path)
	}
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	}); err != nil {
		t.Fatal(err)
	}
	if r2 := up.RequestsFor(testPH)[1]; strings.HasPrefix(r2.Path, "/fastchat/") {
		t.Errorf("normal model path = %q, must not be fastchat", r2.Path)
	}
}

// Full happy-path result: dialogId captured, content assembled, usage
// attached (SDD §2.4/§2.5).
func TestChatResult(t *testing.T) {
	client, _ := chatEnv(t)
	res, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.DialogID != "dlg-0001" {
		t.Errorf("dialogID = %q, want dlg-0001", res.DialogID)
	}
	if res.Content != "Hello world" {
		t.Errorf("content = %q", res.Content)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 2242 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// SSE-013: empty response (zero message deltas) → ErrEmptyResponse. The
// retry itself (fresh msgId + fresh conversationId, exactly once) is the
// API layer's job (SDD §7.1); what this pins is that every Chat call
// mints a fresh msgId so a caller-driven retry is never deduped upstream
// (SDD §2.4).
func TestChatEmptyResponse(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{Body: testutil.FixtureSSEEmpty})

	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("err = %v, want ErrEmptyResponse", err)
	}

	// The caller-driven retry also gets ErrEmptyResponse (script repeats),
	// with a fresh msgId.
	_, err = client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("second call err = %v, want ErrEmptyResponse", err)
	}
	reqs := up.RequestsFor(testPH)
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].MsgID == reqs[1].MsgID {
		t.Errorf("msgId reused across calls: %q — upstream dedupes by msgId (SDD §2.4)", reqs[0].MsgID)
	}
	if reqs[0].ConvID != reqs[1].ConvID {
		// Both calls minted fresh conversationIds (caller passed none) —
		// fine either way; the pin is that a retry CAN pass a fresh one.
		t.Logf("conversationIds: %q vs %q (both fresh-minted)", reqs[0].ConvID, reqs[1].ConvID)
	}
}

// SSE-014: idle-stall watchdog — the upstream stalls mid-stream; with a
// 200ms IdleTimeout the Chat call aborts with ErrIdleStall in ~200ms,
// not after the stall (SDD §7.3).
func TestChatIdleStall(t *testing.T) {
	client, up := chatEnv(t)
	_ = client
	up.ResetScript(testPH)
	fixture := testutil.FixtureSSEStall
	split := strings.Index(fixture, "event:message")
	if split <= 0 {
		t.Fatal("fixture lacks a message boundary")
	}
	up.Script(testPH, testutil.ScriptStep{
		Body:           fixture,
		StallMidStream: 5 * time.Second,
		MidStreamSplit: split,
	})
	small := New(Options{Base: up.URL(), IdleTimeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := small.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrIdleStall) {
		t.Fatalf("err = %v, want ErrIdleStall", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("watchdog fired late: %v", elapsed)
	}
}

// SSE-014 companion (ADR 0036): silence **before any byte** is our own patience
// running out, not a stall. It surfaces as ErrSilentUpstream so the API layer
// answers with our own 504 and leaves the account alone; a slow search must not
// cool a healthy account. Silence *after* the upstream started answering stays
// ErrIdleStall (transport class, POOLATTR-012).
func TestChatSilentUpstreamIsItsOwnBound(t *testing.T) {
	_, up := chatEnv(t)
	up.ResetScript(testPH)
	up.Script(testPH, testutil.ScriptStep{
		Body:        testutil.FixtureSSEBasic,
		StallBefore: 3 * time.Second, // nothing on the wire, not even headers
	})
	small := New(Options{Base: up.URL(), IdleTimeout: 200 * time.Millisecond})

	start := time.Now()
	_, err := small.Chat(context.Background(), testCookie, testPH, ChatRequest{Query: "hi", Model: "mimo-128k"})
	if !errors.Is(err, ErrSilentUpstream) {
		t.Fatalf("err = %v, want ErrSilentUpstream", err)
	}
	if errors.Is(err, ErrIdleStall) {
		t.Error("pre-first-byte silence must not surface as the stall error: the two carry opposite pool decisions")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the pre-header window fired late: %v", elapsed)
	}
}

// SSE-014 companion (ADR 0036): the reader reports activity on every byte even
// when no window is set — "no window" and "no activity signal" are different
// things, and the lease's moving expiry depends on the second.
func TestIdleReaderReportsActivityWithoutAWindow(t *testing.T) {
	r := newIdleWatchedReader(strings.NewReader("abc"), func() {}, 0)
	fired := 0
	r.onActivity = func() { fired++ }
	if _, err := r.Read(make([]byte, 8)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if fired != 1 {
		t.Errorf("activity callbacks = %d, want 1 (it must fire with no window)", fired)
	}
}

// SSE-014 companion: a normally-paced stream never trips the watchdog —
// reads reset the timer.
func TestChatIdleWatchdogNoFalseTrip(t *testing.T) {
	client, _ := chatEnv(t)
	res, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Hello world" {
		t.Errorf("content = %q", res.Content)
	}
}

// RENEW-008 client half: a 401 chat response is auth-expired
// pre-first-byte; Chat itself performs no refresh (retry policy is the
// API layer's, SDD §7.2).
func TestChat401AuthExpired(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{StatusCode: 401})
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	var authExpiredIf interface{ IsAuthExpired() bool }
	if !errors.As(err, &authExpiredIf) {
		t.Fatalf("err = %v, want auth-expired", err)
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 401 {
		t.Errorf("wrapped UpstreamError with 401 not found: %v", err)
	}
	if up.ChatHits() != 1 {
		t.Errorf("chat hits = %d, want 1 (no internal retry)", up.ChatHits())
	}
}

// Other non-200 statuses surface as plain UpstreamError (429/5xx drive
// pool ladders upstream of this layer).
func TestChatNon200UpstreamError(t *testing.T) {
	client, up := chatEnv(t)
	up.Script(testPH, testutil.ScriptStep{StatusCode: 429, Headers: map[string]string{"Retry-After": "60"}})
	_, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-128k",
	})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 429 {
		t.Fatalf("err = %v, want UpstreamError 429", err)
	}
	var notAuthIf interface{ IsAuthExpired() bool }
	if errors.As(err, &notAuthIf) {
		t.Error("429 must not be auth-expired")
	}
}

// OnPiece/OnFirstByte wiring through Chat: pieces stream incrementally,
// first byte fires once — the commit boundary (SDD §7.2).
func TestChatStreamingCallbacks(t *testing.T) {
	client, _ := chatEnv(t)
	var pieces []adapter.Piece
	first := 0
	res, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query:       "hi",
		Model:       "mimo-128k",
		OnPiece:     func(p adapter.Piece) error { pieces = append(pieces, p); return nil },
		OnFirstByte: func() { first++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Errorf("onFirstByte calls = %d, want 1", first)
	}
	var joined strings.Builder
	for _, p := range pieces {
		joined.WriteString(p.Text)
	}
	if joined.String() != res.Content {
		t.Errorf("streamed pieces %q != final content %q", joined.String(), res.Content)
	}
}
