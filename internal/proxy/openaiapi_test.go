package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

func TestOpenAIRoute(t *testing.T) {
	list := []*creds.Credential{{ID: "c1", Provider: provider.CustomOpenAI, Models: []creds.Model{{ID: "Qwen3-Coder"}}}}
	for _, tc := range []struct {
		model, wantProv, wantWire string
		ok                        bool
	}{
		{"privatemode/glm-5.3", "privatemode", "glm-5.3", true},
		{"claude-pm-glm-5.3", "privatemode", "glm-5.3", true}, // Claude Code's alias works too
		{"codex/gpt-5.5", "codex", "gpt-5.5", true},
		{"claude-gpt-5.5", "codex", "gpt-5.5", true},
		{"gemini/gemini-3.8-flash-high", "gemini", "gemini-3.8-flash-high", true},
		{"custom_openai/qwen3-coder", "custom_openai", "Qwen3-Coder", true},
		{"claude-openai-Qwen3-Coder", "custom_openai", "Qwen3-Coder", true},
		// Providers that speak only Anthropic Messages are not offered: this
		// surface does not translate.
		{"anthropic/claude-opus-5", "", "", false},
		{"claude-opus-5", "", "", false},
		{"glm/glm-4.7", "", "", false},
		{"nonsense/x", "", "", false},
		{"custom_openai/unknown", "", "", false},
	} {
		p, wire, _, ok := openAIRoute(list, tc.model)
		if ok != tc.ok || string(p) != tc.wantProv || wire != tc.wantWire {
			t.Errorf("openAIRoute(%q) = %q %q %v, want %q %q %v", tc.model, p, wire, ok, tc.wantProv, tc.wantWire, tc.ok)
		}
	}
}

func TestWithStreamUsage(t *testing.T) {
	got := string(withStreamUsage([]byte(`{"model":"m","stream":true,"messages":[]}`)))
	if !strings.Contains(got, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("streaming request without usage option: %s", got)
	}
	for _, in := range []string{
		`{"model":"m","messages":[]}`,
		`{"model":"m","stream":false}`,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
	} {
		if got := string(withStreamUsage([]byte(in))); got != in {
			t.Errorf("withStreamUsage(%s) = %s, want unchanged", in, got)
		}
	}
}

// Chat Completions reach the Privatemode sidecar's encrypted
// /v1/chat/completions untranslated, salted per user, with usage requested and
// metered from the final chunk; the stream itself is relayed byte for byte.
func TestOpenAIChatThroughPrivatemode(t *testing.T) {
	const stream = "data: {\"id\":\"c1\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"glm-5.3\",\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":100}}}\n\n" +
		"data: [DONE]\n\n"
	var path, auth string
	var body map[string]any
	h, db, _ := privatemodeSetup(t, func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	})
	ctx := context.Background()
	alice, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, OpenAIBasePath+"/chat/completions",
		strings.NewReader(`{"model":"privatemode/glm-5.3","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req = req.WithContext(usertoken.WithIdentity(req.Context(), &usertoken.Identity{UserTokenID: alice.ID}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != stream {
		t.Fatalf("relayed %d %q", w.Code, w.Body.String())
	}
	if path != "/v1/chat/completions" || auth != "Bearer pm-key" {
		t.Fatalf("upstream path/auth = %s / %s", path, auth)
	}
	if body["model"] != "glm-5.3" {
		t.Fatalf("wire model = %v", body["model"])
	}
	if s, _ := body["cache_salt"].(string); len(s) != 64 {
		t.Fatalf("cache_salt = %q", s)
	}
	if opts, _ := body["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Fatalf("stream_options = %v; streamed usage would be lost", body["stream_options"])
	}

	var logPath string
	var in, out, cached int64
	if err := db.QueryRowContext(ctx, `SELECT path, input_tokens, output_tokens, cache_read_tokens FROM request_log`).
		Scan(&logPath, &in, &out, &cached); err != nil {
		t.Fatal(err)
	}
	if logPath != OpenAIBasePath+"/chat/completions" || in != 20 || out != 7 || cached != 100 {
		t.Fatalf("request_log = %s in=%d out=%d cached=%d, want in=20 (prompt minus cached) out=7 cached=100", logPath, in, out, cached)
	}
}

func TestOpenAIModelsAndErrors(t *testing.T) {
	h, _, _ := privatemodeSetup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
		 {"id":"glm-5.3","object":"model","tasks":["generate"]},
		 {"id":"gpt-oss-120b","object":"model","tasks":["generate"]},
		 {"id":"qwen3-embedding-4b","object":"model","tasks":["embed"]}]}`)
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, OpenAIBasePath+"/models", nil))
	var env struct {
		Object string `json:"object"`
		Data   []struct {
			ID            string `json:"id"`
			Object        string `json:"object"`
			OwnedBy       string `json:"owned_by"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	got := map[string]int64{}
	for _, m := range env.Data {
		if m.Object != "model" || m.OwnedBy != "privatemode" {
			t.Errorf("entry %+v", m)
		}
		got[m.ID] = m.ContextLength
	}
	if env.Object != "list" || len(got) != 2 || got["privatemode/glm-5.3"] != 1_048_576 || got["privatemode/gpt-oss-120b"] != 131_072 {
		t.Fatalf("models = %v (%s)", got, w.Body.String())
	}

	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, OpenAIBasePath + "/chat/completions", `{"model":"claude-opus-5","messages":[]}`, http.StatusNotFound},
		{http.MethodPost, OpenAIBasePath + "/chat/completions", `{"messages":[]}`, http.StatusBadRequest},
		{http.MethodPost, OpenAIBasePath + "/embeddings", `{"model":"privatemode/qwen3-embedding-4b"}`, http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != tc.status || e.Error.Message == "" {
			t.Errorf("%s %s = %d %s, want %d with an OpenAI error body", tc.method, tc.path, w.Code, w.Body.String(), tc.status)
		}
	}
}

// The OpenAI base path takes user tokens like /v1, and is never handed to the
// web UI's cookie auth when the UI is enabled.
func TestAuthMiddlewareCoversOpenAIPath(t *testing.T) {
	db := openDB(t)
	ut, err := usertoken.Create(context.Background(), db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var seen *usertoken.Identity
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = usertoken.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := AuthMiddleware("admin-secret", db, true, inner)

	req := httptest.NewRequest(http.MethodGet, OpenAIBasePath+"/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401 (must not fall through to the UI)", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, OpenAIBasePath+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+ut.Token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || seen == nil || seen.UserTokenID != ut.ID {
		t.Fatalf("user token = %d identity %+v", w.Code, seen)
	}
}
