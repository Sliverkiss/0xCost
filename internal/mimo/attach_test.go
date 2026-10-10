package mimo

// Client-level tests for the attachment surface (docs/attachments-reverse.md):
// upload → PUT → parse, the pipeline that lets a caller send far more than the
// 102,400-character query ceiling (ADR 0015).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testutil"
)

// ATTACH-001: upload walks genUploadInfo → signed PUT with the md5 header and
// returns the resource coordinates the parse step needs.
func TestUploadFilePipeline(t *testing.T) {
	c, up := audioClient(t)
	body := []byte("仓库编号 ZQ-7788\n")

	res, err := c.UploadFile(context.Background(), testutil.TestCookie, testutil.TestPH, "probe.txt", body)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.ObjectName == "" || res.ResourceURL == "" {
		t.Fatalf("resource = %+v, want objectName and resourceUrl", res)
	}

	paths := audioPaths(up)
	want := []string{"/open-apis/resource/genUploadInfo", "/mock-upload/audio.wav"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("upstream path order = %v, want %v", paths, want)
	}
	if got := up.LastUpload(); string(got) != string(body) {
		t.Errorf("uploaded bytes = %q, want %q", got, body)
	}
	// The upload URL is signed storage: cookie must NOT travel to it, and the
	// rest of the set is the SPA's XHR branch verbatim.
	puts := 0
	for _, req := range up.AudioRequests() {
		if strings.HasPrefix(req.Path, "/mock-upload/") {
			puts++
			assertStoragePutHeaders(t, req.Header)
		}
	}
	if puts != 1 {
		t.Errorf("storage PUTs = %d, want 1", puts)
	}
}

// assertStoragePutHeaders pins the header set of the signed-storage PUT
// (ADR 0035). The SPA issues that PUT through its own client's XHR branch, so
// the client's two headers ride along and the browser contributes the XHR
// defaults for a cross-origin request: `Accept: */*`, cross-site sec-fetch, and
// **no Cookie** (XHR does not set withCredentials, and the URL is signed).
// An extra header here is a tell in the same way it is on the API surface.
func assertStoragePutHeaders(t *testing.T, h http.Header) {
	t.Helper()
	exact := map[string]string{
		"Accept":          "*/*",
		"Accept-Language": "zh-CN",
		"Content-Type":    "application/octet-stream",
		"sec-fetch-dest":  "empty",
		"sec-fetch-mode":  "cors",
		"sec-fetch-site":  "cross-site",
	}
	for k, v := range exact {
		if got := h.Get(k); got != v {
			t.Errorf("storage PUT %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"x-timeZone", "Origin", "Referer", "User-Agent", "sec-ch-ua", "sec-ch-ua-platform", "content-md5"} {
		if h.Get(k) == "" {
			t.Errorf("storage PUT is missing %s", k)
		}
	}
	for _, k := range []string{"Cookie", "X-Requested-With", "Authorization"} {
		if v := h.Get(k); v != "" {
			t.Errorf("storage PUT sends %s = %q; the SPA's XHR branch never sends it on the signed URL", k, v)
		}
	}
}

// ATTACH-002: parse sends its parameters in the query string (empty JSON body,
// as the SPA does) and surfaces the handle plus the separate token accounting.
func TestParseResourceQueryParams(t *testing.T) {
	c, up := audioClient(t)
	up.SetParseResult(4321, false)

	parsed, err := c.ParseResource(context.Background(), testutil.TestCookie, testutil.TestPH,
		"https://cdn/multimedia/1/probe.txt", "multimedia/1/probe.txt", "mimo-v2.6-pro")
	if err != nil {
		t.Fatalf("ParseResource: %v", err)
	}
	if parsed.ID != "parse-result-1" {
		t.Errorf("id = %q, want the parse handle", parsed.ID)
	}
	if parsed.TokenUsage != 4321 {
		t.Errorf("tokenUsage = %d, want 4321 (attachments are accounted separately from the query text)", parsed.TokenUsage)
	}
	reqs := up.ParseRequests()
	if len(reqs) != 1 {
		t.Fatalf("parse calls = %d, want 1", len(reqs))
	}
	got := reqs[0]
	if got[0] != "https://cdn/multimedia/1/probe.txt" || got[1] != "multimedia/1/probe.txt" || got[2] != "mimo-v2.6-pro" {
		t.Errorf("parse query = %v, want fileUrl/objectName/model in the query string", got)
	}
}

// ATTACH-003: an empty parse (the platform's code 6002 case) is an error, not a
// silent handle-less success.
func TestParseResourceEmptyIsAnError(t *testing.T) {
	c, up := audioClient(t)
	up.SetParseResult(0, true)

	if _, err := c.ParseResource(context.Background(), testutil.TestCookie, testutil.TestPH, "u", "o", "m"); err == nil {
		t.Fatal("want an error when the platform parses nothing")
	}
}

// ATTACH-004: a missing resource coordinate is refused before any request — the
// parse step cannot work without both.
func TestParseResourceRequiresCoordinates(t *testing.T) {
	c, up := audioClient(t)
	if _, err := c.ParseResource(context.Background(), testutil.TestCookie, testutil.TestPH, "", "o", "m"); err == nil {
		t.Fatal("want an error without fileUrl")
	}
	if len(up.ParseRequests()) != 0 {
		t.Error("a request was sent for a call that cannot succeed")
	}
}

// ATTACH-005: an attachment handle reaches the upstream inside multiMedias, and
// a request without attachments still sends an empty array (SDD §2.4: the field
// must never be null).
func TestChatCarriesAttachments(t *testing.T) {
	client, up := chatEnv(t)
	item := map[string]any{
		"name": "probe.txt", "fileUrl": "https://cdn/x/probe.txt",
		"objectName": "multimedia/1/probe.txt", "url": "parse-result-1", "tokenUsage": 4321,
	}
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "仓库编号是什么", Model: "mimo-v2.6-pro", MultiMedias: []any{item},
	}); err != nil {
		t.Fatal(err)
	}
	reqs := up.RequestsFor(testPH)
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	body := reqs[0].RawBody
	for _, want := range []string{`"multiMedias":[{"`, `"url":"parse-result-1"`, `"objectName":"multimedia/1/probe.txt"`} {
		if !strings.Contains(body, want) {
			t.Errorf("chat body = %s, want it to contain %s", body, want)
		}
	}

	// And with no attachments the array stays present and empty.
	if _, err := client.Chat(context.Background(), testCookie, testPH, ChatRequest{
		Query: "hi", Model: "mimo-v2.6-pro",
	}); err != nil {
		t.Fatal(err)
	}
	reqs = up.RequestsFor(testPH)
	if got := reqs[len(reqs)-1].RawBody; !strings.Contains(got, `"multiMedias":[]`) {
		t.Errorf("no-attachment body = %s, want multiMedias:[]", got)
	}
}
