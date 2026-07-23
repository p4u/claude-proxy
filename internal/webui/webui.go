// Package webui serves the management/monitoring dashboard (cookie-authenticated
// REST API under /api/ + embedded SPA) at the root "/". It is mounted by the
// serve command only when a UI password is configured. Legacy /ui* paths
// permanently redirect to the root.
package webui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

//go:embed all:static
var staticFS embed.FS

// Server holds the dependencies and auth state for the web UI.
type Server struct {
	db            *store.DB
	refresher     *creds.Refresher
	password      string
	secureCookies bool

	hmacKey []byte
	static  fs.FS
	limiter *loginLimiter
}

// New builds the web UI HTTP handler. It authenticates itself via a signed
// session cookie derived from password + a random boot salt, so restarting the
// process invalidates all sessions.
func New(db *store.DB, refresher *creds.Refresher, password string, secureCookies bool) http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The embed directive guarantees the directory exists; this cannot fail
		// at runtime, but fall back to the raw FS to stay safe.
		sub = staticFS
	}
	return &Server{
		db:            db,
		refresher:     refresher,
		password:      password,
		secureCookies: secureCookies,
		hmacKey:       deriveKey(password),
		static:        sub,
		limiter:       newLoginLimiter(),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/ui" || strings.HasPrefix(path, "/ui/"):
		// Legacy /ui* paths permanently redirect to the root, stripping the
		// /ui prefix but preserving the remainder and query string.
		target := strings.TrimPrefix(path, "/ui")
		if target == "" {
			target = "/"
		}
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	case path == "/api" || strings.HasPrefix(path, "/api/"):
		s.serveAPI(w, r, strings.TrimPrefix(path, "/api"))
	default:
		s.serveStatic(w, r)
	}
}

// serveAPI dispatches the REST API. `rest` is the path after /api (e.g.
// "/login", "/stats/requests"). Unauthenticated endpoints are handled first;
// everything else requires a valid session cookie.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, rest string) {
	w.Header().Set("Content-Type", "application/json")

	switch rest {
	case "/login":
		s.handleLogin(w, r)
		return
	case "/logout":
		s.handleLogout(w, r)
		return
	case "/session":
		writeJSON(w, map[string]any{"authenticated": s.authenticated(r)})
		return
	}

	if !s.authenticated(r) {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}

	switch {
	case rest == "/overview" && r.Method == http.MethodGet:
		s.handleOverview(w, r)
	case rest == "/stats/requests" && r.Method == http.MethodGet:
		s.handleStatsSeries(w, r, "requests")
	case rest == "/stats/tokens" && r.Method == http.MethodGet:
		s.handleStatsSeries(w, r, "tokens")
	case rest == "/stats/users" && r.Method == http.MethodGet:
		s.handleStatsUsers(w, r)
	case rest == "/stats/latency" && r.Method == http.MethodGet:
		s.handleStatsLatency(w, r)
	case rest == "/usage/current" && r.Method == http.MethodGet:
		s.handleUsageCurrent(w, r)
	case rest == "/usage/history" && r.Method == http.MethodGet:
		s.handleUsageHistory(w, r)
	case rest == "/conversations" && r.Method == http.MethodGet:
		s.handleConversations(w, r)
	case rest == "/credentials" || strings.HasPrefix(rest, "/credentials/"):
		s.handleCredentials(w, r, strings.TrimPrefix(rest, "/credentials"))
	case rest == "/users" || strings.HasPrefix(rest, "/users/"):
		s.handleUsers(w, r, strings.TrimPrefix(rest, "/users"))
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// serveStatic serves embedded assets from the root, with SPA fallback: any
// non-file path (deep link like /dashboard, or a directory) returns index.html
// so client-side hash routing works on hard refresh.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		s.serveFile(w, r, "index.html")
		return
	}

	f, err := s.static.Open(rel)
	if err != nil {
		// SPA fallback.
		s.serveFile(w, r, "index.html")
		return
	}
	st, serr := f.Stat()
	f.Close()
	if serr != nil || st.IsDir() {
		// Directories aren't files → fall back to the SPA entrypoint.
		s.serveFile(w, r, "index.html")
		return
	}
	s.serveFile(w, r, rel)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.static.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
}

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
