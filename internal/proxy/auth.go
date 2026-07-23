package proxy

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// AuthMiddleware authenticates incoming requests.
//
// Priority order:
//  1. /health — always public.
//  2. When uiEnabled, any path that is not /v1/* and not /admin/* is passed
//     through untouched — the web UI (mounted at "/") does its own cookie auth.
//  3. adminToken match — admin identity, all routes allowed.
//  4. user token DB match — user identity, /v1/* and /health only;
//     /admin/* requests are rejected with 403.
//  5. No auth configured (adminToken=="" and no user tokens) — passthrough.
//  6. Otherwise — 401.
func AuthMiddleware(adminToken string, db *store.DB, uiEnabled bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		isProxy := strings.HasPrefix(r.URL.Path, "/v1/")
		isAdmin := strings.HasPrefix(r.URL.Path, "/admin/")

		// When the UI is enabled, everything outside the proxy (/v1/*) and admin
		// (/admin/*) surfaces is handled by the web UI, which authenticates
		// itself with its own session cookie. Bearer auth applies only to the
		// proxy and admin routes.
		if uiEnabled && !isProxy && !isAdmin {
			next.ServeHTTP(w, r)
			return
		}

		bearer := extractToken(r)

		// Admin token check.
		if adminToken != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(adminToken)) == 1 {
			ctx := usertoken.WithIdentity(r.Context(), &usertoken.Identity{IsAdmin: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// User token check.
		if db != nil && bearer != "" {
			ut, err := usertoken.LookupByToken(r.Context(), db, bearer)
			if err == nil && ut.Status == usertoken.StatusActive {
				if strings.HasPrefix(r.URL.Path, "/admin/") {
					http.Error(w,
						`{"type":"error","error":{"type":"authentication_error","message":"proxy: admin endpoints require the admin token"}}`,
						http.StatusForbidden)
					return
				}
				go usertoken.MarkUsed(context.Background(), db, ut.ID)
				ctx := usertoken.WithIdentity(r.Context(), &usertoken.Identity{
					UserTokenID: ut.ID,
					UserName:    ut.Name,
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// No auth configured → passthrough (backward compat).
		if adminToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="claude-proxy"`)
		http.Error(w,
			`{"type":"error","error":{"type":"authentication_error","message":"proxy: invalid or missing bearer token"}}`,
			http.StatusUnauthorized)
	})
}

func extractToken(r *http.Request) string {
	if got := bearerFromHeader(r.Header.Get("Authorization")); got != "" {
		return got
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func bearerFromHeader(v string) string {
	v = strings.TrimSpace(v)
	const p = "Bearer "
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return strings.TrimSpace(v[len(p):])
	}
	return ""
}
