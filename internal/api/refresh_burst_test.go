package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
)

// relearnBurstStack wires a Server whose catalog read (bot/config) is a counting
// test double: every fetch blocks on a channel so a test can hold several
// concurrent relearns mid-flight and count how many reads actually ran.
type relearnBurstStack struct {
	srv     *Server
	fetches chan chan struct{} // one buffered handshake per bot/config read
}

func newRelearnBurstStack(t *testing.T) *relearnBurstStack {
	t.Helper()
	fetches := make(chan chan struct{}, 64)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open-apis/bot/config" {
			http.NotFound(w, r)
			return
		}
		done := make(chan struct{})
		fetches <- done
		<-done
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"modelConfigListNg":[`+
			`{"model":"mimo-x","name":"X","pageType":"chat","redirectTo":"mimo-y"},`+
			`{"model":"mimo-y","name":"Y","pageType":"chat"}],`+
			`"modelConfigList":[{"model":"mimo-y","thinkingDefaultOn":true,"webSearchDefaultStatus":"auto"}],`+
			`"ttsVoices":[],"ext":{}}}`)
	}))
	t.Cleanup(up.Close)

	acct := accounts.Account{
		ID: "acc-refresh-1", Enabled: true,
		Session: &accounts.Session{Cookie: "st-refresh", PH: "ph-refresh", FetchedAt: time.Now().Unix()},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	f, _ := json.MarshalIndent(accounts.File{Accounts: []accounts.Account{acct}}, "", "  ")
	if err := os.WriteFile(path, f, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New([]accounts.Account{acct}, pool.Options{}, nil, nil)
	client := mimo.New(mimo.Options{Base: up.URL})
	srv := New(Options{Pool: p, Accounts: store, Client: client})
	return &relearnBurstStack{srv: srv, fetches: fetches}
}

// warmup stores one snapshot and waits for it to land.
func (st *relearnBurstStack) warmup(t *testing.T) {
	t.Helper()
	go st.srv.RefreshModelRedirects(context.Background())
	(<-st.fetches) <- struct{}{}
	st.waitSnapshot(t)
}

// waitSnapshot blocks until storeSnapshot has stored a map.
func (st *relearnBurstStack) waitSnapshot(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st.srv.redirects.mu.Lock()
		m := st.srv.redirects.m
		st.srv.redirects.mu.Unlock()
		if m != nil && m["mimo-x"] == "mimo-y" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("snapshot never stored")
}

// agePastFloor pretends the stored snapshot AND the per-account bot/config
// cache are older than both the relearn floor and the catalog TTL, so a
// relearn is allowed to rebuild and its catalog read actually reaches the
// upstream instead of being absorbed by botConfigFor's TTL cache.
func (st *relearnBurstStack) agePastFloor(t *testing.T) {
	t.Helper()
	stale := time.Now().Add(-2*redirectRelearnFloor - 24*time.Hour)
	st.srv.redirects.mu.Lock()
	st.srv.redirects.at = stale
	st.srv.redirects.mu.Unlock()
	st.srv.botCfg.mu.Lock()
	if st.srv.botCfg.byID != nil {
		for id, c := range st.srv.botCfg.byID {
			st.srv.botCfg.byID[id] = cachedBotConfig{cfg: c.cfg, at: stale}
		}
	}
	st.srv.botCfg.mu.Unlock()
}

// REFRESH-003: a burst of concurrent relearns — several requests naming a
// model that was just retired — must run ONE catalog read, not one per
// request. The read runs creds per account, and on a session-less account
// that is a login (and a second OTP challenge) per burst member (ADR 0011).
func TestREFRESH003RelearnBurstRunsOneCatalogRead(t *testing.T) {
	st := newRelearnBurstStack(t)
	srv := st.srv
	st.warmup(t)
	st.agePastFloor(t)

	// The burst: 8 concurrent relearns. Under the pre-fix TOCTOU every member
	// passed the floor, zeroed `at`, and ran its own fetch; a pump releases
	// whatever reads arrive (so the test fails on the count, not by hanging)
	// and counts them.
	const burst = 8
	var reads atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case done := <-st.fetches:
				reads.Add(1)
				done <- struct{}{}
			case <-stop:
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.relearnModel(context.Background(), "mimo-x")
		}()
	}
	wg.Wait()
	close(stop)

	if got := reads.Load(); got != 1 {
		t.Fatalf("a burst of %d concurrent relearns caused %d catalog reads; the floor must serialize them into one", burst, got)
	}
}

// REFRESH-004: a cancelled context must not wipe a warm snapshot. The relearn
// path can be cancelled by the client hanging up (or our own budget firing)
// while the catalog read is in flight; caching the cancellation's empty
// catalog for a full TTL would 404 every model until the next janitor pass.
func TestREFRESH004CancelledContextKeepsSnapshot(t *testing.T) {
	st := newRelearnBurstStack(t)
	srv := st.srv
	st.warmup(t)
	st.agePastFloor(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done := <-st.fetches
		cancel()
		close(done)
	}()
	srv.relearnModel(ctx, "mimo-x")

	srv.redirects.mu.Lock()
	m, ids, at := srv.redirects.m, srv.redirects.ids, srv.redirects.at
	srv.redirects.mu.Unlock()
	if m == nil || m["mimo-x"] != "mimo-y" {
		t.Fatalf("cancelled refresh wiped the redirect map: %v", m)
	}
	if ids == nil || !ids["mimo-y"] {
		t.Fatalf("cancelled refresh wiped the served-id set: %v", ids)
	}
	if at.IsZero() {
		t.Fatal("cancelled refresh left the snapshot un-stamped; the next reader would refetch")
	}
}
