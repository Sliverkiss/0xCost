package api

// Failure attribution at the API layer (TDD §3.5.2 POOLATTR-003/004/011, SDD
// §4.5.1). These are internal tests because the classifier is package-private and
// the point is exactly the mapping — an account must not be cooled down on the
// wrong ladder, and a business error must not cool it at all.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// POOLATTR-003/004/011: the class of every failure shape we can produce.
func TestClassifyPoolFailure(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantClass pool.FailureClass
		wantState int
	}{
		{"SSE idle stall", mimo.ErrIdleStall, pool.ClassTransport, 0},
		{"wrapped stall", fmt.Errorf("chat: %w", mimo.ErrIdleStall), pool.ClassTransport, 0},
		{"EOF mid-body", io.ErrUnexpectedEOF, pool.ClassTransport, 0},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), pool.ClassTransport, 0},
		{"broken pipe", syscall.EPIPE, pool.ClassTransport, 0},
		{"context deadline", context.DeadlineExceeded, pool.ClassTransport, 0},
		{"dial failure", &net.OpError{Op: "dial", Err: errors.New("no route to host")}, pool.ClassTransport, 0},
		{"upstream 500", &mimo.UpstreamError{StatusCode: 500, Body: "boom"}, pool.ClassUpstream5xx, 500},
		{"upstream 503", &mimo.UpstreamError{StatusCode: 503, Body: "busy"}, pool.ClassUpstream5xx, 503},
		{"non-SSE 200 body", &mimo.UpstreamError{StatusCode: 200, Body: "<html>"}, pool.ClassUpstream5xx, 200},
		{"unknown error", errors.New("something else"), pool.ClassUpstream5xx, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, status := classifyPoolFailure(tc.err)
			if class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
			if status != tc.wantState {
				t.Errorf("status = %d, want %d", status, tc.wantState)
			}
		})
	}
}

// POOLATTR-011: isTransportErr must not swallow an upstream answer — a 5xx is not
// a transport failure even though it is an error.
func TestIsTransportErrRejectsUpstreamAnswers(t *testing.T) {
	if isTransportErr(&mimo.UpstreamError{StatusCode: 500}) {
		t.Error("a 5xx must not be classified as transport")
	}
	if !isUpstream5xx(&mimo.UpstreamError{StatusCode: 502}) {
		t.Error("502 must be recognised as an upstream 5xx")
	}
	if isUpstream5xx(&mimo.UpstreamError{StatusCode: 429}) {
		t.Error("429 is the throttle class, not 5xx")
	}
	if isTransportErr(nil) {
		t.Error("nil is not a transport failure")
	}
}
