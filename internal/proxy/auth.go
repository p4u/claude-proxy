package proxy

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthMiddleware enforces a shared-secret bearer token on incoming requests.
// It accepts the token via:
//   - Authorization: Bearer <token>
//   - x-api-key: <token>   (Anthropic SDK default)
//
// If token is empty the middleware is a no-op. /health is always allowed
// through so container healthchecks keep working.
func AuthMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Try both header forms.
		got := bearerFromHeader(r.Header.Get("Authorization"))
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}

		if got == "" || subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="claude-proxy"`)
			http.Error(w,
				`{"type":"error","error":{"type":"authentication_error","message":"proxy: invalid or missing bearer token"}}`,
				http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerFromHeader(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	const p = "Bearer "
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return strings.TrimSpace(v[len(p):])
	}
	return ""
}
