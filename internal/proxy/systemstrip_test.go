package proxy

import (
	"bytes"
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

const sdkLine = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

// The three system blocks headless `claude -p` sends (2.1.280), trimmed.
const headlessBody = `{"model":"claude-gemini-3.8-flash-high","max_tokens":32000,` +
	`"system":[` +
	`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.f22; cc_entrypoint=sdk-cli;"},` +
	`{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK.","cache_control":{"type":"ephemeral"}},` +
	`{"type":"text","text":"\nYou are an interactive agent that helps users with software engineering tasks.","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u1"}}`

func TestStripSystemTextBlocks(t *testing.T) {
	out, ok := stripSystemText([]byte(headlessBody), strippedSystemPhrases)
	if !ok {
		t.Fatal("phrase not removed")
	}
	var got struct {
		Model    string            `json:"model"`
		System   []json.RawMessage `json:"system"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Claude Agent SDK") {
		t.Fatalf("SDK line survived: %s", out)
	}
	// The emptied identity block is dropped; the other two are kept as sent,
	// cache_control included.
	if len(got.System) != 2 || !strings.Contains(string(got.System[1]), `"cache_control":{"type":"ephemeral"}`) ||
		!strings.Contains(string(got.System[0]), "x-anthropic-billing-header") {
		t.Fatalf("system blocks = %s", got.System)
	}
	if got.Model != "claude-gemini-3.8-flash-high" || got.Metadata["user_id"] != "u1" {
		t.Fatalf("other fields changed: %s", out)
	}
}

func TestStripSystemTextString(t *testing.T) {
	out, ok := stripSystemText([]byte(`{"system":"`+sdkLine+` Be terse.","messages":[]}`), strippedSystemPhrases)
	if !ok || !strings.Contains(string(out), `"system":"Be terse."`) {
		t.Fatalf("string system = %s (ok=%v)", out, ok)
	}
	out, ok = stripSystemText([]byte(`{"system":"`+sdkLine+`","messages":[]}`), strippedSystemPhrases)
	if !ok || strings.Contains(string(out), `"system"`) {
		t.Fatalf("a system made only of the phrase must be removed: %s", out)
	}
}

// Requests without the phrase — every interactive Claude Code request — must
// reach the upstream byte for byte, so prompt caching and signatures are
// unaffected.
func TestStripSystemTextLeavesOtherBodiesUntouched(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-opus-5","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`,
		`{"model":"m","messages":[{"role":"user","content":"` + strings.ReplaceAll(sdkLine, "'", "\\u0027") + `"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"quoting it: You are a Claude agent, built on Anthropic's Claude Agent SDK."}]}`,
		`not json ` + sdkLine,
	} {
		out, ok := stripSystemText([]byte(body), strippedSystemPhrases)
		if ok || !bytes.Equal(out, []byte(body)) {
			t.Errorf("body changed:\n in: %s\nout: %s", body, out)
		}
	}
}

// End to end: the upstream never sees the line, whichever provider serves the
// request.
func TestHeadlessIdentityLineNeverReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		prov  provider.ID
		model string
	}{
		{provider.Gemini, "claude-gemini-3.8-flash-high"},
		{provider.Codex, "claude-gpt-5.6-luna"},
	} {
		var got []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		db, err := store.Open(filepath.Join(t.TempDir(), "strip.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := creds.InsertKey(context.Background(), db, tc.prov, "gw", "oauth-sidecar", "k", srv.URL, 1); err != nil {
			t.Fatal(err)
		}
		h := New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
		body := strings.Replace(headlessBody, "claude-gemini-3.8-flash-high", tc.model, 1)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		srv.Close()
		db.Close()
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", tc.prov, w.Code, w.Body.String())
		}
		if strings.Contains(string(got), "Claude Agent SDK") || !strings.Contains(string(got), "interactive agent") {
			t.Fatalf("%s: upstream body = %s", tc.prov, got)
		}
	}
}
