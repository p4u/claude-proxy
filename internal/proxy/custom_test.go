package proxy

import (
	"testing"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
)

func customCred(id, status string, models ...string) *creds.Credential {
	ms := make([]creds.Model, 0, len(models))
	for _, m := range models {
		ms = append(ms, creds.Model{ID: m})
	}
	return &creds.Credential{ID: id, Provider: provider.Custom, Status: creds.Status(status), Models: ms}
}

// A custom model can only be placed by asking which credential declares it —
// there is no prefix to match on.
func TestMatchCustomModel(t *testing.T) {
	list := []*creds.Credential{
		{ID: "anth", Provider: provider.Anthropic, Status: creds.StatusActive},
		customCred("c1", "active", "Qwen3.6-fable", "shared-model"),
		customCred("c2", "active", "other-model", "shared-model"),
	}

	for _, tc := range []struct {
		name, model string
		wantModel   string
		wantIDs     []string
	}{
		{"native name", "Qwen3.6-fable", "Qwen3.6-fable", []string{"c1"}},
		// The picker shows the advertised alias, so it must resolve identically.
		{"advertised alias", "claude-Qwen3.6-fable", "Qwen3.6-fable", []string{"c1"}},
		{"case insensitive", "qwen3.6-FABLE", "Qwen3.6-fable", []string{"c1"}},
		{"1m suffix tolerated", "claude-Qwen3.6-fable[1m]", "Qwen3.6-fable", []string{"c1"}},
		// Two hosts serving the same model pool, so both are candidates.
		{"shared across hosts", "shared-model", "shared-model", []string{"c1", "c2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchCustomModel(list, tc.model)
			if !ok {
				t.Fatalf("no match for %q", tc.model)
			}
			if got.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tc.wantModel)
			}
			if len(got.CredIDs) != len(tc.wantIDs) {
				t.Fatalf("cred IDs = %v, want %v", got.CredIDs, tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if got.CredIDs[i] != id {
					t.Errorf("cred IDs = %v, want %v", got.CredIDs, tc.wantIDs)
				}
			}
		})
	}

	// Registry models must stay on the prefix path, or a custom host could
	// hijack a Claude model name.
	for _, m := range []string{"claude-sonnet-5", "glm-4.7", "nonesuch", ""} {
		if _, ok := matchCustomModel(list, m); ok {
			t.Errorf("%q should not match a custom credential", m)
		}
	}
}

func TestMatchCustomOpenAIModelUsesProtocolSpecificAlias(t *testing.T) {
	list := []*creds.Credential{
		customCred("anthropic", "active", "shared-model", "openai-shared-model"),
		{ID: "openai", Provider: provider.CustomOpenAI, Status: creds.StatusActive,
			Models: []creds.Model{{ID: "shared-model"}}},
	}
	got, ok := matchCustomModel(list, "claude-openai-shared-model")
	if !ok || got.Provider != provider.CustomOpenAI || got.Model != "shared-model" || len(got.CredIDs) != 1 || got.CredIDs[0] != "openai" {
		t.Fatalf("OpenAI alias resolved as %+v, ok=%v", got, ok)
	}
	// Native ambiguity keeps the established Anthropic host behavior.
	got, ok = matchCustomModel(list, "shared-model")
	if !ok || got.Provider != provider.Custom || got.CredIDs[0] != "anthropic" {
		t.Fatalf("native model resolution = %+v, ok=%v", got, ok)
	}
}

// Advertising a model whose credential cannot serve it would offer the picker
// something the proxy then has to reject.
func TestCustomModelsGatedOnHealth(t *testing.T) {
	list := []*creds.Credential{
		customCred("ok", "active", "alpha"),
		customCred("limited", "limited", "beta"),
		customCred("dead", "revoked", "gamma"),
		customCred("off", "disabled", "delta"),
	}
	got := map[string]bool{}
	for _, e := range customModels(list, provider.Custom) {
		got[e["id"].(string)] = true
	}
	if !got["alpha"] {
		t.Error("active credential's models must be advertised")
	}
	// Limited is temporary and still reaches the host for a real 429.
	if !got["beta"] {
		t.Error("limited credential's models must still be advertised")
	}
	if got["gamma"] || got["delta"] {
		t.Errorf("revoked/disabled credentials must not advertise models: %v", got)
	}
}

// Two hosts declaring the same model must produce one picker entry, not two.
func TestCustomModelsDeduplicated(t *testing.T) {
	list := []*creds.Credential{
		customCred("c1", "active", "shared"),
		customCred("c2", "active", "shared"),
	}
	if n := len(customModels(list, provider.Custom)); n != 1 {
		t.Errorf("got %d entries, want 1 — duplicates would show twice in the picker", n)
	}
}

// Context window is only knowable when the host published it; guessing one
// would misconfigure the client's context management.
func TestCustomModelsOmitUnknownContext(t *testing.T) {
	list := []*creds.Credential{{
		ID: "c", Provider: provider.Custom, Status: creds.StatusActive,
		Models: []creds.Model{
			{ID: "known", ContextWindow: 200000, MaxOutput: 8192},
			{ID: "unknown"},
		},
	}}
	byID := map[string]map[string]any{}
	for _, e := range customModels(list, provider.Custom) {
		byID[e["id"].(string)] = e
	}
	if byID["known"]["max_input_tokens"] != 200000 {
		t.Errorf("published context window must be forwarded: %v", byID["known"])
	}
	if _, present := byID["unknown"]["max_input_tokens"]; present {
		t.Errorf("unknown context must be omitted, not zero: %v", byID["unknown"])
	}
}
