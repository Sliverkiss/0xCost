package main

// Graceful shutdown (SDD §3, TDD API-025): a SIGTERM must let in-flight
// streams finish — an OpenAI stream that dies mid-body is a client-visible
// truncation — while new requests are refused immediately.

import (
	"context"
	"net"
	"net/http"
	"time"
)

// serveUntil serves srv on ln until ctx is cancelled or the listener fails.
//
// On cancellation it stops accepting new connections at once and waits up to
// grace for in-flight handlers (including streaming SSE responses) to return.
// A handler that overruns the grace is abandoned rather than killed, and the
// overrun is logged: dropping a stream mid-body is the failure mode this
// function exists to avoid.
func serveUntil(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration, logf func(string, ...any)) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		// The listener failed (port in use at startup, or Serve returned for
		// another reason) — there is nothing to drain.
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logf("mimo2api: shutdown: %v", err)
	}
	return nil
}
