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
	"github.com/p4u/claude-proxy/internal/store"
)

func openAIProxySetup(t *testing.T, upstream http.HandlerFunc) (*Handler, *int) {
	t.Helper()
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		upstream(w, r)
	}))
	t.Cleanup(ts.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "openai.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = creds.InsertCustomOpenAIKey(context.Background(), db, "local-openai", "bearer-secret", ts.URL+"/v1",
		[]creds.Model{{ID: "local-model", DisplayName: "Local model"}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	h := New(db, pool.New(db), creds.NewRefresher(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h, &hits
}

func TestCustomOpenAITranslatesMessagesAndTools(t *testing.T) {
	h, _ := openAIProxySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer bearer-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("Anthropic-Version") != "" {
			t.Error("Anthropic-Version leaked to OpenAI host")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "local-model" {
			t.Errorf("wire model = %#v", body["model"])
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 4 {
			t.Fatalf("translated messages = %#v", messages)
		}
		if messages[0].(map[string]any)["role"] != "system" {
			t.Errorf("system message missing: %#v", messages)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Errorf("translated tools = %#v", tools)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl_1","model":"local-model","choices":[{"message":{"role":"assistant","content":"Checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Madrid\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`)
	})

	body := `{"model":"claude-openai-local-model","max_tokens":64,"system":"Be concise","messages":[{"role":"user","content":"Weather?"},{"role":"assistant","content":[{"type":"tool_use","id":"old_call","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"old_call","content":"done"}]}],"tools":[{"name":"weather","description":"Get weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			Input, Output, Cached int64
		} `json:"-"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.StopReason != "tool_use" || len(out.Content) != 2 {
		t.Fatalf("translated response = %s", rec.Body.String())
	}
	if out.Content[0].Text != "Checking" || out.Content[1].Name != "weather" || out.Content[1].Input["city"] != "Madrid" {
		t.Errorf("translated content = %#v", out.Content)
	}
	for _, want := range []string{`"input_tokens":12`, `"output_tokens":4`, `"cache_read_input_tokens":3`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("response missing %s: %s", want, rec.Body.String())
		}
	}
}

func TestCustomOpenAITranslatesStreamingTextAndToolCalls(t *testing.T) {
	h, _ := openAIProxySetup(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Error("stream flag was not forwarded")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_s\",\"model\":\"local-model\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hi \"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_s\",\"model\":\"local-model\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_s\",\"model\":\"local-model\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl_s\",\"model\":\"local-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	req := `{"model":"claude-openai-local-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(req)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream response = %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"event: message_start", `"text":"Hi ","type":"text_delta"`,
		`"id":"call_9","input":{},"name":"lookup","type":"tool_use"`,
		`"partial_json":"{\"q\":\"x\"}","type":"input_json_delta"`,
		`"stop_reason":"tool_use"`, "event: message_stop",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("stream missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestCustomOpenAICountTokensIsLocalAndModelsAreAdvertised(t *testing.T) {
	h, hits := openAIProxySetup(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("count/models should use the stored catalogue without calling upstream")
		w.WriteHeader(http.StatusInternalServerError)
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-openai-local-model","messages":[{"role":"user","content":"hello world"}]}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("count response = %d %s", rec.Code, rec.Body.String())
	}
	models := httptest.NewRecorder()
	h.ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"claude-openai-local-model"`) {
		t.Fatalf("models response = %d %s", models.Code, models.Body.String())
	}
	if *hits != 0 {
		t.Errorf("upstream hits = %d, want 0", *hits)
	}
}
