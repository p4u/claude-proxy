package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/claudeoauth"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

// TestClaudeBrowserLogin drives /api/credentials/oauth/* against a fake
// Anthropic: add a subscription, then reconnect it in place, and check the
// guards that must hold before a single-use code is spent.
func TestClaudeBrowserLogin(t *testing.T) {
	issued := 0
	orgType := "claude_max"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			issued++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "sk-ant-oat01-web" + string(rune('0'+issued)),
				"refresh_token": "sk-ant-ort01-web" + string(rune('0'+issued)),
				"expires_in":    28800, "scope": "user:profile user:inference",
			})
		case "/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account":      map[string]string{"email": "owner@example.com"},
				"organization": map[string]string{"organization_type": orgType},
			})
		}
	}))
	t.Cleanup(upstream.Close)
	oldToken, oldProfile := creds.TokenURL, claudeoauth.ProfileURL
	creds.TokenURL, claudeoauth.ProfileURL = upstream.URL+"/token", upstream.URL+"/profile"
	t.Cleanup(func() { creds.TokenURL, claudeoauth.ProfileURL = oldToken, oldProfile })

	db, err := store.Open(filepath.Join(t.TempDir(), "webui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, claudeLogin: claudeoauth.New()}

	call := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/credentials"+path, strings.NewReader(body)), path)
		return rec
	}
	begin := func() (session, state string) {
		rec := call("/oauth/start", "")
		var out struct{ Session, URL string }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Session == "" {
			t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
		}
		u, _ := url.Parse(out.URL)
		return out.Session, u.Query().Get("state")
	}
	body := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }

	session, state := begin()
	rec := call("/oauth/exchange", body(map[string]any{"session": session, "code": "c#" + state}))
	if rec.Code != http.StatusOK {
		t.Fatalf("add = %d %s", rec.Code, rec.Body.String())
	}
	var added struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Weight int    `json:"weight"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &added)
	if added.Label != "owner@example.com" || added.Weight != 5 || strings.Contains(rec.Body.String(), "sk-ant") {
		t.Fatalf("add response = %s", rec.Body.String())
	}
	c, err := creds.Get(context.Background(), db, added.ID)
	if err != nil || c.SubscriptionType != "max" || c.RefreshToken != "sk-ant-ort01-web1" || time.Until(c.ExpiresAt) < 7*time.Hour {
		t.Fatalf("stored credential = %+v, %v", c, err)
	}

	// Reconnect: a revoked credential is re-pointed at a fresh sign-in.
	_ = creds.SetStatus(context.Background(), db, added.ID, creds.StatusRevoked)
	orgType = "claude_pro" // the account changed plan since it was added
	session, state = begin()
	rec = call("/oauth/exchange", body(map[string]any{"session": session, "code": "c#" + state, "credential_id": added.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect = %d %s", rec.Code, rec.Body.String())
	}
	c, _ = creds.Get(context.Background(), db, added.ID)
	if c.RefreshToken != "sk-ant-ort01-web2" || c.Status != creds.StatusActive || c.Label != "owner@example.com" ||
		c.SubscriptionType != "pro" || c.Weight != 5 {
		t.Fatalf("reconnected credential = %+v", c)
	}
	list, _ := creds.List(context.Background(), db)
	if len(list) != 1 {
		t.Fatalf("reconnect created a second credential: %d rows", len(list))
	}

	// An unknown target is refused before the code is spent, so the same
	// session still completes afterwards.
	session, state = begin()
	rec = call("/oauth/exchange", body(map[string]any{"session": session, "code": "c#" + state, "credential_id": "cred_missing"}))
	if rec.Code != http.StatusNotFound || issued != 2 {
		t.Fatalf("unknown target = %d (token calls %d)", rec.Code, issued)
	}
	rec = call("/oauth/exchange", body(map[string]any{"session": session, "code": "c#" + state, "credential_id": "gateway_codex"}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("gateway target = %d", rec.Code)
	}
	call("/oauth/cancel", body(map[string]any{"session": session}))
	rec = call("/oauth/exchange", body(map[string]any{"session": session, "code": "c#" + state}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired or unknown") {
		t.Fatalf("cancelled session = %d %s", rec.Code, rec.Body.String())
	}
}
