// Package adapter holds the pure translation layer between the OpenAI Chat
// Completions protocol and MiMo AI Studio's web-chat wire format.
//
// Everything in this package is a pure function or an in-memory state machine:
// no network, no filesystem, no clocks. SDD §3 (package table), TDD §4 Wave 1.
package adapter

import (
	"strings"
)

// Thinking markers as they appear in MiMo message deltas. The raw stream sends
// the marker followed by a NUL byte ("<think>\0" ... "</think>\0", SDD §2.5,
// PROTOCOL.md:79-84). The splitter strips NULs on input and matches on the
// bare markers, matching the reference implementation (mimo-web-api
// src/thinking.js) which notes upstream occasionally emits stray NUL deltas.
const (
	ThinkOpen  = "<think>"
	ThinkClose = "</think>"
)

// Piece is one fragment emitted by the ThinkSplitter: either reasoning
// (inside a think block) or content (outside).
type Piece struct {
	Type string // "reasoning" | "content"
	Text string
}

// ThinkSplitter is a 3-state machine (outside | thinking | answer) that splits
// mixed MiMo deltas into reasoning/content pieces. It tolerates marker bytes
// split across chunk boundaries by holding back a partial-marker tail, and
// treats an unterminated think block at stream end as reasoning (SDD §2.5).
type ThinkSplitter struct {
	buf   strings.Builder
	state int // 0=outside, 1=thinking, 2=answer
}

const (
	stateOutside = iota
	stateThinking
	stateAnswer
)

// NewThinkSplitter returns a splitter ready to consume deltas.
func NewThinkSplitter() *ThinkSplitter {
	return &ThinkSplitter{}
}

// Push feeds one delta and returns the pieces that can be emitted now.
// NUL bytes are stripped on entry; output never contains NUL or markers.
func (s *ThinkSplitter) Push(delta string) []Piece {
	if delta == "" {
		return nil
	}
	// Strip NULs: marker location never depends on them (TDD SSE-008).
	s.buf.WriteString(strings.ReplaceAll(delta, "\x00", ""))
	return s.drain(false)
}

// Flush must be called at stream end: emits whatever remains in the buffer
// according to the current state. An unterminated think block counts entirely
// as reasoning (TDD SSE-009).
func (s *ThinkSplitter) Flush() []Piece {
	out := s.drain(true)
	rem := s.buf.String()
	s.buf.Reset()
	if rem == "" {
		return out
	}
	if s.state == stateThinking {
		out = append(out, Piece{Type: "reasoning", Text: rem})
	} else {
		out = append(out, Piece{Type: "content", Text: rem})
	}
	return out
}

// drain scans the buffer, emitting complete segments and holding back a
// partial-marker tail. With final=false it holds back len(marker)-1 bytes so
// a marker split across pushes is recognized next round; with final=true it
// emits everything (the caller handles leftovers).
func (s *ThinkSplitter) drain(final bool) []Piece {
	var out []Piece
	data := s.buf.String()
	s.buf.Reset()

	emit := func(typ, text string) {
		if text != "" {
			out = append(out, Piece{Type: typ, Text: text})
		}
	}

	for {
		switch s.state {
		case stateThinking:
			idx := strings.Index(data, ThinkClose)
			if idx >= 0 {
				emit("reasoning", data[:idx])
				data = data[idx+len(ThinkClose):]
				s.state = stateAnswer
				continue
			}
			hold := partialMarkerSuffix(data, ThinkClose)
			if final {
				hold = 0
			}
			holdBack(&data, &s.buf, hold, "reasoning", emit)
			return out
		default: // outside or answer: both look for an open marker
			idx := strings.Index(data, ThinkOpen)
			if idx >= 0 {
				emit("content", data[:idx])
				data = data[idx+len(ThinkOpen):]
				s.state = stateThinking
				continue
			}
			hold := partialMarkerSuffix(data, ThinkOpen)
			if final {
				hold = 0
			}
			holdBack(&data, &s.buf, hold, "content", emit)
			return out
		}
	}
}

// partialMarkerSuffix returns the length of the longest suffix of data that
// is a proper prefix of marker (0 when none). Only that suffix needs holding
// back for the next push — everything before it is safe to emit now. A
// blanket len(marker)-1 hold would swallow short deltas wholesale and delay
// piece emission until the next chunk (TDD API-019 keepalive regression).
func partialMarkerSuffix(data, marker string) int {
	max := len(marker) - 1
	if max > len(data) {
		max = len(data)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(data, marker[:n]) {
			return n
		}
	}
	return 0
}

// holdBack emits everything except a possible partial-marker suffix (which
// stays in the buffer for the next push). With hold==0 everything is emitted.
func holdBack(data *string, buf *strings.Builder, hold int, typ string, emit func(string, string)) {
	d := *data
	if hold <= 0 || hold >= len(d) {
		if hold >= len(d) && hold > 0 {
			buf.WriteString(d) // entire tail might be a partial marker
		} else {
			emit(typ, d)
		}
		*data = ""
		return
	}
	emit(typ, d[:len(d)-hold])
	buf.WriteString(d[len(d)-hold:])
	*data = ""
}

// MergePieces folds pieces into (reasoning, content) strings. Useful for
// non-streaming collection and tests.
func MergePieces(pieces []Piece) (reasoning, content string) {
	var r, c strings.Builder
	for _, p := range pieces {
		switch p.Type {
		case "reasoning":
			r.WriteString(p.Text)
		case "content":
			c.WriteString(p.Text)
		}
	}
	return r.String(), c.String()
}
