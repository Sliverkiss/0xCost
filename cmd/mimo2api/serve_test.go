package main

// API-025 (TDD §3): graceful-shutdown drain. The listener must stop accepting
// immediately while an in-flight stream is allowed to reach its final frame,
// and an overrun must be bounded by the grace period rather than hanging the
// process.

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamingMux serves one SSE endpoint whose handler flushes a first frame,
// signals that it is in flight, then finishes after `tail` — long enough to
// straddle the shutdown.
func streamingMux(t *testing.T, started chan struct{}, tail time.Duration) *http.ServeMux {
	t.Helper()
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not flushable")
			return
		}
		io.WriteString(w, "event:message\ndata:{\"type\":\"delta\"}\n\n")
		f.Flush()
		once.Do(func() { close(started) })
		time.Sleep(tail)
		io.WriteString(w, "event:message\ndata:[DONE]\n\n")
		f.Flush()
	})
	return mux
}

func TestGracefulShutdownDrainsInFlightStream(t *testing.T) {
	started := make(chan struct{})
	srv := &http.Server{Handler: streamingMux(t, started, 250*time.Millisecond)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, srv, ln, 5*time.Second, t.Logf) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatalf("in-flight request failed to start: %v", err)
	}
	defer resp.Body.Close()
	<-started // the handler is mid-stream now

	// Begin the drain while the stream is unfinished.
	cancel()

	// New connections must be refused during the drain window: poll for the
	// listener to stop accepting (Shutdown closes it synchronously).
	refused := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
		if derr != nil {
			refused = true
			break
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if !refused {
		t.Error("listener still accepted a new connection after shutdown began")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("in-flight stream broke during shutdown: %v", err)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("in-flight stream was truncated instead of drained: %q", body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntil = %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntil did not return after the in-flight stream finished")
	}
}

func TestGracefulShutdownRespectsGraceDeadline(t *testing.T) {
	started := make(chan struct{})
	// The handler outlives the grace period on purpose.
	srv := &http.Server{Handler: streamingMux(t, started, 3*time.Second)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, srv, ln, 300*time.Millisecond, logf) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()

	start := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntil = %v, want nil (the overrun is logged, not returned)", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("serveUntil waited %s, want <= the 300ms grace plus slack", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveUntil ignored the grace deadline and hung")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Error("an overrunning drain must be logged — otherwise the truncation is invisible")
	}
}

func TestServeUntilReturnsListenerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // a closed listener makes Serve fail immediately
	srv := &http.Server{Handler: http.NewServeMux()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case err := <-func() chan error {
		ch := make(chan error, 1)
		go func() { ch <- serveUntil(ctx, srv, ln, time.Second, t.Logf) }()
		return ch
	}():
		if err == nil {
			t.Error("serveUntil = nil, want the listener error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveUntil did not surface the listener failure")
	}
}
