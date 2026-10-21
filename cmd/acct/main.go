// Command acct is the identity-pool operator CLI.
//
// Usage:
//
//	acct list                      list identities and pool counters
//	acct acquire [n]               acquire one or n fresh identities (default 1)
//	acct retire <uid>              discard an identity from the pool
//	acct refill                    top the virgin stock up to warm_size
//	acct export                    print the raw state document
//	acct probe <uid>               read one identity's live chat_status
//
// Mutations go through the running gateway (/admin/*) so they reach the live
// in-memory pool and are persisted by its flusher; editing state.json behind a
// running process would be overwritten on the next flush.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"chatglm2api/internal/config"
)

type statusResponse struct {
	Service string `json:"service"`
	Pool    struct {
		Stock          int  `json:"stock"`
		Total          int  `json:"total"`
		InFlight       int  `json:"in_flight"`
		Waiters        int  `json:"waiters"`
		WarmSize       int  `json:"warm_size"`
		MaxSize        int  `json:"max_size"`
		AcquireFailing bool `json:"acquire_failing"`
		Stats          struct {
			AcquiredTotal     int64 `json:"acquired_total"`
			ServedTotal       int64 `json:"served_total"`
			DiscardTotal      int64 `json:"discard_total"`
			RestockFailures   int64 `json:"restock_failures"`
			MessagesAbandoned int64 `json:"messages_abandoned"`
			BusyRetries       int64 `json:"busy_retries"`
			AcquireFailures   int64 `json:"acquire_failures"`
			WaitTimeouts      int64 `json:"wait_timeouts"`
		} `json:"stats"`
		Identities []struct {
			UID      string `json:"uid"`
			State    string `json:"state"`
			InFlight bool   `json:"in_flight"`
		} `json:"identities"`
	} `json:"pool"`
}

func main() {
	var (
		cfgPath = flag.String("config", "config.json", "path to config json")
		server  = flag.String("server", "", "gateway base URL (overrides config listen)")
		timeout = flag.Duration("timeout", 90*time.Second, "request timeout")
	)
	flag.Usage = usage
	flag.Parse()
	if err := run(flag.Args(), *cfgPath, *server, *timeout, os.Stdout); err != nil {
		if errors.Is(err, errUsage) {
			usage()
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "acct: %v\n", err)
		os.Exit(1)
	}
}

// errUsage signals a malformed invocation (exit code 2).
var errUsage = errors.New("usage")

// run executes one CLI invocation. It is separated from main so tests can drive
// the full command surface against an httptest gateway.
func run(args []string, cfgPath, server string, timeout time.Duration, out io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	base, key, _, err := resolveTarget(cfgPath, server)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}

	switch args[0] {
	case "list", "ls":
		st, err := fetchStatus(client, base, key)
		if err != nil {
			return err
		}
		printStatus(out, st)
		return nil
	case "acquire":
		n := 1
		if len(args) > 1 {
			v, err := strconv.Atoi(args[1])
			if err != nil || v <= 0 {
				return fmt.Errorf("acquire: invalid count %q", args[1])
			}
			n = v
		}
		for i := 0; i < n; i++ {
			raw, err := postJSON(client, base, key, "/admin/identities/acquire", nil)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, string(raw))
		}
		return nil
	case "retire", "rm":
		if len(args) < 2 {
			return fmt.Errorf("retire: a uid is required")
		}
		raw, err := postJSON(client, base, key, "/admin/identities/"+url.PathEscape(args[1])+"/retire", nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(raw))
		return nil
	case "refill":
		raw, err := postJSON(client, base, key, "/admin/pool/refill", nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(raw))
		return nil
	case "export":
		raw, err := getRaw(client, base, key, "/admin/export")
		if err != nil {
			return err
		}
		_, err = out.Write(raw)
		return err
	case "probe":
		if len(args) < 2 {
			return fmt.Errorf("probe: a uid is required")
		}
		return probe(client, base, key, args[1], out)
	default:
		return fmt.Errorf("unknown command %q: %w", args[0], errUsage)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `acct — anonymous identity pool operator CLI

Usage:
  acct list                     list identities, counters and capacity
  acct acquire [n]              acquire one or n fresh identities
  acct retire <uid>             discard an identity from the running pool
  acct refill                   top the virgin stock up to warm_size
  acct export                   print the raw pool state (contains tokens)
  acct probe <uid>              read one identity's live chat_status

Options:
  -config <path>   config path (default config.json)
  -server <url>    gateway base URL, e.g. http://127.0.0.1:7988
  -timeout <dur>   request timeout (default 90s)

Mutations require admin.enabled=true and a non-empty api_key in the config.
`)
}

func resolveTarget(cfgPath, server string) (string, string, *config.Config, error) {
	// Use the same loader as the server so env overrides (CGLM_LISTEN,
	// CGLM_API_KEY, ...) and defaults cannot drift between the two binaries.
	cfg, err := config.LoadOptional(cfgPath)
	if err != nil {
		return "", "", nil, err
	}
	base := strings.TrimRight(server, "/")
	if base == "" {
		base = listenToURL(cfg.Listen)
	}
	return base, cfg.APIKey, cfg, nil
}

func listenToURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func fetchStatus(c *http.Client, base, key string) (*statusResponse, error) {
	raw, err := getRaw(c, base, key, "/status")
	if err != nil {
		return nil, err
	}
	var st statusResponse
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &st, nil
}

func printStatus(out io.Writer, st *statusResponse) {
	p := st.Pool
	fmt.Fprintf(out, "service=%s stock=%d/%d total=%d in_flight=%d waiters=%d\n",
		st.Service, p.Stock, p.WarmSize, p.Total, p.InFlight, p.Waiters)
	fmt.Fprintf(out, "stats: acquired=%d served=%d discarded=%d restock_failures=%d abandoned=%d busy=%d acquire_failures=%d wait_timeouts=%d\n",
		p.Stats.AcquiredTotal, p.Stats.ServedTotal, p.Stats.DiscardTotal, p.Stats.RestockFailures,
		p.Stats.MessagesAbandoned, p.Stats.BusyRetries, p.Stats.AcquireFailures, p.Stats.WaitTimeouts)
	if p.AcquireFailing {
		fmt.Fprintln(out, "WARNING: acquisition is failing; serving from the existing pool")
	}
	if len(p.Identities) == 0 {
		return
	}
	sort.Slice(p.Identities, func(i, j int) bool { return p.Identities[i].UID < p.Identities[j].UID })
	fmt.Fprintf(out, "\n%-26s %-9s %s\n", "UID", "STATE", "INFLT")
	for _, id := range p.Identities {
		fmt.Fprintf(out, "%-26s %-9s %t\n", id.UID, id.State, id.InFlight)
	}
}

func probe(c *http.Client, base, key, uid string, out io.Writer) error {
	raw, err := getRaw(c, base, key, "/admin/identities/"+url.PathEscape(uid)+"/status")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(raw))
	return nil
}

func getRaw(c *http.Client, base, key, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	auth(req, key)
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func postJSON(c *http.Client, base, key, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	auth(req, key)
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func auth(req *http.Request, key string) {
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}
