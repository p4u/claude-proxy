package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/plugins"
)

func TestPluginsAPIRoundTrip(t *testing.T) {
	db, h := newTestServer(t)
	c := loginCookie(t, h)
	ctx := context.Background()
	sha := strings.Repeat("cd", 32)

	meta := `{"version":"1.4.0","bundle_url":"https://example.org/vi.zip","bundle_sha256":"` + sha +
		`","installed_id":"vocdoni-internal@vocdoni"}`
	if w := do(t, h, http.MethodPut, "/api/plugins/vocdoni-internal", meta, c); w.Code != http.StatusOK {
		t.Fatalf("PUT meta: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodPut, "/api/plugins/vocdoni-internal/env",
		`{"VOCDONI_KB_TOKEN":"kb-secret","NOTION_TOKEN":"notion-secret"}`, c)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("PUT env: %d %s", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodGet, "/api/plugins", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "kb-secret") || strings.Contains(w.Body.String(), "notion-secret") {
		t.Fatalf("list shows a value: %s", w.Body.String())
	}
	var list struct {
		Plugins []struct {
			Name         string   `json:"name"`
			Version      string   `json:"version"`
			BundleURL    string   `json:"bundle_url"`
			BundleSHA256 string   `json:"bundle_sha256"`
			InstalledID  string   `json:"installed_id"`
			Enabled      bool     `json:"enabled"`
			UpdatedAt    string   `json:"updated_at"`
			EnvKeys      []string `json:"env_keys"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Plugins) != 1 {
		t.Fatalf("plugins = %+v", list.Plugins)
	}
	p := list.Plugins[0]
	if p.Name != "vocdoni-internal" || p.Version != "1.4.0" || p.BundleURL != "https://example.org/vi.zip" ||
		p.BundleSHA256 != sha || p.InstalledID != "vocdoni-internal@vocdoni" || !p.Enabled || p.UpdatedAt == "" ||
		strings.Join(p.EnvKeys, ",") != "NOTION_TOKEN,VOCDONI_KB_TOKEN" {
		t.Fatalf("plugin = %+v", p)
	}

	// Disable hides it from claudio (GET /v1/claudio/plugins serves ListForClient).
	if w := do(t, h, http.MethodPost, "/api/plugins/vocdoni-internal/disable", "", c); w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if served, _ := plugins.ListForClient(ctx, db); len(served) != 0 {
		t.Fatalf("disabled plugin served: %+v", served)
	}
	if w := do(t, h, http.MethodPost, "/api/plugins/vocdoni-internal/enable", "", c); w.Code != http.StatusOK {
		t.Fatalf("enable: %d", w.Code)
	}
	served, _ := plugins.ListForClient(ctx, db)
	if len(served) != 1 || served[0].Env["VOCDONI_KB_TOKEN"] != "kb-secret" {
		t.Fatalf("served after enable: %+v", served)
	}

	// Replacing the metadata keeps the env.
	if w := do(t, h, http.MethodPut, "/api/plugins/vocdoni-internal", strings.Replace(meta, "1.4.0", "1.5.0", 1), c); w.Code != http.StatusOK {
		t.Fatalf("PUT meta again: %d", w.Code)
	}
	if got, _ := plugins.Get(ctx, db, "vocdoni-internal"); got.Version != "1.5.0" || len(got.EnvKeys) != 2 {
		t.Fatalf("after replace: %+v", got)
	}

	if w := do(t, h, http.MethodDelete, "/api/plugins/vocdoni-internal/env/NOTION_TOKEN", "", c); w.Code != http.StatusOK {
		t.Fatalf("DELETE key: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodDelete, "/api/plugins/vocdoni-internal/env/NOTION_TOKEN", "", c); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE key twice: %d", w.Code)
	}
	if w := do(t, h, http.MethodDelete, "/api/plugins/vocdoni-internal", "", c); w.Code != http.StatusOK {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/api/plugins/vocdoni-internal/enable", "", c); w.Code != http.StatusNotFound {
		t.Fatalf("enable deleted: %d", w.Code)
	}
}

func TestPluginsAPIValidation(t *testing.T) {
	_, h := newTestServer(t)
	c := loginCookie(t, h)
	sha := strings.Repeat("cd", 32)
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{http.MethodPut, "/api/plugins/vocdoni-internal", `{"version":"1","bundle_url":"http://x.test/a.zip","bundle_sha256":"` + sha + `"}`, 400},
		{http.MethodPut, "/api/plugins/Bad_Name", `{"version":"1","bundle_url":"https://x.test/a.zip","bundle_sha256":"` + sha + `"}`, 400},
		{http.MethodPut, "/api/plugins/vocdoni-internal", `{"version":"1"`, 400},
		{http.MethodPut, "/api/plugins/missing/env", `{"A":"1"}`, 404},
		{http.MethodPut, "/api/plugins/missing/env", `{"lower":"1"}`, 400},
		{http.MethodPut, "/api/plugins/missing/env", `{"A":1}`, 400},
		{http.MethodGet, "/api/plugins/vocdoni-internal/env", "", 404},
		{http.MethodPost, "/api/plugins/vocdoni-internal/bogus", "", 404},
	} {
		w := do(t, h, tc.method, tc.path, tc.body, c)
		if w.Code != tc.code {
			t.Errorf("%s %s: %d %s, want %d", tc.method, tc.path, w.Code, w.Body.String(), tc.code)
		}
		if tc.code == 400 && !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s %s: 400 without an error body: %s", tc.method, tc.path, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodGet, "/api/plugins", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
}
