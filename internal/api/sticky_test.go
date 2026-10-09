package api_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// STICKY-005/008..011: end-to-end stickiness integration — streaming turns
// chain onto stored conversations; failure paths respect the commit boundary
// (SDD §4.2/§4.3/§5.3/§7.2).

// warmSticky runs one streaming turn ("hello") on a fresh store and returns
// the server plus the account (PH) that served it — the sticky target for
// the follow-up request.
func warmSticky(t *testing.T, accts []testserver.TestAccount) (*testserver.TestServer, string) {
	t.Helper()
	ts := testserver.NewTestServer(t, accts, nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	if len(events) == 0 {
		t.Fatal("warm-up turn streamed nothing")
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("warm-up upstream calls = %d, want 1", len(reqs))
	}
	if ts.Sessions.Len() != 1 {
		t.Fatalf("warm-up stored %d entries, want 1", ts.Sessions.Len())
	}
	return ts, reqs[0].PH
}

// continuationBody is the follow-up history: warm turn + assistant reply +
// one new user message.
func continuationBody() string {
	return testutil.ChatBody("mimo-v2.6-pro", true,
		[2]string{"user", "hello"},
		[2]string{"assistant", "Hello world"},
		[2]string{"user", "what next"})
}

// waitFor polls cond until true or the deadline expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// STICKY-011: miss path — empty store, streaming request with 2 user
// messages → fresh 32-hex conversationId, full chain replay (2 upstream
// calls, 2nd chained via 1st's dialogId), entry stored with fingerprint +
// msg_count + previousDialogID (SDD §5.3).
func TestStickyMissPathMintAndStore(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true,
		[2]string{"user", "hello"}, [2]string{"user", "what is 2+2"}), "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	if len(events) == 0 {
		t.Fatal("no events streamed")
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (full replay)", len(reqs))
	}
	if !isHex32(reqs[0].ConvID) {
		t.Errorf("conversationId %q not 32-hex", reqs[0].ConvID)
	}
	if reqs[1].ConvID != reqs[0].ConvID {
		t.Errorf("replay broke conversation chaining: %q vs %q", reqs[1].ConvID, reqs[0].ConvID)
	}
	// The fixture's dialogId is dlg-0001 → 2nd turn's previousDialogueId.
	if reqs[1].PrevDlg != "dlg-0001" {
		t.Errorf("2nd turn previousDialogueId = %q, want dlg-0001", reqs[1].PrevDlg)
	}
	// Entry stored with the frontier + anchor.
	entries := ts.Sessions.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("stored entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ConversationID != reqs[0].ConvID {
		t.Errorf("entry conversationId = %q, want %q", e.ConversationID, reqs[0].ConvID)
	}
	if e.PreviousDialogID != "dlg-0001" {
		t.Errorf("entry previousDialogID = %q", e.PreviousDialogID)
	}
	if e.MsgCount != 2 {
		t.Errorf("entry msg_count = %d, want 2", e.MsgCount)
	}
	if e.Fingerprint == "" {
		t.Error("entry fingerprint empty")
	}
}

// STICKY-005: hit path — same account + same conversationId, exactly 1 new
// upstream call carrying only the delta message, previousDialogueId == the
// stored turn's dialogId (SDD §4.2, §5.3).
func TestStickyHitPathDeltaOnly(t *testing.T) {
	ts, stickyPH := warmSticky(t, oneAccount())
	firstReqs := ts.Upstream.Requests()
	firstConv := firstReqs[0].ConvID

	resp, err := ts.Chat(continuationBody(), "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	if len(events) == 0 {
		t.Fatal("no events streamed")
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want exactly 1 new (delta-only)", len(reqs)-1)
	}
	delta := reqs[1]
	if delta.PH != stickyPH {
		t.Errorf("delta call used account %q, want sticky %q", delta.PH, stickyPH)
	}
	if delta.ConvID != firstConv {
		t.Errorf("delta call conversationId = %q, want %q", delta.ConvID, firstConv)
	}
	if delta.Query != "what next" {
		t.Errorf("delta call query = %q, want only the new message", delta.Query)
	}
	if delta.PrevDlg != "dlg-0001" {
		t.Errorf("delta call previousDialogueId = %q, want dlg-0001", delta.PrevDlg)
	}
}

// STICKY-008: pre-first-byte failure → sticky entry discarded, retry on a
// different account with fresh conversationId + full replay, client gets a
// clean 200 stream, failed account cools down (SDD §4.3, §7.2).
func TestStickyPreFirstByteFailureSwitches(t *testing.T) {
	ts, stickyPH := warmSticky(t, twoAccounts())
	firstConv := ts.Upstream.Requests()[0].ConvID
	// The sticky account 502s before any SSE byte.
	ts.Upstream.Script(stickyPH, testutil.ScriptStep{StatusCode: http.StatusBadGateway})

	resp, err := ts.Chat(continuationBody(), "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d body=%s, want clean 200 via account switch", resp.StatusCode, b)
	}
	events, _ := testutil.ReadSSE(t, resp)
	if len(events) == 0 {
		t.Fatal("no events streamed")
	}

	reqs := ts.Upstream.Requests()
	// warm(1) + sticky delta attempt 502(1) + full replay on the other
	// account (2 user turns).
	if len(reqs) != 4 {
		t.Fatalf("upstream calls = %d, want 4", len(reqs))
	}
	other := reqs[2]
	if other.PH == stickyPH {
		t.Error("retry reused the failed sticky account")
	}
	if other.ConvID == firstConv {
		t.Error("retry reused the sticky conversationId — must be fresh")
	}
	if other.PrevDlg != "" {
		t.Errorf("retry first turn previousDialogueId = %q, want null", other.PrevDlg)
	}
	if reqs[3].PrevDlg != "dlg-0001" || reqs[3].ConvID != other.ConvID {
		t.Errorf("retry replay not chained: %+v", reqs[3])
	}
	// The failed account cools down; the sticky entry was discarded.
	stickyID := accountIDForPH(t, ts, stickyPH)
	if ts.Pool.StateOf(stickyID) != pool.StateCooldown {
		t.Errorf("sticky account state = %v, want cooldown", ts.Pool.StateOf(stickyID))
	}
	if ts.Sessions.Len() != 1 {
		t.Errorf("entries = %d, want 1 (old sticky discarded, retry stored fresh)", ts.Sessions.Len())
	}
}

// STICKY-009: post-first-byte failure commit boundary — no account switch,
// no replay, no restarted deltas; stream terminates with error content
// delta + finish_reason:"stop" + [DONE]; stale sticky entry deleted (SDD
// §4.3, §7.2).
func TestStickyPostFirstByteAbortCommits(t *testing.T) {
	ts, stickyPH := warmSticky(t, twoAccounts())
	// The sticky account streams 2 deltas then errors mid-stream.
	ts.Upstream.Script(stickyPH, testutil.ScriptStep{Body: testutil.FixtureSSEError})

	resp, err := ts.Chat(continuationBody(), "")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := testutil.ReadSSE(t, resp)
	var content strings.Builder
	sawFinish, sawDone, sawErr := false, false, false
	chatID := ""
	for i, e := range events {
		if e == "[DONE]" {
			sawDone = true
			continue
		}
		c := testutil.ParseChunk(t, e)
		if i == 0 {
			chatID = c.ID
			continue
		}
		if c.ID != chatID {
			t.Errorf("chunk id restarted mid-error: %q vs %q", c.ID, chatID)
		}
		if len(c.Choices) == 0 {
			continue
		}
		if c.Choices[0].FinishReason != nil && *c.Choices[0].FinishReason == "stop" {
			sawFinish = true
			continue
		}
		if strings.Contains(c.Choices[0].Delta.Content, "[upstream error]") {
			sawErr = true
		}
		content.WriteString(c.Choices[0].Delta.Content)
	}
	if !sawErr || !sawFinish || !sawDone {
		t.Errorf("err/finish/done = %v/%v/%v; streamed = %q", sawErr, sawFinish, sawDone, content.String())
	}
	// No account switch, no replay: exactly 1 new upstream call.
	reqs := ts.Upstream.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (no retry after commit)", len(reqs))
	}
	if reqs[1].PH != stickyPH {
		t.Error("committed stream must not switch accounts")
	}
	// Stale sticky entry deleted.
	if ts.Sessions.Len() != 0 {
		t.Errorf("entries = %d, want 0 (stale sticky deleted)", ts.Sessions.Len())
	}
	// Committed mid-stream failure: no pool penalty on a stream error.
	stickyID := accountIDForPH(t, ts, stickyPH)
	if ts.Pool.StateOf(stickyID) != pool.StateAvailable {
		t.Errorf("sticky account state = %v, want available (no penalty)", ts.Pool.StateOf(stickyID))
	}
}

// STICKY-010: client disconnect mid-stream with dialogId already received →
// upstream aborted, sticky entry kept (context advanced), neither success
// nor failure, pool slot released (SDD §7.3).
func TestStickyClientDisconnectKeepsEntry(t *testing.T) {
	ts, stickyPH := warmSticky(t, oneAccount())
	stickyID := accountIDForPH(t, ts, stickyPH)
	// Stall mid-stream after dialogId + first delta so the client can
	// receive a byte and then cancel.
	ts.Upstream.Script(stickyPH, testutil.ScriptStep{
		Body:           testutil.FixtureSSEStall,
		StallMidStream: 3 * time.Second,
		MidStreamSplit: 134,
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL()+"/v1/chat/completions",
		strings.NewReader(continuationBody()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Read until the role chunk arrives (dialogId already parsed by then).
	br := bufio.NewReader(resp.Body)
	gotRole := false
	for !gotRole {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatalf("reading stream before cancel: %v", rerr)
		}
		if strings.Contains(line, "assistant") {
			gotRole = true
		}
	}
	// Cancel the client request mid-stall.
	cancel()

	// The handler releases the slot and keeps the entry with the advanced
	// anchor (dialogId captured pre-disconnect).
	if !waitFor(t, 3*time.Second, func() bool {
		for _, r := range ts.Upstream.Requests() {
			if r.Cancelled {
				return true
			}
		}
		return false
	}) {
		t.Error("upstream request was never aborted by the disconnect")
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return ts.Sessions.Len() == 1
	}) {
		t.Fatalf("sticky entry lost on disconnect; entries = %d", ts.Sessions.Len())
	}
	entries := ts.Sessions.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].PreviousDialogID != "dlg-0001" {
		t.Errorf("entry anchor = %q, want dlg-0001 (advanced mid-stream)", entries[0].PreviousDialogID)
	}
	// Neither success nor failure: account stays available.
	if ts.Pool.StateOf(stickyID) != pool.StateAvailable {
		t.Errorf("account state = %v, want available (disconnect is neutral)", ts.Pool.StateOf(stickyID))
	}
	// Slot released: a follow-up request acquires immediately.
	resp2, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("follow-up request status = %d — slot never released", resp2.StatusCode)
	}
}

// accountIDForPH maps a scripted PH back to its TestAccount ID.
func accountIDForPH(t *testing.T, ts *testserver.TestServer, ph string) string {
	t.Helper()
	for _, id := range ts.Pool.IDs() {
		if acct, ok := ts.Store.Get(id); ok && acct.Session != nil && acct.Session.PH == ph {
			return id
		}
	}
	t.Fatalf("no account for ph %q", ph)
	return ""
}
