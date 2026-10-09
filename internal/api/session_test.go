package api_test

// Session identity end-to-end (TDD §3.5.1 SESS-*; spec
// docs/specs/session-identity.md; ADR 0009).
//
// The store and ladder have their own unit cases; these drive the real HTTP
// surface so the contract a client actually sees — which session the response
// names, when a chain is reused, what a one-shot does, and what the logs say —
// is asserted against the real pipeline.

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// chatWith posts a body with extra headers and returns the response plus the
// session headers the gateway set.
func chatWith(t *testing.T, ts *testserver.TestServer, body, apiKey string, hdr map[string]string) (*http.Response, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL()+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, resp.Header.Get("X-Mimo-Session-Id"), resp.Header.Get("X-Mimo-Session-Mode")
}

// SESS-061: the effective id comes back on every streaming response, and
// pinning it on the next turn chains onto the same upstream conversation.
func TestSess061SessionIDRoundTrip(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	// Turn 1: no id supplied, so the ladder derives one.
	resp, publicID, mode := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "", nil)
	testutil.ReadSSE(t, resp)
	if publicID == "" {
		t.Fatal("no X-Mimo-Session-Id on the first response")
	}
	if mode != "replay" {
		t.Errorf("first turn mode = %q, want replay", mode)
	}
	firstConv := ts.Upstream.Requests()[0].ConvID

	// Turn 2: pin the id we were handed while continuing the same history.
	body := testutil.ChatBody("mimo-v2.6-pro", true,
		[2]string{"user", "hello"},
		[2]string{"assistant", "ok"},
		[2]string{"user", "what next"})
	resp2, publicID2, mode2 := chatWith(t, ts, body, "", map[string]string{"X-Mimo-Session-Id": publicID})
	testutil.ReadSSE(t, resp2)
	if publicID2 != publicID {
		t.Errorf("pinned id came back as %q, want %q", publicID2, publicID)
	}
	if mode2 != "chain" {
		t.Errorf("pinned turn mode = %q, want chain", mode2)
	}
	reqs := ts.Upstream.Requests()
	if got := reqs[len(reqs)-1].ConvID; got != firstConv {
		t.Errorf("pinned turn used conversation %q, want the chained %q", got, firstConv)
	}

	// The same pin with a rewritten history is honoured as its own stable
	// session — isolated, and never a chain into the original (documented in the
	// spec §6; the alternative would be a silent lookalike match).
	rewritten := testutil.ChatBody("mimo-v2.6-pro", true,
		[2]string{"user", "a totally different opening"},
		[2]string{"assistant", "ok"},
		[2]string{"user", "what next"})
	resp3, id3, mode3 := chatWith(t, ts, rewritten, "", map[string]string{"X-Mimo-Session-Id": publicID})
	testutil.ReadSSE(t, resp3)
	if mode3 == "chain" {
		t.Error("a pin with rewritten inputs chained onto the original session")
	}
	if id3 != publicID {
		t.Errorf("the pin was not echoed back: %q vs %q", id3, publicID)
	}
}

// SESS-030: a non-streaming request is one-shot — no store read, no store
// write, no session header — even when the request names a session.
func TestSess030NonStreamIsOneShot(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	// Warm a session with a streaming turn.
	resp, publicID, _ := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "", nil)
	testutil.ReadSSE(t, resp)
	entriesBefore := ts.Sessions.Len()
	readsBefore := ts.Sessions.Lookups()

	// One-shot with the session named both ways.
	body := testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "one shot"})
	resp2, sid, mode := chatWith(t, ts, body, "", map[string]string{"X-Mimo-Session-Id": publicID})
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	if sid != "" {
		t.Errorf("one-shot response carried X-Mimo-Session-Id = %q, want none", sid)
	}
	if mode != "" {
		t.Errorf("one-shot response carried X-Mimo-Session-Mode = %q, want none", mode)
	}
	if reads := ts.Sessions.Lookups() - readsBefore; reads != 0 {
		t.Errorf("one-shot performed %d session lookups, want 0", reads)
	}
	if after := ts.Sessions.Len(); after != entriesBefore {
		t.Errorf("store size changed on a one-shot: %d → %d", entriesBefore, after)
	}
}

// SESS-031: the body carrier is ignored on a one-shot too — the chain is not
// advanced and not forked.
func TestSess031SessionIDIgnoredOnOneShot(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	resp, publicID, _ := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "", nil)
	testutil.ReadSSE(t, resp)
	before, _ := ts.Sessions.Get(sessionKeyOf(t, ts))

	body := strings.Replace(
		testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "again"}),
		`"stream":false`, `"stream":false,"session_id":"`+publicID+`"`, 1)
	resp2, _, _ := chatWith(t, ts, body, "", nil)
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	after, ok := ts.Sessions.Get(sessionKeyOf(t, ts))
	if !ok {
		t.Fatal("the warm session disappeared")
	}
	if after.MsgCount != before.MsgCount || after.UpdatedAt != before.UpdatedAt {
		t.Errorf("a one-shot advanced the chain: %+v → %+v", before, after)
	}
}

// SESS-003 (API half): the OpenAI `user` field does not become a session id.
func TestSess003UserFieldIsNotASession(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), nil)
	bodyA := strings.Replace(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}),
		`"stream":true`, `"stream":true,"user":"session-a"`, 1)
	bodyB := strings.Replace(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}),
		`"stream":true`, `"stream":true,"user":"session-b"`, 1)

	respA, idA, _ := chatWith(t, ts, bodyA, "", nil)
	testutil.ReadSSE(t, respA)
	respB, idB, _ := chatWith(t, ts, bodyB, "", nil)
	testutil.ReadSSE(t, respB)

	if idA != idB {
		t.Errorf("`user` split one history into two sessions: %q vs %q", idA, idB)
	}
	if ts.Sessions.Len() != 1 {
		t.Errorf("store holds %d sessions, want 1 — `user` must not be an identity input", ts.Sessions.Len())
	}
}

// SESS-010: sessions never span credentials — a forged pin under another API key
// resolves to its own (empty) session, and the owner's entry is untouched.
func TestSess010SessionsAreCredentialScoped(t *testing.T) {
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{APIKeys: []string{"sk-tenant-a", "sk-tenant-b"}})
	resp, idA, _ := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "sk-tenant-a", nil)
	testutil.ReadSSE(t, resp)
	ownerKey := sessionKeyOf(t, ts)
	ownerEntry, _ := ts.Sessions.Get(ownerKey)

	body := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "who are you"})
	resp2, idB, mode := chatWith(t, ts, body, "sk-tenant-b", map[string]string{"X-Mimo-Session-Id": idA})
	testutil.ReadSSE(t, resp2)

	if mode == "chain" {
		t.Error("a second credential chained onto the first credential's session")
	}
	reqs := ts.Upstream.Requests()
	if got := reqs[len(reqs)-1].ConvID; got == ownerEntry.ConversationID {
		t.Error("a second credential reached the first credential's upstream conversation")
	}
	if idB == "" {
		t.Error("the second credential got no session id of its own")
	}
	if after, _ := ts.Sessions.Get(ownerKey); after.ConversationID != ownerEntry.ConversationID {
		t.Error("the owner's session was modified by another credential's request")
	}
}

// SESS-020: with the immediate-fork policy, a second concurrent request for one
// session gets its own session id and its own upstream conversation.
func TestSess020BusySessionForks(t *testing.T) {
	var sink bytes.Buffer
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		ForkPolicy:  "fork",
		SessionWait: 0,
		Debug:       true,
		LogSink:     &sink,
	})
	// Warm the session first, then hold the next turn open upstream.
	resp, publicID, _ := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "", nil)
	testutil.ReadSSE(t, resp)
	ph := ts.Upstream.Requests()[0].PH
	// The step must carry a body: a bare step is served as a status-only
	// response (an empty 200, which the gateway correctly retries).
	ts.Upstream.Script(ph, testutil.ScriptStep{Body: testutil.FixtureSSEBasic, StallBefore: 300 * time.Millisecond})

	body := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "slow one"})
	body2 := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "concurrent one"})

	type result struct {
		id, mode string
		err      error
	}
	first := make(chan result, 1)
	go func() {
		r, id, mode := chatWith(t, ts, body, "", map[string]string{"X-Mimo-Session-Id": publicID})
		testutil.ReadSSE(t, r)
		first <- result{id, mode, nil}
	}()
	time.Sleep(50 * time.Millisecond) // let the first request take the session
	resp2, id2, mode2 := chatWith(t, ts, body2, "", map[string]string{"X-Mimo-Session-Id": publicID})
	if resp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Fatalf("forked request status = %d body=%s\nlog:\n%s", resp2.StatusCode, raw, sink.String())
	}
	testutil.ReadSSE(t, resp2)
	got := <-first

	if got.err != nil {
		t.Fatalf("first request failed: %v", got.err)
	}
	if mode2 != "fork" {
		t.Errorf("concurrent request mode = %q, want fork", mode2)
	}
	if id2 == publicID {
		t.Error("the forked request kept the busy session's id")
	}
	// The fork must release its own claim: pinning the forked id on a later
	// turn has to reuse that session, not fork away from it again.
	time.Sleep(50 * time.Millisecond)
	body3 := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "after the fork"})
	resp3, _, mode3 := chatWith(t, ts, body3, "", map[string]string{"X-Mimo-Session-Id": id2})
	testutil.ReadSSE(t, resp3)
	if mode3 == "fork" {
		t.Error("the forked session stayed claimed after its request finished")
	}

	if got.id != publicID || got.mode != "replay" {
		// The first request may legitimately have advanced the chain; what must
		// hold is that it kept its own id.
		if got.id != publicID {
			t.Errorf("first request id = %q, want %q", got.id, publicID)
		}
	}
}

// SESS-040: when the pinned account is gone the session migrates — new upstream
// conversation, full replay, and the session is re-pinned to the new account.
func TestSess040SessionMigratesWhenAccountIsUnusable(t *testing.T) {
	ts := testserver.NewTestServer(t, twoAccounts(), nil)
	resp, publicID, _ := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "", nil)
	testutil.ReadSSE(t, resp)
	entry, _ := ts.Sessions.Get(sessionKeyOf(t, ts))
	oldConv := entry.ConversationID
	oldAccount := entry.AccountID

	// Take the pinned account out of service (POOL-012 semantics).
	ts.Pool.ReportDisabled(oldAccount, "test: session migration", true)

	body := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}, [2]string{"assistant", "hi"}, [2]string{"user", "after migration"})
	resp2, _, mode := chatWith(t, ts, body, "", map[string]string{"X-Mimo-Session-Id": publicID})
	events, _ := testutil.ReadSSE(t, resp2)
	if len(events) == 0 {
		t.Fatal("migrated request streamed nothing")
	}
	if mode == "chain" {
		t.Error("migration must not present itself as a continuation of the old chain")
	}
	reqs := ts.Upstream.Requests()
	last := reqs[len(reqs)-1]
	if last.ConvID == oldConv {
		t.Error("migrated request reused the old upstream conversation")
	}
}

// SESS-060/062: one debug line per decision, carrying the required fields and
// nothing that leaks content or credentials.
func TestSess060DecisionLinesAreCompleteAndPrivate(t *testing.T) {
	var sink bytes.Buffer
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		Debug:   true,
		LogSink: &sink,
		APIKeys: []string{"sk-secret-key"},
	})
	secret := "the-patient-says-my-secret-symptom"
	resp, publicID, _ := chatWith(t, ts, testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", secret}), "sk-secret-key", nil)
	testutil.ReadSSE(t, resp)
	body := testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", secret}, [2]string{"assistant", "hi"}, [2]string{"user", "next"})
	resp2, _, _ := chatWith(t, ts, body, "sk-secret-key", map[string]string{"X-Mimo-Session-Id": publicID})
	testutil.ReadSSE(t, resp2)

	logs := sink.String()
	var decisions []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "session: decision=") {
			decisions = append(decisions, line)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("decision lines = %d, want 2 (one per streaming request):\n%s", len(decisions), logs)
	}
	if !strings.Contains(decisions[0], "decision=replay") {
		t.Errorf("first decision line = %q, want a replay", decisions[0])
	}
	if !strings.Contains(decisions[1], "decision=chain") {
		t.Errorf("second decision line = %q, want a chain", decisions[1])
	}
	for _, line := range decisions {
		for _, field := range []string{"session=", "source=", "frontier=", "history="} {
			if !strings.Contains(line, field) {
				t.Errorf("decision line %q is missing %s", line, field)
			}
		}
	}
	if strings.Contains(logs, secret) {
		t.Error("a session log line leaked message content (INV-3)")
	}
	if strings.Contains(logs, "sk-secret-key") {
		t.Error("a log line leaked the API key (INV-3)")
	}
}

// sessionKeyOf returns the single stored session key (the tests that use this
// helper keep exactly one live session).
func sessionKeyOf(t *testing.T, ts *testserver.TestServer) string {
	t.Helper()
	entries := ts.Sessions.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("stored sessions = %d, want exactly 1", len(entries))
	}
	return entries[0].SessionKey
}
