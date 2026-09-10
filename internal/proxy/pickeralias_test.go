package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddPickerAliases(t *testing.T) {
	entries := []map[string]any{
		{"id": "claude-opus-5", "display_name": "Claude Opus 5"},
		{"id": "claude-sonnet-5[1m]", "display_name": "Claude Sonnet 5 (1M context)"},
		{"id": "claude-sonnet-5", "display_name": "Claude Sonnet 5"},
		{"id": "claude-fable-5", "display_name": "Claude Fable 5"},
		{"id": "claude-opus-4-8", "display_name": "Claude Opus 4.8"},
		{"id": "claude-haiku-4-5-20251001", "display_name": "Claude Haiku 4.5"},
		// A dated snapshot of a hidden model survives the client's exact-match
		// dedup on its own, so it must not gain a redundant alias row.
		{"id": "claude-opus-5-20260825", "display_name": "Claude Opus 5"},
	}
	out := addPickerAliases(entries)
	var ids []string
	for _, e := range out {
		ids = append(ids, e["id"].(string))
	}
	want := []string{
		// The alias row follows its base; models the client can already reach
		// (Fable, legacy Opus, Haiku) and "[1m]" variants gain nothing.
		"claude-opus-5", "claude-opus-5-gw",
		"claude-sonnet-5[1m]",
		"claude-sonnet-5", "claude-sonnet-5-gw",
		"claude-fable-5",
		"claude-opus-4-8",
		"claude-haiku-4-5-20251001",
		"claude-opus-5-20260825",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	// The alias keeps the honest display name: it is the row the picker shows.
	if out[1]["display_name"] != "Claude Opus 5" {
		t.Fatalf("alias display_name = %v", out[1]["display_name"])
	}
	// Aliasing an already-aliased list must not duplicate rows.
	if again := addPickerAliases(out); len(again) != len(out) {
		t.Fatalf("re-alias grew the list: %d -> %d", len(out), len(again))
	}
}

func TestStripPickerAlias(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-gw":          "claude-opus-5",
		"claude-sonnet-5-gw":        "claude-sonnet-5",
		"claude-opus-5-20260825-gw": "claude-opus-5-20260825",
		// Not proxy aliases: never touched.
		"claude-opus-5":     "claude-opus-5",
		"claude-opus-4-8":   "claude-opus-4-8",
		"claude-foo-gw":     "claude-foo-gw",
		"claude-opus-5-gw2": "claude-opus-5-gw2",
		"glm-4.7":           "glm-4.7",
	} {
		if got := stripPickerAlias(in); got != want {
			t.Fatalf("stripPickerAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// The alias must be gone by the time the request reaches the upstream, and the
// rest of the body must be forwarded byte-identically.
func TestPickerAliasForwardsNativeModel(t *testing.T) {
	var gotBody string
	h, cs, _, ts := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	defer ts.Close()

	body := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}],"model":"claude-opus-5-gw"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rw.Code, rw.Body.String())
	}
	var sent struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "claude-opus-5" {
		t.Fatalf("upstream saw model %q, want claude-opus-5", sent.Model)
	}
	if string(sent.Messages) != `[{"role":"user","content":"hi"}]` {
		t.Fatalf("messages altered: %s", sent.Messages)
	}
	if len(cs) == 0 {
		t.Fatal("no credentials seeded")
	}
}
