package api

// Failure attribution (SDD §4.5.1, spec docs/specs/account-pool.md §4).
//
// The pool keeps one counter and one ladder per failure class, so the API layer
// must say which class a failure belongs to. The rule: a failure with no HTTP
// response behind it is a transport failure (and may be network- or egress-shaped
// rather than the account's fault); a failure the upstream itself answered is an
// upstream failure, and keeps the stricter ladder. Anything unclassifiable is
// treated as an upstream failure on purpose — inventing a shorter penalty for a
// signal nobody classified would hide it.

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// classifyPoolFailure maps an upstream failure to its pool class and, when the
// upstream answered, its status (TDD POOLATTR-003/004/011).
func classifyPoolFailure(err error) (pool.FailureClass, int) {
	var ue *mimo.UpstreamError
	if errors.As(err, &ue) {
		// The upstream answered: 5xx keeps the 5xx ladder, and a non-SSE 200
		// body (SDD §7.1) is equally an upstream-side fault.
		return pool.ClassUpstream5xx, ue.StatusCode
	}
	if isTransportErr(err) {
		return pool.ClassTransport, 0
	}
	return pool.ClassUpstream5xx, 0
}

// isUpstream5xx reports whether the upstream answered with a 5xx.
func isUpstream5xx(err error) bool {
	var ue *mimo.UpstreamError
	return errors.As(err, &ue) && ue.StatusCode >= 500
}

// isTransportErr reports whether the failure has no HTTP response behind it: a
// dead connection, a timeout, the SSE idle-stall watchdog, or a body that ended
// mid-stream. These are the failures that used to share the 5xx ladder.
func isTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mimo.ErrIdleStall) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var oe *net.OpError
	return errors.As(err, &oe)
}

// reportFailure feeds a failure to the pool under the right class. Content-level
// and business failures must not reach this function: they say nothing about the
// account's health (SDD §7.1), which is why callers filter them first.
func (s *Server) reportFailure(accountID string, err error) {
	class, status := classifyPoolFailure(err)
	s.pool.ReportFailure(accountID, class, status)
}
