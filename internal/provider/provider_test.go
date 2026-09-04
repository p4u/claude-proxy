package provider

import (
	"strings"
	"testing"
)

func TestForModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  ID
	}{
		{"claude alias", "claude-sonnet-5", Anthropic},
		{"claude dated snapshot", "claude-opus-4-8-20260101", Anthropic},
		{"claude with 1m alias", "claude-sonnet-5[1m]", Anthropic},
		{"glm", "glm-4.7", GLM},
		{"glm latest", "glm-5.2", GLM},
		// Z.AI 400s on the suffixed form, so it must never reach the wire — but
		// routing still has to land on GLM if a client forwards it.
		{"glm with 1m alias", "glm-5.2[1m]", GLM},
		{"case insensitive", "GLM-4.7", GLM},
		{"surrounding space", "  glm-4.7  ", GLM},
		// Anything unclaimed falls to the default so unknown/future Claude IDs
		// keep working without this table being updated.
		{"codex", "gpt-5", Codex},
		{"unknown model", "future-model", Anthropic},
		{"empty", "", Anthropic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForModel(tc.model); got != tc.want {
				t.Errorf("ForModel(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestGetFallsBackToDefault(t *testing.T) {
	if got := Get("nonesuch").ID; got != Default {
		t.Errorf("Get(unknown).ID = %q, want %q", got, Default)
	}
	if got := Get("").ID; got != Default {
		t.Errorf("Get(empty).ID = %q, want %q", got, Default)
	}
	if got := Get(GLM).ID; got != GLM {
		t.Errorf("Get(GLM).ID = %q, want %q", got, GLM)
	}
}

func TestValid(t *testing.T) {
	for _, id := range []ID{Anthropic, GLM, MiMo, Custom, CustomOpenAI, Codex} {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
	for _, id := range []ID{"", "openai", "Anthropic"} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

// The capability flags drive whether the refresher, the usage poller and the
// [1m] augmenter touch a credential at all, so pin them explicitly rather than
// letting a registry edit silently change background behaviour.
func TestCapabilities(t *testing.T) {
	a, g := Get(Anthropic), Get(GLM)

	if !a.Refreshable || !a.PollsUsage || !a.Augment1M {
		t.Errorf("anthropic capabilities regressed: %+v", a)
	}
	if g.Refreshable {
		t.Error("GLM keys are static: refreshing one would burn a request and mislead the 401 path")
	}
	if g.PollsUsage {
		t.Error("Z.AI exposes no usage API; polling would write 0% snapshots that read as idle")
	}
	if g.Augment1M {
		t.Error("Z.AI rejects <id>[1m] with HTTP 400; advertising the variant would break the picker")
	}
	if a.BaseURL == g.BaseURL {
		t.Error("providers must not share a base URL")
	}
}

func TestAllReturnsACopy(t *testing.T) {
	all := All()
	if len(all) != len(registry) {
		t.Fatalf("All() len = %d, want %d", len(all), len(registry))
	}
	all[0].BaseURL = "https://evil.example"
	if Get(all[0].ID).BaseURL == "https://evil.example" {
		t.Error("All() leaked the registry: mutating the result changed the source of truth")
	}
}

// The alias deliberately borrows Anthropic's prefix, so ordering inside
// ForModel is load-bearing: a naive prefix scan hands every GLM model to
// Anthropic and the feature silently routes to the wrong upstream.
func TestAliasedModelRouting(t *testing.T) {
	tests := []struct {
		model string
		want  ID
	}{
		{"claude-glm-4.7", GLM},
		{"claude-glm-5.2", GLM},
		{"CLAUDE-GLM-4.7", GLM},
		{"claude-glm-4.7[1m]", GLM},
		{"glm-4.7", GLM}, // native form still routes
		{"claude-sonnet-5", Anthropic},
		{"claude-opus-4-8", Anthropic},
		// Not an alias — "glmx" is not a GLM model prefix.
		{"claude-glmx", Anthropic},
	}
	for _, tc := range tests {
		if got := ForModel(tc.model); got != tc.want {
			t.Errorf("ForModel(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestWireModelAndAdvertisedID(t *testing.T) {
	// Z.AI rejects the prefixed name, so the alias must be stripped on the wire.
	if got := WireModel("claude-glm-4.7"); got != "glm-4.7" {
		t.Errorf("WireModel(claude-glm-4.7) = %q, want glm-4.7", got)
	}
	// Everything else passes through untouched — this must never mangle a name
	// the upstream would have accepted.
	for _, m := range []string{"glm-4.7", "claude-sonnet-5", "claude-opus-4-8", "", "gpt-5"} {
		if got := WireModel(m); got != m {
			t.Errorf("WireModel(%q) = %q, want it unchanged", m, got)
		}
	}

	if got := AdvertisedID("glm-4.7", GLM); got != "claude-glm-4.7" {
		t.Errorf("AdvertisedID(glm-4.7) = %q, want claude-glm-4.7", got)
	}
	if got := AdvertisedID("claude-sonnet-5", Anthropic); got != "claude-sonnet-5" {
		t.Errorf("AdvertisedID must not touch Anthropic ids, got %q", got)
	}
	// Round trip is the invariant that matters.
	for _, m := range []string{"glm-4.7", "glm-5.2", "glm-4.5-air"} {
		if got := WireModel(AdvertisedID(m, GLM)); got != m {
			t.Errorf("round trip broke for %q: got %q", m, got)
		}
	}
}

func TestMiMoRouting(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  ID
	}{
		{"mimo-v2.5-pro", MiMo},
		{"mimo-v2.5", MiMo},
		{"claude-mimo-v2.5-pro", MiMo}, // advertised alias
		{"claude-mimo-v2.5-pro[1m]", MiMo},
		{"MIMO-V2.5-PRO", MiMo},
		{"claude-sonnet-5", Anthropic},
		{"glm-4.7", GLM},
	} {
		if got := ForModel(tc.model); got != tc.want {
			t.Errorf("ForModel(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
	if got := WireModel("claude-mimo-v2.5-pro"); got != "mimo-v2.5-pro" {
		t.Errorf("WireModel = %q, want mimo-v2.5-pro", got)
	}
	// MiMo serves no /v1/models, so its catalogue must be declared statically or
	// nothing can ever be advertised for it.
	p := Get(MiMo)
	if p.HasModelsAPI {
		t.Error("MiMo has no /v1/models (nginx 404 on every cluster)")
	}
	if len(p.StaticModels) == 0 {
		t.Error("MiMo needs a static catalogue since it cannot be discovered")
	}
}

func TestCodexRouting(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  ID
	}{
		{"gpt-5.6-codex", Codex},
		{"claude-gpt-5.6-codex", Codex},
		{"claude-gpt-5.6-codex[1m]", Codex},
		{"GPT-5.6-CODEX", Codex},
		{"claude-sonnet-5", Anthropic},
	} {
		if got := ForModel(tc.model); got != tc.want {
			t.Errorf("ForModel(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
	if got := AdvertisedID("gpt-5.6-codex", Codex); got != "claude-gpt-5.6-codex" {
		t.Errorf("AdvertisedID = %q", got)
	}
	if got := WireModel("claude-gpt-5.6-codex"); got != "gpt-5.6-codex" {
		t.Errorf("WireModel = %q", got)
	}
}

// A key is bound to one cluster and fails with a bare "Invalid API Key" on the
// others, so endpoint resolution has to be exact and its errors actionable.
func TestResolveEndpoint(t *testing.T) {
	sgp := "https://token-plan-sgp.xiaomimimo.com/anthropic"
	ams := "https://token-plan-ams.xiaomimimo.com/anthropic"

	if got, err := ResolveEndpoint(MiMo, ""); err != nil || got != sgp {
		t.Errorf("empty should select the default: got %q err %v", got, err)
	}
	if got, err := ResolveEndpoint(MiMo, "ams"); err != nil || got != ams {
		t.Errorf("named endpoint: got %q err %v", got, err)
	}
	if got, err := ResolveEndpoint(MiMo, "AMS"); err != nil || got != ams {
		t.Errorf("name match should be case-insensitive: got %q err %v", got, err)
	}
	// A cluster added after this build must remain reachable.
	custom := "https://token-plan-xyz.example.com/anthropic"
	if got, err := ResolveEndpoint(MiMo, custom+"/"); err != nil || got != custom {
		t.Errorf("full URL should pass through trimmed: got %q err %v", got, err)
	}
	err := func() error { _, e := ResolveEndpoint(MiMo, "nope"); return e }()
	if err == nil {
		t.Fatal("unknown endpoint name should error")
	}
	for _, want := range []string{"sgp", "ams"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list valid names, missing %q: %v", want, err)
		}
	}
}

func TestResolveBaseURL(t *testing.T) {
	if got := ResolveBaseURL(MiMo, ""); got != Get(MiMo).BaseURL {
		t.Errorf("empty override should fall back to the provider default, got %q", got)
	}
	override := "https://token-plan-ams.xiaomimimo.com/anthropic"
	if got := ResolveBaseURL(MiMo, override+"/"); got != override {
		t.Errorf("override should win (trailing slash trimmed), got %q", got)
	}
}
