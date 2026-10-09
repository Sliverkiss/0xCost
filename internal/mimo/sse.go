// Package mimo implements the upstream MiMo AI Studio client: SSE parsing
// (SDD §2.5), the passport login flow (§2.2), STS renewal (§2.3), and the
// chat call (§2.4).
package mimo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/yg/mimo2api/internal/adapter"
)

// Event is one parsed SSE event frame from the upstream stream (SDD §2.5).
type Event struct {
	ID   string // id: line, when present
	Name string // event: name (dialogId, message, usage, error, ...)
	Data string // data: payload (JSON for most events)
}

// ErrContentFilter marks sensitive_query / sensitive_title blocks
// (SDD §2.5, TDD ERRMAP-009).
type ErrContentFilter struct{ Reason string }

func (e *ErrContentFilter) Error() string {
	return "content filtered by upstream: " + e.Reason
}

// ErrUpstreamStream marks an `error` event mid-stream (SDD §2.5).
type ErrUpstreamStream struct{ Message string }

func (e *ErrUpstreamStream) Error() string { return "upstream stream error: " + e.Message }

// streamer drives one upstream chat stream end-to-end: SSE byte parsing,
// thinking-marker splitting, usage last-wins, and the per-turn state the
// chat path needs (dialogId chaining, SDD §2.4/§2.5).
type streamer struct {
	splitter    *adapter.ThinkSplitter
	dialogID    string
	reason      strings.Builder
	content     strings.Builder
	usage       *adapter.MiMoUsage
	sawDeltas   bool
	firstFired  bool
	onPiece     func(piece adapter.Piece) error // streaming passthrough (client.go)
	onFirstByte func()                          // commit boundary signal (SDD §7.2)
	// onDialogID, when set, is invoked with the captured dialogId as soon
	// as the dialogId event arrives (client.go wires ChatRequest.OnDialogID).
	onDialogID func(dialogID string)
	// onTap, when set, observes every parsed event before dispatch
	// (test hook for event-order assertions).
	onTap func(ev Event)
}

func newStreamer() *streamer {
	return &streamer{splitter: adapter.NewThinkSplitter()}
}

// msgPayload is the data JSON of message/dialogId/finish events.
type msgPayload struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// handleEvent processes one parsed event. Returns (done, err): done=true on
// finish [DONE]; a non-nil error fails the turn (error / sensitive events,
// TDD SSE-005).
func (s *streamer) handleEvent(ev Event) (bool, error) {
	if s.onTap != nil {
		s.onTap(ev)
	}
	if !s.firstFired {
		s.firstFired = true
		if s.onFirstByte != nil {
			s.onFirstByte()
		}
	}
	switch ev.Name {
	case "dialogId":
		var p msgPayload
		if err := json.Unmarshal([]byte(ev.Data), &p); err == nil && p.Content != "" {
			s.dialogID = p.Content
			if s.onDialogID != nil {
				s.onDialogID(p.Content)
			}
		}
	case "message":
		var p msgPayload
		if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
			return false, nil // malformed delta: skip, never kill the stream
		}
		s.sawDeltas = true
		for _, piece := range s.splitter.Push(p.Content) {
			if s.onPiece != nil {
				if err := s.onPiece(piece); err != nil {
					return false, err
				}
			}
			if piece.Type == "reasoning" {
				s.reason.WriteString(piece.Text)
			} else {
				s.content.WriteString(piece.Text)
			}
		}
	case "usage":
		var u adapter.MiMoUsage
		if err := json.Unmarshal([]byte(ev.Data), &u); err == nil {
			cu := u // copy: last one wins (SDD §2.5, TDD SSE-012)
			s.usage = &cu
		}
	case "finish":
		var p msgPayload
		_ = json.Unmarshal([]byte(ev.Data), &p)
		if p.Content == "[DONE]" {
			return true, nil
		}
	case "error":
		var p msgPayload
		_ = json.Unmarshal([]byte(ev.Data), &p)
		return false, &ErrUpstreamStream{Message: p.Content}
	case "sensitive_query", "sensitive_title":
		return false, &ErrContentFilter{Reason: ev.Name}
	case "web_search", "doc", "tip_ratio", "tip_truncate":
		// Search/parse progress: ignored (web search disabled in v1).
	}
	return false, nil
}

// finish drains the splitter (unterminated think block counts as reasoning,
// TDD SSE-009) and returns the turn result. Flushed pieces also flow
// through onPiece so streaming consumers receive the complete text.
func (s *streamer) finish() (dialogID, reasoning, content string, usage *adapter.MiMoUsage) {
	for _, piece := range s.splitter.Flush() {
		if s.onPiece != nil {
			if err := s.onPiece(piece); err != nil {
				// Late-flush consumer error: keep the assembled text, drop
				// the callback error (the stream is already over).
				_ = err
			}
		}
		if piece.Type == "reasoning" {
			s.reason.WriteString(piece.Text)
		} else {
			s.content.WriteString(piece.Text)
		}
	}
	return s.dialogID, s.reason.String(), s.content.String(), s.usage
}

// parseEvents reads an SSE byte stream and feeds every complete frame to
// handle. Honors ctx cancellation between reads (client disconnect
// propagation, SDD §7.3). Returns (done, err): done=true when a finish
// [DONE] frame ended the turn cleanly.
//
// Wire format (SDD §2.5): frames separated by a blank line; field lines
// `id:`, `event:`, `data:` — colon WITHOUT space in the MiMo dialect, but
// `event: x` and CRLF endings are tolerated (TDD SSE-002/003). Comment
// lines starting with ':' are ignored (keepalive frames).
func (s *streamer) parseEvents(ctx context.Context, r io.Reader) (bool, error) {
	type frame struct {
		id, event, data string
	}
	var cur frame
	flush := func() (bool, error) {
		if cur.event == "" && cur.data == "" {
			return false, nil
		}
		done, err := s.handleEvent(Event{ID: cur.id, Name: cur.event, Data: cur.data})
		cur = frame{}
		return done, err
	}

	br := bufio.NewReaderSize(r, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		line, err := br.ReadString('\n')
		if line != "" {
			clean := strings.TrimRight(line, "\r\n")
			if clean == "" {
				if done, ferr := flush(); done || ferr != nil {
					return done, ferr
				}
			} else if clean[0] != ':' {
				// Field line: strip one optional space after the colon.
				if i := strings.IndexByte(clean, ':'); i >= 0 {
					name, val := clean[:i], clean[i+1:]
					if strings.HasPrefix(val, " ") {
						val = val[1:]
					}
					switch name {
					case "id":
						cur.id = val
					case "event":
						cur.event = val
					case "data":
						if cur.data != "" {
							cur.data += "\n"
						}
						cur.data += val
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if done, ferr := flush(); done || ferr != nil {
					return done, ferr
				}
				return false, io.ErrUnexpectedEOF // stream ended without finish
			}
			return false, fmt.Errorf("sse read: %w", err)
		}
	}
}
