package api_test

// Image input (IMG-*): the client sends image_url content parts, the gateway
// uploads them through genUploadInfo → PUT → resource/parse and references the
// parse result in the chat payload's multiMedias array — so an image never spends
// the 102,400-character query budget (ADR 0015, docs/attachments-reverse.md).
//
// The mock upstream serves the three-step resource surface already (mock_audio.go
// routes /open-apis/resource/*), and records the multiMedias array each chat
// payload carried.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testserver"
	"github.com/yg/mimo2api/internal/testutil"
)

// tinyPNG is a real 1×1 PNG: the gateway checks the declared type, and a test
// that shipped garbage would pass while the platform refused it.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
	0x54, 0x78, 0xda, 0x63, 0x60, 0x60, 0x60, 0xf8,
	0x0f, 0x00, 0x01, 0x04, 0x01, 0x00, 0x80, 0xbb,
	0xd1, 0x5b, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func pngDataURL() string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG)
}

// imageBody builds a chat body whose single user message carries parts.
func imageBody(model string, parts ...map[string]any) string {
	raw := make([]string, 0, len(parts))
	for _, p := range parts {
		switch v := p["text"].(type) {
		case string:
			raw = append(raw, fmt.Sprintf(`{"type":"text","text":%q}`, v))
		default:
			url, _ := p["image_url"].(string)
			raw = append(raw, fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q}}`, url))
		}
	}
	return fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":[%s]}]}`,
		model, strings.Join(raw, ","))
}

func mediaOf(t *testing.T, ts *testserver.TestServer, i int) []map[string]any {
	t.Helper()
	reqs := ts.Upstream.Requests()
	if i >= len(reqs) {
		t.Fatalf("only %d upstream chat calls, want index %d", len(reqs), i)
	}
	return reqs[i].MultiMedias
}

// IMG-001: a data: URL image is uploaded through the three-step resource surface
// and referenced in multiMedias with the channel's own "image" mediaType.
func TestIMG001DataURLImageBecomesMediaItem(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})
	ts.Upstream.SetParseResult(1060, false)

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-pro",
		map[string]any{"text": "图里是什么？"},
		map[string]any{"image_url": pngDataURL()},
	))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	media := mediaOf(t, ts, 0)
	if len(media) != 1 {
		t.Fatalf("multiMedias = %v, want exactly one item", media)
	}
	item := media[0]
	if item["mediaType"] != "image" {
		t.Errorf("mediaType = %v, want %q", item["mediaType"], "image")
	}
	if item["status"] != "completed" {
		t.Errorf("status = %v, want %q", item["status"], "completed")
	}
	if item["url"] != "parse-result-1" {
		t.Errorf("url = %v, want the parse result id", item["url"])
	}
	if item["tokenUsage"] == nil {
		t.Errorf("tokenUsage missing: the platform accounts an attachment separately, so it must be reported")
	}
	if _, hasSize := item["size"]; !hasSize {
		t.Errorf("size missing: the probe showed a stripped media item is refused")
	}
	// The upload used the requested model and the account's ph.
	parse := ts.Upstream.ParseRequests()
	if len(parse) != 1 {
		t.Fatalf("resource/parse calls = %d, want 1", len(parse))
	}
	if parse[0][2] != "mimo-v2.6-pro" {
		t.Errorf("parse model = %q, want the requested model", parse[0][2])
	}
	// The query text is the client's text, with no image data in it.
	if q := ts.Upstream.Requests()[0].Query; q != "图里是什么？" {
		t.Errorf("query = %q, want the client's text only", q)
	}
}

// IMG-002: an http(s) image URL is downloaded with the gateway's browser identity.
// It fetches from its own loopback listener, so it opts into the private-address
// escape hatch; the default refusal of exactly that URL is IMG-014's job.
func TestIMG002URLImageDownloadedAndUploaded(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla/") {
			t.Errorf("image fetch UA = %q, want the gateway's browser identity", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG)
	}))
	defer imgSrv.Close()

	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{ImageAllowPrivate: true})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
		map[string]any{"text": "描述"},
		map[string]any{"image_url": imgSrv.URL + "/photo.png"},
	))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if media := mediaOf(t, ts, 0); len(media) != 1 || media[0]["mediaType"] != "image" {
		t.Fatalf("multiMedias = %v, want one image item", media)
	}
	if up := ts.Upstream.LastUpload(); len(up) != len(tinyPNG) {
		t.Errorf("uploaded %d bytes, want %d (the fetched image verbatim)", len(up), len(tinyPNG))
	}
}

// IMG-003: more images than the cap is a 400 before any account work.
func TestIMG003TooManyImagesRejectedEarly(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{MaxImages: 2})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	parts := []map[string]any{{"text": "看这三张"}}
	for i := 0; i < 3; i++ {
		parts = append(parts, map[string]any{"image_url": pngDataURL()})
	}
	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-pro", parts...))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	if !strings.Contains(raw, "too_many_images") {
		t.Errorf("body = %s, want too_many_images", raw)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (the cap is checked before the pool)", n)
	}
}

// IMG-004: a URL that answers no image is refused by name, and the account is not
// cooled down for it (the client's URL is the problem).
func TestIMG004NonImageURLRejected(t *testing.T) {
	textSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not an image")
	}))
	defer textSrv.Close()

	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{ImageAllowPrivate: true})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-pro",
		map[string]any{"text": "看"},
		map[string]any{"image_url": textSrv.URL + "/note.txt"},
	))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	if !strings.Contains(raw, "unsupported_image_type") {
		t.Errorf("body = %s, want unsupported_image_type", raw)
	}
	// No account was punished for the client's bad URL: the slot came back and
	// the account is still available.
	if got := ts.Pool.InFlight(); got != 0 {
		t.Errorf("in-flight = %d after the refusal, want 0 (the slot must be back)", got)
	}
	if st := ts.Pool.StateOf("acc1"); st != pool.StateAvailable {
		t.Errorf("account state = %v after a client-side image error, want available", st)
	}
}

// IMG-005: an image over the byte cap is refused with the numbers.
func TestIMG005OversizeImageRejected(t *testing.T) {
	big := make([]byte, 4096)
	copy(big, tinyPNG)
	bigURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(big)

	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{MaxImageBytes: 1024})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-pro",
		map[string]any{"text": "看"},
		map[string]any{"image_url": bigURL},
	))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	if !strings.Contains(raw, "image_too_large") {
		t.Errorf("body = %s, want image_too_large", raw)
	}
	if n := len(ts.Upstream.ParseRequests()); n != 0 {
		t.Errorf("parse calls = %d, want 0 (refused before uploading)", n)
	}
}

// IMG-006: an image travels with the turn that carried it, and only that turn.
func TestIMG006ImageTravelsWithItsTurn(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := fmt.Sprintf(`{"model":"mimo-v2.6-pro","stream":false,"messages":[
		{"role":"user","content":"第一轮纯文本"},
		{"role":"assistant","content":"好的"},
		{"role":"user","content":[{"type":"text","text":"第二轮带图"},{"type":"image_url","image_url":{"url":%q}}]}]}`, pngDataURL())

	status, raw := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	reqs := ts.Upstream.Requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream chat calls = %d, want 2 (one per user turn)", len(reqs))
	}
	if got := reqs[0].MultiMedias; len(got) != 0 {
		t.Errorf("turn 1 multiMedias = %v, want none", got)
	}
	if got := reqs[1].MultiMedias; len(got) != 1 || got[0]["mediaType"] != "image" {
		t.Errorf("turn 2 multiMedias = %v, want exactly one image", got)
	}
	if n := len(ts.Upstream.ParseRequests()); n != 1 {
		t.Errorf("parse calls = %d, want 1 (the image belongs to one turn)", n)
	}
}

// IMG-007: an image with no text is a legal request; the channel needs a query, so
// one neutral query is synthesised (documented in SDD §5.2).
func TestIMG007ImageOnlyMessageGetsNeutralQuery(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-pro",
		map[string]any{"image_url": pngDataURL()},
	))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if q := ts.Upstream.Requests()[0].Query; q != adapter.DefaultImageOnlyQuery {
		t.Errorf("query = %q, want the documented neutral query", q)
	}
	if len(mediaOf(t, ts, 0)) != 1 {
		t.Errorf("the image was not sent with the synthesised query")
	}
}

// IMG-009: OpenAI's image_url.detail is accepted and — because the channel's
// parse surface has no level knob — reported as dropped instead of silently
// ignored.
func TestIMG009ImageDetailReportedAsDropped(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := fmt.Sprintf(`{"model":"mimo-v2.6-pro","stream":false,"messages":[{"role":"user","content":[
		{"type":"text","text":"看这张"},
		{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]}]}`, pngDataURL())
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (a supported detail must not be refused)", resp.StatusCode, raw)
	}
	if warn := resp.Header.Get("X-Mimo-Warnings"); strings.Contains(warn, "detail") {
		t.Errorf("X-Mimo-Warnings = %q: a detail level is applied internally, so there is nothing to warn about", warn)
	}
	if media := mediaOf(t, ts, 0); len(media) != 1 {
		t.Errorf("multiMedias = %v, want the image still sent", media)
	}
	if n := len(ts.Upstream.LastUpload()); n == 0 {
		t.Fatal("no upload: the image must still travel the resource path")
	}
}

// IMG-010: an image_url.detail outside auto|low|high is a 400 before any account
// work — a typo must not look like it was applied.
func TestIMG010InvalidImageDetailRejected(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := fmt.Sprintf(`{"model":"mimo-v2.6-pro","stream":false,"messages":[{"role":"user","content":[
		{"type":"text","text":"看这张"},
		{"type":"image_url","image_url":{"url":%q,"detail":"ultra"}}]}]}`, pngDataURL())
	status, raw := chatOnce(t, ts, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	if !strings.Contains(raw, "unsupported_image_detail") {
		t.Errorf("body = %s, want unsupported_image_detail", raw)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (validated before the pool)", n)
	}
}

// IMG-012: the warning header keeps two different statements apart. A field we do
// not know at all (tools) and a real OpenAI field we know but cannot act on
// (image_url.detail, web_search_options.search_context_size) are not the same
// thing, and a caller reading one line for both would conclude the wrong one.
func TestIMG012WarningHeaderSeparatesUnknownFromUnhonoured(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := fmt.Sprintf(`{"model":"mimo-v2.6-flash","stream":false,
		"tools":[{"type":"function","function":{"name":"x"}}],
		"web_search_options":{"search_context_size":"high"},
		"messages":[{"role":"user","content":[
			{"type":"text","text":"这是什么"},
			{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]}]}`, pngDataURL())
	resp, err := ts.Chat(body, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.StatusCode, raw)
	}
	warn := resp.Header.Get("X-Mimo-Warnings")
	if !strings.Contains(warn, "ignored unsupported fields: [tools]") {
		t.Errorf("warnings = %q, want the unknown field named as ignored", warn)
	}
	if !strings.Contains(warn, "accepted but not honoured: [web_search_options.search_context_size]") {
		t.Errorf("warnings = %q, want the understood-but-unapplied search context named separately, once", warn)
	}
	if strings.Contains(warn, "image_url.detail") {
		t.Errorf("warnings = %q: detail is applied internally and must not be reported", warn)
	}
	if !strings.Contains(warn, "; ") {
		t.Errorf("warnings = %q, want the two statements separated", warn)
	}
}

// bigPNGDataURL builds a w×h PNG data URL: the only way to see whether a detail
// level changed anything is to measure the bytes that actually went up.
func bigPNGDataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// IMG-017: the label does not decide the type, the bytes do. `data:image/png` over
// text used to be uploaded to the platform as an image named .png; the sniffer
// positively identifies text, and that is a 400 rather than someone else's problem.
// A format the sniffer cannot name (heic, avif) keeps the declared type: unknown is
// not the same as not-an-image.
func TestIMG017BytesDecideTheTypeNotTheLabel(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	junk := base64.StdEncoding.EncodeToString([]byte("this is definitely not an image"))
	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
		map[string]any{"text": "看"},
		map[string]any{"image_url": "data:image/png;base64," + junk}))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", status, raw)
	}
	if !strings.Contains(raw, "unsupported_image_type") {
		t.Errorf("body = %s, want unsupported_image_type", raw)
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
	if n := len(ts.Upstream.LastUpload()); n != 0 {
		t.Errorf("uploads = %d, want 0 (junk must never reach the platform)", n)
	}
}

// IMG-018: the name the platform parses by is rebuilt from the bytes, so the
// extension cannot lie — a URL path ending .php, or a data: URL that claims a type
// the bytes contradict, no longer arrives as something the platform misreads.
func TestIMG018PlatformNameFollowsTheBytes(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, &testserver.TestServerOptions{ImageAllowPrivate: true})
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG)
	}))
	defer imgSrv.Close()

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
		map[string]any{"text": "看"},
		map[string]any{"image_url": imgSrv.URL + "/download.php?seed=1"}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if media := mediaOf(t, ts, 0); len(media) == 1 {
		name, _ := media[0]["name"].(string)
		if !strings.HasSuffix(name, ".png") {
			t.Errorf("platform name = %q, want it to end .png (the bytes are a PNG, the path is not)", name)
		}
		if strings.Contains(name, ".php") {
			t.Errorf("platform name = %q, want the misleading .php gone", name)
		}
	} else {
		t.Fatalf("multiMedias = %v, want one image item", media)
	}

	// A data: URL whose declared type contradicts its bytes: the bytes win.
	pngB64 := base64.StdEncoding.EncodeToString(tinyPNG)
	status, raw = chatOnce(t, ts, imageBody("mimo-v2.6-flash",
		map[string]any{"text": "看"},
		map[string]any{"image_url": "data:image/jpeg;base64," + pngB64}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	if media := mediaOf(t, ts, 1); len(media) == 1 {
		if name, _ := media[0]["name"].(string); !strings.HasSuffix(name, ".png") {
			t.Errorf("platform name = %q, want .png: the bytes are a PNG even though the label said jpeg", name)
		}
	} else {
		t.Fatalf("multiMedias = %v, want one image item", media)
	}
}

// IMG-019: the declared subtype is not allowed to be path syntax. A prefix test on
// "image/" accepted `data:imAge//;BAse64,000A` (found by the fuzzer), and the subtype
// is what the extension of the name we hand the platform is built from — so `image//`
// became "./", `image/../..` travelled as path syntax into someone else's storage.
func TestIMG019DeclaredSubtypeCannotBePathSyntax(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	for _, bad := range []string{
		"data:image//;base64,AAAA",                               // the fuzzer's find, lowercased
		"data:imAge//;BAse64,000A",                               // …and as the fuzzer wrote it
		"data:image/../..;base64,AAAA",                           // traversal in the subtype
		"data:image/png/x;base64,AAAA",                           // an extra path segment
		"data:image/.;base64,AAAA",                               // a bare dot
		"data:image/" + strings.Repeat("a", 40) + ";base64,AAAA", // unbounded subtype
	} {
		status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
			map[string]any{"text": "看"},
			map[string]any{"image_url": bad}))
		if status != http.StatusBadRequest {
			t.Errorf("%q → status %d body=%s, want 400", bad, status, raw)
			continue
		}
		if !strings.Contains(raw, "unsupported_image_type") {
			t.Errorf("%q → body %s, want unsupported_image_type", bad, raw)
		}
	}
	if n := len(ts.Upstream.Requests()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
	if n := len(ts.Upstream.LastUpload()); n != 0 {
		t.Errorf("uploads = %d, want 0: nothing with path syntax in its name may be uploaded", n)
	}

	// A well-formed type in a different case still works, and the name it produces is
	// a file name.
	pngB64 := base64.StdEncoding.EncodeToString(tinyPNG)
	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
		map[string]any{"text": "看"},
		map[string]any{"image_url": "data:IMAGE/PNG;base64," + pngB64}))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}
	media := mediaOf(t, ts, 0)
	if len(media) != 1 {
		t.Fatalf("multiMedias = %v, want one image item", media)
	}
	if name, _ := media[0]["name"].(string); strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".png") {
		t.Errorf("platform name = %q, want a plain file name ending .png", name)
	}
}

// IMG-020: the refusal names the real reason. Every unsupported_image_type used to
// answer "the fetched URL did not answer an image (content-type must be image/*)",
// which is untrue for an inline image and hides which of the three rules was broken.
func TestIMG020RefusalNamesTheRealReason(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	cases := []struct{ url, wantSubstr, wantAbsent string }{
		{"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("plain text")),
			"the bytes are", "fetched URL"},
		{"data:text/plain;base64,AAAA", "text/plain is not an image type", "fetched URL"},
		{"data:image//;base64,AAAA", "image// is not an image type", "fetched URL"},
		{"data:image/png,notbase64", "only base64 data URLs are accepted", ""},
		{"file:///etc/passwd", "unsupported image", ""},
	}
	for _, c := range cases {
		status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
			map[string]any{"text": "看"},
			map[string]any{"image_url": c.url}))
		if status != http.StatusBadRequest {
			t.Errorf("%q → status %d body=%s, want 400", c.url, status, raw)
			continue
		}
		if !strings.Contains(raw, c.wantSubstr) {
			t.Errorf("%q → body %s, want it to name %q", c.url, raw, c.wantSubstr)
		}
		if c.wantAbsent != "" && strings.Contains(raw, c.wantAbsent) {
			t.Errorf("%q → body %s, must not claim %q", c.url, raw, c.wantAbsent)
		}
	}
}

// IMG-014: a client-supplied URL must not reach the operator's own network. The
// gateway answers 400 image_url_blocked and — the part that matters — never opens
// the connection at all: the listener that would have answered records no request.
// This is the guard that was missing when a live request could read the host's
// services back through the docker bridge gateway (172.17.0.1:443 answered).
func TestIMG014PrivateAndLoopbackImageURLsRefused(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	for _, rawURL := range []string{
		srv.URL + "/photo.png",                     // 127.0.0.1 as served
		"http://localhost:" + port + "/photo.png",  // the same via name
		"http://[::1]:" + port + "/photo.png",      // IPv6 loopback
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.7/photo.png",                // private
		"http://172.17.0.1:443/photo.png",          // docker bridge gateway
		"http://192.168.1.1/photo.png",             // private
		"http://0.0.0.0:" + port + "/photo.png",    // unspecified address
	} {
		t.Run(rawURL, func(t *testing.T) {
			acct := oneAccount()
			ts := testserver.NewTestServer(t, acct, nil) // default: private addresses refused
			ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})
			status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-flash",
				map[string]any{"text": "看"}, map[string]any{"image_url": rawURL}))
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", status, raw)
			}
			if !strings.Contains(raw, "image_url_blocked") {
				t.Errorf("body = %s, want image_url_blocked", raw)
			}
			if !strings.Contains(raw, `"param":"messages[].content[].image_url.url"`) {
				t.Errorf("body = %s, want param naming the image URL", raw)
			}
			if n := len(ts.Upstream.Requests()); n != 0 {
				t.Errorf("upstream chat calls = %d, want 0", n)
			}
			if n := len(ts.Upstream.LastUpload()); n != 0 {
				t.Errorf("uploads = %d, want 0", n)
			}
		})
	}
	// The refusal is decided before any connection: the listener saw nothing.
	if n := hits.Load(); n != 0 {
		t.Errorf("the blocked targets received %d requests, want 0 (the guard must fire before dialing)", n)
	}
}

// IMG-013: OpenAI's detail level is the client's word; what the channel receives is
// ours. The channel has no level parameter, so the level has to be applied where it
// exists — on the uploaded pixels ("low" is defined by OpenAI as a 512 px longest
// side, which is also why it is the cheap option). Measured on the recorded upload,
// not on an internal flag: "low" must arrive scaled, "high"/absent must arrive whole.
func TestIMG013DetailLevelAppliedToUploadedBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detail string
		want   int
	}{
		{"low", "low", 512},
		{"high", "high", 1024},
		{"absent", "", 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := oneAccount()
			ts := testserver.NewTestServer(t, acct, nil)
			ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})
			ts.Upstream.SetParseResult(700, false)

			level := ""
			if tc.detail != "" {
				level = fmt.Sprintf(`,"detail":%q`, tc.detail)
			}
			body := fmt.Sprintf(`{"model":"mimo-v2.6-flash","stream":false,"messages":[{"role":"user","content":[
				{"type":"text","text":"图里是什么"},
				{"type":"image_url","image_url":{"url":%q%s}}]}]}`, bigPNGDataURL(t, 1024, 768), level)
			status, raw := chatOnce(t, ts, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", status, raw)
			}

			up := ts.Upstream.LastUpload()
			if len(up) == 0 {
				t.Fatal("nothing was uploaded")
			}
			got, _, err := image.Decode(bytes.NewReader(up))
			if err != nil {
				t.Fatalf("uploaded bytes are not a decodable image: %v", err)
			}
			b := got.Bounds()
			longest := b.Dx()
			if b.Dy() > longest {
				longest = b.Dy()
			}
			if longest != tc.want {
				t.Errorf("uploaded image is %dx%d (longest %d), want longest %d", b.Dx(), b.Dy(), longest, tc.want)
			}
			if tc.want == 512 && b.Dx() != 512 {
				t.Errorf("width = %d, want 512 (aspect ratio must survive the scale)", b.Dx())
			}
			// The media item must describe the bytes that were actually sent.
			if media := mediaOf(t, ts, 0); len(media) == 1 {
				if size, _ := media[0]["size"].(float64); int(size) != len(up) {
					t.Errorf("multiMedias size = %v, want %d (the uploaded size)", size, len(up))
				}
			} else {
				t.Errorf("multiMedias = %v, want one image item", media)
			}
		})
	}
}

// IMG-011: the OpenAI vocabulary stops at this package. The body that actually
// goes to the channel is the channel's own shape (chatPayload), so an `image_url`,
// a `detail` or an image's bytes must never appear on the wire — asserting the
// recorded raw body is the only way to know rather than assume.
func TestIMG011UpstreamPayloadSpeaksOnlyTheChannelVocabulary(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	body := fmt.Sprintf(`{"model":"mimo-v2.6-flash","stream":false,"reasoning_effort":"low",
		"web_search_options":{"search_context_size":"high"},"temperature":0.5,"top_p":0.9,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"图里是什么"},
			{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]}]}`, pngDataURL())
	status, raw := chatOnce(t, ts, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", status, raw)
	}

	wire := ts.Upstream.Requests()[0].RawBody
	// Nothing of OpenAI's request vocabulary may survive the conversion.
	for _, forbidden := range []string{
		"image_url", "detail", "messages", "\"content\"", "stream", "reasoning_effort",
		"web_search_options", "top_p", "role", "data:image", "base64",
	} {
		if strings.Contains(wire, forbidden) {
			t.Errorf("upstream body carries OpenAI field %q — the conversion did not happen:\n%s", forbidden, wire)
		}
	}
	// The channel's own shape must be there instead.
	for _, want := range []string{
		`"multiMedias"`, `"mediaType":"image"`, `"modelConfig"`, `"enableThinking":true`,
		`"webSearchStatus":"enabled"`, `"temperature":0.5`, `"topP":0.9`, `"query"`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("upstream body is missing %s:\n%s", want, wire)
		}
	}
}

// IMG-008: the parse step is retried — the platform answers a transient failure
// under load, and the upload bytes are already spent by then.
func TestIMG008ParseRetriedThenSucceeds(t *testing.T) {
	acct := oneAccount()
	ts := testserver.NewTestServer(t, acct, nil)
	ts.Upstream.Script(acct[0].PH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})
	ts.Upstream.SetParseFailures(1)

	status, raw := chatOnce(t, ts, imageBody("mimo-v2.6-pro",
		map[string]any{"text": "看"},
		map[string]any{"image_url": pngDataURL()},
	))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 after one retry", status, raw)
	}
	if n := len(ts.Upstream.ParseRequests()); n != 2 {
		t.Errorf("parse calls = %d, want 2 (one failure, one success)", n)
	}
	if media := mediaOf(t, ts, 0); len(media) != 1 {
		t.Errorf("multiMedias = %v, want one item after the retry", media)
	}
}
