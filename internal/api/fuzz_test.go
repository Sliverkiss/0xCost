package api

// Fuzz targets (Go's built-in fuzzer, no dependency) for the three surfaces that
// parse or transform bytes a client chose. Each target is also a unit test: without
// -fuzz it runs the seed corpus, so the invariant it encodes is checked on every
// gate run. The invariants are about *our* bounds and panics — never about what the
// upstream would say.

import (
	"strings"
	"testing"
)

// FuzzChatRequestDecode: arbitrary JSON must never panic, must never smuggle an
// unbounded warning header out, and anything that decodes must survive projection
// onto adapter messages. FOUND before the bound: 3000 unknown fields produced a
// warning header past 64 KiB, so a client that sent extra metadata could not parse
// the response at all.
func FuzzChatRequestDecode(f *testing.F) {
	f.Add([]byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"low"}}]}]}`))
	f.Add([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":""}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"unknown","x":1}]}],"tools":[{}],"max_tokens":-1}`))
	f.Add([]byte(`{"model":"m","messages":[]}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"","content":null}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		req, err := decodeChatBody(raw)
		if err != nil {
			return
		}
		if hdr := req.warningsHeader(); len(hdr) > 4096 {
			t.Fatalf("warning header is %d bytes for %d bytes of request: %q", len(hdr), len(raw), hdr[:min(200, len(hdr))])
		}
		if _, err := req.toAdapterMsgs(); err != nil {
			return
		}
		_ = req.mediaErr(10)
		_ = req.imageDetailErr()
		_ = req.thinkingEnabled()
	})
}

// FuzzImageDetailLevel: any level string and any bytes. The bounds that matter are
// ours: never panic, never hand back an empty body for a non-empty one, never return
// a name that is not a usable file name.
func FuzzImageDetailLevel(f *testing.F) {
	f.Add([]byte("not an image"), "low")
	f.Add([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, "high")
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "low")
	f.Add([]byte{}, "")
	f.Add([]byte("x"), "LOW")
	f.Fuzz(func(t *testing.T, data []byte, detail string) {
		out, name, _ := applyImageDetail(data, "f.png", detail, defaultMaxImagePixels)
		if len(out) == 0 && len(data) > 0 {
			t.Fatalf("output is empty for %d bytes of input", len(data))
		}
		if name == "" {
			t.Fatal("name must never be empty")
		}
		if strings.ContainsAny(name, "/\\") {
			t.Fatalf("name %q must be a file name, not a path", name)
		}
		if _, err := finaliseImage(&fetchedImage{Data: data, Name: name, Mime: "image/png", Bytes: len(data)}); err != nil {
			return // a refusal is a fine answer; a panic is not
		}
	})
}

// FuzzDataImageURL: the inline path is string parsing plus one base64 decode, and it
// is the one image path a client drives with no network at all.
func FuzzDataImageURL(f *testing.F) {
	f.Add("data:image/png;base64,AAAA")
	f.Add("data:image/png,notbase64")
	f.Add("data:text/plain;base64,AAAA")
	f.Add("data:image/svg+xml;base64,PHN2Zy8+")
	f.Add("data:")
	f.Add("data:image/png;base64,")
	f.Fuzz(func(t *testing.T, raw string) {
		if !strings.HasPrefix(raw, "data:") {
			return // http(s) fetching is IMG-014's subject, not this target's
		}
		img, err := decodeDataImage(raw, 1<<20)
		if err != nil {
			return
		}
		if img.Bytes > 1<<20 {
			t.Fatalf("decoded %d bytes over the 1 MiB cap", img.Bytes)
		}
		if img.Name == "" || strings.ContainsAny(img.Name, "/\\") {
			t.Fatalf("name %q must be a file name, not a path", img.Name)
		}
	})
}
