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
// (SDD §2.5, TDD ERRMAP-009). Reason is the upstream's own event name and
// Message its own `content` payload, both kept verbatim: the platform's sentence
// is the authoritative account of why it refused, and a caller that only sees our
// paraphrase cannot tell a content judgement from a transport failure.
type ErrContentFilter struct {
	Reason  string // event name: sensitive_query | sensitive_title
	Message string // the event's own `content`, when it sent one
}

func (e *ErrContentFilter) Error() string {
	s := "content filtered by upstream (" + e.Reason + ")"
	if m := strings.TrimSpace(e.Message); m != "" {
		s += ": " + m
	}
	return s
}

// UpstreamMessage returns the upstream platform's own wording behind err, when the
// failure carried one. Every path that reports an upstream refusal includes this
// verbatim (ADR 0033): the text is the platform's, so nothing downstream has to
// guess what it meant, and it is the only part a caller can act on.
func UpstreamMessage(err error) string {
	var fe *upstreamFrameError
	if errors.As(err, &fe) {
		return fe.message
	}
	var cf *ErrContentFilter
	if errors.As(err, &cf) {
		return cf.Error()
	}
	var us *ErrUpstreamStream
	if errors.As(err, &us) {
		return us.Message
	}
	return ""
}

// In-band `event:error` frames are NOT interchangeable. Two of the messages
// seen in production need opposite handling:
//
//   - "服务器繁忙，请稍后再试" is a transient platform throttle. Observed live on
//     both channels to clear within seconds (busy on two consecutive turns,
//     answered normally on the next). Retrying is correct.
//   - "模型名称错误" is deterministic: the channel refuses that model id (an
//     ultraspeed model sent to the normal channel). Retrying burns upstream
//     calls on a request that can never succeed.
//
// Everything else keeps the historical behaviour (a failed turn, no retry).
var (
	// ErrUpstreamBusy is a transient upstream throttle: retry with backoff.
	ErrUpstreamBusy = errors.New("upstream busy")
	// ErrUpstreamModelRejected is a model this channel refuses: never retry.
	ErrUpstreamModelRejected = errors.New("upstream rejected the model name")
)

// upstreamFrameError carries the upstream's own text while still matching a
// sentinel with errors.Is, so callers classify without string matching.
type upstreamFrameError struct {
	sentinel error
	message  string
}

func (e *upstreamFrameError) Error() string { return e.sentinel.Error() + ": " + e.message }
func (e *upstreamFrameError) Unwrap() error { return e.sentinel }

// classifyInBandError maps an `event:error` payload onto a sentinel. The
// upstream sends a human-readable Chinese message, so this is pattern-based;
// unknown messages fall back to the generic stream error.
func classifyInBandError(msg string) error {
	switch {
	case strings.Contains(msg, "繁忙"), strings.Contains(msg, "稍后再试"):
		return &upstreamFrameError{sentinel: ErrUpstreamBusy, message: msg}
	case strings.Contains(msg, "模型名称"), strings.Contains(msg, "模型不存在"):
		return &upstreamFrameError{sentinel: ErrUpstreamModelRejected, message: msg}
	default:
		return &ErrUpstreamStream{Message: msg}
	}
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
	// citations accumulates web-search results across events (the upstream
	// may send more than one web_search event per turn).
	citations []adapter.Citation
	// onTap, when set, observes every parsed event before dispatch
	// (test hook for event-order assertions).
	onTap func(ev Event)
	// onEvent, when set, reports every event's name and payload size (plus a
	// payload prefix for web_search, whose shape is the thing an operator
	// needs when citations come back empty). Debug-level only.
	onEvent func(name string, dataLen int, prefix string)
	// citationsLog records skipped citation payloads for the debug log.
	citationsLog []string
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
	if s.onEvent != nil {
		prefix := ""
		if ev.Name == "web_search" {
			prefix = truncateForLog(ev.Data, 160)
		}
		s.onEvent(ev.Name, len(ev.Data), prefix)
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
		return false, classifyInBandError(p.Content)
	case "sensitive_query", "sensitive_title":
		var p msgPayload
		_ = json.Unmarshal([]byte(ev.Data), &p)
		return false, &ErrContentFilter{Reason: ev.Name, Message: p.Content}
	case "web_search":
		// Search results (M19). Tolerated strictly: a malformed payload is
		// logged and skipped — it must never break the turn, and the parse
		// does not depend on the request having asked for search (the
		// upstream returns these even when webSearchStatus is disabled).
		cits, perr := parseWebSearch(ev.Data)
		if perr != nil {
			// Record, never fail the turn: a client that asked for an answer
			// still gets one. The reason surfaces at debug level.
			s.citationsLog = append(s.citationsLog, perr.Error())
		}
		if len(cits) > 0 {
			// Index each result by arrival order across events.
			for i := range cits {
				cits[i].Index = len(s.citations) + i
			}
			s.citations = append(s.citations, cits...)
		}
	case "doc", "tip_ratio", "tip_truncate":
		// Search/parse progress: nothing client-visible to carry.
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

// The `web_search` payload is schema-loose and the two live surprises were
// both "one odd field killed the whole array": `usedContent` is a JSON boolean
// (not the "True" string an earlier capture showed) and `metadata` is an
// OBJECT (not the "{}" string that capture showed). So each item is decoded
// field by field with per-field tolerance: a strange field costs that field,
// never the citation and never the turn.
//
// Live shape (verified 2026-10-09): a bare array of up to 16 entries, ~94 KB,
// fields {id, text (page body, tens of KB), metadata, name, url, displayUrl,
// snippet, usedContent}.

// parseWebSearch turns a `web_search` payload into citations. A payload that is
// not JSON at all is reported (never silently skipped); individual odd fields
// are not — they degrade one citation's richness, not the result set.
func parseWebSearch(data string) ([]adapter.Citation, error) {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	items, err := decodeWebSearchItems(trimmed)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Citation, 0, len(items))
	for _, it := range items {
		url := rawString(it["url"])
		if url == "" {
			// A citation without a source is noise.
			continue
		}
		out = append(out, adapter.Citation{
			Title:      rawString(it["name"]),
			URL:        url,
			DisplayURL: rawString(it["displayUrl"]),
			Snippet:    rawString(it["snippet"]),
			Used:       rawBool(it["usedContent"]),
		})
	}
	return out, nil
}

// decodeWebSearchItems accepts both shapes the upstream has used: a bare array
// and an object wrapping the array in `data`.
func decodeWebSearchItems(trimmed string) ([]map[string]json.RawMessage, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
		var wrapped struct {
			Data []map[string]json.RawMessage `json:"data"`
		}
		if werr := json.Unmarshal([]byte(trimmed), &wrapped); werr != nil {
			return nil, fmt.Errorf("web_search payload unreadable: %w", err)
		}
		items = wrapped.Data
	}
	return items, nil
}

// rawString reads a JSON string; a null, number, object or array yields "" so
// one unexpected type cannot sink the citation.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// rawBool reads the boolean forms seen in the wild: true/false, "True"/"False"
// (case-insensitive), 1/0.
func rawBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	switch strings.ToLower(rawString(raw)) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// truncateForLog shortens a payload for the debug trace without splitting
// UTF-8 runes.
func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// flexBool accepts the boolean forms the upstream mixes: true/false, the
// strings "true"/"false"/"True"/"False", and 0/1. Anything else is false.
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch strings.ToLower(s) {
	case "true", "1":
		*b = true
	case "false", "0", "null", "":
		*b = false
	default:
		*b = false
	}
	return nil
}
