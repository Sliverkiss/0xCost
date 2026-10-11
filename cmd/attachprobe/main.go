// attachprobe: does the attachment path really lift the 102,400-character query
// ceiling (ADR 0015, docs/attachments-reverse.md)?
//
// The question cannot be answered by reading the bundle: the SPA sends a chat
// payload with fields we can only guess at, and a field the server ignores looks
// exactly like a field it accepts. So the probe plants facts that cannot be
// guessed, buries them in files, attaches them, and asks for them:
//
//	go run ./cmd/attachprobe data/accounts.json [model] [chars-per-file]
//
// Environment:
//
//	MARKER=head|tail   where each file's fact sits (default tail)
//	FILES=n            how many documents to attach in one request (default 1)
//	VARIANTS=1         only the full media item (a minimal item is refused)
//
// PASS means the model answered with every file's marker: the attachments were
// read, are accounted separately, and the ceiling is not the query's anymore.
// With FILES>1 the answer also says whether several documents share one ingest
// window or each gets its own.
//
// It talks to the live upstream with the stored session (never prints it) and
// spends one account call per variant.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/mimo"
)

// markers are per-file facts; they are deliberately unguessable and unique so a
// "PASS" cannot come from the model inventing something plausible.
var markers = []string{"ZQ-7788", "ZQ-7789", "ZQ-7790", "ZQ-7791", "ZQ-7792", "ZQ-7793"}

type accountFile struct {
	Accounts []struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
		UserID  string `json:"user_id"`
		Session struct {
			Cookie string `json:"cookie"`
			PH     string `json:"ph"`
		} `json:"session"`
		Device map[string]string `json:"device"`
	} `json:"accounts"`
}

func main() {
	path := "data/accounts.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	model := "mimo-v2.6-pro"
	if len(os.Args) > 2 {
		model = os.Args[2]
	}
	chars := 200000
	if len(os.Args) > 3 {
		n, err := strconv.Atoi(os.Args[3])
		if err == nil && n > 0 {
			chars = n
		}
	}
	// Where each fact sits decides what a NO-MARKER answer means: at the TAIL it
	// conflates "the attachment was not delivered" with "it was delivered and
	// truncated"; at the HEAD only the former.
	markerAtHead := os.Getenv("MARKER") == "head"
	files := 1
	if n, err := strconv.Atoi(os.Getenv("FILES")); err == nil && n > 0 {
		files = n
	}
	if files > len(markers) {
		files = len(markers)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("read accounts:", err)
		os.Exit(1)
	}
	var af accountFile
	if err := json.Unmarshal(raw, &af); err != nil {
		fmt.Println("parse accounts:", err)
		os.Exit(1)
	}
	var cookie, ph string
	var device map[string]string
	for _, a := range af.Accounts {
		if a.Enabled && a.Session.Cookie != "" && a.Session.PH != "" {
			cookie, ph, device = a.Session.Cookie, a.Session.PH, a.Device
			break
		}
	}
	if cookie == "" || ph == "" {
		fmt.Println("no enabled account with a session (cookie + ph)")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	c := mimo.New(mimo.Options{
		Base:      cfg.UpstreamBase(),
		UserAgent: cfg.UserAgent(),
		SecChUA:   cfg.SecChUA(),
		Timezone:  cfg.Timezone(),
	}).WithDevice(device)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Each document is comfortably larger than a share of the query ceiling, and
	// its fact sits where the environment says.
	// CJK filler by default; ASCII on request, because the platform accounts
	// attachments in TOKENS — so an English document should fit far more
	// characters in the same window than a Chinese one.
	filler := "这是一段用于测试附件通路的长文本，内容本身没有意义。"
	if strings.EqualFold(os.Getenv("FILLER"), "ascii") {
		filler = "This filler text exists only to give the attachment a measurable length. "
	}
	pad := strings.Repeat(filler, chars/len([]rune(filler))+1)

	var items []any
	where := "tail"
	if markerAtHead {
		where = "head"
	}
	fmt.Printf("attaching %d document(s) of ~%d chars each (fact at the %s): %s\n",
		files, chars, where, strings.Join(markers[:files], " "))

	for i := 0; i < files; i++ {
		name := fmt.Sprintf("probe%d.txt", i+1)
		var body []byte
		if markerAtHead {
			body = []byte("仓库编号 " + markers[i] + "\n" + pad)
		} else {
			body = []byte(pad + "\n仓库编号 " + markers[i] + "\n")
		}
		up, err := c.UploadFile(ctx, cookie, ph, name, body)
		if err != nil {
			fmt.Printf("UPLOAD-FAILED [%s]: %v\n", name, err)
			os.Exit(1)
		}
		parsed, err := c.ParseResource(ctx, cookie, ph, up.ResourceURL, up.ObjectName, model)
		if err != nil {
			fmt.Printf("PARSE-FAILED [%s]: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("  [%s] %d bytes → tokenUsage=%d id=%s\n", name, len(body), parsed.TokenUsage, parsed.ID)
		items = append(items, map[string]any{
			"name": name, "mediaType": "file", "fileUrl": up.ResourceURL,
			"objectName": up.ObjectName, "url": parsed.ID, "tokenUsage": parsed.TokenUsage,
			"status": "completed",
		})
	}

	query := "只回答这些文档里的仓库编号，按文档顺序用英文逗号分隔，不要解释。"
	if files == 1 {
		query = "只回答该文档里的仓库编号，不要解释。"
	}
	variants := []struct {
		name string
		item []any
	}{
		{"full media items", items},
		{"minimal media item", minimalize(items)},
	}
	if os.Getenv("VARIANTS") == "1" {
		variants = variants[:1]
	}

	for _, v := range variants {
		start := time.Now()
		var res *mimo.ChatResult
		var err error
		// The upstream answers 服务器繁忙 under load; retries are the difference
		// between a measurement and a hole in the data.
		for attempt := 1; attempt <= 3; attempt++ {
			res, err = c.Chat(ctx, cookie, ph, mimo.ChatRequest{
				Query: query, Model: model, MultiMedias: v.item,
			})
			if err == nil || !strings.Contains(err.Error(), "繁忙") {
				break
			}
			time.Sleep(time.Duration(attempt) * 4 * time.Second)
		}
		if err != nil {
			fmt.Printf("CHAT-FAILED [%s] after %s: %v\n", v.name, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		answer := strings.TrimSpace(res.Content)
		var found []string
		for i := 0; i < files; i++ {
			if strings.Contains(answer, markers[i]) {
				found = append(found, markers[i])
			}
		}
		verdict := "NO-MARKER"
		switch {
		case len(found) == files:
			verdict = "PASS (all documents)"
		case len(found) > 0:
			verdict = fmt.Sprintf("PARTIAL (%d/%d documents)", len(found), files)
		}
		fmt.Printf("CHAT [%s] %s in %s: %q\n", v.name, verdict,
			time.Since(start).Round(time.Millisecond), firstRunes(answer, 140))
	}
}

// minimalize strips a media item down to the coordinates only — the shape that
// the upstream refuses with its busy frame.
func minimalize(items []any) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"name": m["name"], "fileUrl": m["fileUrl"], "objectName": m["objectName"], "url": m["url"],
		})
	}
	return out
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
