// Redis (Upstash) account store — pure Go, stdlib only (ADR 0018).
//
// Why not go-redis: this project builds one static binary from the standard
// library alone; the store needs five commands (GET/SET/SADD/SREM/SMEMBERS),
// and a RESP2 client for five commands is ~200 lines against a dependency
// that would pull a module graph into the build. Tests exercise it against a
// fake RESP server, so the protocol code is covered without a dependency.
//
// Key schema (RedisBackend):
//
//	mimo2api:accounts:<id>   → JSON Account (credentials, device, runtime state)
//	mimo2api:accounts:index  → SET of every <id> in the store
//
// One key per account keeps a write O(1) instead of a read-modify-write of a
// whole document.
package accounts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// redisKeyPrefix / redisIndexKey are the store's key schema. A deployment
// sharing one Redis with another service stays namespaced by the prefix.
const (
	redisKeyPrefix = "mimo2api:accounts:"
	redisIndexKey  = "mimo2api:accounts:index"
)

// redisRootCAs is an optional extra CA pool (a test hook for a self-signed
// test server); nil in production, where the system roots verify Upstash.
var redisRootCAs *x509.CertPool

// ErrTokenRequired reports a DS_REDIS_HOST without DS_REDIS_TOKEN: the host
// needs assembly into a connection string, and dialing unauthenticated would
// fail (or, worse, silently read another tenant's namespace).
var ErrTokenRequired = errors.New("DS_REDIS_HOST requires DS_REDIS_TOKEN (or use a full rediss:// connection string)")

// respConn is a minimal RESP2 client: one TCP/TLS connection, AUTH, and the
// commands the store needs. No pipelining — one command, one reply, under a
// connection mutex.
type respConn struct {
	addr     string
	username string
	password string
	useTLS   bool
	tlsCfg   *tls.Config

	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

// errClosed marks a broken connection so the next command re-dials once.
var errClosed = errors.New("redis: connection closed")

// parseRedisURL parses redis:// and rediss:// connection strings. A password
// in the URL is the AUTH password; "user:pass" sends AUTH <user> <pass>, and
// the Redis default user degrades to AUTH <pass>.
func parseRedisURL(raw string) (addr, password, username string, useTLS bool, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", false, fmt.Errorf("parse redis url: %w", err)
	}
	switch u.Scheme {
	case "rediss":
		useTLS = true
	case "redis":
		useTLS = false
	default:
		return "", "", "", false, fmt.Errorf("redis url scheme must be redis:// or rediss://, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", "", false, errors.New("redis url has no host")
	}
	port := u.Port()
	if port == "" {
		port = "6379"
	}
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	return net.JoinHostPort(host, port), password, username, useTLS, nil
}

// newRespConn builds a lazy client; nothing is dialed until the first command.
func newRespConn(rawURL string, tlsCfg *tls.Config) (*respConn, error) {
	addr, password, username, useTLS, err := parseRedisURL(rawURL)
	if err != nil {
		return nil, err
	}
	if username == "default" {
		username = "" // the default user: plain AUTH <password>
	}
	return &respConn{addr: addr, username: username, password: password, useTLS: useTLS, tlsCfg: tlsCfg}, nil
}

// do writes one command and reads one reply, dialing (and authenticating) if
// needed. Safe for concurrent use.
func (c *respConn) do(args ...string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.dialLocked(); err != nil {
			return nil, err
		}
	}
	reply, err := c.roundTripLocked(args)
	if errors.Is(err, errClosed) {
		// One re-dial per command: a transient broken pipe heals on the next
		// call instead of bricking the store.
		if err := c.dialLocked(); err != nil {
			return nil, err
		}
		return c.roundTripLocked(args)
	}
	return reply, err
}

// dialLocked establishes (or re-establishes) the connection and runs the
// AUTH/PING handshake. Caller holds c.mu.
func (c *respConn) dialLocked() error {
	c.closeLocked()
	d := &net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var err error
	if c.useTLS {
		conn, err = tls.DialWithDialer(d, "tcp", c.addr, c.tlsCfg)
	} else {
		conn, err = d.Dial("tcp", c.addr)
	}
	if err != nil {
		return fmt.Errorf("redis: dial %s: %w", c.addr, err)
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)
	c.w = bufio.NewWriter(conn)
	if c.password != "" {
		authArgs := []string{"AUTH", c.password}
		if c.username != "" {
			authArgs = []string{"AUTH", c.username, c.password}
		}
		if _, err := c.roundTripLocked(authArgs); err != nil {
			c.closeLocked()
			return fmt.Errorf("redis: auth against %s: %w", c.addr, err)
		}
	}
	if _, err := c.roundTripLocked([]string{"PING"}); err != nil {
		c.closeLocked()
		return fmt.Errorf("redis: ping %s: %w", c.addr, err)
	}
	return nil
}

// doPing forces the lazy connection up with one PING round trip, so Open
// fails fast on a dead or unauthenticated Redis.
func (c *respConn) doPing() error {
	_, err := c.do("PING")
	return err
}

// roundTripLocked writes args as one RESP array and reads one reply. Caller
// holds c.mu.
func (c *respConn) roundTripLocked(args []string) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.w.WriteString(b.String()); err != nil {
		return nil, errClosed
	}
	if err := c.w.Flush(); err != nil {
		return nil, errClosed
	}
	return readReply(c.r)
}

// closeLocked tears down the connection. Caller holds c.mu.
func (c *respConn) closeLocked() {
	if c.conn != nil {
		c.conn.Close() // best-effort: a failed close drops the socket anyway
		c.conn = nil
		c.r, c.w = nil, nil
	}
}

// Close closes the connection.
func (c *respConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	c.roundTripLocked([]string{"QUIT"}) // best-effort
	c.closeLocked()
	return nil
}

// readReply parses one RESP2 reply: +simple, -error, :integer, $bulk,
// *array. Nil bulk ($-1) and nil array (*-1) decode to nil, nil — GET on a
// missing key.
func readReply(r *bufio.Reader) (any, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, errClosed
	}
	if len(line) < 1 {
		return nil, errors.New("redis: empty reply line")
	}
	typ, rest := line[0], line[1:]
	switch typ {
	case '+':
		return string(rest), nil
	case '-':
		return nil, errors.New("redis: " + string(rest))
	case ':':
		n, err := strconv.ParseInt(string(rest), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: bad integer reply %q: %w", line, err)
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(string(rest))
		if err != nil {
			return nil, fmt.Errorf("redis: bad bulk length %q", line)
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2) // payload + \r\n
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, errClosed
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(string(rest))
		if err != nil {
			return nil, fmt.Errorf("redis: bad array length %q", line)
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			item, err := readReply(r)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("redis: unknown reply type %q", line)
	}
}

// readLine reads one \r\n-terminated line without the terminator.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(line, []byte("\r\n")), nil
}

// RedisBackend persists accounts into Redis — a managed Upstash instance in
// the hosted deployment (Belmo), where there is no durable file system.
//
// Credentials live in Redis, so the store refuses a plaintext redis:// URL
// to a non-loopback host at Open time: passwords (and session cookies) must
// not transit unencrypted. DS_REDIS_INSECURE=1 accepts it for a local dev
// server only.
type RedisBackend struct {
	conn   *respConn
	logger Logger

	mu sync.Mutex // serializes read-modify-write cycles (index repair)
}

// Logger is the smallest logging surface this package needs (satisfied by
// *log.Logger); nil means quiet.
type Logger interface {
	Printf(format string, args ...any)
}

// NewRedisBackend dials, authenticates and pings. Any failure is fatal at
// startup: when a Redis host is configured it IS the source of truth, and
// starting on an empty fallback would silently serve nobody.
func NewRedisBackend(rawURL string, logger Logger) (*RedisBackend, error) {
	_, _, _, useTLS, err := parseRedisURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !useTLS && !isLoopbackURL(rawURL) && os.Getenv("DS_REDIS_INSECURE") != "1" {
		return nil, errors.New("redis account store: refusing plaintext redis:// to a non-localhost host — credentials would transit unencrypted; use rediss:// (TLS) or set DS_REDIS_INSECURE=1 for a local server")
	}
	tlsCfg := &tls.Config{ServerName: serverName(rawURL), RootCAs: redisRootCAs}
	conn, err := newRespConn(rawURL, tlsCfg)
	if err != nil {
		return nil, err
	}
	b := &RedisBackend{conn: conn, logger: logger}
	if err := b.conn.doPing(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("redis account store: %w", err)
	}
	return b, nil
}

// OpenUpstash opens the store from the two-variable Upstash contract
// (DS_REDIS_HOST + DS_REDIS_TOKEN), the same shape the reference deployment
// (Sliverkiss/simple-chat) uses:
//
//   - "" → no Redis configured; the caller keeps the file backend
//   - already redis:// / rediss:// → used as-is (the token is ignored)
//   - otherwise (https://…, host:port or bare host) → assembled into
//     rediss://default:<token>@<host>[:6379]
//
// Assembly without a token is ErrTokenRequired, never a silent
// unauthenticated dial.
func OpenUpstash(host, token string, logger Logger) (*RedisBackend, error) {
	rawURL, err := normalizeRedisURL(host, token)
	if err != nil {
		return nil, err
	}
	return NewRedisBackend(rawURL, logger)
}

// normalizeRedisURL composes DS_REDIS_HOST + DS_REDIS_TOKEN into a
// connection string.
func normalizeRedisURL(host, token string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", nil
	}
	if strings.HasPrefix(host, "rediss://") || strings.HasPrefix(host, "redis://") {
		return host, nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrTokenRequired
	}
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// A host that already carries a port keeps it; only a bare host gets the
	// Redis default (blind appending would produce "host:6380:6379").
	port := ":6379"
	if u, err := url.Parse("//" + host); err == nil && u.Port() != "" {
		port = ""
	}
	return "rediss://default:" + token + "@" + host + port, nil
}

// Addr reports the dialed address for startup logs (never the token).
func (b *RedisBackend) Addr() string { return b.conn.addr }

// Load returns every account in the index. A key that is missing
// (deleted out-of-band) or unparseable is skipped with a log line: one bad
// row must not take the pool down.
func (b *RedisBackend) Load(ctx context.Context) ([]Account, error) {
	reply, err := b.conn.do("SMEMBERS", redisIndexKey)
	if err != nil {
		return nil, err
	}
	members, _ := reply.([]any)
	var out []Account
	for _, m := range members {
		id, _ := m.(string)
		raw, err := b.conn.do("GET", redisKeyPrefix+id)
		if err != nil {
			return nil, err
		}
		s, ok := raw.(string)
		if !ok {
			continue // index ahead of data: skip, do not brick the pool
		}
		var acct Account
		if err := json.Unmarshal([]byte(s), &acct); err != nil {
			b.logf("redis store: skipping unparseable account key %s", id)
			continue
		}
		out = append(out, acct)
	}
	if len(out) == 0 {
		b.logf("redis store: no accounts under %s — starting empty (seed via the API or -import-redis)", redisIndexKey)
	}
	return out, nil
}

// SaveAccount upserts one account: SET the per-id key (whole record as JSON)
// and SADD the id to the index. Redis is single-threaded per command, so
// there is no read-modify-write to race.
func (b *RedisBackend) SaveAccount(ctx context.Context, a Account) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err := b.conn.do("SET", redisKeyPrefix+a.ID, string(raw)); err != nil {
		return err
	}
	_, err = b.conn.do("SADD", redisIndexKey, a.ID)
	return err
}

// Close closes the underlying connection.
func (b *RedisBackend) Close() error { return b.conn.Close() }

// logf logs through the backend's logger when set.
func (b *RedisBackend) logf(format string, args ...any) {
	if b != nil && b.logger != nil {
		b.logger.Printf(format, args...)
	}
}

// urlHost extracts the bare hostname from a redis/rediss URL.
func urlHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// isLoopbackURL reports whether the URL's host is 127.0.0.0/8, ::1 or
// "localhost".
func isLoopbackURL(rawURL string) bool {
	host := strings.Trim(urlHost(rawURL), "[]")
	if host == "localhost" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serverName extracts the SNI hostname from the URL.
func serverName(rawURL string) string { return strings.Trim(urlHost(rawURL), "[]") }
