package codexgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

func TestAccountsAreSanitizedAndCodexOnly(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
			{"name": "codex-a.json", "type": "codex", "email": "owner@example.com", "id_token": map[string]any{"secretish": "claim"}},
			{"name": "other.json", "type": "gemini", "email": "skip@example.com"},
		}})
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, APIKey: "api", ManagementKey: "management"})
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer management" {
		t.Fatalf("management Authorization = %q", gotAuth)
	}
	if len(accounts) != 1 || accounts[0].Name != "codex-a.json" || accounts[0].Email != "owner@example.com" {
		t.Fatalf("accounts = %#v", accounts)
	}
	raw, _ := json.Marshal(accounts)
	if string(raw) == "" || contains(string(raw), "secretish") {
		t.Fatalf("raw upstream claims leaked: %s", raw)
	}
}

func TestReconcileCredential(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, _ := New(Config{BaseURL: "http://sidecar:8317", APIKey: "internal-key", ManagementKey: "management"})
	if err := ReconcileCredential(context.Background(), db, c); err != nil {
		t.Fatal(err)
	}
	got, err := creds.Get(context.Background(), db, GatewayCredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != provider.Codex || got.AccessToken != "internal-key" || got.BaseURL != "http://sidecar:8317" {
		t.Fatalf("gateway credential = %#v", got)
	}
}

func TestSetWeight(t *testing.T) {
	var method, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, APIKey: "api", ManagementKey: "management"})
	if err := c.SetWeight(context.Background(), "owner.json", 7); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || path != "/v0/management/auth-files/fields" || body["name"] != "owner.json" || body["weight"] != float64(7) {
		t.Fatalf("request = %s %s %#v", method, path, body)
	}
	for _, invalid := range []int64{0, 1_000_001} {
		if err := c.SetWeight(context.Background(), "owner.json", invalid); err == nil {
			t.Errorf("SetWeight(%d) succeeded", invalid)
		}
	}
}

func contains(s, want string) bool {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
