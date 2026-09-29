package webui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

const (
	sessionCookie = "cpui_session"
	// sessionTTL is long because the session renews itself while in use
	// (see renewSession): an operator who opens the dashboard at least once a
	// fortnight never sees the login form again.
	sessionTTL = 30 * 24 * time.Hour
	cookiePath = "/"

	sessionSecretName = "webui_session_key"
)

// deriveKey builds the cookie-signing key: HMAC-SHA256 over SHA256(password)
// keyed by a random secret. Changing the password therefore invalidates every
// session, while a restart does not — the secret is persisted (see
// sessionSecret). It used to be regenerated per boot, which logged everyone
// out on every deploy and every watchtower update.
func deriveKey(password string, secret []byte) []byte {
	sum := sha256.Sum256([]byte(password))
	mac := hmac.New(sha256.New, secret)
	mac.Write(sum[:])
	return mac.Sum(nil)
}

// sessionSecret returns the persisted 32-byte signing secret, creating it on
// first use. INSERT OR IGNORE then SELECT makes concurrent first starts agree
// on one value. Without a database (or if it fails) a per-boot random secret
// is used: sessions then end at restart, which is the old behaviour, never an
// insecure one.
func sessionSecret(db *store.DB) []byte {
	fresh := make([]byte, 32)
	_, _ = rand.Read(fresh)
	if db == nil {
		return fresh
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO app_secret(name, value) VALUES (?, ?)`, sessionSecretName, fresh); err != nil {
		return fresh
	}
	var stored []byte
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM app_secret WHERE name = ?`, sessionSecretName).Scan(&stored); err != nil || len(stored) < 32 {
		return fresh
	}
	return stored
}

// signSession returns a cookie value "expiry|nonce|mac".
func (s *Server) signSession(expiry time.Time) string {
	var nb [16]byte
	_, _ = rand.Read(nb[:])
	nonce := hex.EncodeToString(nb[:])
	body := strconv.FormatInt(expiry.Unix(), 10) + "|" + nonce
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(body))
	return body + "|" + hex.EncodeToString(mac.Sum(nil))
}

// validSession verifies a cookie value and its expiry.
func (s *Server) validSession(val string) bool {
	_, ok := s.sessionExpiry(val)
	return ok
}

// sessionExpiry verifies a cookie value and returns its expiry.
func (s *Server) sessionExpiry(val string) (time.Time, bool) {
	parts := strings.Split(val, "|")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	body := parts[0] + "|" + parts[1]
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return time.Time{}, false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.validSession(c.Value)
}

func (s *Server) secure(r *http.Request) bool {
	return s.secureCookies || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.password)) != 1 {
		s.limiter.fail(ip)
		writeErr(w, http.StatusUnauthorized, "invalid password")
		return
	}
	s.issueSession(w, r)
	writeJSON(w, map[string]any{"authenticated": true})
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.signSession(exp),
		Path:     cookiePath,
		HttpOnly: true,
		Secure:   s.secure(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  exp,
	})
}

// handleSession answers the app shell's page-load check and slides a valid
// session forward once it is past half its lifetime, so regular use never
// hits the expiry. Renewing only then keeps it to one Set-Cookie per couple
// of weeks rather than one per page load.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if c, err := r.Cookie(sessionCookie); err == nil {
		if exp, ok := s.sessionExpiry(c.Value); ok {
			authenticated = true
			if time.Until(exp) < sessionTTL/2 {
				s.issueSession(w, r)
			}
		}
	}
	writeJSON(w, map[string]any{"authenticated": authenticated})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     cookiePath,
		HttpOnly: true,
		Secure:   s.secure(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, map[string]any{"authenticated": false})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter is a simple in-memory sliding-window rate limiter: at most 5
// failed login attempts per IP per minute.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

const (
	loginWindow   = time.Minute
	loginMaxFails = 5
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: make(map[string][]time.Time)}
}

// allow reports whether a login attempt from ip is permitted right now.
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip)) < loginMaxFails
}

// fail records a failed attempt for ip.
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.prune(ip), time.Now())
}

// prune drops timestamps older than the window and returns the remainder.
// Caller must hold the lock.
func (l *loginLimiter) prune(ip string) []time.Time {
	cutoff := time.Now().Add(-loginWindow)
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}
