package provider

import "testing"

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
		{"unknown model", "gpt-5", Anthropic},
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
	for _, id := range []ID{Anthropic, GLM} {
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
