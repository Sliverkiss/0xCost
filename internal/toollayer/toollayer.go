// Package toollayer is the client for the tool-calling middleware (ToolForge).
//
// The gateway owns the model surface; the middleware owns the function-calling
// protocol: it strips `tools` from the request, injects an XYML prompt protocol,
// and parses the model's text back into standard `tool_calls` (ADR 0012). Our
// job here is only to hand it the call and give its answer back unchanged.
//
// Two rules make the round trip safe:
//
//   - The caller's own credential travels with the request. The middleware
//     forwards it (its `key_passthrough`) to the path it calls back on, so the
//     session state behind us stays scoped to the real caller instead of
//     collapsing every client into the middleware's own key (spec REQ-3/INV-2).
//   - Nothing else about the request is rewritten. The body is forwarded as the
//     bytes the client sent, so `tools` and tool results keep their exact shape.
package toollayer

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ErrUnavailable is returned when the middleware cannot be reached or answers
// nothing usable. The caller maps it to a 502 rather than silently dropping the
// tools, because an answer without its tool calls is worse than an error.
var ErrUnavailable = errors.New("tool layer unavailable")

// Client forwards chat completions to the middleware.
type Client struct {
	url   string
	key   string // optional: used when the deployment runs without client keys
	hc    *http.Client
	log   *log.Logger
	debug bool
}

// New builds a client for the middleware at url. An empty key means the
// caller's own credential is always the one presented.
func New(url, key string, logger *log.Logger, debug bool) *Client {
	if logger == nil {
		logger = log.Default()
	}
	return &Client{
		url: strings.TrimRight(url, "/"),
		key: key,
		hc: &http.Client{
			// No total timeout: the answer is a stream and may legitimately
			// take minutes. A stalled middleware is caught at the header
			// boundary and by the caller's context.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConnsPerHost:   8,
				ResponseHeaderTimeout: 180 * time.Second,
			},
		},
		log:   logger,
		debug: debug,
	}
}

// URL is the configured middleware endpoint (never contains a credential).
func (c *Client) URL() string { return c.url }

// Forward sends the request body to the middleware and copies its answer back
// verbatim, streaming SSE as it arrives. callerHeaders are the headers the
// caller sent; only its credential and the X-Mimo-* extensions are carried.
func (c *Client) Forward(ctx context.Context, raw []byte, callerHeaders http.Header, w http.ResponseWriter) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if credential := c.credentialFor(callerHeaders); credential != "" {
		req.Header.Set("Authorization", credential)
	}
	for name, values := range callerHeaders {
		if !strings.HasPrefix(strings.ToLower(name), "x-mimo-") {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		c.logf("tool layer: %s: %v", c.url, err)
		return ErrUnavailable
	}
	defer resp.Body.Close()

	// Copy the answer through: status, content type, then the body as it
	// arrives so a streamed tool call is not buffered here.
	for name, values := range resp.Header {
		if isHopByHop(name) {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(&flushWriter{w: w}, resp.Body); err != nil {
		// The status is already out; the only honest thing left is to stop and
		// say so in the log.
		c.logf("tool layer: stream from %s ended early: %v", c.url, err)
		return nil
	}
	if c.debug {
		c.logf("tool layer: %s -> %d", c.url, resp.StatusCode)
	}
	return nil
}

// credentialFor prefers the caller's own credential, so the middleware can pass
// the caller's identity on to the path it calls back on.
func (c *Client) credentialFor(callerHeaders http.Header) string {
	if v := callerHeaders.Get("Authorization"); v != "" {
		return v
	}
	if c.key != "" {
		return "Bearer " + c.key
	}
	return ""
}

func (c *Client) logf(format string, args ...any) {
	if c.log != nil {
		c.log.Printf(format, args...)
	}
}

// flushWriter pushes each write out, so SSE chunks reach the client as they
// arrive instead of when the middleware closes the stream.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func isHopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "transfer-encoding", "te", "trailer",
		"upgrade", "proxy-authenticate", "proxy-authorization", "content-length":
		return true
	}
	return false
}
