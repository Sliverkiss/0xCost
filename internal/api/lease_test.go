package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// ADR 0019: a real client-facing deadline, a per-turn upstream deadline, and the
// slot-hold policy. These cases assert the four things an operator can act on:
// the status, the code, the numbers in the message, and the account's state.

// TIMEOUT-001: the request budget aborts a request the upstream is still
// stalling on, answers 504 chat_timeout with the budget in the message, and
// leaves the account alone — our clock is not the account's fault.
func TestRequestBudgetReturnsChatTimeout(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
		RequestTimeout: 1 * time.Second,
		LeaseTTL:       30 * time.Second, // the per-turn TTL is not what fires here
	})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{
		Body:        testutil.FixtureSSEBasic,
		StallBefore: 4 * time.Second,
	})

	start := time.Now()
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	elapsed := time.Since(start)

	status, msg, typ, code := readErr(t, resp)
	if status != http.StatusGatewayTimeout || code != "chat_timeout" || typ != "api_error" {
		t.Fatalf("got %d/%s/%s (%s), want 504 api_error chat_timeout", status, typ, code, msg)
	}
	if elapsed > 3*time.Second {
		t.Errorf("answered after %s: the budget was 1s, so the stall was not cut short", elapsed)
	}
	if !strings.Contains(msg, "1s") || !strings.Contains(msg, "MIMO2API_REQUEST_TIMEOUT_MS") {
		t.Errorf("message %q must name the budget and the knob", msg)
	}
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("account state = %v after a gateway timeout, want available (no cooldown)", st)
	}
	if n := ts.Pool.InFlight(); n != 0 {
		t.Errorf("in-flight slots = %d after the timeout, want 0 (the slot must come back)", n)
	}
}

// TIMEOUT-002: the per-turn lease TTL is the other half — a turn that outlives
// its TTL is cut with the same client-facing shape, and the message names the
// smaller of the two budgets (the one that actually bit).
func TestLeaseTTLReturnsChatTimeout(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
		RequestTimeout: 30 * time.Second,
		LeaseTTL:       1 * time.Second,
	})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{
		Body:        testutil.FixtureSSEBasic,
		StallBefore: 4 * time.Second,
	})

	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	status, msg, _, code := readErr(t, resp)
	if status != http.StatusGatewayTimeout || code != "chat_timeout" {
		t.Fatalf("got %d/%s (%s), want 504 chat_timeout", status, code, msg)
	}
	if !strings.Contains(msg, "1s") || !strings.Contains(msg, "LEASE_TTL") {
		t.Errorf("message %q must name the lease TTL (the smaller budget)", msg)
	}
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("account state = %v, want available", st)
	}
}

// SLOT-001 (first-byte policy): with ONE in-flight slot per account, a second
// request can only be served while the first is still generating if the first
// gave its slot back at the first byte.
func TestFirstBytePolicyFreesCapacityMidGeneration(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
		MaxInflight: 1,
		QueueWait:   300 * time.Millisecond,
		SlotHold:    "first-byte",
		LeaseTTL:    30 * time.Second,
	})
	ts.Upstream.Script(acct[0].PH,
		testutil.ScriptStep{
			Body:           testutil.FixtureSSEStall,
			StallMidStream: 900 * time.Millisecond,
			MidStreamSplit: 134,
		},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
	)

	type result struct {
		status int
		code   string
		err    error
	}
	first := make(chan result, 1)
	go func() {
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "stall"}), "")
		if err != nil {
			first <- result{err: err}
			return
		}
		status, _, _, code := readErr(t, resp)
		first <- result{status: status, code: code}
	}()

	time.Sleep(300 * time.Millisecond) // the first request has its first byte and is stalled
	start := time.Now()
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "second"}), "")
	if err != nil {
		t.Fatalf("second request could not be served at all: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		status, _, _, code := readErr(t, resp)
		t.Fatalf("second request got %d/%s while the first was still generating: "+
			"the first-byte policy did not return its slot", status, code)
	}
	resp.Body.Close()
	if waited := time.Since(start); waited > 700*time.Millisecond {
		t.Errorf("second request waited %s; the first was stalled for 900ms in total", waited)
	}

	select {
	case got := <-first:
		if got.err != nil {
			t.Fatalf("first request failed: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("first request ended %d/%s, want 200 — releasing the slot early must not abandon its own answer", got.status, got.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request never finished")
	}
	if n := ts.Pool.InFlight(); n != 0 {
		t.Errorf("in-flight slots = %d after both requests, want 0", n)
	}
}

// SLOT-002 (default last-byte policy): the same setup must NOT serve a second
// request mid-generation — the slot means "this account is working on this
// request", which is the conservative default (ADR 0019).
func TestLastBytePolicyHoldsTheSlotUntilTheEnd(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
		MaxInflight: 1,
		QueueWait:   300 * time.Millisecond,
		// SlotHold unset: last-byte.
		LeaseTTL: 30 * time.Second,
	})
	ts.Upstream.Script(acct[0].PH,
		testutil.ScriptStep{
			Body:           testutil.FixtureSSEStall,
			StallMidStream: 900 * time.Millisecond,
			MidStreamSplit: 134,
		},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "stall"}), "")
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "second"}), "")
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	status, msg, _, code := readErr(t, resp)
	if status != http.StatusTooManyRequests || code != "pool_busy" {
		t.Fatalf("second request got %d/%s (%s), want 429 pool_busy while the only slot is busy", status, code, msg)
	}
	<-done
}
