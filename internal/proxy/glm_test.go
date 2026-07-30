package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// hostRewriter routes upstream calls to per-host stub servers, so a test can
// tell "went to Anthropic" from "went to Z.AI" — which a single catch-all
// httptest server (as withUpstream installs) cannot distinguish.
type hostRewriter struct {
	byHost map[string]*url.URL
	seen   *[]string
}

func (h *hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	target, ok := h.byHost[req.URL.Host]
	if !ok {
		return nil, fmt.Errorf("unexpected upstream host %q", req.URL.Host)
	}
	if h.seen != nil {
		*h.seen = append(*h.seen, req.URL.Host+req.URL.Path)
	}
	req.URL.Scheme, req.URL.Host, req.Host = target.Scheme, target.Host, target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func glmSetup(t *testing.T, anthropicH, glmH http.HandlerFunc) (*Handler, *store.DB, *[]string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	if anthropicH != nil {
		if _, err := creds.Insert(ctx, db, "anth", "max", "sk-ant-oat-x", "rt-x", time.Now().Add(time.Hour), 1); err != nil {
			t.Fatal(err)
		}
	}
	if glmH != nil {
		if _, err := creds.InsertKey(ctx, db, provider.GLM, "zai", "pro", "zai-key", 1); err != nil {
			t.Fatal(err)
		}
	}

	byHost := map[string]*url.URL{}
	mk := func(h http.HandlerFunc, base string) {
		if h == nil {
			return
		}
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		u, _ := url.Parse(ts.URL)
		bu, _ := url.Parse(base)
		byHost[bu.Host] = u
	}
	mk(anthropicH, provider.Get(provider.Anthropic).BaseURL)
	mk(glmH, provider.Get(provider.GLM).BaseURL)

	seen := []string{}
	h := New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.client = &http.Client{Transport: &hostRewriter{byHost: byHost, seen: &seen}}
	return h, db, &seen
}

func postMessages(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, model)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func okJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	fmt.Fprint(w, `{"ok":true}`)
}

// The core promise: the model the client picked decides the upstream.
func TestModelRoutesToItsProvider(t *testing.T) {
	h, _, seen := glmSetup(t, okJSON, okJSON)

	if got := postMessages(t, h, "glm-4.7").Code; got != 200 {
		t.Fatalf("glm request status = %d, want 200", got)
	}
	if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], "api.z.ai/api/anthropic/") {
		t.Fatalf("glm-4.7 did not reach Z.AI, upstream hits: %v", *seen)
	}

	if got := postMessages(t, h, "claude-sonnet-5").Code; got != 200 {
		t.Fatalf("claude request status = %d, want 200", got)
	}
	if len(*seen) != 2 || !strings.HasPrefix((*seen)[1], "api.anthropic.com/") {
		t.Fatalf("claude-sonnet-5 did not reach Anthropic, upstream hits: %v", *seen)
	}
}

// GLM's base URL carries a path prefix; forwarding must join onto it rather
// than swap the host, or every request 404s.
func TestGLMBaseURLPathPrefixPreserved(t *testing.T) {
	h, _, seen := glmSetup(t, nil, okJSON)
	postMessages(t, h, "glm-4.7")
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "/api/anthropic/v1/messages") {
		t.Fatalf("upstream path lost the /api/anthropic prefix: %v", *seen)
	}
}

// A model whose provider has no credential must fail clearly, naming the
// provider — not silently borrow the other provider's credential.
func TestNoCredentialForProviderFailsClearly(t *testing.T) {
	h, _, seen := glmSetup(t, okJSON, nil) // Anthropic only

	w := postMessages(t, h, "glm-4.7")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("X-Router-Reason"); got != "provider-unavailable" {
		t.Errorf("X-Router-Reason = %q, want provider-unavailable", got)
	}
	if !strings.Contains(w.Body.String(), "GLM") {
		t.Errorf("error should name the missing provider, got: %s", w.Body.String())
	}
	if len(*seen) != 0 {
		t.Errorf("no request should have been forwarded, got: %v", *seen)
	}
}

// Discovery advertises exactly the providers that can currently serve traffic.
func TestModelsMergesOnlyUsableProviders(t *testing.T) {
	anthropic := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5","display_name":"Sonnet 5","type":"model"}],"has_more":false}`)
	}
	// Z.AI's real envelope uses camelCase pagination keys.
	glm := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"glm-4.7","display_name":"GLM-4.7","type":"model"}],"firstId":"glm-4.7","lastId":"glm-4.7","hasMore":false}`)
	}

	ids := func(t *testing.T, h *Handler) []string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
		if w.Code != 200 {
			t.Fatalf("models status = %d: %s", w.Code, w.Body.String())
		}
		var env struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode models: %v (%s)", err, w.Body.String())
		}
		out := make([]string, len(env.Data))
		for i, e := range env.Data {
			out[i] = e.ID
		}
		return out
	}

	t.Run("both providers", func(t *testing.T) {
		h, _, _ := glmSetup(t, anthropic, glm)
		got := strings.Join(ids(t, h), ",")
		for _, want := range []string{"claude-sonnet-5", "glm-4.7"} {
			if !strings.Contains(got, want) {
				t.Errorf("merged list missing %q: %s", want, got)
			}
		}
		// [1m] is an Anthropic-only alias — Z.AI 400s on the suffixed form.
		if !strings.Contains(got, "claude-sonnet-5[1m]") {
			t.Errorf("anthropic entries should be [1m]-augmented: %s", got)
		}
		if strings.Contains(got, "glm-4.7[1m]") {
			t.Errorf("GLM entries must not be [1m]-augmented: %s", got)
		}
	})

	t.Run("glm absent", func(t *testing.T) {
		h, _, _ := glmSetup(t, anthropic, nil)
		if got := strings.Join(ids(t, h), ","); strings.Contains(got, "glm") {
			t.Errorf("no GLM credential, so no GLM models should be offered: %s", got)
		}
	})

	t.Run("anthropic absent", func(t *testing.T) {
		h, _, _ := glmSetup(t, nil, glm)
		got := strings.Join(ids(t, h), ",")
		if !strings.Contains(got, "glm-4.7") {
			t.Errorf("GLM-only deployment should still offer GLM models: %s", got)
		}
		if strings.Contains(got, "claude") {
			t.Errorf("no Anthropic credential, so no Claude models should be offered: %s", got)
		}
	})

	// One provider failing degrades the list rather than failing discovery.
	t.Run("one provider down", func(t *testing.T) {
		down := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
		h, _, _ := glmSetup(t, anthropic, down)
		got := strings.Join(ids(t, h), ",")
		if !strings.Contains(got, "claude-sonnet-5") {
			t.Errorf("healthy provider should still be listed: %s", got)
		}
		if strings.Contains(got, "glm") {
			t.Errorf("failed provider must not contribute rows: %s", got)
		}
	})
}

// A 401 on a static key is a bad key, not a stale token: it must not trigger an
// OAuth refresh (which would burn a second upstream request and report a
// misleading failure).
func TestGLM401DoesNotAttemptRefresh(t *testing.T) {
	hits := 0
	glm := func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}
	h, db, _ := glmSetup(t, nil, glm)

	postMessages(t, h, "glm-4.7")
	if hits != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1 (no refresh-and-retry)", hits)
	}
	list, err := creds.List(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Status != creds.StatusRevoked {
		t.Errorf("status = %q, want revoked so the operator sees the key needs replacing", list[0].Status)
	}
}

// A session that mixes providers must keep one stable binding per provider,
// not migrate the pin back and forth on every alternating request.
func TestProviderQualifiedStickiness(t *testing.T) {
	h, db, _ := glmSetup(t, okJSON, okJSON)
	ctx := context.Background()

	for range 3 {
		postMessages(t, h, "glm-4.7")
		postMessages(t, h, "claude-sonnet-5")
	}

	rows, err := db.QueryContext(ctx, `SELECT id, request_count FROM conversations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		counts[id] = n
	}
	if len(counts) != 2 {
		t.Fatalf("want 2 conversation rows (one per provider), got %d: %v", len(counts), counts)
	}
	for id, n := range counts {
		if n != 3 {
			t.Errorf("conversation %q saw %d requests, want 3 — the binding was not sticky", id, n)
		}
	}
	var glmKeys int
	for id := range counts {
		if strings.HasPrefix(id, string(provider.GLM)+":") {
			glmKeys++
		}
	}
	if glmKeys != 1 {
		t.Errorf("expected exactly one glm-qualified conversation key, got %d: %v", glmKeys, counts)
	}
}

func TestRequestModelAndProviderFor(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		want                     provider.ID
	}{
		{"glm messages", "POST", "/v1/messages", `{"model":"glm-4.7"}`, provider.GLM},
		{"claude messages", "POST", "/v1/messages", `{"model":"claude-sonnet-5"}`, provider.Anthropic},
		{"glm count_tokens", "POST", "/v1/messages/count_tokens", `{"model":"glm-5.2"}`, provider.GLM},
		{"no model", "POST", "/v1/messages", `{"max_tokens":8}`, provider.Anthropic},
		{"malformed body", "POST", "/v1/messages", `not json`, provider.Anthropic},
		{"empty body", "POST", "/v1/messages", ``, provider.Anthropic},
		{"models discovery", "GET", "/v1/models", ``, provider.Anthropic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if got := providerFor(r, []byte(tc.body)); got != tc.want {
				t.Errorf("providerFor = %q, want %q", got, tc.want)
			}
		})
	}
}
