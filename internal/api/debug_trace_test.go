package api_test

// Tests for the operator-facing routing trace (MIMO2API_LOG_LEVEL=debug).
//
// Why this exists: when a conversation appears to lose context, the first
// question is always "what did the gateway actually send upstream?" — and the
// answer is invisible from the outside, because the request returns a
// well-formed 200 either way. The trace line is the only observable, so it is
// pinned here. (The sticky-continuation variant died with the session store,
// ADR 0017: every request is a full replay now.)

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// DEBUG-002: every chat request logs the full-replay decision, so an operator
// can tell "no context was sent" apart from "context was sent and ignored".
func TestDebugTraceFullReplay(t *testing.T) {
	var buf bytes.Buffer
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		Debug:   true,
		LogSink: &buf,
	})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}), "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.ReadJSON(t, resp)

	got := buf.String()
	if !strings.Contains(got, "debug: chat: full replay") {
		t.Fatalf("log = %q, want a full-replay trace line", got)
	}
	if !strings.Contains(got, "replayCap=") {
		t.Errorf("log = %q, want the effective replayCap in the line", got)
	}
}

// DEBUG-003: with debug off, no trace line leaks into the log.
func TestDebugTraceSilentWhenOff(t *testing.T) {
	var buf bytes.Buffer
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{LogSink: &buf})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", false, [2]string{"user", "hello"}), "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.ReadJSON(t, resp)
	if strings.Contains(buf.String(), "debug:") {
		t.Fatalf("debug off leaked a trace line: %q", buf.String())
	}
}
