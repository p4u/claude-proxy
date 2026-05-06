package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/store"
)

// withUpstream redirects api.anthropic.com to the test server by replacing the
// proxy's HTTP client transport with one that rewrites the URL.
func withUpstream(h *Handler, ts *httptest.Server) {
	tsURL, _ := url.Parse(ts.URL)
	h.client = &http.Client{
		Transport: &rewriter{base: tsURL, rt: ts.Client().Transport},
	}
}

type rewriter struct {
	base *url.URL
	rt   http.RoundTripper
}

func (r *rewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = r.base.Scheme
	req.URL.Host = r.base.Host
	req.Host = r.base.Host
	rt := r.rt
	if rt == nil {
		rt = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return rt.RoundTrip(req)
}

func setupProxy(t *testing.T, upstreamHandler http.HandlerFunc) (*Handler, []*creds.Credential, *store.DB, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	var cs []*creds.Credential
	for _, lbl := range []string{"A", "B"} {
		c, err := creds.Insert(ctx, db, lbl, "max",
			"sk-ant-oat-fake-"+lbl, "rt-"+lbl, time.Now().Add(time.Hour), 0)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}

	ts := httptest.NewServer(upstreamHandler)
	t.Cleanup(ts.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(db, pool.New(db), creds.NewRefresher(db), logger)
	withUpstream(h, ts)
	return h, cs, db, ts
}

func TestForwardSuccessAndStickyBind(t *testing.T) {
	var seenAuth atomic.Value // string
	upstream := func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"ok":true}`)
	}
	h, _, db, _ := setupProxy(t, upstream)

	body := []byte(`{"metadata":{"user_id":"sess-1"},"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-side-token")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status=%d body=%s", rw.Code, rw.Body.String())
	}
	auth := seenAuth.Load().(string)
	if !strings.HasPrefix(auth, "Bearer sk-ant-oat-fake-") {
		t.Fatalf("upstream did not see swapped Authorization, got %q", auth)
	}

	// Second request, same conversation -> same credential.
	rw2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	h.ServeHTTP(rw2, req2)
	if rw2.Code != 200 {
		t.Fatalf("second request: %d", rw2.Code)
	}
	auth2 := seenAuth.Load().(string)
	if auth2 != auth {
		t.Fatalf("sticky broken: %q -> %q", auth, auth2)
	}

	// Confirm conversation count == 1.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 conversation, got %d", n)
	}
}

func TestForward429MarksLimited(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
		fmt.Fprintln(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}
	h, _, db, _ := setupProxy(t, upstream)

	body := []byte(`{"metadata":{"user_id":"sess-x"},"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 429 {
		t.Fatalf("expected 429 passthrough, got %d", rw.Code)
	}

	// One credential should be limited now.
	var status string
	var retryAfter int64
	row := db.QueryRow(`SELECT status, COALESCE(retry_after,0) FROM credentials WHERE status='limited'`)
	if err := row.Scan(&status, &retryAfter); err != nil {
		t.Fatalf("expected one limited row: %v", err)
	}
	if retryAfter <= time.Now().Unix() {
		t.Fatalf("retry_after not in future: %d", retryAfter)
	}
}

func TestForward401TriggersRefresh(t *testing.T) {
	var hits int32
	// Refresher needs a fake token endpoint too. Build a single mux that handles both.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(401)
			fmt.Fprintln(w, `{"error":"expired"}`)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"ok":true}`)
	})
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"access_token":"sk-ant-oat-refreshed","refresh_token":"rt-new","expires_in":3600}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	dir := t.TempDir()
	db, _ := store.Open(filepath.Join(dir, "t.db"))
	defer db.Close()
	ctx := context.Background()
	c, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-stale", "rt-A", time.Now().Add(time.Hour), 0)

	// Patch refresher's token URL via direct HTTP override: simplest is to
	// route a custom client. We'll point a custom refresher at the mux by
	// rewriting URLs the same way as the proxy.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tsURL, _ := url.Parse(ts.URL)
	r := creds.NewRefresher(db)
	// replace internal client via patch helper added below
	creds.SetTokenClient(r, &http.Client{Transport: &rewriter{base: tsURL, rt: ts.Client().Transport}})
	creds.SetTokenURL("http://placeholder-host.invalid/v1/oauth/token") // host gets rewritten

	h := New(db, pool.New(db), r, logger)
	withUpstream(h, ts)

	body := []byte(`{"metadata":{"user_id":"sess-401"},"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("expected refreshed retry to succeed, got %d body=%s", rw.Code, rw.Body.String())
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("expected 2 upstream hits (401 then 200), got %d", hits)
	}

	// Credential's access token must have been rotated.
	got, _ := creds.Get(ctx, db, c.ID)
	if got.AccessToken != "sk-ant-oat-refreshed" {
		t.Fatalf("token not rotated: %s", got.AccessToken)
	}
}
