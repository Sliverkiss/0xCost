package accounts

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A fake RESP server: enough of the protocol to exercise the store without a
// Redis dependency (ADR 0018 — the client is ours, so it is tested against a
// server we control rather than a real instance in CI).
// ---------------------------------------------------------------------------

type fakeRedis struct {
	ln   net.Listener
	mu   sync.Mutex
	data map[string]string
	sets map[string]map[string]bool

	// password, when non-empty, is required in AUTH.
	password string
	// failAuth makes AUTH answer an error (dead credentials at startup).
	failAuth bool
	// dropAfterFirstCommand closes the connection after N commands, so the
	// client's re-dial path can be exercised.
	dropAfter int
	commands  []string
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{ln: ln, data: map[string]string{}, sets: map[string]map[string]bool{}}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

// addr returns the dialable address (host:port).
func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

// url returns a redis:// connection string for the fake server.
func (f *fakeRedis) url() string { return "redis://" + f.addr() }

func (f *fakeRedis) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRedis) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	served := 0
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		f.mu.Lock()
		f.commands = append(f.commands, args[0])
		drop := f.dropAfter > 0 && served >= f.dropAfter
		f.mu.Unlock()
		if drop {
			return // simulate a broken pipe mid-session
		}
		served++
		reply := f.dispatch(args)
		if _, err := w.WriteString(reply); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		if args[0] == "QUIT" {
			return
		}
	}
}

func (f *fakeRedis) dispatch(args []string) string {
	switch strings.ToUpper(args[0]) {
	case "AUTH":
		if f.failAuth {
			return "-ERR invalid password\r\n"
		}
		if f.password != "" {
			pass := args[len(args)-1]
			if pass != f.password {
				return "-WRONGPASS invalid username-password pair\r\n"
			}
		}
		return "+OK\r\n"
	case "PING":
		return "+PONG\r\n"
	case "QUIT":
		return "+OK\r\n"
	case "SET":
		if len(args) < 3 {
			return "-ERR wrong number of arguments\r\n"
		}
		f.mu.Lock()
		f.data[args[1]] = args[2]
		f.mu.Unlock()
		return "+OK\r\n"
	case "GET":
		f.mu.Lock()
		v, ok := f.data[args[1]]
		f.mu.Unlock()
		if !ok {
			return "$-1\r\n"
		}
		return fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)
	case "SADD":
		f.mu.Lock()
		if f.sets[args[1]] == nil {
			f.sets[args[1]] = map[string]bool{}
		}
		added := 0
		for _, m := range args[2:] {
			if !f.sets[args[1]][m] {
				f.sets[args[1]][m] = true
				added++
			}
		}
		f.mu.Unlock()
		return fmt.Sprintf(":%d\r\n", added)
	case "SREM":
		f.mu.Lock()
		removed := 0
		for _, m := range args[2:] {
			if f.sets[args[1]][m] {
				delete(f.sets[args[1]], m)
				removed++
			}
		}
		f.mu.Unlock()
		return fmt.Sprintf(":%d\r\n", removed)
	case "SMEMBERS":
		f.mu.Lock()
		members := []string{}
		for m := range f.sets[args[1]] {
			members = append(members, m)
		}
		f.mu.Unlock()
		var b strings.Builder
		fmt.Fprintf(&b, "*%d\r\n", len(members))
		for _, m := range members {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(m), m)
		}
		return b.String()
	default:
		return "-ERR unknown command\r\n"
	}
}

// readCommand reads one RESP array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] != '*' {
		return nil, errors.New("fake redis: expected an array")
	}
	n := 0
	fmt.Sscanf(string(line[1:]), "%d", &n)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if len(hdr) == 0 || hdr[0] != '$' {
			return nil, errors.New("fake redis: expected a bulk string")
		}
		size := 0
		fmt.Sscanf(string(hdr[1:]), "%d", &size)
		buf := make([]byte, size+2)
		if _, err := ioReadFull(r, buf); err != nil {
			return nil, err
		}
		out = append(out, string(buf[:size]))
	}
	return out, nil
}

// ioReadFull is a tiny indirection so the fake server has no extra import.
func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// URL assembly (the operator-facing contract)
// ---------------------------------------------------------------------------

func TestNormalizeRedisURL(t *testing.T) {
	cases := []struct {
		name       string
		host, tok  string
		want       string
		wantErr    error
		wantSubstr string
	}{
		{name: "empty host means no redis", host: "", tok: "s3cret", want: ""},
		{name: "bare host gets rediss + default user + port", host: "us1-cool-1234.upstash.io", tok: "tok", want: "rediss://default:tok@us1-cool-1234.upstash.io:6379"},
		{name: "host with a port keeps its own port", host: "us1.upstash.io:6380", tok: "tok", want: "rediss://default:tok@us1.upstash.io:6380"},
		{name: "https url is stripped and rebuilt", host: "https://us1.upstash.io", tok: "tok", want: "rediss://default:tok@us1.upstash.io:6379"},
		{name: "a full rediss url passes through", host: "rediss://default:tok@us1.upstash.io:6379", tok: "", want: "rediss://default:tok@us1.upstash.io:6379"},
		{name: "a full redis url passes through", host: "redis://127.0.0.1:6379", tok: "", want: "redis://127.0.0.1:6379"},
		{name: "bare host without a token is refused", host: "us1.upstash.io", tok: "", wantErr: ErrTokenRequired},
		{name: "whitespace is tolerated", host: "  us1.upstash.io  ", tok: " tok ", want: "rediss://default:tok@us1.upstash.io:6379"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeRedisURL(tc.host, tc.tok)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseRedisURLHelpers(t *testing.T) {
	addr, pass, user, tls, err := parseRedisURL("rediss://default:tok@host.example:6380")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "host.example:6380" || pass != "tok" || user != "default" || !tls {
		t.Fatalf("parsed = %q %q %q %v", addr, pass, user, tls)
	}
	if _, _, _, _, err := parseRedisURL("http://host"); err == nil {
		t.Error("a non-redis scheme must be refused")
	}
	if _, _, _, _, err := parseRedisURL("redis://"); err == nil {
		t.Error("a url with no host must be refused")
	}
	if !isLoopbackURL("redis://127.0.0.1:6379") || !isLoopbackURL("redis://localhost:6379") ||
		!isLoopbackURL("redis://[::1]:6379") {
		t.Error("loopback detection missed a loopback host")
	}
	if isLoopbackURL("redis://10.0.0.5:6379") {
		t.Error("loopback detection accepted a non-loopback host")
	}
	if serverName("rediss://default:tok@host.example:6380") != "host.example" {
		t.Errorf("serverName = %q", serverName("rediss://default:tok@host.example:6380"))
	}
}

// ---------------------------------------------------------------------------
// The store over the fake server
// ---------------------------------------------------------------------------

func TestRedisBackendRoundTrip(t *testing.T) {
	f := newFakeRedis(t)
	b, err := OpenUpstash("redis://"+f.addr(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Addr() != f.addr() {
		t.Errorf("Addr = %q, want %q", b.Addr(), f.addr())
	}

	// Empty store is a valid state, not an error.
	got, err := b.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty store returned %d accounts", len(got))
	}

	want := Account{ID: "acc-1", Phone: "13800000001", Password: "pw", Enabled: true,
		UserID: "u-1", PassToken: "pt-1", Device: map[string]string{"deviceId": "wb_1"}}
	if err := b.SaveAccount(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	again := Account{ID: "acc-2", Enabled: true}
	if err := b.SaveAccount(context.Background(), again); err != nil {
		t.Fatal(err)
	}

	got, err = b.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d accounts, want 2", len(got))
	}
	byID := map[string]Account{}
	for _, a := range got {
		byID[a.ID] = a
	}
	if a := byID["acc-1"]; a.Phone != "13800000001" || a.PassToken != "pt-1" || a.Device["deviceId"] != "wb_1" {
		t.Errorf("acc-1 round trip lost fields: %+v", a)
	}
	if _, ok := byID["acc-2"]; !ok {
		t.Error("acc-2 missing from the index")
	}

	// An upsert replaces rather than duplicates.
	upd := want
	upd.PassToken = "pt-2"
	if err := b.SaveAccount(context.Background(), upd); err != nil {
		t.Fatal(err)
	}
	got, _ = b.Load(context.Background())
	if len(got) != 2 {
		t.Fatalf("upsert changed the account count: %d", len(got))
	}
	for _, a := range got {
		if a.ID == "acc-1" && a.PassToken != "pt-2" {
			t.Errorf("upsert did not replace: %+v", a)
		}
	}
}

func TestRedisBackendAuthAndFailures(t *testing.T) {
	f := newFakeRedis(t)
	f.password = "s3cret"

	// Wrong password: Open must fail rather than serve an unauthorised store.
	if _, err := OpenUpstash("redis://default:wrong@"+f.addr(), "", nil); err == nil {
		t.Fatal("expected an auth failure")
	}

	b, err := OpenUpstash("redis://default:s3cret@"+f.addr(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// A server error on a command is surfaced, not swallowed.
	if _, err := b.conn.do("NOPE"); err == nil {
		t.Error("expected an error reply to surface")
	}
}

func TestRedisBackendPlaintextRefusedOffLoopback(t *testing.T) {
	os.Unsetenv("DS_REDIS_INSECURE")
	_, err := NewRedisBackend("redis://10.1.2.3:6379", nil)
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("err = %v, want a refusal naming encryption", err)
	}
	// The escape hatch is explicit and loopback-only by intent.
	t.Setenv("DS_REDIS_INSECURE", "1")
	if _, err := NewRedisBackend("redis://10.1.2.3:6379", nil); err == nil {
		t.Error("with DS_REDIS_INSECURE the dial should be attempted (and fail on connect, not on policy)")
	}
	if _, err := NewRedisBackend("not-a-url", nil); err == nil {
		t.Error("a malformed url must be refused")
	}
}

// ---------------------------------------------------------------------------
// DS_REDIS_CA_FILE (ADR 0018): a private CA must be loaded, and a broken
// configuration must fail loudly instead of falling back to the system roots.
// ---------------------------------------------------------------------------

// Upstash hands the host out as an https:// URL with a trailing slash, and that
// exact shape has to compose into a Redis connection string (ADR 0018).
func TestNormalizeRedisURLUpstashForms(t *testing.T) {
	const token = "gQAAAAAA_token_like_value"
	cases := []struct {
		name string
		host string
		want string
	}{
		{"https with trailing slash", "https://<db>.upstash.io/",
			"rediss://default:" + token + "@<db>.upstash.io:6379"},
		{"bare host", "<db>.upstash.io",
			"rediss://default:" + token + "@<db>.upstash.io:6379"},
		{"host with port keeps it", "<db>.upstash.io:6380",
			"rediss://default:" + token + "@<db>.upstash.io:6380"},
		{"https with path and spaces", "  https://host.example.com/redis  ",
			"rediss://default:" + token + "@host.example.com:6379"},
		{"full url passes through", "rediss://default:" + token + "@h.example.com:6379",
			"rediss://default:" + token + "@h.example.com:6379"},
	}
	for _, tc := range cases {
		got, err := normalizeRedisURL(tc.host, token)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		// The composed URL must parse back into the intended address.
		addr, pass, user, useTLS, perr := parseRedisURL(got)
		if perr != nil {
			t.Errorf("%s: composed URL does not parse: %v", tc.name, perr)
			continue
		}
		if !useTLS || pass != token || user != "default" {
			t.Errorf("%s: parsed tls=%v user=%q (token withheld), want TLS + default user", tc.name, useTLS, user)
		}
		if strings.HasSuffix(addr, "/") || strings.Contains(addr, "/:") {
			t.Errorf("%s: composed address %q carries path junk", tc.name, addr)
		}
	}

	if _, err := normalizeRedisURL("https:///", token); err == nil {
		t.Error("a host with nothing after the scheme must be an error, not an empty dial")
	}
	if _, err := normalizeRedisURL("https://host/", "  "); !errors.Is(err, ErrTokenRequired) {
		t.Errorf("missing token: err = %v, want ErrTokenRequired", err)
	}
}

func TestLoadRedisCAFile(t *testing.T) {
	dir := t.TempDir()

	if err := loadRedisCAFile(""); err != nil {
		t.Fatalf("empty path must be a no-op: %v", err)
	}
	if redisRootCAs != nil {
		t.Fatal("empty path must leave the system roots in place")
	}

	err := loadRedisCAFile(filepath.Join(dir, "missing.crt"))
	if err == nil || !strings.Contains(err.Error(), "DS_REDIS_CA_FILE") {
		t.Fatalf("missing CA file: err = %v, want a DS_REDIS_CA_FILE error", err)
	}

	notPEM := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(notPEM, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = loadRedisCAFile(notPEM)
	if err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("non-PEM CA file: err = %v, want a \"no PEM certificates\" error", err)
	}

	// A real (self-signed) CA installs an extra root pool, and the hook is
	// restored so it cannot leak into another case.
	certPEM, err := generateSelfSignedCert(t)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadRedisCAFile(caPath); err != nil {
		t.Fatalf("valid CA: %v", err)
	}
	if redisRootCAs == nil {
		t.Fatal("valid CA must install a root pool")
	}
	t.Cleanup(func() { redisRootCAs = nil })
}

// generateSelfSignedCert mints a throwaway CA-shaped certificate for the CA
// case, so the suite never depends on a fixture file on disk.
func generateSelfSignedCert(t *testing.T) ([]byte, error) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mimo2api-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func TestRedisBackendSkipsBadRows(t *testing.T) {
	f := newFakeRedis(t)
	f.data[redisKeyPrefix+"broken"] = "{not json"
	f.data[redisIndexKey+"-ignore"] = "x"
	f.sets[redisIndexKey] = map[string]bool{"broken": true, "gone": true, "ok": true}
	f.data[redisKeyPrefix+"ok"] = `{"id":"ok","enabled":true}`

	b, err := OpenUpstash("redis://"+f.addr(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := b.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// "broken" is skipped (logged), "gone" has no data key (skipped): only
	// the good row survives, and one bad row never takes the pool down.
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("loaded %+v, want just the parseable row", got)
	}
}

func TestRedisBackendRedialsAfterBrokenPipe(t *testing.T) {
	f := newFakeRedis(t)
	b, err := OpenUpstash("redis://"+f.addr(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Break the live connection the way a managed provider does at idle, then
	// use the store again: one re-dial per command heals it.
	b.conn.mu.Lock()
	if b.conn.conn != nil {
		b.conn.conn.Close()
	}
	b.conn.mu.Unlock()

	if err := b.SaveAccount(context.Background(), Account{ID: "after-drop", Enabled: true}); err != nil {
		t.Fatalf("a broken pipe must heal on the next command: %v", err)
	}
	got, err := b.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "after-drop" {
		t.Fatalf("loaded %+v after the re-dial", got)
	}
}

func TestRedisBackendLoadsThroughStoreValidation(t *testing.T) {
	f := newFakeRedis(t)
	b, err := OpenUpstash("redis://"+f.addr(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SaveAccount(context.Background(), Account{ID: "dup", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Duplicate enabled ids come from the source of truth and are rejected
	// loudly by the store (TDD ACC-005), same as the file path.
	f.sets[redisIndexKey]["dup2"] = true
	f.data[redisKeyPrefix+"dup2"] = `{"id":"dup","enabled":true}`

	if _, err := LoadBackend(context.Background(), b); err == nil {
		t.Fatal("expected the duplicate-id validation to fail the load")
	}
}

// ---------------------------------------------------------------------------
// RESP reply parsing
// ---------------------------------------------------------------------------

func TestReadReplyShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"simple string", "+OK\r\n", "OK"},
		{"integer", ":42\r\n", int64(42)},
		{"bulk", "$5\r\nhello\r\n", "hello"},
		{"nil bulk", "$-1\r\n", nil},
		{"nil array", "*-1\r\n", nil},
		{"empty array", "*0\r\n", []any{}},
		{"nested", "*2\r\n$1\r\na\r\n:7\r\n", []any{"a", int64(7)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readReply(bufio.NewReader(strings.NewReader(tc.in)))
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %#v (%v), want %#v", got, fmt.Sprint(got), tc.want)
			}
		})
	}

	if _, err := readReply(bufio.NewReader(strings.NewReader("-ERR nope\r\n"))); err == nil {
		t.Error("an error reply must become an error")
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader("?weird\r\n"))); err == nil {
		t.Error("an unknown reply type must be refused")
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader(":notanumber\r\n"))); err == nil {
		t.Error("a bad integer must be refused")
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader("$notanumber\r\n"))); err == nil {
		t.Error("a bad bulk length must be refused")
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader("*notanumber\r\n"))); err == nil {
		t.Error("a bad array length must be refused")
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader(""))); err == nil {
		t.Error("an empty stream must be a closed-connection error")
	}
}

// A slow server must not hang Open forever: dial has a timeout, and the
// handshake is bounded by the same connection timeout (ADR 0018 keeps the
// startup path fail-fast).
func TestOpenUpstashFailsFastOnMalformedHost(t *testing.T) {
	os.Unsetenv("DS_REDIS_INSECURE")
	if _, err := OpenUpstash("127.0.0.1:1", "", nil); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("err = %v, want ErrTokenRequired (bare host without a token)", err)
	}
	done := make(chan error, 1)
	go func() {
		// Nothing listens on this port: the dial must fail, not block.
		_, err := OpenUpstash("redis://127.0.0.1:1", "", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a dial failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OpenUpstash hung on an unreachable address")
	}
}
