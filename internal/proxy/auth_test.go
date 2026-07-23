package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
