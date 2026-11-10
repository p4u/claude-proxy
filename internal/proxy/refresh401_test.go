package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/store"
)

const upstream401Body = `{"type":"error","error":{"type":"authentication_error","message":"OAuth authentication is currently not supported."}}`

// fake401 is one server playing the upstream, the token endpoint and the
// profile endpoint. The upstream answers 401 to any token but the refreshed
// one. Configure it before the first request.
type fake401 struct {
	tokenCalls    atomic.Int32
	upstreamHits  atomic.Int32
	profileStatus int    // profile endpoint status
	tokenStatus   int    // 0 answers 200 with sk-ant-oat-refreshed
	onStale       func() // runs before a stale-token 401
}

// logLines is a goroutine-safe log sink.
type logLines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// count returns how many lines are at level and carry label=label.
func (l *logLines) count(level, label string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(l.b.String(), "\n") {
		f := " " + line + " "
		if strings.Contains(f, " level="+level+" ") && strings.Contains(f, " label="+label+" ") {
			n++
		}
	}
	return n
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// setup401 builds a proxy with one Anthropic credential, "acct-a", whose
// token expires in an hour, so a 401 on it goes through the liveness check.
func setup401(t *testing.T, f *fake401) (*Handler, *creds.Credential, *store.DB, *logLines) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			f.tokenCalls.Add(1)
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"sk-ant-oat-refreshed","refresh_token":"rt-new","expires_in":28800}`)
		case "/api/oauth/profile":
			w.WriteHeader(f.profileStatus)
		default:
			f.upstreamHits.Add(1)
			if r.Header.Get("Authorization") == "Bearer sk-ant-oat-refreshed" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
				return
			}
			if f.onStale != nil {
				f.onStale()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Upstream-Marker", "relayed")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, upstream401Body)
		}
	}))
	t.Cleanup(ts.Close)
	prevToken, prevProfile := creds.TokenURL, creds.ProfileURL
	creds.SetTokenURL(ts.URL + "/v1/oauth/token")
	creds.ProfileURL = ts.URL + "/api/oauth/profile"
	t.Cleanup(func() { creds.SetTokenURL(prevToken); creds.ProfileURL = prevProfile })

	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := creds.Insert(context.Background(), db, "acct-a", "max", "sk-ant-oat-stale", "rt-A", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logLines{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := creds.NewRefresher(db)
	r.SetLogger(logger)
	h := New(db, pool.New(db), r, logger)
	withUpstream(h, ts)
	return h, c, db, logs
}

func send401(h *Handler) *httptest.ResponseRecorder {
	body := `{"model":"claude-x","metadata":{"user_id":"sess-401"},"messages":[{"role":"user","content":"hi"}]}`
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	return rw
}

// AC6: a 401 on a token the profile endpoint accepts is relayed as it came,
// with no refresh and no change to the credential.
func TestUpstream401OnLiveTokenIsRelayed(t *testing.T) {
	f := &fake401{profileStatus: http.StatusOK}
	h, c, db, logs := setup401(t, f)
	rw := send401(h)
	if rw.Code != http.StatusUnauthorized || rw.Body.String() != upstream401Body || rw.Header().Get("X-Upstream-Marker") != "relayed" {
		t.Fatalf("client got %d %q headers=%v, want upstream's 401", rw.Code, rw.Body.String(), rw.Header())
	}
	got, _ := creds.Get(context.Background(), db, c.ID)
	if f.tokenCalls.Load() != 0 || got.Status != creds.StatusActive || got.AccessToken != c.AccessToken ||
		!got.ExpiresAt.Equal(c.ExpiresAt.Truncate(time.Second)) || got.ErrorCount != 1 {
		t.Fatalf("token calls %d, stored %+v", f.tokenCalls.Load(), got)
	}
	if f.upstreamHits.Load() != 1 || logs.count("WARN", "acct-a") != 1 || !strings.Contains(logs.String(), "path=/v1/messages") {
		t.Fatalf("upstream hits %d, logs:\n%s", f.upstreamHits.Load(), logs)
	}
}

// AC6: a 401 the profile endpoint confirms refreshes once and retries once.
func TestUpstream401OnDeadTokenRefreshesAndRetries(t *testing.T) {
	f := &fake401{profileStatus: http.StatusUnauthorized}
	h, c, db, _ := setup401(t, f)
	if rw := send401(h); rw.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want the retried 200", rw.Code, rw.Body.String())
	}
	if f.upstreamHits.Load() != 2 || f.tokenCalls.Load() != 1 {
		t.Fatalf("upstream hits %d, token calls %d; want 2 and 1", f.upstreamHits.Load(), f.tokenCalls.Load())
	}
	if got, _ := creds.Get(context.Background(), db, c.ID); got.AccessToken != "sk-ant-oat-refreshed" {
		t.Fatalf("token not rotated: %s", got.AccessToken)
	}
}

// AC6: two requests rejected on the same token cost one refresh.
func TestConcurrentUpstream401sRefreshOnce(t *testing.T) {
	var arrived atomic.Int32
	both := make(chan struct{})
	f := &fake401{profileStatus: http.StatusUnauthorized, onStale: func() {
		if arrived.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
		}
	}}
	h, _, _, _ := setup401(t, f)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { codes <- send401(h).Code })
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
	}
	if arrived.Load() != 2 || f.tokenCalls.Load() != 1 {
		t.Fatalf("stale 401s %d, token calls %d; want 2 and 1", arrived.Load(), f.tokenCalls.Load())
	}
}

// AC6: a transient refresh failure is a 502 that leaves the status alone, and
// the next 401 within 30 s does not call the token endpoint again.
func TestUpstream401TransientRefreshFailure(t *testing.T) {
	f := &fake401{profileStatus: http.StatusUnauthorized, tokenStatus: http.StatusInternalServerError}
	h, c, db, _ := setup401(t, f)
	for range 2 {
		rw := send401(h)
		if rw.Code != http.StatusBadGateway || strings.TrimSpace(rw.Body.String()) != "proxy: token refresh failed; retry" {
			t.Fatalf("got %d %q", rw.Code, rw.Body.String())
		}
	}
	got, _ := creds.Get(context.Background(), db, c.ID)
	if got.Status != creds.StatusActive || got.RetryAfter == nil || time.Until(*got.RetryAfter) < 50*time.Second {
		t.Fatalf("status %q retry_after %v, want active and about a minute ahead", got.Status, got.RetryAfter)
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token calls %d, want 1", n)
	}
}

// AC6 and AC7b: a rejected login is revoked (never expired), answered with
// the fixed 502 and logged as one ERROR with the label.
func TestUpstream401RevokedLogin(t *testing.T) {
	f := &fake401{profileStatus: http.StatusUnauthorized, tokenStatus: http.StatusBadRequest}
	h, c, db, logs := setup401(t, f)
	rw := send401(h)
	if rw.Code != http.StatusBadGateway || strings.TrimSpace(rw.Body.String()) != "proxy: credential revoked by Anthropic; sign in again" {
		t.Fatalf("got %d %q", rw.Code, rw.Body.String())
	}
	if got, _ := creds.Get(context.Background(), db, c.ID); got.Status != creds.StatusRevoked {
		t.Fatalf("status %q, want revoked", got.Status)
	}
	if n := logs.count("ERROR", "acct-a"); n != 1 {
		t.Fatalf("%d ERROR lines with the label, want 1:\n%s", n, logs)
	}
}

// AC6: the background catalogue fetch takes the same branch.
func TestCatalogue401OnLiveTokenDoesNotRefresh(t *testing.T) {
	f := &fake401{profileStatus: http.StatusOK}
	h, c, db, _ := setup401(t, f)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	_, _ = io.Copy(io.Discard, rw.Result().Body)
	got, _ := creds.Get(context.Background(), db, c.ID)
	if f.tokenCalls.Load() != 0 || got.AccessToken != c.AccessToken || got.Status != creds.StatusActive {
		t.Fatalf("token calls %d, stored %+v", f.tokenCalls.Load(), got)
	}
}
