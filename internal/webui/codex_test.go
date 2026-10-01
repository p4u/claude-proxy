package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/codexgateway"
	"github.com/p4u/claude-proxy/internal/store"
)

func TestValidateSidecarRedirect(t *testing.T) {
	cases := []struct {
		ch   codexgateway.Channel
		good []string
		bad  []string
	}{
		{
			ch: codexgateway.CodexChannel,
			good: []string{
				"http://localhost:1455/auth/callback?code=abc&state=state_1",
				"http://127.0.0.1:8317/codex/callback?code=abc&scope=openid+offline_access&state=state_1",
			},
			bad: []string{
				"https://proxy.example/auth/callback?code=abc&state=state_1",
				"http://localhost:1455/auth/callback?code=abc&state=wrong",
				"http://localhost:1455/other?code=abc&state=state_1",
				"http://localhost:1455/auth/callback?state=state_1",
				"http://127.0.0.1:8317/anthropic/callback?code=abc&state=state_1",
				"http://localhost:8317/codex/callback?code=abc&state=state_1",
				// Another channel's callback must not complete a Codex login.
				"http://127.0.0.1:8317/antigravity/callback?code=abc&state=state_1",
			},
		},
		{
			ch: codexgateway.GeminiChannel,
			good: []string{
				"http://localhost:51121/oauth-callback?code=4/0Ab&state=state_1",
				// The exact shape a browser lands on after the sidecar's
				// forwarder hop (observed during the 2026-10-01 trial).
				"http://127.0.0.1:8317/antigravity/callback?state=state_1&iss=https://accounts.google.com&code=4/0Ab&scope=email%20profile&authuser=1&prompt=consent",
			},
			bad: []string{
				"http://localhost:1455/auth/callback?code=abc&state=state_1",
				"http://127.0.0.1:8317/codex/callback?code=abc&state=state_1",
				"http://localhost:51121/oauth-callback?code=abc&state=wrong",
				"http://localhost:51121/oauth-callback?state=state_1",
			},
		},
	}
	for _, c := range cases {
		for _, good := range c.good {
			if err := validateSidecarRedirect(c.ch, good, "state_1"); err != nil {
				t.Errorf("%s: valid redirect %q: %v", c.ch.Key, good, err)
			}
		}
		for _, raw := range c.bad {
			if err := validateSidecarRedirect(c.ch, raw, "state_1"); err == nil {
				t.Errorf("%s: validateSidecarRedirect(%q) succeeded", c.ch.Key, raw)
			}
		}
	}
}

func TestValidateOAuthState(t *testing.T) {
	for _, state := range []string{"abc-DEF_123", "a.b"} {
		if err := validateOAuthState(state); err != nil {
			t.Errorf("valid state %q: %v", state, err)
		}
	}
	for _, state := range []string{"", "../x", "has/slash", "has space"} {
		if err := validateOAuthState(state); err == nil {
			t.Errorf("invalid state %q accepted", state)
		}
	}
}

func TestCodexManagementAPI(t *testing.T) {
	var callbackBody string
	var weightBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-key" {
			http.Error(w, `{"error":"bad auth"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": "owner.json", "type": "codex", "email": "owner@example.com", "weight": 3, "access_token": "must-not-leak"},
				{"name": "skip.json", "type": "gemini"},
			}})
		case "/v0/management/auth-files/fields":
			raw, _ := io.ReadAll(r.Body)
			weightBody = string(raw)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/v0/management/codex-auth-url":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "url": "https://auth.openai.com/oauth/authorize", "state": "state_1"})
		case "/v0/management/get-auth-status":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "wait"})
		case "/v0/management/oauth-callback":
			raw, _ := io.ReadAll(r.Body)
			callbackBody = string(raw)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/v0/management/oauth-session":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "cancelled": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	client, err := codexgateway.New(codexgateway.Config{
		BaseURL: upstream.URL, APIKey: "api-key", ManagementKey: "management-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "codex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := codexgateway.ReconcileCredential(t.Context(), db, client); err != nil {
		t.Fatal(err)
	}
	h := NewWithCodex(db, nil, testPassword, false, client)
	cookie := loginCookie(t, h)
	credentials := do(t, h, http.MethodGet, "/api/credentials", "", cookie)
	if credentials.Code != http.StatusOK || strings.Contains(credentials.Body.String(), codexgateway.GatewayCredentialID) {
		t.Fatalf("internal gateway leaked into credentials = %d %s", credentials.Code, credentials.Body.String())
	}
	gatewayDelete := do(t, h, http.MethodDelete, "/api/credentials/"+codexgateway.GatewayCredentialID, "", cookie)
	if gatewayDelete.Code != http.StatusConflict {
		t.Fatalf("gateway delete = %d %s", gatewayDelete.Code, gatewayDelete.Body.String())
	}

	accounts := do(t, h, http.MethodGet, "/api/codex/accounts", "", cookie)
	if accounts.Code != http.StatusOK || !strings.Contains(accounts.Body.String(), "owner@example.com") || !strings.Contains(accounts.Body.String(), `"weight":3`) || strings.Contains(accounts.Body.String(), "must-not-leak") || strings.Contains(accounts.Body.String(), "skip.json") {
		t.Fatalf("accounts = %d %s", accounts.Code, accounts.Body.String())
	}
	weight := do(t, h, http.MethodPost, "/api/codex/accounts/weight", `{"name":"owner.json","weight":7}`, cookie)
	if weight.Code != http.StatusOK {
		t.Fatalf("weight = %d %s; upstream=%s", weight.Code, weight.Body.String(), weightBody)
	}
	// Option A: /weight stores the operator's base weight in our DB; the
	// rebalance loop is the only writer to the sidecar's weight field.
	// Verify round-trip via /accounts.
	afterWeight := do(t, h, http.MethodGet, "/api/codex/accounts", "", cookie)
	if !strings.Contains(afterWeight.Body.String(), `"base_weight":7`) {
		t.Fatalf("base weight not persisted: %s", afterWeight.Body.String())
	}
	invalidWeight := do(t, h, http.MethodPost, "/api/codex/accounts/weight", `{"name":"owner.json","weight":0}`, cookie)
	if invalidWeight.Code != http.StatusBadRequest {
		t.Fatalf("invalid weight = %d %s", invalidWeight.Code, invalidWeight.Body.String())
	}
	start := do(t, h, http.MethodPost, "/api/codex/oauth/start", "", cookie)
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), "localhost:1455") || !strings.Contains(start.Body.String(), "127.0.0.1:8317") {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	status := do(t, h, http.MethodGet, "/api/codex/oauth/status?state=state_1", "", cookie)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"wait"`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	callbackURL := "http://127.0.0.1:8317/codex/callback?code=once&scope=openid&state=state_1"
	callback := do(t, h, http.MethodPost, "/api/codex/oauth/callback", `{"state":"state_1","redirect_url":"`+callbackURL+`"}`, cookie)
	var submitted struct {
		RedirectURL string `json:"redirect_url"`
	}
	_ = json.Unmarshal([]byte(callbackBody), &submitted)
	if callback.Code != http.StatusOK || submitted.RedirectURL != callbackURL {
		t.Fatalf("callback = %d %s; upstream=%s", callback.Code, callback.Body.String(), callbackBody)
	}
	invalid := do(t, h, http.MethodPost, "/api/codex/oauth/callback", `{"state":"state_1","redirect_url":"https://proxy.example/callback?code=x&state=state_1"}`, cookie)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid callback = %d %s", invalid.Code, invalid.Body.String())
	}
	cancel := do(t, h, http.MethodPost, "/api/codex/oauth/cancel", `{"state":"state_1"}`, cookie)
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", cancel.Code, cancel.Body.String())
	}
}

// The sidecar's mutation endpoints take a bare file name for any provider, so
// each channel's panel must refuse accounts that belong to another channel.
func TestGeminiPanelIsScopedToGeminiAccounts(t *testing.T) {
	var deleted, started []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			if r.Method == http.MethodDelete {
				deleted = append(deleted, r.URL.Query().Get("name"))
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": "codex-owner.json", "type": "codex", "email": "owner@example.com"},
				{"name": "antigravity-g@example.com.json", "type": "antigravity", "email": "g@example.com", "access_token": "must-not-leak"},
			}})
		case "/v0/management/antigravity-auth-url", "/v0/management/codex-auth-url":
			started = append(started, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "url": "https://accounts.google.com/o/oauth2/v2/auth", "state": "state_g"})
		case "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 403, "body": "{}"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	client, err := codexgateway.New(codexgateway.Config{BaseURL: upstream.URL, APIKey: "api-key", ManagementKey: "management-key"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "gemini.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := codexgateway.ReconcileCredential(t.Context(), db, client); err != nil {
		t.Fatal(err)
	}
	h := NewWithCodex(db, nil, testPassword, false, client)
	cookie := loginCookie(t, h)

	credentials := do(t, h, http.MethodGet, "/api/credentials", "", cookie)
	if strings.Contains(credentials.Body.String(), codexgateway.GeminiChannel.CredentialID) {
		t.Fatalf("gemini gateway leaked into credentials: %s", credentials.Body.String())
	}

	accounts := do(t, h, http.MethodGet, "/api/gemini/accounts", "", cookie)
	body := accounts.Body.String()
	if accounts.Code != http.StatusOK || !strings.Contains(body, "g@example.com") || strings.Contains(body, "owner@example.com") || strings.Contains(body, "must-not-leak") {
		t.Fatalf("gemini accounts = %d %s", accounts.Code, body)
	}

	cross := do(t, h, http.MethodPost, "/api/gemini/accounts/delete", `{"name":"codex-owner.json"}`, cookie)
	if cross.Code != http.StatusNotFound || len(deleted) != 0 {
		t.Fatalf("deleting a Codex account via the Gemini panel = %d (deleted %v)", cross.Code, deleted)
	}
	own := do(t, h, http.MethodPost, "/api/gemini/accounts/delete", `{"name":"antigravity-g@example.com.json"}`, cookie)
	if own.Code != http.StatusOK || len(deleted) != 1 {
		t.Fatalf("deleting own account = %d %s (deleted %v)", own.Code, own.Body.String(), deleted)
	}

	start := do(t, h, http.MethodPost, "/api/gemini/oauth/start", "", cookie)
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), "localhost:51121/oauth-callback") ||
		!strings.Contains(start.Body.String(), "127.0.0.1:8317/antigravity/callback") ||
		len(started) != 1 || started[0] != "/v0/management/antigravity-auth-url" {
		t.Fatalf("gemini oauth start = %d %s (upstream %v)", start.Code, start.Body.String(), started)
	}
}
