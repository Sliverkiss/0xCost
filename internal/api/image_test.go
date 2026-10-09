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
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
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
	ts := testserver.NewTestServer(t, acct, nil)
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
	ts := testserver.NewTestServer(t, acct, nil)
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
	// nothing was recorded against the account.
	if got := ts.Pool.InFlight(); got != 0 {
		t.Errorf("in-flight = %d after the refusal, want 0 (the slot must be back)", got)
	}
	recorded := 0
	for _, n := range ts.Pool.FailuresOf("acc1") {
		recorded += n
	}
	if recorded != 0 {
		t.Errorf("account recorded %d failures for a client-side image error, want none (%v)", recorded, ts.Pool.FailuresOf("acc1"))
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
	warn := resp.Header.Get("X-Mimo-Warnings")
	if !strings.Contains(warn, "image_url.detail") {
		t.Errorf("X-Mimo-Warnings = %q, want the unhonoured detail level named", warn)
	}
	if media := mediaOf(t, ts, 0); len(media) != 1 {
		t.Errorf("multiMedias = %v, want the image still sent", media)
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
	if !strings.Contains(warn, "accepted but not honoured: [web_search_options.search_context_size image_url.detail]") {
		t.Errorf("warnings = %q, want the understood-but-unapplied fields named separately, search context first, each once", warn)
	}
	if !strings.Contains(warn, "; ") {
		t.Errorf("warnings = %q, want the two statements separated", warn)
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
