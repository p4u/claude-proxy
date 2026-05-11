package users

import (
	"context"
	"net/http"
	"strings"
)

type contextKey struct{}

// Middleware enforces per-user bearer token authentication.
// It parses "Authorization: Bearer <token>", looks up the user by token hash,
// returns 401 JSON on any failure, and on success attaches the User to the
// request context and asynchronously updates last_used_at.
func Middleware(s *Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeUnauthorized(w)
			return
		}
		u, err := s.Lookup(r.Context(), token)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		go func() {
			_ = s.TouchLastUsed(context.Background(), u.ID)
		}()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, u)))
	})
}

// UserFromContext extracts the authenticated User from the request context.
// ok is false if the context carries no user (unauthenticated path).
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok
}

func bearerToken(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="claude-proxy"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}
