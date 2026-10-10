// Attachment upload and parse (docs/attachments-reverse.md).
//
// The Studio's chat surface accepts files: the client uploads bytes to the
// resource surface, asks the platform to parse the file for a model, and then
// references the parse result in the chat request's `multiMedias` array. This is
// the one input path that does not spend the 102,400-character query budget
// (ADR 0015): `resource/parse` accounts for a file with its own `tokenUsage`,
// and the live catalog advertises 100 MB per file and 50 files per message.
//
// Pipeline (reverse-documented from the live bundle, not guessed):
//
//	POST /open-apis/resource/genUploadInfo {fileName, fileContentMd5}
//	  → {uploadUrl, resourceUrl, objectName}
//	PUT  <uploadUrl>                                  (signed object storage)
//	POST /open-apis/resource/parse?fileUrl&objectName&model
//	  → {id, tokenUsage, ...}                         (id = the media handle)
package mimo

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	// parseAttempts / parseRetryDelay bound the parse retry: three attempts with
	// a 0.5 s then 1 s pause, short enough to stay inside one request's budget
	// (ADR 0019) while covering the platform's transient parse failures.
	parseAttempts   = 3
	parseRetryDelay = 500 * time.Millisecond
)

// UploadedResource is what genUploadInfo returns for a file that is about to be
// PUT to storage.
type UploadedResource struct {
	UploadURL   string `json:"uploadUrl"`
	ResourceURL string `json:"resourceUrl"`
	ObjectName  string `json:"objectName"`
}

// ParsedResource is one parsed attachment. ID is the handle a chat request
// references; TokenUsage is how much of the model's budget the file occupies,
// accounted separately from the query text.
type ParsedResource struct {
	ID         string `json:"id"`
	TokenUsage int    `json:"tokenUsage"`
	// Extra keeps every other field the parse result carried (the payload is
	// schema-loose and the SPA only reads id/tokenUsage): useful for probing.
	Extra map[string]any `json:"-"`
}

// UploadFile uploads bytes to the resource surface. The PUT goes to signed
// object storage, so it carries the md5 and the browser header set but no
// upstream cookie — exactly like the ASR upload path (audio.go).
func (c *Client) UploadFile(ctx context.Context, cookie, ph, fileName string, data []byte) (*UploadedResource, error) {
	if fileName == "" {
		fileName = "file.txt"
	}
	sum := md5.Sum(data)
	digest := hex.EncodeToString(sum[:])
	var info UploadedResource
	if err := c.callJSON(ctx, cookie, ph, http.MethodPost, "/open-apis/resource/genUploadInfo", nil,
		map[string]any{"fileName": fileName, "fileContentMd5": digest}, &info); err != nil {
		return nil, err
	}
	if info.UploadURL == "" || info.ResourceURL == "" {
		return nil, errors.New("resource upload: genUploadInfo returned no uploadUrl/resourceUrl")
	}

	put, err := http.NewRequestWithContext(ctx, http.MethodPut, info.UploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c.setAistudioStorageHeaders(put.Header)
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Header.Set("content-md5", digest)
	put.ContentLength = int64(len(data))
	resp, err := c.http.Do(put)
	if err != nil {
		return nil, fmt.Errorf("resource upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("resource upload: storage %d: %.200s", resp.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return &info, nil
}

// ParseResource asks the platform to parse an uploaded file for a model. The
// parameters travel in the query string with an empty JSON body, which is what
// the SPA does.
func (c *Client) ParseResource(ctx context.Context, cookie, ph, fileURL, objectName, model string) (*ParsedResource, error) {
	if fileURL == "" || objectName == "" {
		return nil, errors.New("resource parse: fileUrl and objectName are required")
	}
	q := url.Values{}
	q.Set("fileUrl", fileURL)
	q.Set("objectName", objectName)
	if model != "" {
		q.Set("model", model)
	}
	var out struct {
		ID         string `json:"id"`
		TokenUsage int    `json:"tokenUsage"`
	}
	// The parse step is the flaky one: the platform answers a transient
	// business-code failure under load (the sibling project retries it five
	// times), and a request that carries an image is worth two more attempts —
	// the upload bytes are already spent by the time we get here. Context
	// cancellation and an auth failure are never retried.
	var lastErr error
	for attempt := 0; attempt < parseAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * parseRetryDelay):
			}
		}
		err := c.callJSON(ctx, cookie, ph, http.MethodPost, "/open-apis/resource/parse", q,
			map[string]any{}, &out)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		// A 401 (auth expired) and any 4xx are the caller's problem, not a
		// transient: only business codes and 5xx are worth another attempt.
		if _, ok := err.(*BizError); !ok {
			var ue *UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode < 500 {
				return nil, err
			}
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("resource parse: %w", lastErr)
	}
	if out.ID == "" {
		return nil, errors.New("resource parse: no parse result id (the platform parsed nothing)")
	}
	return &ParsedResource{ID: out.ID, TokenUsage: out.TokenUsage}, nil
}
