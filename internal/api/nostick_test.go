package api_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// NOSTICK-001..004: non-streaming requests never touch the sticky store —
// every turn is a fresh conversation (SDD §4.2).

// NOSTICK-001: identical history ≠ same conversation — two byte-identical
// non-streaming requests land on different accounts (round-robin) with
// different upstream conversationIds.
func TestNostickIdenticalHistoryDifferentConversation(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	body := testutil.ChatBody("mimo-v2.6-pro", false,
		[2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "what's 2+2"})
	for i := 0; i < 2; i++ {
		resp, err := ts.Chat(body, "")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("request %d: status = %d body=%s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 4 { // 2 user turns × 2 requests (assistant history not re-sent, SDD §5.3B)
		t.Fatalf("upstream requests = %d, want 4", len(reqs))
	}
	// Different accounts per request (round-robin band).
	if reqs[0].PH == reqs[2].PH {
		t.Errorf("identical history reused account %q", reqs[0].PH)
	}
	// Different fresh conversationIds per request.
	if reqs[0].ConvID == reqs[2].ConvID {
		t.Errorf("identical history reused conversationId %q", reqs[0].ConvID)
	}
}

// NOSTICK-002: fresh conversationId every time — 5 non-streaming requests
// (mixed histories) produce 5 distinct 32-hex conversationIds.
func TestNostickFreshConversationIDs(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	for i := 0; i < 5; i++ {
		body := testutil.ChatBody("mimo-v2.6-pro", false,
			[2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "what's 2+2"})
		resp, err := ts.Chat(body, "")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("request %d: status = %d body=%s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 10 { // 2 user turns × 5 requests (assistant history not re-sent)
		t.Fatalf("upstream requests = %d, want 10", len(reqs))
	}
	// Every request's FIRST turn carries a distinct 32-hex conversationId;
	// turns within one request share it (chaining happens inside the
	// request's own replay, SDD §5.3).
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		first := reqs[i*2]
		if seen[first.ConvID] {
			t.Errorf("conversationId %q reused across requests", first.ConvID)
		}
		seen[first.ConvID] = true
		if !isHex32(first.ConvID) {
			t.Errorf("conversationId %q is not 32-char hex", first.ConvID)
		}
		if reqs[i*2+1].ConvID != first.ConvID {
			t.Errorf("turn 2 of request %d broke conversation chaining", i)
		}
	}
}

// NOSTICK-003: the store is never consulted for non-streaming requests —
// size unchanged, zero lookups.
func TestNostickStoreUntouched(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	before := ts.Sessions.Len()
	lookupsBefore := ts.Sessions.Lookups()
	for i := 0; i < 3; i++ {
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if after := ts.Sessions.Len(); after != before {
		t.Errorf("store size changed: %d → %d", before, after)
	}
	if n := ts.Sessions.Lookups() - lookupsBefore; n != 0 {
		t.Errorf("non-streaming performed %d store lookups, want 0", n)
	}
}

// NOSTICK-004: no cross-request chaining — both requests' first upstream
// turn carries previousDialogueId null; chaining only inside a request's
// own replay.
func TestNostickNoCrossRequestChaining(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	hist := testutil.ChatBody("mimo-v2.6-pro", false,
		[2]string{"user", "first question"}, [2]string{"user", "second question"})
	for i := 0; i < 2; i++ {
		resp, err := ts.Chat(hist, "")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("request %d: status = %d body=%s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 4 {
		t.Fatalf("upstream requests = %d, want 4", len(reqs))
	}
	// First turn of EACH request: previousDialogueId null.
	for i := 0; i < 2; i++ {
		if r := reqs[i*2]; r.PrevDlg != "" {
			t.Errorf("request %d first turn carried previousDialogueId %q — must be null", i, r.PrevDlg)
		}
	}
	// Second turn of each request chains to its own first turn's dialogId —
	// same-request chaining only (SDD §5.3).
	for i := 0; i < 2; i++ {
		if r := reqs[i*2+1]; r.PrevDlg == "" {
			t.Errorf("request %d second turn: previousDialogueId missing (intra-request chaining)", i)
		}
	}
}

// isHex32 reports whether s is 32 lowercase hex chars (uuid4 sans dashes,
// SDD §2.4).
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
