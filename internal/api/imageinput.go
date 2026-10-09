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
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/mimo"
)

// imageHTTPClient fetches client-supplied image URLs. It is deliberately its own
// client: a slow or hostile URL must not hold a pooled account's connection
// settings, and the fetch is bounded by its own timeout and byte cap.
var imageHTTPClient = &http.Client{Timeout: 30 * time.Second}

// attachmentMaxRedirects is not configurable on purpose: a redirect chain is not
// how anyone serves an image, and each hop is a fresh chance to fetch something
// enormous.
// (Kept as a named constant so the intent survives future edits.)
const attachmentMaxRedirects = 5

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
func fetchImage(ctx context.Context, rawURL string, maxBytes int, ua string) (*fetchedImage, error) {
	switch {
	case strings.HasPrefix(strings.ToLower(rawURL), "data:"):
		return decodeDataImage(rawURL, maxBytes)
	case strings.HasPrefix(strings.ToLower(rawURL), "http://"),
		strings.HasPrefix(strings.ToLower(rawURL), "https://"):
		return downloadImage(ctx, rawURL, maxBytes, ua)
	default:
		return nil, errUnsupportedImageURL
	}
}

// decodeDataImage decodes a data:image/...;base64, payload.
func decodeDataImage(rawURL string, maxBytes int) (*fetchedImage, error) {
	header, payload, ok := strings.Cut(rawURL, ",")
	if !ok {
		return nil, fmt.Errorf("%w: data URL has no comma before its payload", errUnsupportedImageURL)
	}
	meta := strings.TrimPrefix(strings.ToLower(header), "data:")
	mime, encoding, _ := strings.Cut(meta, ";")
	if !strings.HasPrefix(mime, "image/") {
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
	return &fetchedImage{Data: data, Name: "image-1" + imageExt(mime), Mime: mime, Bytes: len(data)}, nil
}

// downloadImage fetches an http(s) image URL with the byte cap applied while
// reading (a Content-Length header is a claim, not a fact).
func downloadImage(ctx context.Context, rawURL string, maxBytes int, ua string) (*fetchedImage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnsupportedImageURL, err)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Header.Set("Accept", "image/*")
	resp, err := imageHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errImageFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return nil, fmt.Errorf("%w: %s answered HTTP %d", errImageFetch, hostOf(rawURL), resp.StatusCode)
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if mime != "" && !strings.HasPrefix(mime, "image/") {
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
		if !strings.HasPrefix(mime, "image/") {
			return nil, fmt.Errorf("%w: %s answered %q bytes with no image content-type",
				errUnsupportedImageType, hostOf(rawURL), mime)
		}
	}
	name := path.Base(req.URL.Path)
	if name == "" || name == "/" || name == "." {
		name = "image"
	}
	if !strings.Contains(name, ".") {
		name += imageExt(mime)
	}
	return &fetchedImage{Data: data, Name: name, Mime: mime, Bytes: len(data)}, nil
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
		if sub, ok := strings.CutPrefix(strings.ToLower(mime), "image/"); ok && sub != "" {
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
		img, err := fetchImage(ctx, ref.URL, s.maxImageBytes, s.client.UserAgent())
		if err != nil {
			return nil, fmt.Errorf("image %d of %d: %w", i+1, len(refs), err)
		}
		if len(img.Data) == 0 {
			return nil, fmt.Errorf("image %d of %d: %w", i+1, len(refs), errImageFetch)
		}
		// image_url.detail is the client's; what the channel receives is ours.
		data, name, applied := applyImageDetail(img.Data, img.Name, ref.Detail)
		if !applied {
			s.debugf("chat: image %d/%d: level %q not applied (format not decodable here) — uploading full resolution",
				i+1, len(refs), ref.Detail)
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
