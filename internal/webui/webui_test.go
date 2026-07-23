package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

const testPassword = "s3cret"

func newTestServer(t *testing.T) (*store.DB, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, New(db, nil, testPassword, false)
}

// do issues a request and returns the recorder. cookie may be nil.
func do(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.RemoteAddr = "192.0.2.10:5555"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := do(t, h, http.MethodPost, "/ui/api/login", `{"password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie set on login")
	return nil
}

func TestSessionUnauthenticated(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodGet, "/ui/api/session", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("session code = %d", w.Code)
	}
	var body struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Authenticated {
		t.Error("expected unauthenticated")
	}
}

func TestProtectedRequires401(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, http.MethodGet, "/ui/api/overview", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("overview without cookie = %d, want 401", w.Code)
	}
}

func TestLoginSessionFlow(t *testing.T) {
	_, h := newTestServer(t)

	// Wrong password.
	if w := do(t, h, http.MethodPost, "/ui/api/login", `{"password":"nope"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", w.Code)
	}

	cookie := loginCookie(t, h)

	// Session now authenticated.
	w := do(t, h, http.MethodGet, "/ui/api/session", "", cookie)
	if !strings.Contains(w.Body.String(), "true") {
		t.Errorf("session not authenticated: %s", w.Body.String())
	}

	// Protected endpoint works with cookie.
	if w := do(t, h, http.MethodGet, "/ui/api/overview", "", cookie); w.Code != http.StatusOK {
		t.Fatalf("overview with cookie = %d: %s", w.Code, w.Body.String())
	}

	// Tampered cookie rejected.
	bad := &http.Cookie{Name: sessionCookie, Value: cookie.Value + "x"}
	if w := do(t, h, http.MethodGet, "/ui/api/overview", "", bad); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie = %d, want 401", w.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, h := newTestServer(t)
	// 5 failed attempts allowed, 6th is rate-limited.
	for i := 0; i < loginMaxFails; i++ {
		w := do(t, h, http.MethodPost, "/ui/api/login", `{"password":"wrong"}`, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, w.Code)
		}
	}
	w := do(t, h, http.MethodPost, "/ui/api/login", `{"password":"wrong"}`, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt = %d, want 429", w.Code)
	}
}

func TestStatsUsersEndpoint(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()

	ut, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	seed := func(status int, tin, tout, cacheRead int64, conv string) {
		_, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', ?, ?, '/v1/messages', ?, 10, 20, 100,
			        'claude', ?, ?, 0, ?)`,
			ut.ID, conv, now, status, tin, tout, cacheRead)
		if err != nil {
			t.Fatal(err)
		}
	}
	seed(200, 100, 50, 5, "conv-a")
	seed(200, 200, 60, 7, "conv-a")
	seed(429, 0, 0, 0, "conv-b")

	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/ui/api/stats/users?period=24h", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stats/users = %d: %s", w.Code, w.Body.String())
	}
	var rows []usertoken.UserStat
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 user row, got %d", len(rows))
	}
	r := rows[0]
	if r.Requests != 3 || r.OK != 2 || r.Errors != 1 {
		t.Errorf("counts: requests=%d ok=%d errors=%d, want 3/2/1", r.Requests, r.OK, r.Errors)
	}
	if r.TokensIn != 300 || r.TokensOut != 110 || r.CacheRead != 12 {
		t.Errorf("tokens: in=%d out=%d cacheRead=%d, want 300/110/12", r.TokensIn, r.TokensOut, r.CacheRead)
	}
	if r.Conversations != 2 {
		t.Errorf("conversations = %d, want 2", r.Conversations)
	}
	if r.AvgLatencyMs != 100 {
		t.Errorf("avg latency = %d, want 100", r.AvgLatencyMs)
	}
}

func TestStatsRequestsSeries(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "bob")
	now := time.Now().Unix()
	for i := 0; i < 4; i++ {
		_, err := db.ExecContext(ctx, `
			INSERT INTO request_log
			  (user_token_id, credential_id, conv_id, ts, path, status_code,
			   bytes_sent, bytes_received, latency_ms,
			   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
			VALUES (?, 'cred_1', 'c', ?, '/v1/messages', 200, 0, 0, 5, 'm', 10, 5, 0, 0)`,
			ut.ID, now-int64(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	cookie := loginCookie(t, h)
	w := do(t, h, http.MethodGet, "/ui/api/stats/requests?period=1h&buckets=10&group_by=user", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stats/requests = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Buckets []int64 `json:"buckets"`
		Series  []struct {
			ID       string  `json:"id"`
			Label    string  `json:"label"`
			Requests []int64 `json:"requests"`
			TokensIn []int64 `json:"tokens_in"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 10 {
		t.Fatalf("buckets = %d, want 10", len(resp.Buckets))
	}
	if len(resp.Series) != 1 || resp.Series[0].Label != "bob" {
		t.Fatalf("series = %+v", resp.Series)
	}
	var total, tin int64
	for i := range resp.Series[0].Requests {
		total += resp.Series[0].Requests[i]
		tin += resp.Series[0].TokensIn[i]
	}
	if total != 4 {
		t.Errorf("total requests across buckets = %d, want 4", total)
	}
	if tin != 40 {
		t.Errorf("total tokens_in = %d, want 40", tin)
	}
}

func TestStaticSPAFallback(t *testing.T) {
	_, h := newTestServer(t)
	// Unknown non-api path should serve index.html (SPA fallback), not 404.
	w := do(t, h, http.MethodGet, "/ui/dashboard", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("SPA fallback = %d, want 200", w.Code)
	}
}
