package claudioapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/plugins"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

func getPlugins(t *testing.T, mux http.Handler, id *usertoken.Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/claudio/plugins", nil)
	if id != nil {
		req = withIdentity(req, id)
	}
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	return rw
}

func TestPluginsRequireUserToken(t *testing.T) {
	mux, _ := setupHandler(t, &fakeCatalogue{})
	for name, id := range map[string]*usertoken.Identity{
		"anonymous":  nil,
		"admin":      {IsAdmin: true},
		"empty user": {UserTokenID: ""},
	} {
		if rw := getPlugins(t, mux, id); rw.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403; body=%s", name, rw.Code, rw.Body.String())
		}
	}
}

func TestPluginsServesEnabledWithEnv(t *testing.T) {
	mux, db := setupHandler(t, &fakeCatalogue{})
	ctx := context.Background()
	user := &usertoken.Identity{UserTokenID: "utok_a", UserName: "alice"}

	rw := getPlugins(t, mux, user)
	if rw.Code != http.StatusOK || strings.TrimSpace(rw.Body.String()) != `{"plugins":[]}` {
		t.Fatalf("empty: %d %s", rw.Code, rw.Body.String())
	}

	sha := strings.Repeat("0f", 32)
	if err := plugins.Set(ctx, db, "vocdoni-internal", plugins.Meta{
		Version: "1.4.0", BundleURL: "https://example.org/vi.zip", BundleSHA256: sha,
		InstalledID: "vocdoni-internal@vocdoni",
	}); err != nil {
		t.Fatal(err)
	}
	if err := plugins.SetEnv(ctx, db, "vocdoni-internal", map[string]string{"VOCDONI_KB_TOKEN": "kb-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Set(ctx, db, "off", plugins.Meta{Version: "1", BundleURL: "https://example.org/off.zip", BundleSHA256: sha}); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Enable(ctx, db, "off", false); err != nil {
		t.Fatal(err)
	}

	rw = getPlugins(t, mux, user)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d %s", rw.Code, rw.Body.String())
	}
	if cc := rw.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	var body struct {
		Plugins []struct {
			Name   string `json:"name"`
			Ver    string `json:"version"`
			Bundle struct {
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
			} `json:"bundle"`
			InstalledID string            `json:"installed_id"`
			Env         map[string]string `json:"env"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 1 {
		t.Fatalf("plugins = %+v (disabled one must be omitted)", body.Plugins)
	}
	p := body.Plugins[0]
	if p.Name != "vocdoni-internal" || p.Ver != "1.4.0" || p.Bundle.URL != "https://example.org/vi.zip" ||
		p.Bundle.SHA256 != sha || p.InstalledID != "vocdoni-internal@vocdoni" || p.Env["VOCDONI_KB_TOKEN"] != "kb-secret" {
		t.Fatalf("plugin = %+v", p)
	}
}

func TestPluginsWrongMethod(t *testing.T) {
	mux, _ := setupHandler(t, &fakeCatalogue{})
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/claudio/plugins", nil),
		&usertoken.Identity{UserTokenID: "utok_a"})
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rw.Code)
	}
}
