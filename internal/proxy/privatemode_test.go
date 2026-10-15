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
	"regexp"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// privatemodeSetup fronts a fake privatemode-proxy sidecar with one key.
func privatemodeSetup(t *testing.T, sidecar http.HandlerFunc) (*Handler, *store.DB, string) {
	t.Helper()
	srv := httptest.NewServer(sidecar)
	t.Cleanup(srv.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "pm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := creds.InsertKey(context.Background(), db, provider.Privatemode, "pm", "", "pm-key", srv.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	return New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil))), db, c.ID
}

func postAsUser(t *testing.T, h *Handler, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(usertoken.WithIdentity(req.Context(),
		&usertoken.Identity{UserTokenID: user, FullCapture: true}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The live /v1/models lists embedding, speech and OCR models next to the chat
// ones; only chat models are advertised, under the claude-pm- alias, with the
// window each family really has.
func TestPrivatemodeCatalogue(t *testing.T) {
	h, _, _ := privatemodeSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer pm-key" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
		 {"id":"glm-5.3","object":"model","tasks":["generate","tool_calling"]},
		 {"id":"gpt-oss-120b","object":"model","tasks":["generate","tool_calling"]},
		 {"id":"deepseek-ocr-2","object":"model","tasks":["generate","vision"]},
		 {"id":"qwen3-embedding-4b","object":"model","tasks":["embed"]},
		 {"id":"whisper-large-v3","object":"model","tasks":["transcribe"]}]}`)
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("models = %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			MaxInputTokens int64  `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, m := range env.Data {
		got[m.ID] = m.MaxInputTokens
	}
	want := map[string]int64{
		"claude-pm-glm-5.3[1m]":  1_048_576,
		"claude-pm-glm-5.3":      1_048_576,
		"claude-pm-gpt-oss-120b": 131_072,
	}
	if len(got) != len(want) {
		t.Fatalf("advertised %v, want %v", got, want)
	}
	for id, n := range want {
		if got[id] != n {
			t.Errorf("%s: max_input_tokens = %d, want %d (all: %v)", id, got[id], n, got)
		}
	}
}

// Requests reach the sidecar under the native name with a per-user cache salt,
// and nothing about them is stored, even for a user opted into full capture.
func TestPrivatemodeMessages(t *testing.T) {
	var bodies []map[string]any
	h, db, _ := privatemodeSetup(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-5.3","content":[{"type":"text","text":"secret reply"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	})
	h.PromptRetentionDays = 7
	// Real tokens: prompt_log and request_log reference user_tokens, so an
	// unknown user ID would fail those inserts and pass this test vacuously.
	ctx := context.Background()
	alice, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := usertoken.Create(ctx, db, "bob")
	if err != nil {
		t.Fatal(err)
	}

	msg := `{"model":"claude-pm-glm-5.3","max_tokens":8,"messages":[{"role":"user","content":"confidential prompt"}]}`
	for _, user := range []string{alice.ID, alice.ID, bob.ID} {
		if w := postAsUser(t, h, user, msg); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", user, w.Code, w.Body.String())
		}
	}
	own := `{"model":"claude-pm-glm-5.3","max_tokens":8,"cache_salt":"client-chosen-salt-of-at-least-32-chars","messages":[{"role":"user","content":"x"}]}`
	if w := postAsUser(t, h, alice.ID, own); w.Code != http.StatusOK {
		t.Fatalf("own salt: %d %s", w.Code, w.Body.String())
	}

	if bodies[0]["model"] != "glm-5.3" {
		t.Fatalf("wire model = %v, want the native glm-5.3", bodies[0]["model"])
	}
	salt := func(i int) string { s, _ := bodies[i]["cache_salt"].(string); return s }
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(salt(0)) {
		t.Fatalf("cache_salt = %q, want 64 hex chars", salt(0))
	}
	if salt(0) != salt(1) {
		t.Fatal("one user's requests must share a salt, or nothing is ever cached")
	}
	if salt(0) == salt(2) {
		t.Fatal("two users share a salt, so one could probe the other's prompt cache")
	}
	if salt(3) != "client-chosen-salt-of-at-least-32-chars" {
		t.Fatalf("a client's own salt was replaced: %q", salt(3))
	}

	for _, table := range []string{"prompt_log", "conversation_message"} {
		var n int
		if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows; Privatemode prompts must never be stored", table, n)
		}
	}
	var logged, tokens int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*), COALESCE(SUM(output_tokens),0) FROM request_log").Scan(&logged, &tokens); err != nil {
		t.Fatal(err)
	}
	if logged != 4 || tokens != 8 {
		t.Errorf("request_log = %d rows / %d output tokens, want 4 / 8: usage is still metered", logged, tokens)
	}
}

// A sidecar 401 that never judged the key (attestation or network failure)
// must leave the credential alone; a 401 that rejects the key revokes it.
func TestPrivatemode401(t *testing.T) {
	reply := `{"error":{"message":"trying API key: setting secrets: dial tcp: lookup api.privatemode.ai: no such host","type":""}}`
	h, db, id := privatemodeSetup(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, reply)
	})
	msg := `{"model":"claude-pm-glm-5.3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	status := func() creds.Status {
		c, err := creds.Get(context.Background(), db, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.Status
	}

	w := postAsUser(t, h, "alice", msg)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "confidential deployment") {
		t.Fatalf("gateway failure = %d %s", w.Code, w.Body.String())
	}
	if s := status(); s != creds.StatusActive {
		t.Fatalf("status after a gateway failure = %q, want active", s)
	}

	reply = `{"error":{"message":"trying API key: setting secrets: doing attest request: Unauthorized (status code: 401)","type":""}}`
	postAsUser(t, h, "alice", msg)
	if s := status(); s != creds.StatusRevoked {
		t.Fatalf("status after a rejected key = %q, want revoked", s)
	}
}
