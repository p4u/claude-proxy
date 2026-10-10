package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/claudioapi"
	"github.com/p4u/claude-proxy/internal/sessionbind"
)

// TestUserSessionSwitchEndToEnd drives the whole path: a Claude Code session
// is served, the client asks POST /v1/claudio/session/switch, and the
// session's next request goes out on the other credential carrying
// X-Router-Rebalance: switched, which GET /v1/claudio/session then reflects.
func TestUserSessionSwitchEndToEnd(t *testing.T) {
	var up rebalanceUpstream
	h, cs, db, _ := setupProxy(t, up.handler("application/json", rebalanceRespJSON))
	mux := http.NewServeMux()
	api := claudioapi.New(mux, db, h)
	api.SetSessions(h.Sessions)
	api.SetPool(h.pool)
	mux.Handle("/v1/", h)
	srv := AuthMiddleware("", db, false, api.WrapHandler(mux))

	const sid = "5e55a0a1-0000-4000-8000-000000000001"
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(sessionbind.Header, sid)
		rw := httptest.NewRecorder()
		srv.ServeHTTP(rw, req)
		return rw
	}
	authFor := map[string]string{"Bearer sk-ant-oat-fake-A": cs[0].ID, "Bearer sk-ant-oat-fake-B": cs[1].ID}

	first := do(http.MethodPost, "/v1/messages", rebalanceReqBody)
	if first.Code != http.StatusOK || first.Header().Get("X-Router-Rebalance") != "" {
		t.Fatalf("first: %d %v", first.Code, first.Header())
	}
	auths, _ := up.snapshot()
	from := authFor[auths[0]]

	sw := do(http.MethodPost, "/v1/claudio/session/switch", `{"id":"`+sid+`"}`)
	if sw.Code != http.StatusAccepted {
		t.Fatalf("switch: %d %s", sw.Code, sw.Body.String())
	}
	var plan struct {
		From  struct{ ID string }  `json:"from"`
		To    *struct{ ID string } `json:"to"`
		State string               `json:"state"`
	}
	if err := json.Unmarshal(sw.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.From.ID != from || plan.To == nil || plan.To.ID == from || plan.State != "pending" {
		t.Fatalf("plan = %s (served by %s)", sw.Body.String(), from)
	}
	if n := up.authCount(); n != 1 {
		t.Fatalf("switch request reached the upstream: %d calls", n)
	}

	second := do(http.MethodPost, "/v1/messages", rebalanceReqBody)
	if second.Code != http.StatusOK {
		t.Fatalf("second: %d", second.Code)
	}
	if got := second.Header().Get("X-Router-Rebalance"); got != "switched" {
		t.Fatalf("X-Router-Rebalance = %q", got)
	}
	if msg := second.Header().Get("X-Router-Message"); !strings.Contains(msg, "as requested") {
		t.Fatalf("X-Router-Message = %q", msg)
	}
	auths, _ = up.snapshot()
	if got := authFor[auths[1]]; got != plan.To.ID {
		t.Fatalf("second request served by %s, want %s", got, plan.To.ID)
	}
	for _, leak := range []string{cs[0].ID, cs[1].ID, "sk-ant"} {
		if strings.Contains(second.Header().Get("X-Router-Message"), leak) {
			t.Fatalf("notice leaks %q", leak)
		}
	}

	view := do(http.MethodGet, "/v1/claudio/session?id="+sid, "")
	var session struct {
		Credential struct{ ID string } `json:"credential"`
		SwitchedAt *string             `json:"switched_at"`
	}
	if err := json.Unmarshal(view.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Credential.ID != plan.To.ID || session.SwitchedAt == nil {
		t.Fatalf("session view = %s", view.Body.String())
	}

	third := do(http.MethodPost, "/v1/messages", rebalanceReqBody)
	auths, _ = up.snapshot()
	if third.Header().Get("X-Router-Rebalance") != "" || authFor[auths[2]] != plan.To.ID {
		t.Fatalf("new pin not sticky: %v served by %s", third.Header(), authFor[auths[2]])
	}
}
