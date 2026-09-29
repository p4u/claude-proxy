package webui

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func sessionOK(t *testing.T, h http.Handler, c *http.Cookie) bool {
	t.Helper()
	w := do(t, h, http.MethodGet, "/api/session", "", c)
	return strings.Contains(w.Body.String(), `"authenticated":true`)
}

// A restart builds a new Server over the same database. The session secret
// used to be random per boot, so every deploy logged every operator out.
func TestSessionSurvivesRestart(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	first := New(db, nil, testPassword, false)
	cookie := loginCookie(t, first)

	restarted := New(db, nil, testPassword, false)
	if !sessionOK(t, restarted, cookie) {
		t.Fatal("session must survive a restart over the same database")
	}
	if w := do(t, restarted, http.MethodGet, "/api/usage/current", "", cookie); w.Code != http.StatusOK {
		t.Fatalf("authenticated API after restart = %d", w.Code)
	}

	// Rotating the password is the way to end every session.
	rotated := New(db, nil, "a-new-password", false)
	if sessionOK(t, rotated, cookie) {
		t.Fatal("a password change must invalidate existing sessions")
	}

	// Another database (another deployment) must not accept the cookie.
	_, other := newTestServer(t)
	if sessionOK(t, other, cookie) {
		t.Fatal("a cookie signed by one deployment must not validate on another")
	}
}

// The page-load session check renews a session past half its lifetime, and
// leaves a fresh one alone.
func TestSessionSlidesForward(t *testing.T) {
	_, h := newTestServer(t)
	s := h.(*Server)

	fresh := loginCookie(t, h)
	if w := do(t, h, http.MethodGet, "/api/session", "", fresh); len(w.Result().Cookies()) != 0 {
		t.Fatal("a fresh session must not be re-issued on every page load")
	}

	aging := &http.Cookie{Name: sessionCookie, Value: s.signSession(time.Now().Add(sessionTTL/2 - time.Hour))}
	w := do(t, h, http.MethodGet, "/api/session", "", aging)
	if !strings.Contains(w.Body.String(), `"authenticated":true`) {
		t.Fatalf("aging session rejected: %s", w.Body.String())
	}
	var renewed *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			renewed = c
		}
	}
	if renewed == nil {
		t.Fatal("a session past half its lifetime must be renewed")
	}
	if exp, ok := s.sessionExpiry(renewed.Value); !ok || time.Until(exp) < sessionTTL-time.Minute {
		t.Fatalf("renewed session expiry = %v (ok=%v), want ~%v ahead", exp, ok, sessionTTL)
	}

	expired := &http.Cookie{Name: sessionCookie, Value: s.signSession(time.Now().Add(-time.Minute))}
	if w := do(t, h, http.MethodGet, "/api/session", "", expired); strings.Contains(w.Body.String(), `"authenticated":true`) || len(w.Result().Cookies()) != 0 {
		t.Fatal("an expired session must be neither accepted nor renewed")
	}
}
