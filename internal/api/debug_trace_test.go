package api_test

// Tests for the operator-facing routing trace (MIMO2API_LOG_LEVEL=debug).
//
// Why this exists: when a conversation appears to lose context, the first
// question is always "did the gateway continue the upstream conversation or
// start a fresh one?" — and the answer is invisible from the outside, because
// both paths return a well-formed 200 stream. The trace line is the only
// observable, so it is pinned here.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// DEBUG-001: a sticky continuation logs the continuation decision (account,
// conversation, previous dialog, delta size) when debug is on, and nothing
// when it is off.
func TestDebugTraceStickyContinuation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		debug      bool
		wantPrefix string
	}{
		{name: "debug on", debug: true, wantPrefix: "debug: chat: sticky continuation"},
		{name: "debug off", debug: false, wantPrefix: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
				Debug:   tc.debug,
				LogSink: &buf,
			})
			// Warm-up turn stores the sticky entry.
			resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "")
			if err != nil {
				t.Fatal(err)
			}
			testutil.ReadSSE(t, resp)
			if ts.Sessions.Len() != 1 {
				t.Fatalf("warm-up stored %d entries, want 1", ts.Sessions.Len())
			}
			// Follow-up that extends the history must continue it.
			resp, err = ts.Chat(continuationBody(), "")
			if err != nil {
				t.Fatal(err)
			}
			testutil.ReadSSE(t, resp)

			got := buf.String()
			if tc.wantPrefix == "" {
				if strings.Contains(got, "debug:") {
					t.Fatalf("debug off leaked a trace line: %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantPrefix) {
				t.Fatalf("log = %q, want a %q line", got, tc.wantPrefix)
			}
			// The trace must name the replayed delta size, not just the path taken.
			if !strings.Contains(got, "deltaQueries=1") {
				t.Errorf("log = %q, want deltaQueries=1 for a one-message follow-up", got)
			}
		})
	}
}

// DEBUG-002: a fresh conversation logs the full-replay decision, so an operator
// can tell "no context was sent" apart from "context was sent and ignored".
func TestDebugTraceFullReplay(t *testing.T) {
	var buf bytes.Buffer
	ts := testserver.NewTestServer(t, oneAccount(), &testserver.TestServerOptions{
		Debug:   true,
		LogSink: &buf,
	})
	resp, err := ts.Chat(testutil.ChatBody("mimo-v2.6-pro", true, [2]string{"user", "hello"}), "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.ReadSSE(t, resp)

	got := buf.String()
	if !strings.Contains(got, "debug: chat: full replay") {
		t.Fatalf("log = %q, want a full-replay trace line", got)
	}
	if !strings.Contains(got, "replayCap=") {
		t.Errorf("log = %q, want the effective replayCap in the line", got)
	}
}
