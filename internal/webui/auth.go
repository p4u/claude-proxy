package webui

import (
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
)

const (
	sessionCookie = "cpui_session"
	sessionTTL    = 24 * time.Hour
	cookiePath    = "/"
)

// deriveKey builds the cookie-signing key: HMAC-SHA256 over SHA256(password)
// keyed by a random per-boot salt, so process restarts invalidate sessions.
func deriveKey(password string) []byte {
	var salt [32]byte
	_, _ = rand.Read(salt[:])
	sum := sha256.Sum256([]byte(password))
	mac := hmac.New(sha256.New, salt[:])
	mac.Write(sum[:])
	return mac.Sum(nil)
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
	parts := strings.Split(val, "|")
	if len(parts) != 3 {
		return false
	}
	body := parts[0] + "|" + parts[1]
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	return true
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
	writeJSON(w, map[string]any{"authenticated": true})
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
