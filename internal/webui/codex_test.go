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

func TestValidateCodexRedirect(t *testing.T) {
	for _, good := range []string{
		"http://localhost:1455/auth/callback?code=abc&state=state_1",
		"http://127.0.0.1:8317/codex/callback?code=abc&scope=openid+offline_access&state=state_1",
	} {
		if err := validateCodexRedirect(good, "state_1"); err != nil {
			t.Errorf("valid redirect %q: %v", good, err)
		}
	}
	for _, raw := range []string{
		"https://proxy.example/auth/callback?code=abc&state=state_1",
		"http://localhost:1455/auth/callback?code=abc&state=wrong",
		"http://localhost:1455/other?code=abc&state=state_1",
		"http://localhost:1455/auth/callback?state=state_1",
		"http://127.0.0.1:8317/anthropic/callback?code=abc&state=state_1",
		"http://localhost:8317/codex/callback?code=abc&state=state_1",
	} {
		if err := validateCodexRedirect(raw, "state_1"); err == nil {
			t.Errorf("validateCodexRedirect(%q) succeeded", raw)
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
	if weight.Code != http.StatusOK || !strings.Contains(weightBody, `"name":"owner.json"`) || !strings.Contains(weightBody, `"weight":7`) {
		t.Fatalf("weight = %d %s; upstream=%s", weight.Code, weight.Body.String(), weightBody)
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
