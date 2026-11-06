package claudioapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/p4u/claude-proxy/internal/claudioapi"
	"github.com/p4u/claude-proxy/internal/proxy"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// TestLoginThroughFullStack runs the login routes through the production
// chain (AuthMiddleware → WrapHandler → mux): no credential and a stale one
// both reach the local handler, nothing is forwarded or logged, and a
// look-alike path still needs a credential.
func TestLoginThroughFullStack(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "stack.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := usertoken.Create(context.Background(), db, "someone"); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	mux := http.NewServeMux()
	claudioH := claudioapi.New(mux, db, &fakeCatalogue{})
	claudioH.SetLogins(claudioapi.NewLogins("https://proxy.test/claudio/login"))
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	handler := proxy.AuthMiddleware("admin-secret", db, true, claudioH.WrapHandler(mux))

	for _, bearer := range []string{"", "stale-token"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/claudio/login/start", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"device_code"`) {
			t.Fatalf("start with bearer %q: %d %s", bearer, rw.Code, rw.Body.String())
		}
		req = httptest.NewRequest(http.MethodPost, "/v1/claudio/login/poll", strings.NewReader(`{"device_code":"nope"}`))
		rw = httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		if rw.Code != http.StatusNotFound {
			t.Fatalf("poll unknown: %d %s", rw.Code, rw.Body.String())
		}
	}
	rw := httptest.NewRecorder()
	handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/v1/claudio/login/../poll", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("dot-segment login path: %d", rw.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("forwarded upstream %d times", hits.Load())
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&n)
	if n != 0 {
		t.Fatalf("request_log rows: %d", n)
	}
}
