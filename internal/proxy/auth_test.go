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
		path        string
		authHeader  string
		wantStatus  int
		wantHitNext bool
	}{
		{"no token configured = passthrough", "", "/v1/messages", "", 200, true},
		{"correct token", "secret", "/v1/messages", "Bearer secret", 200, true},
		{"wrong token", "secret", "/v1/messages", "Bearer nope", 401, false},
		{"missing header", "secret", "/v1/messages", "", 401, false},
		{"non-bearer scheme", "secret", "/v1/messages", "Basic c2VjcmV0", 401, false},
		{"case-insensitive Bearer prefix", "secret", "/v1/messages", "bearer secret", 200, true},
		{"health bypass even without token", "secret", "/health", "", 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit = false
			h := AuthMiddleware(tc.token, nil, inner)
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
