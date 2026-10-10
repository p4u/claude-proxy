package claudioapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/claudioapi"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/proxy"
	"github.com/p4u/claude-proxy/internal/sessionbind"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

const (
	testSession  = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	secretAccess = "sk-ant-oat-SECRET-access-token"
	secretRefr   = "sk-ant-ort-SECRET-refresh-token"
)

type sessionStack struct {
	handler  http.Handler
	db       *store.DB
	sessions *sessionbind.Registry
	pool     *pool.Pool
	tokenA   *usertoken.UserToken
	tokenB   *usertoken.UserToken
	cred     *creds.Credential
}

func setupSessionStack(t *testing.T, wire bool) *sessionStack {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	a, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := usertoken.Create(ctx, db, "bob")
	if err != nil {
		t.Fatal(err)
	}
	c, err := creds.Insert(ctx, db, "work-max", "max", secretAccess, secretRefr, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}

	reg := sessionbind.New(16)
	p := pool.New(db)
	mux := http.NewServeMux()
	h := claudioapi.New(mux, db, &fakeCatalogue{})
	if wire {
		h.SetSessions(reg)
		h.SetPool(p)
	}
	mux.Handle("/v1/", http.NotFoundHandler())
	return &sessionStack{
		handler:  proxy.AuthMiddleware("", db, false, h.WrapHandler(mux)),
		db:       db,
		sessions: reg,
		pool:     p,
		tokenA:   a,
		tokenB:   b,
		cred:     c,
	}
}

func (s *sessionStack) get(t *testing.T, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/claudio/session"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rw := httptest.NewRecorder()
	s.handler.ServeHTTP(rw, req)
	return rw
}

func (s *sessionStack) owner(ut *usertoken.UserToken) string {
	return sessionbind.OwnerKey(&usertoken.Identity{UserTokenID: ut.ID})
}

func TestSessionRequiresAuth(t *testing.T) {
	s := setupSessionStack(t, true)
	s.sessions.Record(s.owner(s.tokenA), testSession, s.cred.ID)

	if rw := s.get(t, "", "?id="+testSession); rw.Code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d", rw.Code)
	}
	if rw := s.get(t, "not-a-real-token", "?id="+testSession); rw.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: expected 401, got %d", rw.Code)
	}
}

func TestSessionHappyPathShape(t *testing.T) {
	s := setupSessionStack(t, true)
	s.sessions.Record(s.owner(s.tokenA), testSession, s.cred.ID)
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO usage_history(credential_id, captured_at, five_hour_pct, seven_day_pct)
		VALUES (?, ?, 37.5, 12)`, s.cred.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	rw := s.get(t, s.tokenA.Token, "?id="+testSession)
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rw.Code, rw.Body.String())
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got struct {
		SessionID  string `json:"session_id"`
		Credential struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			Provider string `json:"provider"`
			Plan     string `json:"plan"`
		} `json:"credential"`
		BoundAt     string  `json:"bound_at"`
		LastSeen    string  `json:"last_seen"`
		SwitchedAt  *string `json:"switched_at"`
		Utilization *struct {
			FiveHourPct float64 `json:"five_hour_pct"`
			SevenDayPct float64 `json:"seven_day_pct"`
			CapturedAt  string  `json:"captured_at"`
		} `json:"utilization"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != testSession {
		t.Errorf("session_id = %q", got.SessionID)
	}
	if got.Credential.ID != s.cred.ID || got.Credential.Label != "work-max" ||
		got.Credential.Provider != "anthropic" || got.Credential.Plan != "max" {
		t.Errorf("credential = %+v", got.Credential)
	}
	for name, v := range map[string]string{"bound_at": got.BoundAt, "last_seen": got.LastSeen} {
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			t.Errorf("%s = %q is not RFC3339", name, v)
		}
	}
	if got.SwitchedAt != nil {
		t.Errorf("switched_at should be absent for a never-switched session, got %q", *got.SwitchedAt)
	}
	if got.Utilization == nil || got.Utilization.FiveHourPct != 37.5 || got.Utilization.SevenDayPct != 12 {
		t.Errorf("utilization = %+v", got.Utilization)
	}

	body := rw.Body.String()
	for _, secret := range []string{secretAccess, secretRefr, "access_token", "refresh_token"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
	}
}

func TestSessionOmitsUtilizationWithoutSnapshot(t *testing.T) {
	s := setupSessionStack(t, true)
	s.sessions.Record(s.owner(s.tokenA), testSession, s.cred.ID)

	rw := s.get(t, s.tokenA.Token, "?id="+testSession)
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	var m map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &m)
	if _, present := m["utilization"]; present {
		t.Errorf("utilization should be omitted without a snapshot: %v", m)
	}
}

func TestSessionSwitchedAt(t *testing.T) {
	s := setupSessionStack(t, true)
	other, err := creds.Insert(context.Background(), s.db, "second", "pro", "a2", "r2", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	s.sessions.Record(s.owner(s.tokenA), testSession, s.cred.ID)
	s.sessions.Record(s.owner(s.tokenA), testSession, other.ID)

	rw := s.get(t, s.tokenA.Token, "?id="+testSession)
	var got struct {
		Credential struct {
			ID   string `json:"id"`
			Plan string `json:"plan"`
		} `json:"credential"`
		SwitchedAt string `json:"switched_at"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Credential.ID != other.ID || got.Credential.Plan != "pro" {
		t.Errorf("credential = %+v", got.Credential)
	}
	if _, err := time.Parse(time.RFC3339, got.SwitchedAt); err != nil {
		t.Errorf("switched_at = %q: %v", got.SwitchedAt, err)
	}
}

func TestSessionOtherUserGets404(t *testing.T) {
	s := setupSessionStack(t, true)
	s.sessions.Record(s.owner(s.tokenA), testSession, s.cred.ID)

	rwB := s.get(t, s.tokenB.Token, "?id="+testSession)
	if rwB.Code != http.StatusNotFound {
		t.Fatalf("other user: expected 404, got %d", rwB.Code)
	}
	// Identical to the response for a session nobody ever recorded.
	rwUnknown := s.get(t, s.tokenB.Token, "?id=ffffffff-ffff-4fff-8fff-ffffffffffff")
	if rwUnknown.Code != http.StatusNotFound || rwUnknown.Body.String() != rwB.Body.String() {
		t.Errorf("cross-user 404 differs from unknown 404:\n%s\n%s", rwB.Body.String(), rwUnknown.Body.String())
	}
	if rwA := s.get(t, s.tokenA.Token, "?id="+testSession); rwA.Code != http.StatusOK {
		t.Fatalf("owner: expected 200, got %d", rwA.Code)
	}
}

func TestSessionBadID(t *testing.T) {
	s := setupSessionStack(t, true)
	for _, q := range []string{
		"", "?id=", "?id=short", "?id=" + strings.Repeat("a", 65),
		"?id=has%20space-1234", "?id=semi%3Bcolon1234",
	} {
		rw := s.get(t, s.tokenA.Token, q)
		if rw.Code != http.StatusBadRequest {
			t.Errorf("%q: expected 400, got %d", q, rw.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(rw.Body.Bytes(), &m); err != nil || m["type"] != "error" {
			t.Errorf("%q: expected error envelope, got %s", q, rw.Body.String())
		}
	}
}

func TestSessionWrongMethod(t *testing.T) {
	s := setupSessionStack(t, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/claudio/session?id="+testSession, nil)
	req.Header.Set("Authorization", "Bearer "+s.tokenA.Token)
	rw := httptest.NewRecorder()
	s.handler.ServeHTTP(rw, req)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestSessionUnwiredRegistryIs404(t *testing.T) {
	s := setupSessionStack(t, false)
	if rw := s.get(t, s.tokenA.Token, "?id="+testSession); rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 without a registry, got %d", rw.Code)
	}
}

func TestSessionDeletedCredentialIs404(t *testing.T) {
	s := setupSessionStack(t, true)
	s.sessions.Record(s.owner(s.tokenA), testSession, s.cred.ID)
	if err := creds.Delete(context.Background(), s.db, s.cred.ID); err != nil {
		t.Fatal(err)
	}
	if rw := s.get(t, s.tokenA.Token, "?id="+testSession); rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a deleted credential, got %d", rw.Code)
	}
}

func TestSessionAdvertisedInDiscovery(t *testing.T) {
	s := setupSessionStack(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/claudio", nil)
	req.Header.Set("Authorization", "Bearer "+s.tokenA.Token)
	rw := httptest.NewRecorder()
	s.handler.ServeHTTP(rw, req)
	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &body)
	for _, c := range body.Capabilities {
		if c == "session" {
			return
		}
	}
	t.Fatalf("capabilities %v missing \"session\"", body.Capabilities)
}
