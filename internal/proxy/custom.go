package proxy

import (
	"strings"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
)

// Custom hosts are routed differently from registry providers.
//
// A registry provider owns a model prefix known at compile time, so
// provider.ForModel can place "glm-4.7" without touching the database. A custom
// host serves whatever its operator declared — "Qwen3.6-fable", "my-llama",
// anything — so the only way to route it is to ask which credential declares
// that model. That lookup reads the credential list the caller already loaded
// rather than issuing its own query.
//
// The mapping is model → credential IDs, not model → provider: two custom hosts
// are both provider "custom" while serving disjoint models, so the provider
// alone is too coarse a filter and the pool needs the ID set.

// customMatch is the resolution of a model name against the custom credentials.
type customMatch struct {
	Model   string   // the model ID as the host declared it (alias stripped)
	CredIDs []string // credentials that declare it, in list order
}

// matchCustomModel finds the custom credentials serving a model.
//
// Matching is case-insensitive and tolerates the advertised "claude-" alias, so
// a picker selection ("claude-Qwen3.6-fable") and a direct API call
// ("Qwen3.6-fable") resolve to the same host. Returns ok=false when no custom
// credential declares it, leaving the caller on the registry-prefix path.
func matchCustomModel(list []*creds.Credential, model string) (customMatch, bool) {
	want := strings.ToLower(strings.TrimSpace(model))
	want = strings.TrimSuffix(want, "[1m]")
	if want == "" {
		return customMatch{}, false
	}
	// The name as sent first, then with the advertising prefix removed: a host
	// that genuinely named a model "claude-foo" then still wins over the alias
	// interpretation of the same string.
	candidates := []string{want}
	if p := provider.Get(provider.Custom).AdvertisePrefix; p != "" && strings.HasPrefix(want, p) {
		candidates = append(candidates, strings.TrimPrefix(want, p))
	}

	for _, cand := range candidates {
		var ids []string
		var declared string
		for _, c := range list {
			if credProvider(c) != provider.Custom {
				continue
			}
			for _, m := range c.Models {
				if strings.EqualFold(strings.TrimSpace(m.ID), cand) {
					ids = append(ids, c.ID)
					declared = m.ID
					break
				}
			}
		}
		if len(ids) > 0 {
			return customMatch{Model: declared, CredIDs: ids}, true
		}
	}
	return customMatch{}, false
}

// customModels returns every model declared by a custom credential that can
// currently serve traffic.
//
// Used by model discovery. Gating on credential health is the same rule the
// registry providers follow: offering a model the proxy would then have to
// reject is worse than omitting it.
func customModels(list []*creds.Credential) []map[string]any {
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, c := range list {
		if credProvider(c) != provider.Custom {
			continue
		}
		if c.Status != creds.StatusActive && c.Status != creds.StatusLimited {
			continue
		}
		for _, m := range c.Models {
			id := strings.TrimSpace(m.ID)
			if id == "" || seen[strings.ToLower(id)] {
				continue
			}
			seen[strings.ToLower(id)] = true
			e := map[string]any{"id": id, "type": "model"}
			if m.DisplayName != "" {
				e["display_name"] = m.DisplayName
			} else {
				e["display_name"] = id
			}
			// Claude Code sizes its context management from these when they are
			// present. Omitted rather than guessed when the host never told us:
			// a wrong window is worse than no window.
			if m.ContextWindow > 0 {
				e["max_input_tokens"] = m.ContextWindow
			}
			if m.MaxOutput > 0 {
				e["max_tokens"] = m.MaxOutput
			}
			out = append(out, e)
		}
	}
	return out
}
