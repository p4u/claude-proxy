package webui

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	vocauth "github.com/vocdoni/oauth/go"
)

// SSOConfig enables single sign-on through an OpenID Connect provider (the
// central Vocdoni one, oauth.vocdoni.org, by default). When it is set the UI
// password is not accepted at all: /api/login answers 404.
type SSOConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the exact callback URL registered at the provider,
	// https://<host>/auth/callback.
	RedirectURL string
	// CookieKeys is a comma-separated list of base64 32-byte keys; the first
	// encrypts, any decrypts (rotation).
	CookieKeys string
	// AllowedEmails narrows sign-in to these addresses. Empty admits every
	// verified account of the provider's domain (vocdoni.org).
	AllowedEmails []string
	// Insecure allows http:// URLs and drops the Secure cookie attribute.
	// Local development against a dev provider only.
	Insecure bool
}

// DefaultSSOIssuer is the central Vocdoni sign-in service.
const DefaultSSOIssuer = "https://oauth.vocdoni.org"

// ssoSessionAge is the sign-in lifetime. It is absolute, not sliding, so it
// is set to the kit's maximum: the password session this replaces lasted 30
// days, and signing in again is one click while the browser's Google session
// is alive.
const ssoSessionAge = vocauth.MaxMaxAge

// SSO is a configured single sign-on, ready to hand to NewWithOptions.
type SSO struct {
	auth *vocauth.Auth
	// origin is scheme://host of the redirect URL, the form a browser sends
	// in the Origin header; state-changing API calls must come from it.
	origin string
}

// NewSSO validates cfg. It does no network I/O: the provider is discovered
// on the first sign-in, so the proxy starts even when the provider is down.
func NewSSO(cfg SSOConfig) (*SSO, error) {
	keys, err := vocauth.ParseCookieKeys(cfg.CookieKeys)
	if err != nil {
		return nil, err
	}
	issuer := cfg.Issuer
	if issuer == "" {
		issuer = DefaultSSOIssuer
	}
	u, err := url.Parse(cfg.RedirectURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("sso: redirect URL must be an absolute URL")
	}
	if u.Path != "/auth/callback" {
		return nil, errors.New("sso: redirect URL path must be /auth/callback")
	}
	limiter := newSignInLimiter()
	auth, err := vocauth.New(vocauth.Config{
		Issuer:        issuer,
		ClientID:      cfg.ClientID,
		ClientSecret:  cfg.ClientSecret,
		RedirectURL:   cfg.RedirectURL,
		CookieKeys:    keys,
		AllowedEmails: cfg.AllowedEmails,
		MaxAge:        ssoSessionAge,
		Insecure:      cfg.Insecure,
		Limiter:       func(r *http.Request) bool { return limiter.allow(clientIP(r)) },
	})
	if err != nil {
		return nil, err
	}
	return &SSO{auth: auth, origin: originOf(u)}, nil
}

// originOf is scheme://host lower-cased and without a default port, the form
// a browser writes in the Origin header.
func originOf(u *url.URL) string {
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndexByte(host, ':')]
	}
	return scheme + "://" + host
}

// user returns the signed-in user. The kit exposes the session only through
// its Require middleware, so it is run against a throwaway writer; the one
// header it may legitimately set on the way (clearing a tampered or expired
// cookie) is copied to the real response.
func (s *SSO) user(w http.ResponseWriter, r *http.Request) (vocauth.User, bool) {
	var (
		u  vocauth.User
		ok bool
	)
	probe := &headerSink{h: http.Header{}}
	s.auth.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		u, ok = vocauth.FromContext(r.Context())
	})).ServeHTTP(probe, r)
	for _, c := range probe.h.Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", c)
	}
	return u, ok
}

// sameOrigin reports whether a state-changing request comes from the UI's
// own pages, the same rule the kit applies to sign-out. The session cookie is
// SameSite=Lax (the provider's redirect back must carry the state cookie), so
// this, not the cookie, is what stops a cross-site form posting to /api.
func (s *SSO) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == s.origin
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// headerSink is an http.ResponseWriter that keeps headers and drops the body.
type headerSink struct{ h http.Header }

func (s *headerSink) Header() http.Header         { return s.h }
func (s *headerSink) Write(b []byte) (int, error) { return len(b), nil }
func (s *headerSink) WriteHeader(int)             {}

// signInLimiter caps sign-in starts and callbacks per client IP: a fixed
// window of signInMax per minute, generous for a person and tight for a
// script hammering the provider through this proxy.
type signInLimiter struct {
	mu     sync.Mutex
	start  time.Time
	counts map[string]int
}

const signInMax = 20

func newSignInLimiter() *signInLimiter {
	return &signInLimiter{counts: make(map[string]int)}
}

func (l *signInLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := time.Now(); now.Sub(l.start) >= time.Minute {
		l.start, l.counts = now, make(map[string]int)
	}
	l.counts[ip]++
	return l.counts[ip] <= signInMax
}

// handleSSOSession answers the app shell's page-load check in SSO mode.
func (s *Server) handleSSOSession(w http.ResponseWriter, r *http.Request) {
	u, ok := s.sso.user(w, r)
	if !ok {
		writeJSON(w, map[string]any{"authenticated": false, "method": "sso"})
		return
	}
	writeJSON(w, map[string]any{
		"authenticated": true,
		"method":        "sso",
		"user":          map[string]string{"email": u.Email, "name": u.Name},
	})
}

// ssoAuthorize gates an /api request in SSO mode and logs who changed what,
// since the e-mail is now the operator's identity.
func (s *Server) ssoAuthorize(w http.ResponseWriter, r *http.Request) bool {
	u, ok := s.sso.user(w, r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !s.sso.sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
			return false
		}
		slog.Info("web UI change", "user", u.Email, "method", r.Method, "path", r.URL.Path)
	}
	return true
}
