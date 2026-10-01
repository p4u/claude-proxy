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

// A 401 from the Codex sidecar is relayed but must not revoke the gateway
// credential. It happened in production: a transient OpenAI-side auth failure
// made the sidecar answer one request with 401, the proxy revoked
// gateway_codex, and every later Codex request got 503 — while all four
// accounts behind the sidecar were healthy — until the container restarted.
func TestCodex401DoesNotRevokeGateway(t *testing.T) {
	hits := 0
	h := codexProxySetup(t, func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		if hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"Incorrect API key provided"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	w := postMessages(t, h, "claude-gpt-6-astra")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Incorrect API key") {
		t.Fatalf("sidecar 401 not relayed as-is: %d %s", w.Code, w.Body.String())
	}
	list, err := creds.List(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Status != creds.StatusActive {
		t.Fatalf("gateway credential status = %q after a sidecar 401, want active", list[0].Status)
	}

	if w := postMessages(t, h, "claude-gpt-6-astra"); w.Code != http.StatusOK {
		t.Fatalf("follow-up request = %d %s, want 200 (provider must stay reachable)", w.Code, w.Body.String())
	}
	if hits != 2 {
		t.Fatalf("sidecar hit %d times, want 2 (no refresh-and-retry on 401)", hits)
	}
}

// The sidecar's /v1/models is the union of every signed-in account. This is
// the live shape after an Antigravity account was added on 2026-10-01: its
// catalogue carries claude-* and gpt-oss-* models next to the Gemini ones.
const sharedSidecarCatalogue = `{"data":[
 {"id":"gpt-5.6-luna","object":"model","owned_by":"openai"},
 {"id":"codex-auto-review","object":"model","owned_by":"openai"},
 {"id":"gemini-pro-agent","object":"model","owned_by":"antigravity"},
 {"id":"gemini-3.8-flash-high","object":"model","owned_by":"antigravity"},
 {"id":"gemini-3.1-flash-image","object":"model","owned_by":"antigravity"},
 {"id":"gpt-oss-120b-medium","object":"model","owned_by":"antigravity"},
 {"id":"claude-sonnet-4-6","object":"model","owned_by":"antigravity"}
]}`

func TestSidecarCatalogueIsSplitPerProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sharedSidecarCatalogue)
	}))
	t.Cleanup(srv.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "sidecar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for _, p := range []provider.ID{provider.Codex, provider.Gemini} {
		if _, err := creds.InsertKey(ctx, db, p, string(p)+" gateway", "oauth-sidecar", "sidecar-key", srv.URL, 1); err != nil {
			t.Fatal(err)
		}
	}
	h := New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil)))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("models = %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range env.Data {
		got[m.ID] = m.DisplayName
	}
	want := map[string]string{
		"claude-gpt-5.6-luna":              "gpt-5.6-luna (OpenAI Codex)",
		"claude-gemini-pro-agent":          "gemini-pro-agent (Google Gemini)",
		"claude-gemini-pro-agent[1m]":      "gemini-pro-agent (1M context) (Google Gemini)",
		"claude-gemini-3.8-flash-high":     "gemini-3.8-flash-high (Google Gemini)",
		"claude-gemini-3.8-flash-high[1m]": "gemini-3.8-flash-high (1M context) (Google Gemini)",
	}
	for id, name := range want {
		if got[id] != name {
			t.Errorf("%s: display_name = %q, want %q", id, got[id], name)
		}
	}
	for _, id := range []string{
		"claude-gpt-oss-120b-medium",    // Antigravity's, but would route to Codex
		"claude-claude-sonnet-4-6",      // Antigravity's Claude
		"claude-sonnet-4-6",             // ditto, under any spelling
		"claude-gemini-3.1-flash-image", // image generation, not a chat model
		"claude-codex-auto-review",      // owned by openai, but unroutable (no gpt- prefix)
	} {
		if _, ok := got[id]; ok {
			t.Errorf("%s must not be advertised", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("advertised %d models, want %d: %v", len(got), len(want), got)
	}
}

func TestGeminiRequestUsesGatewayAndWireModel(t *testing.T) {
	var gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"model":"gemini-pro-agent","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "gemini.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := creds.InsertKey(context.Background(), db, provider.Gemini, "Gemini gateway", "oauth-sidecar", "sidecar-key", srv.URL, 1); err != nil {
		t.Fatal(err)
	}
	h := New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil)))

	w := postMessages(t, h, "claude-gemini-pro-agent")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotModel != "gemini-pro-agent" || gotAuth != "Bearer sidecar-key" {
		t.Fatalf("wire model/auth = %q / %q", gotModel, gotAuth)
	}
}
