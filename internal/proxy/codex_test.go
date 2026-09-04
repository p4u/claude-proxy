package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

func codexProxySetup(t *testing.T, upstream http.HandlerFunc) *Handler {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "codex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := creds.InsertKey(context.Background(), db, provider.Codex, "Codex gateway", "oauth-sidecar", "sidecar-key", srv.URL, 1); err != nil {
		t.Fatal(err)
	}
	return New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCodexRequestUsesGatewayAndWireModel(t *testing.T) {
	var gotModel, gotAuth string
	h := codexProxySetup(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"gpt-5.6-codex","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	w := postMessages(t, h, "claude-gpt-5.6-codex")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotModel != "gpt-5.6-codex" || gotAuth != "Bearer sidecar-key" {
		t.Fatalf("wire model/auth = %q / %q", gotModel, gotAuth)
	}
}

func TestCodexModelsAreAdvertisedForClaudeCode(t *testing.T) {
	h := codexProxySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-5.6-codex","display_name":"GPT-5.6 Codex","type":"model"}]}`)
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"claude-gpt-5.6-codex"`) {
		t.Fatalf("models = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"id":"gpt-5.6-codex"`) {
		t.Fatalf("bare GPT model would be hidden by Claude Code: %s", w.Body.String())
	}
}
