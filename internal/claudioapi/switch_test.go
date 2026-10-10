package claudioapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/sessionbind"
)

const switchConv = "switch-conv"

// seedSwitch pins switchConv to the stack's credential and records testSession
// for alice with the route the proxy would have recorded.
func (s *sessionStack) seedSwitch(t *testing.T) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES (?,?,?,?,?)`,
		switchConv, s.cred.ID, now, now, now); err != nil {
		t.Fatal(err)
	}
	s.sessions.RecordRoute(s.owner(s.tokenA), testSession, s.cred.ID,
		sessionbind.Route{ConvID: switchConv, Provider: provider.Anthropic})
}

func (s *sessionStack) addCred(t *testing.T, label, plan string) *creds.Credential {
	t.Helper()
	c, err := creds.Insert(context.Background(), s.db, label, plan, "sk-ant-oat-"+label, "rt-"+label, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *sessionStack) postSwitch(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/claudio/session/switch", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rw := httptest.NewRecorder()
	s.handler.ServeHTTP(rw, req)
	return rw
}

type switchBody struct {
	SessionID string `json:"session_id"`
	From      struct {
		ID, Label, Provider, Plan string
	} `json:"from"`
	To *struct {
		ID, Label, Provider, Plan string
	} `json:"to"`
	State string `json:"state"`
}

func errorType(t *testing.T, rw *httptest.ResponseRecorder) string {
	t.Helper()
	var m struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &m); err != nil || m.Type != "error" {
		t.Fatalf("not an error envelope: %s", rw.Body.String())
	}
	return m.Error.Type
}

var switchReq = `{"id":"` + testSession + `"}`

func TestSwitchRequiresAuth(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	for _, token := range []string{"", "not-a-real-token"} {
		if rw := s.postSwitch(t, token, switchReq); rw.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: expected 401, got %d", token, rw.Code)
		}
	}
}

func TestSwitchAcceptedShape(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	other := s.addCred(t, "spare-pro", "pro")

	rw := s.postSwitch(t, s.tokenA.Token, switchReq)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rw.Code, rw.Body.String())
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got switchBody
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != testSession || got.State != "pending" {
		t.Errorf("session_id=%q state=%q", got.SessionID, got.State)
	}
	if got.From.ID != s.cred.ID || got.From.Label != "work-max" || got.From.Provider != "anthropic" || got.From.Plan != "max" {
		t.Errorf("from = %+v", got.From)
	}
	if got.To == nil || got.To.ID != other.ID || got.To.Label != "spare-pro" || got.To.Provider != "anthropic" || got.To.Plan != "pro" {
		t.Errorf("to = %+v", got.To)
	}
	for _, secret := range []string{secretAccess, secretRefr, "sk-ant-oat-spare", "access_token", "refresh_token"} {
		if strings.Contains(rw.Body.String(), secret) {
			t.Errorf("response leaks %q: %s", secret, rw.Body.String())
		}
	}

	// Pending: nothing has moved yet, and asking again returns the same plan.
	var pinned string
	if err := s.db.QueryRow(`SELECT credential_id FROM conversations WHERE id=?`, switchConv).Scan(&pinned); err != nil || pinned != s.cred.ID {
		t.Fatalf("pin moved before the next request: %q %v", pinned, err)
	}
	again := s.postSwitch(t, s.tokenA.Token, switchReq)
	if again.Code != http.StatusAccepted || again.Body.String() != rw.Body.String() {
		t.Fatalf("second POST differs: %d %s\nfirst: %s", again.Code, again.Body.String(), rw.Body.String())
	}
}

func TestSwitchToIsNullWhenUndetermined(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	other := s.addCred(t, "spare", "pro")
	if rw := s.postSwitch(t, s.tokenA.Token, switchReq); rw.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rw.Code)
	}
	// The planned destination disappears while the plan is pending.
	if err := creds.Delete(context.Background(), s.db, other.ID); err != nil {
		t.Fatal(err)
	}
	rw := s.postSwitch(t, s.tokenA.Token, switchReq)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rw.Code, rw.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &m)
	if to, present := m["to"]; !present || to != nil {
		t.Fatalf("to should be present and null: %s", rw.Body.String())
	}
}

func TestSwitchNoAlternative(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	// The stack's only other credential would be the current one.
	rw := s.postSwitch(t, s.tokenA.Token, switchReq)
	if rw.Code != http.StatusConflict || errorType(t, rw) != "no_alternative" {
		t.Fatalf("expected 409 no_alternative, got %d: %s", rw.Code, rw.Body.String())
	}
	// A credential the picker excludes is not an alternative either.
	spare := s.addCred(t, "spare", "pro")
	if _, err := s.db.Exec(`UPDATE credentials SET status='disabled' WHERE id=?`, spare.ID); err != nil {
		t.Fatal(err)
	}
	if rw := s.postSwitch(t, s.tokenA.Token, switchReq); rw.Code != http.StatusConflict {
		t.Fatalf("disabled spare: expected 409, got %d", rw.Code)
	}
	// Account-bound conversations must keep their account.
	if _, err := s.db.Exec(`UPDATE credentials SET status='active' WHERE id=?`, spare.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE conversations SET account_bound=1 WHERE id=?`, switchConv); err != nil {
		t.Fatal(err)
	}
	rw = s.postSwitch(t, s.tokenA.Token, switchReq)
	if rw.Code != http.StatusConflict || errorType(t, rw) != "no_alternative" {
		t.Fatalf("account-bound: expected 409 no_alternative, got %d: %s", rw.Code, rw.Body.String())
	}
}

func TestSwitchOwnerScopingAnd404s(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	s.addCred(t, "spare", "pro")

	unknown := s.postSwitch(t, s.tokenB.Token, `{"id":"ffffffff-ffff-4fff-8fff-ffffffffffff"}`)
	if unknown.Code != http.StatusNotFound || errorType(t, unknown) != "not_found" {
		t.Fatalf("unknown: expected 404, got %d", unknown.Code)
	}
	cross := s.postSwitch(t, s.tokenB.Token, switchReq)
	if cross.Code != http.StatusNotFound || cross.Body.String() != unknown.Body.String() {
		t.Fatalf("cross-user 404 differs from unknown 404:\n%s\n%s", cross.Body.String(), unknown.Body.String())
	}
	// Bob's attempt must not have scheduled anything for alice's session.
	var pinned string
	_ = s.db.QueryRow(`SELECT credential_id FROM conversations WHERE id=?`, switchConv).Scan(&pinned)
	if pinned != s.cred.ID {
		t.Fatalf("pin = %s", pinned)
	}

	// Recorded without a pool route (cannot be located): same 404.
	const bare = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	s.sessions.Record(s.owner(s.tokenA), bare, s.cred.ID)
	if rw := s.postSwitch(t, s.tokenA.Token, `{"id":"`+bare+`"}`); rw.Code != http.StatusNotFound || rw.Body.String() != unknown.Body.String() {
		t.Fatalf("route-less session: %d %s", rw.Code, rw.Body.String())
	}

	// Deleted credential: same 404.
	if err := creds.Delete(context.Background(), s.db, s.cred.ID); err != nil {
		t.Fatal(err)
	}
	if rw := s.postSwitch(t, s.tokenA.Token, switchReq); rw.Code != http.StatusNotFound || rw.Body.String() != unknown.Body.String() {
		t.Fatalf("deleted credential: %d %s", rw.Code, rw.Body.String())
	}
}

func TestSwitchBadRequest(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	for i, body := range []string{
		"", "{", "null", "[]", `{}`, `{"id":""}`, `{"id":"short"}`, `{"id":42}`,
		`{"id":"has space-1234"}`, `{"id":"` + strings.Repeat("a", 65) + `"}`,
		`{"id":"` + testSession + `","pad":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		// Alternate callers so the per-identity burst (10) is not the limit.
		token := s.tokenA.Token
		if i%2 == 1 {
			token = s.tokenB.Token
		}
		rw := s.postSwitch(t, token, body)
		if rw.Code != http.StatusBadRequest || errorType(t, rw) != "invalid_request_error" {
			t.Errorf("%.40q: expected 400 invalid_request_error, got %d: %s", body, rw.Code, rw.Body.String())
		}
	}
}

func TestSwitchWrongMethod(t *testing.T) {
	s := setupSessionStack(t, true)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/claudio/session/switch", strings.NewReader(switchReq))
		req.Header.Set("Authorization", "Bearer "+s.tokenA.Token)
		rw := httptest.NewRecorder()
		s.handler.ServeHTTP(rw, req)
		if rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s: expected 405 Allow: POST, got %d %q", method, rw.Code, rw.Header().Get("Allow"))
		}
	}
}

func TestSwitchUnwiredIs404(t *testing.T) {
	s := setupSessionStack(t, false)
	s.seedSwitch(t)
	s.addCred(t, "spare", "pro")
	if rw := s.postSwitch(t, s.tokenA.Token, switchReq); rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 without a pool/registry, got %d", rw.Code)
	}
}

func TestSwitchRateLimited(t *testing.T) {
	s := setupSessionStack(t, true)
	s.seedSwitch(t)
	s.addCred(t, "spare", "pro")
	var limited bool
	for range 20 {
		if rw := s.postSwitch(t, s.tokenA.Token, switchReq); rw.Code == http.StatusTooManyRequests {
			limited = errorType(t, rw) == "rate_limit_error"
			break
		}
	}
	if !limited {
		t.Fatal("20 rapid switch requests were never rate limited")
	}
}

func TestSwitchAdvertisedInDiscovery(t *testing.T) {
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
		if c == "session_switch" {
			return
		}
	}
	t.Fatalf("capabilities %v missing \"session_switch\"", body.Capabilities)
}
