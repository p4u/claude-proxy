package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// openDB opens a temporary SQLite store for the auth tests.
func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAuthMiddleware(t *testing.T) {
	hit := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(200)
	})

	cases := []struct {
		name        string
		token       string
		uiEnabled   bool
		path        string
		authHeader  string
		wantStatus  int
		wantHitNext bool
	}{
		{"no token configured = passthrough", "", false, "/v1/messages", "", 200, true},
		{"correct token", "secret", false, "/v1/messages", "Bearer secret", 200, true},
		{"wrong token", "secret", false, "/v1/messages", "Bearer nope", 401, false},
		{"missing header", "secret", false, "/v1/messages", "", 401, false},
		{"non-bearer scheme", "secret", false, "/v1/messages", "Basic c2VjcmV0", 401, false},
		{"case-insensitive Bearer prefix", "secret", false, "/v1/messages", "bearer secret", 200, true},
		{"health bypass even without token", "secret", false, "/health", "", 200, true},

		// UI disabled: unknown (non-proxy, non-admin) paths keep pre-UI behavior.
		{"ui disabled, unknown path, token set = 401", "secret", false, "/dashboard", "", 401, false},
		{"ui disabled, unknown path, no token = passthrough", "", false, "/dashboard", "", 200, true},
		{"ui disabled, /admin needs token", "secret", false, "/admin/credentials", "", 401, false},

		// UI enabled: everything outside /v1/* and /admin/* passes through.
		{"ui enabled, root passthrough", "secret", true, "/", "", 200, true},
		{"ui enabled, static passthrough", "secret", true, "/js/app.js", "", 200, true},
		{"ui enabled, api passthrough", "secret", true, "/api/overview", "", 200, true},
		{"ui enabled, /ui redirect passthrough", "secret", true, "/ui/", "", 200, true},
		{"ui enabled, /v1 still needs token", "secret", true, "/v1/messages", "", 401, false},
		{"ui enabled, /v1 with token", "secret", true, "/v1/messages", "Bearer secret", 200, true},
		{"ui enabled, /admin still needs token", "secret", true, "/admin/credentials", "", 401, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit = false
			h := AuthMiddleware(tc.token, nil, tc.uiEnabled, inner)
			req := httptest.NewRequest("POST", "http://x"+tc.path, nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d body=%s", rw.Code, tc.wantStatus, rw.Body.String())
			}
			if hit != tc.wantHitNext {
				t.Fatalf("inner hit: got %v want %v", hit, tc.wantHitNext)
			}
		})
	}
}

// TestAuthMiddlewareRevokedTokenWithNoAdminToken is the security regression
// test for finding 2 (SECURITY, pre-existing): a disabled, deleted or unknown
// bearer token must never pass through as anonymous when any auth mechanism
// is configured — even when adminToken is empty.
func TestAuthMiddlewareRevokedTokenWithNoAdminToken(t *testing.T) {
	ctx := context.Background()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	// -------------------------------------------------------------------------
	// Sub-case 1: disabled token with adminToken="" must get 401.
	// -------------------------------------------------------------------------
	t.Run("disabled token, no admin token", func(t *testing.T) {
		db := openDB(t)
		ut, err := usertoken.Create(ctx, db, "alice")
		if err != nil {
			t.Fatal(err)
		}
		// Disable the token.
		if err := usertoken.SetStatus(ctx, db, ut.ID, usertoken.StatusDisabled); err != nil {
			t.Fatal(err)
		}
		h := AuthMiddleware("", db, false, inner)
		req := httptest.NewRequest("GET", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+ut.Token)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("disabled token: got %d, want 401; body=%s", rw.Code, rw.Body)
		}
	})

	// -------------------------------------------------------------------------
	// Sub-case 2: completely unknown bearer with adminToken="" and user
	// tokens in DB must get 401.
	// -------------------------------------------------------------------------
	t.Run("unknown token, user tokens exist, no admin token", func(t *testing.T) {
		db := openDB(t)
		if _, err := usertoken.Create(ctx, db, "bob"); err != nil {
			t.Fatal(err)
		}
		h := AuthMiddleware("", db, false, inner)
		req := httptest.NewRequest("GET", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer totally-unknown-token")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("unknown token: got %d, want 401; body=%s", rw.Code, rw.Body)
		}
	})

	// -------------------------------------------------------------------------
	// Sub-case 3: rotated (deleted) token with adminToken="" must get 401.
	// -------------------------------------------------------------------------
	t.Run("rotated token, no admin token", func(t *testing.T) {
		db := openDB(t)
		ut, err := usertoken.Create(ctx, db, "charlie")
		if err != nil {
			t.Fatal(err)
		}
		oldToken := ut.Token
		if _, err := usertoken.Refresh(ctx, db, ut.ID); err != nil {
			t.Fatal(err)
		}
		h := AuthMiddleware("", db, false, inner)
		req := httptest.NewRequest("GET", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+oldToken)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("rotated token: got %d, want 401; body=%s", rw.Code, rw.Body)
		}
	})

	// -------------------------------------------------------------------------
	// Sub-case 4: disabled token WITH adminToken configured must also get 401
	// (existing behaviour — unchanged).
	// -------------------------------------------------------------------------
	t.Run("disabled token, admin token configured", func(t *testing.T) {
		db := openDB(t)
		ut, err := usertoken.Create(ctx, db, "dave")
		if err != nil {
			t.Fatal(err)
		}
		if err := usertoken.SetStatus(ctx, db, ut.ID, usertoken.StatusDisabled); err != nil {
			t.Fatal(err)
		}
		h := AuthMiddleware("admin-secret", db, false, inner)
		req := httptest.NewRequest("GET", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+ut.Token)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("disabled token+admin: got %d, want 401; body=%s", rw.Code, rw.Body)
		}
	})

	// -------------------------------------------------------------------------
	// Sub-case 5: no user tokens and no admin token → passthrough still works.
	// -------------------------------------------------------------------------
	t.Run("no tokens, no admin token → passthrough", func(t *testing.T) {
		db := openDB(t)
		h := AuthMiddleware("", db, false, inner)
		req := httptest.NewRequest("GET", "/v1/messages", nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("no-auth passthrough: got %d, want 200; body=%s", rw.Code, rw.Body)
		}
	})

	// -------------------------------------------------------------------------
	// Sub-case 6: user tokens exist, no bearer, no admin token → 401.
	// -------------------------------------------------------------------------
	t.Run("user tokens exist, no bearer, no admin token → 401", func(t *testing.T) {
		db := openDB(t)
		if _, err := usertoken.Create(ctx, db, "eve"); err != nil {
			t.Fatal(err)
		}
		h := AuthMiddleware("", db, false, inner)
		req := httptest.NewRequest("GET", "/v1/messages", nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("no-bearer+tokens: got %d, want 401; body=%s", rw.Code, rw.Body)
		}
	})
}

// TestAuthMiddlewareClaudioLoginExempt: the two device-login routes pass with
// no credential and with a stale one (ignored, never validated), and carry no
// identity; look-alike paths get no exemption.
func TestAuthMiddlewareClaudioLoginExempt(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	ut, err := usertoken.Create(ctx, db, "frank")
	if err != nil {
		t.Fatal(err)
	}
	if err := usertoken.SetStatus(ctx, db, ut.ID, usertoken.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	var hit bool
	var gotID *usertoken.Identity
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit, gotID = true, usertoken.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := AuthMiddleware("admin-secret", db, true, inner)

	cases := []struct {
		name, path, bearer string
		wantCode           int
		wantHit            bool
	}{
		{"start, no credential", "/v1/claudio/login/start", "", 200, true},
		{"poll, no credential", "/v1/claudio/login/poll", "", 200, true},
		{"start, stale bearer", "/v1/claudio/login/start", ut.Token, 200, true},
		{"poll, stale bearer", "/v1/claudio/login/poll", "garbage", 200, true},
		{"poll, admin bearer ignored", "/v1/claudio/login/poll", "admin-secret", 200, true},
		{"startx is not exempt", "/v1/claudio/login/startx", "", 401, false},
		{"dot segment is not exempt", "/v1/claudio/login/../poll", "", 401, false},
		{"trailing slash is not exempt", "/v1/claudio/login/start/", "", 401, false},
		{"encoded slash is not exempt", "/v1/claudio/login%2Fstart", "", 401, false},
		{"other claudio route still needs auth", "/v1/claudio/plugins", "", 401, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, gotID = false, nil
			req := httptest.NewRequest(http.MethodPost, "http://x"+tc.path, nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.wantCode || hit != tc.wantHit {
				t.Fatalf("got %d hit=%v, want %d hit=%v; body=%s", rw.Code, hit, tc.wantCode, tc.wantHit, rw.Body.String())
			}
			if hit && gotID != nil {
				t.Fatalf("login route carried identity %+v", gotID)
			}
		})
	}
}
