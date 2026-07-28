package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

const suggestionBody = `{"model":"claude-y","stream":false,"metadata":{"user_id":"s1"},"messages":[
  {"role":"user","content":"fix the bug"},
  {"role":"assistant","content":"done"},
  {"role":"user","content":"[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]\n\nFIRST: Look at the user's recent messages."}
]}`

func TestIsSuggestionRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"real suggestion request", suggestionBody, true},
		{
			"content blocks form",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"[SUGGESTION MODE: Suggest what the user might type.]"}]}]}`,
			true,
		},
		{
			"leading whitespace still matches",
			`{"messages":[{"role":"user","content":"\n  [SUGGESTION MODE: x]"}]}`,
			true,
		},
		{
			// The case that must not misfire: a user asking ABOUT the
			// suggestion prompt, quoting it after their own words.
			"marker quoted mid-message",
			`{"messages":[{"role":"user","content":"why does the CLI send this?\n\n[SUGGESTION MODE: Suggest what the user might type.]"}]}`,
			false,
		},
		{"ordinary prompt", `{"messages":[{"role":"user","content":"hello"}]}`, false},
		{"last message is assistant", `{"messages":[{"role":"user","content":"[SUGGESTION MODE: x]"},{"role":"assistant","content":"hi"}]}`, false},
		{"no messages", `{"messages":[]}`, false},
		{"malformed", `not json`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSuggestionRequest([]byte(tc.body)); got != tc.want {
				t.Fatalf("isSuggestionRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// With the option on, the request must be answered locally: nothing reaches
// Anthropic, no credential is bound, and no prompt is stored.
func TestSuggestionBlockedNeverReachesUpstream(t *testing.T) {
	var upstreamHits atomic.Int32
	h, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(200)
	})
	h.PromptRetentionDays = 7 // capture on, so we can prove nothing was stored

	// A real row: request_log.user_token_id is a foreign key, so a synthetic ID
	// would make the log insert fail silently and hide what we assert below.
	ut, err := usertoken.Create(context.Background(), db, "alice")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	rw := postAs(t, h, &usertoken.Identity{
		UserTokenID: ut.ID, UserName: ut.Name, BlockSuggestions: true,
	}, suggestionBody)

	if n := upstreamHits.Load(); n != 0 {
		t.Fatalf("upstream was called %d times, want 0", n)
	}
	if got := rw.Header().Get("X-Router-Reason"); got != "suggestion-blocked" {
		t.Fatalf("X-Router-Reason = %q, want suggestion-blocked", got)
	}

	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, rw.Body.String())
	}
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Fatalf("malformed synthetic message: %+v", resp)
	}
	if resp.Model != "claude-y" {
		t.Fatalf("model = %q, want the requested claude-y", resp.Model)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "" {
		t.Fatalf("content = %+v, want a single empty text block", resp.Content)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
		t.Fatalf("usage = %+v, want zeroes (nothing was spent)", resp.Usage)
	}

	// No credential bound, no prompt stored.
	var convs, prompts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&convs); err != nil {
		t.Fatal(err)
	}
	if convs != 0 {
		t.Fatalf("conversations = %d, want 0 (no credential should be bound)", convs)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM prompt_log`).Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if prompts != 0 {
		t.Fatalf("prompt_log = %d, want 0 (blocked suggestions must not pollute captures)", prompts)
	}

	// The attempt is still recorded, with no credential and zero tokens, so the
	// saving stays visible in per-user stats.
	var logged, credID string
	var out int64
	if err := db.QueryRow(
		`SELECT COALESCE(user_token_id,''), credential_id, output_tokens FROM request_log`).
		Scan(&logged, &credID, &out); err != nil {
		t.Fatalf("expected one request_log row: %v", err)
	}
	if logged != ut.ID || credID != "" || out != 0 {
		t.Fatalf("request_log row = user %q cred %q out %d; want %s / empty / 0", logged, credID, out, ut.ID)
	}
}

// Streaming requests get an SSE reply, since that is what the client parses.
func TestSuggestionBlockedStreamingShape(t *testing.T) {
	h, _, _, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	})
	body := strings.Replace(suggestionBody, `"stream":false`, `"stream":true`, 1)
	rw := postAs(t, h, &usertoken.Identity{UserTokenID: "utok_1", BlockSuggestions: true}, body)

	if ct := rw.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	got := rw.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream missing %q:\n%s", want, got)
		}
	}
	// Every data line must parse — a malformed frame would break the client.
	for line := range strings.SplitSeq(got, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatalf("unparseable SSE frame %q: %v", line, err)
		}
	}
}

// Default is unchanged: without the option, suggestion traffic is forwarded
// exactly as before.
func TestSuggestionForwardedWhenOptionOff(t *testing.T) {
	var upstreamHits atomic.Int32
	h, _, _, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	for _, id := range []*usertoken.Identity{
		nil, // unauthenticated / no per-user token
		{UserTokenID: "utok_1", BlockSuggestions: false},
	} {
		upstreamHits.Store(0)
		postAs(t, h, id, suggestionBody)
		if n := upstreamHits.Load(); n != 1 {
			t.Fatalf("identity %+v: upstream hits = %d, want 1 (must forward by default)", id, n)
		}
	}
}

// A normal prompt from a user who blocks suggestions still goes upstream.
func TestOrdinaryPromptUnaffectedByBlocking(t *testing.T) {
	var upstreamHits atomic.Int32
	h, _, _, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(
		[]byte(`{"model":"claude-y","messages":[{"role":"user","content":"real work please"}]}`)))
	req = req.WithContext(usertoken.WithIdentity(req.Context(),
		&usertoken.Identity{UserTokenID: "utok_1", BlockSuggestions: true}))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status = %d: %s", rw.Code, rw.Body.String())
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
}
