package mimo

// Long-form speech splitting and joining (TTS hardening). The upstream caps one
// synthesis at 1024 characters, so the gateway splits and rejoins; these tests
// pin the two properties that matter: nothing is lost or corrupted in the text,
// and the joined audio is a valid single stream.

import (
	"encoding/binary"
	"strings"
	"testing"
)

// wav builds a minimal PCM WAV with the given data payload.
func wav(dataLen int, sampleRate uint32) []byte {
	buf := []byte("RIFF")
	buf = append(buf, u32(uint32(36+dataLen))...)
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = append(buf, u32(16)...)
	buf = append(buf, []byte{1, 0}...)        // PCM
	buf = append(buf, []byte{1, 0}...)        // mono
	buf = append(buf, u32(sampleRate)...)     // sample rate
	buf = append(buf, u32(sampleRate*2)...)   // byte rate
	buf = append(buf, []byte{2, 0, 16, 0}...) // block align, bits
	buf = append(buf, "data"...)
	buf = append(buf, u32(uint32(dataLen))...)
	return append(buf, make([]byte, dataLen)...)
}

func dataChunk(t *testing.T, b []byte) []byte {
	t.Helper()
	_, d, err := splitWAV(b)
	if err != nil {
		t.Fatalf("splitWAV: %v", err)
	}
	return d
}

// SPLIT-001/002: text within the limit is one untouched piece.
func TestSplitShortTextIsOnePiece(t *testing.T) {
	if got := SplitSpeech("你好", 1024); len(got) != 1 || got[0] != "你好" {
		t.Fatalf("SplitSpeech(short) = %v, want one unchanged piece", got)
	}
	exact := strings.Repeat("字", 1024)
	got := SplitSpeech(exact, 1024)
	if len(got) != 1 || got[0] != exact {
		t.Fatalf("SplitSpeech(exactly at the limit) = %d pieces, want 1 unchanged", len(got))
	}
}

// SPLIT-003: sentences are kept whole, so the prosody survives the split.
func TestSplitPrefersSentenceBoundaries(t *testing.T) {
	sentence := strings.Repeat("句", 400) + "。"
	text := sentence + sentence + sentence
	pieces := SplitSpeech(text, 1024)
	if len(pieces) != 2 {
		t.Fatalf("pieces = %d (%v), want 2 (400+400 then 400)", len(pieces), pieces)
	}
	for i, p := range pieces {
		if n := RuneLen(p); n > 1024 {
			t.Errorf("piece %d is %d runes, over the limit", i, n)
		}
	}
	if pieces[0] != sentence+sentence {
		t.Errorf("piece 0 does not hold two whole sentences: %d runes", RuneLen(pieces[0]))
	}
}

// SPLIT-004/005: a single over-long sentence is hard-split, and nothing is lost
// or broken — rejoining the pieces reproduces the input.
func TestSplitHardSplitsAndLosesNothing(t *testing.T) {
	text := strings.Repeat("这是一个没有句号的超长句子", 200) // no terminator at all
	pieces := SplitSpeech(text, 1024)
	if len(pieces) < 2 {
		t.Fatalf("pieces = %d, want the text hard-split", len(pieces))
	}
	var back strings.Builder
	for i, p := range pieces {
		if n := RuneLen(p); n > 1024 {
			t.Fatalf("piece %d is %d runes, over the limit", i, n)
		}
		back.WriteString(p)
	}
	if back.String() != text {
		t.Errorf("rejoined text differs from the input (%d vs %d runes)",
			RuneLen(back.String()), RuneLen(text))
	}
}

// SPLIT-006: mixed CJK, ASCII and emoji never split mid-rune.
func TestSplitIsRuneSafe(t *testing.T) {
	text := strings.Repeat("a🙂中文 b ", 200)
	pieces := SplitSpeech(text, 100)
	if len(pieces) < 2 {
		t.Fatalf("pieces = %d, want several", len(pieces))
	}
	for i, p := range pieces {
		if !utf8Valid(p) {
			t.Errorf("piece %d is not valid UTF-8: %q", i, p)
		}
		if RuneLen(p) > 100 {
			t.Errorf("piece %d is %d runes, over 100", i, RuneLen(p))
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// SPLIT-007: nothing to speak yields no pieces.
func TestSplitEmpty(t *testing.T) {
	if got := SplitSpeech("   \n ", 1024); got != nil {
		t.Errorf("SplitSpeech(blank) = %v, want nil", got)
	}
}

// JOIN-001: one piece is returned untouched (no re-encoding of what the
// upstream produced).
func TestJoinSinglePiece(t *testing.T) {
	one := wav(64, 24000)
	out, err := JoinAudio("wav", [][]byte{one})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(one) {
		t.Error("a single piece must pass through byte-for-byte")
	}
}

// JOIN-002: two WAVs become one valid WAV with the summed data length and the
// original format chunk.
func TestJoinWAV(t *testing.T) {
	a, b := wav(100, 24000), wav(50, 24000)
	out, err := JoinWAV([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0:4]) != "RIFF" || string(out[8:12]) != "WAVE" {
		t.Fatalf("joined head = %q, want a RIFF/WAVE container", out[:12])
	}
	if got := int(binary.LittleEndian.Uint32(out[4:8])); got != len(out)-8 {
		t.Errorf("RIFF size = %d, want %d (file length - 8)", got, len(out)-8)
	}
	data := dataChunk(t, out)
	if len(data) != 150 {
		t.Errorf("data length = %d, want 150 (100+50)", len(data))
	}
	// The format chunk must be the upstream's, not a guess.
	if f, ok := chunkBytes(out, "fmt "); !ok || len(f) != 16 {
		t.Errorf("fmt chunk = %v/%v, want the first piece's 16-byte fmt", f, ok)
	}
}

// JOIN-003: joining different encodings would produce noise, so it is refused.
func TestJoinWAVRejectsFormatMismatch(t *testing.T) {
	a, b := wav(100, 24000), wav(100, 48000)
	if _, err := JoinWAV([][]byte{a, b}); err == nil {
		t.Fatal("JoinWAV(mixed sample rates) = nil error, want a refusal")
	}
}

// JOIN-004: MP3 is a plain concatenation.
func TestJoinMP3(t *testing.T) {
	a, b := []byte{0xff, 0xf3, 0x01}, []byte{0xff, 0xf3, 0x02}
	out, err := JoinAudio("mp3", [][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 6 || out[0] != 0xff || out[3] != 0xff {
		t.Errorf("joined mp3 = %v, want the frames concatenated", out)
	}
}

// JOIN-005: relay semantics for a single piece — the upstream's bytes are
// returned untouched (the gateway does not re-encode what it did not join).
// Joining is where the bytes are parsed, and there garbage is refused rather
// than turned into a corrupt file.
func TestJoinWAVSinglePieceIsRelayedUntouched(t *testing.T) {
	raw := []byte("not a wav at all")
	out, err := JoinWAV([][]byte{raw})
	if err != nil {
		t.Fatalf("err = %v, want nil for a single relayed piece", err)
	}
	if string(out) != string(raw) {
		t.Error("a single piece must be relayed byte-for-byte")
	}
	if _, err := JoinWAV([][]byte{wav(64, 24000), raw}); err == nil {
		t.Error("JoinWAV(good, garbage) = nil error, want a refusal")
	}
}

// JOIN-006: a part whose data chunk is cut short is joined on the bytes that
// actually exist: the header is rebuilt to match, so the result is playable and
// no length is invented.
func TestJoinWAVTruncatedPartUsesRealBytes(t *testing.T) {
	good := wav(200, 24000)
	truncated := good[:len(good)-100] // header claims 200 data bytes, 100 present
	out, err := JoinWAV([][]byte{good, truncated})
	if err != nil {
		t.Fatalf("JoinWAV(truncated part) = %v, want the real bytes joined", err)
	}
	if got := int(binary.LittleEndian.Uint32(out[4:8])); got != len(out)-8 {
		t.Errorf("RIFF size = %d, want %d (no invented length)", got, len(out)-8)
	}
	if data := dataChunk(t, out); len(data) != 300 {
		t.Errorf("data length = %d, want 300 (200 + the 100 bytes that exist)", len(data))
	}
}

// JOIN-007: an unknown format cannot be joined.
func TestJoinUnknownFormat(t *testing.T) {
	if _, err := JoinAudio("flac", [][]byte{wav(10, 24000)}); err == nil {
		t.Error("JoinAudio(flac) = nil error, want a refusal")
	}
}
