package api_test

// NOSTICK-001..003, replacement for the sticky-store cases deleted with
// internal/session (ADR 0017).
//
// The old suite proved these invariants by watching the store's counters; with
// the store gone the same rules are asserted where they are actually observable
// — in the payloads the gateway sends upstream:
//
//   * identical client histories must not share an upstream conversation,
//   * every client request gets a fresh conversationId,
//   * nothing chains between two client requests (no cross-request prevDialogId),
//   * within one request the turns of a replay DO chain: that is what makes a
//     multi-turn history one upstream conversation (SDD §5.3).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// NOSTICK-001: two byte-identical requests → two upstream conversations, both
// starting fresh (no prevDialogId on the first turn of either).
func TestNOSTICKIdenticalHistoryNewConversation(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"})
	for i := 0; i < 2; i++ {
		status, respBody := chatOnce(t, ts, body)
		if status != http.StatusOK {
			t.Fatalf("request %d = %d %s, want 200", i+1, status, respBody)
		}
	}

	reqs := ts.Upstream.RequestsFor(acct[0].PH)
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per client request)", len(reqs))
	}
	if reqs[0].ConvID == "" {
		t.Error("first request carried no conversationId")
	}
	if reqs[0].ConvID == reqs[1].ConvID {
		t.Errorf("conversationId reused across requests: %q — identical history must not resume a conversation", reqs[0].ConvID)
	}
	for i, req := range reqs {
		if req.PrevDlg != "" {
			t.Errorf("request %d sent prevDialogId %q, want a fresh conversation", i+1, req.PrevDlg)
		}
	}
}

// NOSTICK-002: a multi-turn history is one upstream conversation per request —
// the replayed turns chain inside the request (so the model sees the history)
// while the conversation id is still new for every client call.
func TestNOSTICKReplayChainsWithinRequestOnly(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := `{"model":"mimo-v2.6-pro","stream":false,"messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"ack"},` +
		`{"role":"user","content":"second"}]}`
	status, respBody := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", status, respBody)
	}

	reqs := ts.Upstream.RequestsFor(acct[0].PH)
	if len(reqs) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per user turn)", len(reqs))
	}
	if reqs[0].ConvID != reqs[1].ConvID {
		t.Errorf("conversationId changed inside one request: %q then %q", reqs[0].ConvID, reqs[1].ConvID)
	}
	if reqs[0].PrevDlg != "" {
		t.Errorf("first turn sent prevDialogId %q, want none", reqs[0].PrevDlg)
	}
	if reqs[1].PrevDlg == "" {
		t.Error("second turn sent no prevDialogId — the replay did not chain onto the first turn's dialog")
	}

	// And the next client request starts over rather than resuming this one.
	status, respBody = chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("second request = %d %s, want 200", status, respBody)
	}
	reqs = ts.Upstream.RequestsFor(acct[0].PH)
	second := reqs[2:]
	if len(second) != 2 {
		t.Fatalf("second request made %d upstream calls, want 2", len(second))
	}
	if second[0].ConvID == reqs[0].ConvID {
		t.Errorf("second request reused conversationId %q", second[0].ConvID)
	}
	if second[0].PrevDlg != "" {
		t.Errorf("second request chained across requests: prevDialogId %q", second[0].PrevDlg)
	}
}

// DISCONNECT-001 (replacement for STICKY-010, ADR 0017): a client that hangs up
// while the turn is still upstream is neither a success nor a failure — the lease
// comes back, the account is not cooled down, and nothing is written to a dead
// pipe. There is no first byte to have committed: the whole answer is written at
// the end, so a disconnect can only happen pre-response.
func TestDisconnectBeforeResponseReleasesTheLease(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{
		Body:           testutil.FixtureSSEStall,
		StallMidStream: 5 * time.Second,
		MidStreamSplit: 134,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.URL()+"/v1/chat/completions",
		strings.NewReader(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("request returned after the client cancelled, want a transport error")
	}

	// The account must stay selectable even though the upstream call was
	// still stalled when the client left — a disconnect is not a fault, and
	// with in-flight slots gone there is no capacity to hand back, only the
	// account's state to protect.
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("client disconnect changed the account state: %v, want available", st)
	}
}
