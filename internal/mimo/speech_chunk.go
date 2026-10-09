package mimo

// Long-form speech (TTS hardening).
//
// The upstream refuses any single synthesis whose spoken text exceeds 1024
// characters ("播报内容长度不能超过1024，当前长度为：N" — verified live), while
// OpenAI-compatible clients happily send thousands. Forwarding such a request
// unchanged produced a 502 that told the caller nothing. So the gateway splits
// the text on sentence boundaries, synthesises the pieces and joins the audio:
// the caller asks for long text and gets long audio.
//
// Joining is format-aware and deliberately conservative: WAV is rebuilt from
// the first piece's own header (never from assumptions about the upstream's
// encoding), MP3 is a byte concatenation, which every mainstream decoder plays
// as one stream.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// SpeechTextLimit is the upstream's per-request spoken-text cap, verified live
// (docs/web-surface-reverse.md §3). Anything longer must be split.
const SpeechTextLimit = 1024

// SplitSpeech breaks text into pieces of at most limit RUNES, preferring
// sentence boundaries. A single sentence longer than the limit is hard-split,
// so the result is never larger than limit. Runes are never split mid-rune
// (the upstream counts characters, and a broken rune would be spoken as
// garbage). Empty input yields no pieces.
func SplitSpeech(text string, limit int) []string {
	if limit <= 0 {
		limit = SpeechTextLimit
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	runes := []rune(trimmed)
	if len(runes) <= limit {
		return []string{trimmed}
	}

	var out []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, strings.TrimSpace(string(cur)))
		cur = cur[:0]
	}

	// Walk sentence terminators plus newlines; a terminator ends a sentence
	// (keeping it, since it carries the prosody).
	const terminators = "。！？；!?;\n"
	var sentence []rune
	for _, r := range runes {
		sentence = append(sentence, r)
		if !strings.ContainsRune(terminators, r) {
			continue
		}
		// A whole sentence that already exceeds the limit is hard-split.
		for len(sentence) > limit {
			if len(cur) > 0 {
				flush()
			}
			out = append(out, string(sentence[:limit]))
			sentence = append([]rune(nil), sentence[limit:]...)
		}
		if len(cur)+len(sentence) > limit {
			flush()
		}
		cur = append(cur, sentence...)
		sentence = sentence[:0]
	}
	// Trailing text with no terminator: hard-split if needed.
	for len(sentence) > limit {
		if len(cur) > 0 {
			flush()
		}
		out = append(out, string(sentence[:limit]))
		sentence = append([]rune(nil), sentence[limit:]...)
	}
	if len(sentence) > 0 {
		if len(cur)+len(sentence) > limit {
			flush()
		}
		cur = append(cur, sentence...)
	}
	flush()
	return out
}

// JoinAudio concatenates per-piece audio in the requested format.
func JoinAudio(format string, parts [][]byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "wav":
		return JoinWAV(parts)
	case "mp3":
		return JoinMP3(parts), nil
	default:
		return nil, fmt.Errorf("cannot join audio in format %q", format)
	}
}

// JoinWAV rebuilds one WAV from several, reusing the first piece's non-data
// chunks (format, and anything else the upstream added) and concatenating the
// data chunks. A single piece is returned untouched.
func JoinWAV(parts [][]byte) ([]byte, error) {
	if len(parts) == 0 {
		return nil, errors.New("join wav: no parts")
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	header, first, err := splitWAV(parts[0])
	if err != nil {
		return nil, err
	}
	total := len(first)
	datas := [][]byte{first}
	for i, p := range parts[1:] {
		h, d, err := splitWAV(p)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", i+1, err)
		}
		if !sameFormat(h, header) {
			// Mixing encodings would produce a file that plays as noise.
			return nil, fmt.Errorf("part %d has a different audio format", i+1)
		}
		total += len(d)
		datas = append(datas, d)
	}

	// Rebuild: RIFF header, the first piece's non-data chunks, one data chunk.
	out := make([]byte, 0, len(header)+8+total)
	out = append(out, "RIFF"...)
	out = append(out, u32(uint32(len(header)-8+8+total))...) // everything after "RIFF<size>"
	out = append(out, "WAVE"...)
	out = append(out, header[12:]...) // fmt and friends, up to the first data chunk
	out = append(out, "data"...)
	out = append(out, u32(uint32(total))...)
	for _, d := range datas {
		out = append(out, d...)
	}
	return out, nil
}

// JoinMP3 concatenates MPEG frames; decoders play the result as one stream.
func JoinMP3(parts [][]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// splitWAV returns the bytes before the first data chunk (the header, which
// includes "RIFF".."WAVE" and every non-data chunk) and the data payload.
func splitWAV(b []byte) (header, data []byte, err error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, nil, errors.New("not a RIFF/WAVE stream")
	}
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if size < 0 || body+size > len(b) {
			// Truncated or bogus chunk: treat everything from here as data.
			if id == "data" {
				return b[:pos], b[body:], nil
			}
			return nil, nil, fmt.Errorf("truncated %q chunk", id)
		}
		if id == "data" {
			return b[:pos], b[body : body+size], nil
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // chunks are word-aligned
		}
	}
	return nil, nil, errors.New("no data chunk")
}

// sameFormat compares the two headers' format chunks byte for byte.
func sameFormat(a, b []byte) bool {
	fa, oka := chunkBytes(a, "fmt ")
	fb, okb := chunkBytes(b, "fmt ")
	if !oka || !okb {
		return false
	}
	return string(fa) == string(fb)
}

// chunkBytes returns the body of the named chunk.
func chunkBytes(b []byte, id string) ([]byte, bool) {
	pos := 12
	for pos+8 <= len(b) {
		cur := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		if size < 0 || pos+8+size > len(b) {
			return nil, false
		}
		if cur == id {
			return b[pos+8 : pos+8+size], true
		}
		pos += 8 + size
		if size%2 == 1 {
			pos++
		}
	}
	return nil, false
}

func u32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// RuneLen counts characters the way the upstream does (runes, not bytes).
func RuneLen(s string) int { return utf8.RuneCountInString(s) }
