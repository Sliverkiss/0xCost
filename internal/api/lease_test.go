package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// ADR 0019/0036: a real client-facing deadline and a per-turn upstream
// silence window. These cases assert the four things an operator can act on:
// the status, the code, the numbers in the message, and the account's state.

// TIMEOUT-001: the request budget aborts a request the upstream is still
// stalling on, answers 504 chat_timeout with the budget in the message, and
// leaves the account alone — our clock is not the account's fault.
func TestRequestBudgetReturnsChatTimeout(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
		RequestTimeout: 1 * time.Second,
		LeaseTTL:       30 * time.Second, // the per-turn window is not what fires here
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
	// The wall clock is what bit here, so the silence window must not be named:
	// an operator sent to the wrong knob stays stalled (TIMEOUT-004).
	if strings.Contains(msg, "MIMO2API_LEASE_TTL_S") {
		t.Errorf("message %q names the lease window the wall clock never used", msg)
	}
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("account state = %v after a gateway timeout, want available (no cooldown)", st)
	}
}

// TIMEOUT-003 (ADR 0036): a turn that keeps talking is never cut for taking
// long. The lease window here is far shorter than the whole turn, but every gap
// between upstream bytes is shorter still, so the request must finish — this is
// the case that used to die at the window while the platform was working
// (a web search that took 120s+ because it was fetching pages).
func TestSlowButLiveTurnSurvivesTheSilenceWindow(t *testing.T) {
	acct := oneAccount()
	const window = 300 * time.Millisecond
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
		LeaseTTL: window,
	})
	const (
		chunks = 12
		gap    = 120 * time.Millisecond
	)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{
		Body:       testutil.FixtureSSEBasic,
		DripChunks: chunks,
		DripGap:    gap,
	})

	start := time.Now()
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hi"}), "")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 200: a live turn must not be cut by the silence window",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if elapsed <= window {
		t.Fatalf("the turn finished in %s; this case needs it to outlive the %s window", elapsed, window)
	}
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("account state = %v after a slow but live turn, want available (nothing was wrong with it)", st)
	}
}

// TIMEOUT-002: the per-turn silence window is the other half — a turn that goes
// **silent** for longer than the window, before any byte, is cut with the same
// client-facing shape, and the message names the smaller of the two budgets
// (the one that actually bit).
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
	// The silence window is what bit here; naming the wall clock would point the
	// operator at a knob that is disabled on the shipped default (TIMEOUT-004).
	if strings.Contains(msg, "MIMO2API_REQUEST_TIMEOUT_MS") {
		t.Errorf("message %q names the disabled wall clock", msg)
	}
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("account state = %v, want available", st)
	}
}

// NOSLOT-003 (owner ruling 2026-10-14, "并发槽砍了"): with the in-flight slot
// cap gone, a second request on the same account is served while the first is
// still generating — the upstream, not the gateway, decides what it will serve.
// There is no pool_busy answer and no queue wait.
func TestConcurrentRequestsShareTheAccount(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{
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

	type result struct {
		status int
		body   string
	}
	first := make(chan result, 1)
	go func() {
		resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "stall"}), "")
		if err != nil {
			first <- result{status: -1, body: err.Error()}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		first <- result{status: resp.StatusCode, body: string(b)}
	}()

	time.Sleep(300 * time.Millisecond) // the first request is mid-generation
	start := time.Now()
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "second"}), "")
	if err != nil {
		t.Fatalf("second request could not be served at all: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		status, msg, _, code := readErr(t, resp)
		t.Fatalf("second request got %d/%s (%s), want 200 — no local concurrency cap exists", status, code, msg)
	}
	resp.Body.Close()
	// No queue wait: the second request must not have waited for the first's
	// 900ms stall to finish before being accepted.
	if waited := time.Since(start); waited > 700*time.Millisecond {
		t.Errorf("second request waited %s; there is no queue to wait in", waited)
	}

	select {
	case got := <-first:
		if got.status != http.StatusOK {
			t.Fatalf("first request ended %d (%s), want 200 — serving a second request concurrently must not abandon the first", got.status, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request never finished")
	}
	if st := ts.Pool.StateOf(acct[0].ID); st != pool.StateAvailable {
		t.Errorf("account state = %v, want available", st)
	}
}
