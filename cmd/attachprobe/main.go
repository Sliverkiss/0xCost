// attachprobe: does the attachment path really lift the 102,400-character query
// ceiling (ADR 0015, docs/attachments-reverse.md)?
//
// The question cannot be answered by reading the bundle: the SPA sends a chat
// payload with fields we can only guess at, and a field the server ignores looks
// exactly like a field it accepts. So the probe plants a fact that cannot be
// guessed, buries it in a file larger than the query ceiling, attaches it, and
// asks for it:
//
//	go run ./cmd/attachprobe data/accounts.json [model] [chars]
//
// PASS means the model answered with the marker: attachments are read, are
// accounted separately, and the ceiling is not the query's anymore.
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

const marker = "ZQ-7788"

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
	// Where the marker sits decides what a NO-MARKER answer means: at the TAIL it
	// conflates "the attachment was not delivered" with "it was delivered and
	// truncated"; at the HEAD only the former.
	markerAtHead := os.Getenv("MARKER") == "head"

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// A file comfortably larger than the query ceiling, with the fact at the END:
	// a model that only sees the head cannot answer it.
	filler := "这是一段用于测试附件通路的长文本，内容本身没有意义。"
	pad := strings.Repeat(filler, chars/len([]rune(filler))+1)
	var body []byte
	where := "tail"
	if markerAtHead {
		where = "head"
		body = []byte("仓库编号 " + marker + "\n" + pad)
	} else {
		body = []byte(pad + "\n仓库编号 " + marker + "\n")
	}
	fmt.Printf("file: %d bytes (~%d chars), marker at the %s: %s\n", len(body), len([]rune(string(body))), where, marker)

	up, err := c.UploadFile(ctx, cookie, ph, "probe.txt", body)
	if err != nil {
		fmt.Println("UPLOAD-FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("UPLOAD-OK: objectName=%s\n", up.ObjectName)

	parsed, err := c.ParseResource(ctx, cookie, ph, up.ResourceURL, up.ObjectName, model)
	if err != nil {
		fmt.Println("PARSE-FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("PARSE-OK: id=%s tokenUsage=%d (the query ceiling is not involved)\n", parsed.ID, parsed.TokenUsage)

	query := "只回答该文档里的仓库编号，不要解释。"
	variants := []struct {
		name string
		item map[string]any
	}{
		{"full media item", map[string]any{
			"name": "probe.txt", "mediaType": "file", "fileUrl": up.ResourceURL,
			"objectName": up.ObjectName, "url": parsed.ID, "tokenUsage": parsed.TokenUsage,
			"status": "completed",
		}},
		{"minimal media item", map[string]any{
			"name": "probe.txt", "fileUrl": up.ResourceURL, "objectName": up.ObjectName,
			"url": parsed.ID,
		}},
	}
	if os.Getenv("VARIANTS") == "1" {
		// A minimal media item is refused by the upstream (busy frame), so a
		// sweep of sizes only needs the full item.
		variants = variants[:1]
	}
	for _, v := range variants {
		start := time.Now()
		var res *mimo.ChatResult
		var err error
		// The upstream answers 服务器繁忙 under load; one retry is the difference
		// between a measurement and a hole in the data.
		for attempt := 1; attempt <= 3; attempt++ {
			res, err = c.Chat(ctx, cookie, ph, mimo.ChatRequest{
				Query: query, Model: model, MultiMedias: []any{v.item},
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
		verdict := "NO-MARKER"
		if strings.Contains(answer, marker) {
			verdict = "PASS"
		}
		fmt.Printf("CHAT [%s] %s in %s: %q\n", v.name, verdict, time.Since(start).Round(time.Millisecond),
			firstRunes(answer, 120))
	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
