// Image input (image_url content parts) — the client→bytes half of the
// attachment path.
//
// The bytes never travel in the query: they are uploaded through
// resource/genUploadInfo → PUT → resource/parse and referenced from the chat
// payload's `multiMedias` array, so an image does not spend the 102,400-character
// query budget (ADR 0015, docs/attachments-reverse.md). internal/mimo owns the
// upload itself; this file owns getting the bytes the client pointed at, with the
// limits and the failure names the client needs to see.
package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
)

// attachmentMaxRedirects is not configurable on purpose: a redirect chain is not
// how anyone serves an image, and each hop is a fresh chance to fetch something
// enormous (or to bounce us onto an address the first hop was not).
const attachmentMaxRedirects = 5

// imageBlockedNets are the destinations a client-supplied URL must never reach.
// The gateway is a public endpoint whose caller picks the URL, so "fetch this
// image for me" must not double as "probe your host's network for me": loopback,
// the private ranges, link-local (cloud metadata lives at 169.254.169.254),
// CGNAT, unspecified and multicast are all refused. Live-verified before this
// guard existed: a request could reach the host's own services through the docker
// bridge gateway and read their HTTP status codes back.
var imageBlockedNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("imageinput: bad blocked CIDR " + cidr)
		}
		nets = append(nets, n)
	}
	return nets
}()

// newImageHTTPClient builds the client that fetches client-supplied image URLs.
// It is deliberately its own client — a slow or hostile URL must not hold a pooled
// account's connection settings — and it carries two guards:
//
//   - every connection goes through imageAddressAllowed, the first hop and every
//     redirect hop alike, because the check runs at dial time on the address the
//     name actually resolved to;
//   - a redirect chain is capped at attachmentMaxRedirects.
//
// allowPrivate is the operator's escape hatch (MIMO2API_IMAGE_ALLOW_PRIVATE) for a
// deployment that legitimately serves images from its own network; it is off by
// default because the safe assumption is that a client-supplied URL is hostile.
func newImageHTTPClient(allowPrivate bool) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return imageAddressAllowed(address, allowPrivate)
		},
	}).DialContext
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: t,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= attachmentMaxRedirects {
				return fmt.Errorf("%w: more than %d redirects", errImageFetch, attachmentMaxRedirects)
			}
			return nil
		},
	}
}

// imageAddressAllowed is the dial-time gate. It runs after DNS resolution and
// before the connection, so a name that resolves into a blocked range is refused
// with the same certainty as a bare IP literal, on every hop of a redirect chain.
func imageAddressAllowed(address string, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", errImageFetch, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: %s did not resolve to an IP address", errImageFetch, host)
	}
	for _, n := range imageBlockedNets {
		if n.Contains(ip) {
			return fmt.Errorf("%w: %s is in %s (loopback, private and link-local addresses are never fetched on a client's behalf)",
				errImageURLBlocked, ip, n)
		}
	}
	return nil
}

// fetchedImage is one client image resolved to bytes, ready to upload.
type fetchedImage struct {
	Data  []byte
	Name  string // the file name handed to the platform; the extension carries the type
	Mime  string // detected/declared media type (image/png, image/jpeg, …)
	Bytes int
}

// fetchImage resolves one image_url value to bytes: a data: URL is decoded, an
// http(s) URL is downloaded with the configured cap. Everything else is refused
// by name (IMG-001/002/004/005).
func fetchImage(ctx context.Context, hc *http.Client, rawURL string, maxBytes int, ua string) (*fetchedImage, error) {
	switch {
	case strings.HasPrefix(strings.ToLower(rawURL), "data:"):
		return decodeDataImage(rawURL, maxBytes)
	case strings.HasPrefix(strings.ToLower(rawURL), "http://"),
		strings.HasPrefix(strings.ToLower(rawURL), "https://"):
		return downloadImage(ctx, hc, rawURL, maxBytes, ua)
	default:
		return nil, fmt.Errorf("%w: scheme %q is not fetchable, use https:, http: or data:image/...",
			errUnsupportedImageURL, urlSchemeOf(rawURL))
	}
}

// urlSchemeOf is the scheme of a URL-shaped string, shortened, for an error message.
func urlSchemeOf(rawURL string) string {
	scheme := rawURL
	if i := strings.IndexByte(scheme, ':'); i >= 0 {
		scheme = scheme[:i]
	}
	if len(scheme) > 20 {
		scheme = scheme[:20] + "..."
	}
	return strings.ToLower(scheme)
}

// decodeDataImage decodes a data:image/...;base64, payload.
func decodeDataImage(rawURL string, maxBytes int) (*fetchedImage, error) {
	header, payload, ok := strings.Cut(rawURL, ",")
	if !ok {
		return nil, fmt.Errorf("%w: data URL has no comma before its payload", errUnsupportedImageURL)
	}
	meta := strings.TrimPrefix(strings.ToLower(header), "data:")
	mime, encoding, _ := strings.Cut(meta, ";")
	if !imageMimeToken(mime) {
		return nil, fmt.Errorf("%w: %s is not an image type", errUnsupportedImageType, mime)
	}
	if encoding != "base64" {
		return nil, fmt.Errorf("%w: only base64 data URLs are accepted (got %q)", errUnsupportedImageURL, encoding)
	}
	// The base64 payload is 4/3 of the decoded size; check before decoding so a
	// huge inline image does not have to be materialised twice.
	if maxBytes > 0 && len(payload)/4*3 > maxBytes {
		return nil, fmt.Errorf("%w: inline image is about %d bytes, over the %d-byte cap",
			errImageTooLarge, len(payload)/4*3, maxBytes)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: base64 payload is malformed: %v", errUnsupportedImageURL, err)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return nil, fmt.Errorf("%w: inline image is %d bytes, over the %d-byte cap", errImageTooLarge, len(data), maxBytes)
	}
	return finaliseImage(&fetchedImage{Data: data, Name: "image-1", Mime: mime, Bytes: len(data)})
}

// downloadImage fetches an http(s) image URL with the byte cap applied while
// reading (a Content-Length header is a claim, not a fact).
func downloadImage(ctx context.Context, hc *http.Client, rawURL string, maxBytes int, ua string) (*fetchedImage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnsupportedImageURL, err)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Header.Set("Accept", "image/*")
	resp, err := hc.Do(req)
	if err != nil {
		// A refused address is its own answer — "I would not go there" — not a
		// fetch that happened to fail; keep the sentinel recognisable so the
		// mapping can say which of the two happened.
		if errors.Is(err, errImageURLBlocked) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errImageFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return nil, fmt.Errorf("%w: %s answered HTTP %d", errImageFetch, hostOf(rawURL), resp.StatusCode)
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if mime != "" && !imageMimeToken(mime) {
		return nil, fmt.Errorf("%w: %s answered %q", errUnsupportedImageType, hostOf(rawURL), mime)
	}
	limit := int64(maxBytes)
	if maxBytes > 0 {
		limit++ // read one byte past the cap so "exactly at the cap" is not "over"
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", errImageFetch, hostOf(rawURL), err)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return nil, fmt.Errorf("%w: %s is larger than the %d-byte cap", errImageTooLarge, hostOf(rawURL), maxBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: %s answered an empty body", errImageFetch, hostOf(rawURL))
	}
	if mime == "" {
		mime = http.DetectContentType(data)
		if !imageMimeToken(mime) {
			return nil, fmt.Errorf("%w: %s answered %q bytes with no image content-type",
				errUnsupportedImageType, hostOf(rawURL), mime)
		}
	}
	return finaliseImage(&fetchedImage{Data: data, Name: path.Base(req.URL.Path), Mime: mime, Bytes: len(data)})
}

// maxImageNameStem bounds the part of a client-supplied name that is kept; the
// extension this package appends is its own.
const maxImageNameStem = 80

// finaliseImage makes the bytes and what we tell the platform about them agree.
// Both the declared type and the name are client-controlled (a data: URL's media
// type, a URL's last path segment), so neither is allowed to be the answer:
//
//   - bytes the sniffer positively identifies as something other than an image are
//     refused (`data:image/png` over text used to be uploaded as an image), and
//   - the name the platform sees is rebuilt from the type the bytes are, so an
//     image served from /photo.php no longer reaches the platform as ".php".
//
// A format the sniffer cannot name (heic, avif — it answers application/octet-stream)
// keeps the client's declared type: unknown is not the same as not-an-image.
func finaliseImage(img *fetchedImage) (*fetchedImage, error) {
	if sniffed, ok := sniffedImageType(img.Data); ok {
		img.Mime = sniffed
	} else if sniffed != "" {
		return nil, fmt.Errorf("%w: the bytes are %s, not an image", errUnsupportedImageType, sniffed)
	}
	img.Name = nameForUpload(img.Name, img.Mime)
	return img, nil
}

// maxImageSubtypeLen bounds the subtype we are willing to carry into a file name.
const maxImageSubtypeLen = 24

// imageMimeToken reports whether a declared media type is a plain image type:
// "image/" followed by one token-shaped subtype. A prefix test on its own let
// "image//", "image/../.." and "image/png/x" through (found by FuzzDataImageURL with
// `data:imAge//;BAse64,000A`), and the subtype is exactly what the uploaded name's
// extension is built from — so a non-token subtype is a client putting path syntax
// where a file name goes, in the one place we then hand to someone else's storage.
func imageMimeToken(declared string) bool {
	sub, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(declared)), "image/")
	if !ok || sub == "" || len(sub) > maxImageSubtypeLen || strings.Contains(sub, "..") {
		return false
	}
	for i := 0; i < len(sub); i++ {
		c := sub[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '-':
		case c == '.':
			if i == 0 || i == len(sub)-1 { // leading/trailing dot: not a subtype
				return false
			}
		default:
			return false
		}
	}
	return true
}

// sniffedImageType reports the media type the bytes themselves declare. ok is true
// only when the sniffer named an image type; a non-empty result with ok false is a
// positive identification of something else (text, JSON, HTML…).
func sniffedImageType(data []byte) (mime string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(http.DetectContentType(data)))
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if strings.HasPrefix(s, "image/") {
		return s, true
	}
	if s == "application/octet-stream" {
		return "", false // unidentified, not disproved
	}
	return s, false
}

// nameForUpload rebuilds the platform-facing name so its extension matches the type
// the bytes actually are, keeping a sanitised stem from whatever the client sent.
func nameForUpload(raw, mime string) string {
	stem := strings.TrimSpace(path.Base(raw))
	if i := strings.LastIndexByte(stem, '.'); i > 0 {
		stem = stem[:i]
	}
	if len(stem) > maxImageNameStem {
		stem = stem[:maxImageNameStem]
	}
	if stem == "" || stem == "." || stem == "/" {
		stem = "image"
	}
	return stem + imageExt(mime)
}

// imageExt maps a media type onto the extension the platform should see
// (jpeg → .jpg, which is what the sibling client sends).
func imageExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	case "image/heic":
		return ".heic"
	case "image/tiff":
		return ".tiff"
	default:
		// An unknown but well-formed subtype keeps its own extension; anything else
		// falls back, so no client string can reach the uploaded name as path syntax.
		if sub, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/"); ok && imageMimeToken(mime) {
			return "." + sub
		}
		return ".img"
	}
}

// hostOf is the part of a URL worth showing in an error: the host (never the
// query string, which can carry tokens).
func hostOf(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rest := rawURL[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return "the image URL"
}

// uploadTurnAttachments resolves and uploads one turn's images through the
// account's session and returns the `multiMedias` entries to send with that turn.
//
// Uploads are per attempt and never cached: a parse result belongs to the session
// that created it, and a retry runs on another account (SDD §7.1).
func (s *Server) uploadTurnAttachments(ctx context.Context, cookie, ph, model string, refs []adapter.MediaRef) ([]any, error) {
	items := make([]any, 0, len(refs))
	for i, ref := range refs {
		img, err := fetchImage(ctx, s.imageClient, ref.URL, s.maxImageBytes, s.client.UserAgent())
		if err != nil {
			return nil, fmt.Errorf("image %d of %d: %w", i+1, len(refs), err)
		}
		if len(img.Data) == 0 {
			return nil, fmt.Errorf("image %d of %d: %w", i+1, len(refs), errImageFetch)
		}
		// image_url.detail is the client's; what the channel receives is ours.
		data, name, note := applyImageDetail(img.Data, img.Name, ref.Detail, s.maxImagePixels)
		if note != "" {
			s.debugf("chat: image %d/%d: level %q not applied — %s; uploading as it arrived",
				i+1, len(refs), ref.Detail, note)
		}
		up, err := s.client.UploadFile(ctx, cookie, ph, name, data)
		if err != nil {
			return nil, err
		}
		parsed, err := s.client.ParseResource(ctx, cookie, ph, up.ResourceURL, up.ObjectName, model)
		if err != nil {
			return nil, err
		}
		s.debugf("chat: attachment %d/%d %s %d bytes → tokenUsage=%d", i+1, len(refs), name, len(data), parsed.TokenUsage)
		items = append(items, mimo.AttachmentItem("image", name, len(data), up, parsed))
	}
	return items, nil
}
