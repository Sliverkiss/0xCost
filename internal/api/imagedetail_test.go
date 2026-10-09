package api

// Unit tests for the two guards that sit between a client-supplied image and this
// process's memory: the decode bound (IMG-015) and the address gate plus the
// redirect cap on the fetching client (IMG-016). They live in the package because
// the reasons and the dial-time decision are not observable from the HTTP surface —
// what is observable there (400 image_url_blocked with no connection) is IMG-014.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hugePNGHeader builds a PNG whose header declares w×h and which carries no pixel
// data at all: DecodeConfig answers from the header alone, which is exactly how a
// decompression bomb announces itself before it costs anything.
func hugePNGHeader(w, h uint32) []byte {
	var ihdr bytes.Buffer
	_ = binary.Write(&ihdr, binary.BigEndian, w)
	_ = binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8-bit truecolour, deflate, adaptive, no interlace

	var out bytes.Buffer
	out.Write([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	payload := append([]byte("IHDR"), ihdr.Bytes()...)
	_ = binary.Write(&out, binary.BigEndian, uint32(len(ihdr.Bytes())))
	out.Write(payload)
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(payload))
	return out.Bytes()
}

func samplePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// IMG-015: a header that asks for more pixels than the bound is never decoded —
// the bytes go up as they arrived and the reason comes back for the log.
func TestIMG015DecodeBoundRefusesToDecodeABomb(t *testing.T) {
	bomb := hugePNGHeader(20000, 20000) // 400 MP ≈ 1.6 GB if decoded
	if len(bomb) > 128 {
		t.Fatalf("the bomb should be a header only, got %d bytes", len(bomb))
	}
	out, name, note := applyImageDetail(bomb, "big.png", "low", defaultMaxImagePixels)
	if note == "" {
		t.Error("a 400 MP image must come back with a reason")
	}
	if !strings.Contains(note, "megapixels") {
		t.Errorf("note = %q, want it to name the size", note)
	}
	if !bytes.Equal(out, bomb) {
		t.Error("the bytes must pass through untouched")
	}
	if name != "big.png" {
		t.Errorf("name = %q, want it unchanged", name)
	}

	// The same header under the bound is decoded and scaled: the bound is a limit,
	// not a way to skip the level.
	ok := samplePNG(t, 1024, 256)
	out, name, note = applyImageDetail(ok, "photo.png", "low", defaultMaxImagePixels)
	if note != "" {
		t.Fatalf("a 0.26 MP image must be scaled, note = %q", note)
	}
	got, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != 512 || b.Dy() != 128 {
		t.Errorf("scaled to %dx%d, want 512x128 (aspect preserved)", b.Dx(), b.Dy())
	}
	if name != "photo.png" {
		t.Errorf("name = %q, want photo.png", name)
	}

	// A format we cannot decode is passed through with its own reason.
	webp := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
	if _, _, note := applyImageDetail(webp, "x.webp", "low", defaultMaxImagePixels); note == "" {
		t.Error("an undecodable format must come back with a reason")
	}
	// auto/high have nothing to do and must not be reported as skipped.
	if _, _, note := applyImageDetail(bomb, "big.png", "high", defaultMaxImagePixels); note != "" {
		t.Errorf("note = %q, want empty for a level with nothing to do", note)
	}
}

// IMG-016: the address gate is what makes "fetch this URL for me" safe, and the
// redirect cap is a real cap (it used to be a constant nothing consulted).
func TestIMG016AddressGateAndRedirectCap(t *testing.T) {
	blocked := []string{
		"127.0.0.1:443", "10.1.2.3:80", "172.17.0.1:443", "172.31.255.1:80",
		"192.168.0.1:80", "169.254.169.254:80", "100.64.0.1:80", "0.0.0.0:80",
		"224.0.0.1:80", "[::1]:80", "[fc00::1]:80", "[fe80::1]:80", "[ff02::1]:80",
	}
	for _, addr := range blocked {
		if err := imageAddressAllowed(addr, false); err == nil {
			t.Errorf("%s was allowed, want refused", addr)
		} else if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "never fetched") {
			t.Errorf("%s: error = %v, want it to say the address is refused", addr, err)
		}
	}
	allowed := []string{"1.1.1.1:443", "8.8.8.8:53", "93.184.216.34:80", "[2606:4700:4700::1111]:443"}
	for _, addr := range allowed {
		if err := imageAddressAllowed(addr, false); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
	// The operator's escape hatch: a deployment serving its own images on purpose.
	if err := imageAddressAllowed("127.0.0.1:8080", true); err != nil {
		t.Errorf("allowPrivate must let a private address through, got %v", err)
	}
	// A name that does not resolve to an IP is refused rather than dialed blindly.
	if err := imageAddressAllowed("example.com:80", false); err == nil {
		t.Error("a non-literal host was allowed, want refused")
	}

	// A redirect chain longer than attachmentMaxRedirects must stop there.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	hc := newImageHTTPClient(true) // loopback on purpose: this is the cap under test
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/loop", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("an endless redirect chain was followed to completion")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d redirects", attachmentMaxRedirects)) {
		t.Errorf("error = %v, want the redirect cap named", err)
	}
}
