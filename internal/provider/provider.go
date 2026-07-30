// Package provider describes the upstream APIs the proxy can forward to.
//
// Anthropic subscriptions and Z.AI GLM coding plans are both spoken to with the
// Anthropic Messages API — GLM exposes an Anthropic-compatible surface at
// https://api.z.ai/api/anthropic, verified against the live service for
// /v1/models, /v1/messages, /v1/messages/count_tokens, SSE streaming, tool use
// and cache_control. So the proxy needs no request/response translation: the
// only things that differ per upstream are the base URL, how a credential is
// kept alive, and whether a usage API exists to feed the selection score.
//
// This package is the single place those differences are declared. Everything
// else (pool selection, forwarding, model discovery, the refresher, the usage
// poller) asks here rather than testing for a provider by name.
package provider

import "strings"

// ID identifies an upstream. It is stored verbatim in credentials.provider.
type ID string

const (
	// Anthropic is the default for every credential that predates
	// multi-provider support, which is why its value must never change.
	Anthropic ID = "anthropic"
	GLM       ID = "glm"
)

// Provider is the per-upstream behaviour table.
type Provider struct {
	ID   ID
	Name string // human-readable, for CLI/TUI/web UI display

	// BaseURL is prefixed to the incoming request path. GLM's includes a path
	// segment (/api/anthropic), so callers must join rather than swap the host.
	BaseURL string

	// ModelPrefixes routes a request to this provider by its model ID. Empty
	// for the default provider, which catches everything unmatched.
	ModelPrefixes []string

	// Refreshable is true when credentials are OAuth tokens that can be renewed
	// via a refresh token. GLM credentials are static API keys: there is
	// nothing to refresh, and a 401 means the key is bad, not stale.
	Refreshable bool

	// PollsUsage is true when the upstream exposes a utilization API
	// (Anthropic's /api/oauth/usage) for the usage-aware selection score.
	//
	// Z.AI publishes no such endpoint and returns no rate-limit response
	// headers — confirmed by probing. GLM credentials therefore carry no
	// usage_history rows at all, which is deliberate: a synthesized 0% snapshot
	// would be indistinguishable from a genuinely idle credential and would
	// make the pool treat an exhausted key as wide open. With no snapshot the
	// pool falls through to its existing headroom=1.0 bootstrap path and
	// selects on weight alone, correcting reactively on a 429.
	PollsUsage bool

	// Augment1M is true when "<id>[1m]" rows should be added to /v1/models.
	//
	// The suffix is a Claude Code client-side alias that it strips before
	// sending, so the wire model name is always bare. Z.AI rejects the suffixed
	// form with HTTP 400 ("Unknown Model"), so GLM entries are left alone.
	Augment1M bool

	// AdvertisePrefix is prepended to this provider's model IDs in
	// GET /v1/models, and stripped again before the request goes upstream.
	//
	// This exists for exactly one reason: Claude Code discards gateway models
	// whose ID does not look like Anthropic's. Its /v1/models bootstrap runs
	//
	//	.filter(o => /^(claude|anthropic)/i.test(o.id))
	//	.filter(o => { let i = QQ(o.id); return i === null || i === kot })
	//
	// so a bare "glm-4.7" is dropped client-side and never reaches the /model
	// picker, no matter what this proxy returns. The second filter keeps IDs
	// the CLI does not recognise (QQ returns null for anything outside its
	// built-in table), so "claude-glm-4.7" passes both and shows up.
	//
	// The prefix is presentation only. It is stripped in WireModel before the
	// request is forwarded, because Z.AI would reject the prefixed name — so
	// the alias never escapes this proxy, and the native "glm-4.7" keeps
	// working for clients that address it directly.
	AdvertisePrefix string
}

var registry = []Provider{
	{
		ID:            Anthropic,
		Name:          "Anthropic",
		BaseURL:       "https://api.anthropic.com",
		ModelPrefixes: []string{"claude-"},
		Refreshable:   true,
		PollsUsage:    true,
		Augment1M:     true,
	},
	{
		ID:              GLM,
		Name:            "Z.AI GLM",
		BaseURL:         "https://api.z.ai/api/anthropic",
		ModelPrefixes:   []string{"glm-"},
		Refreshable:     false,
		PollsUsage:      false,
		Augment1M:       false,
		AdvertisePrefix: "claude-",
	},
}

// Default is the provider assumed for credentials and requests that name none.
const Default = Anthropic

// All returns every known provider, in a stable order.
func All() []Provider {
	out := make([]Provider, len(registry))
	copy(out, registry)
	return out
}

// Get returns the provider with the given ID, falling back to Default for an
// empty or unrecognised value so a malformed database row can never make a
// credential unroutable.
func Get(id ID) Provider {
	for _, p := range registry {
		if p.ID == id {
			return p
		}
	}
	for _, p := range registry {
		if p.ID == Default {
			return p
		}
	}
	return registry[0]
}

// Valid reports whether id names a known provider.
func Valid(id ID) bool {
	for _, p := range registry {
		if p.ID == id {
			return true
		}
	}
	return false
}

// ForModel routes a model ID to the provider that serves it.
//
// A trailing "[1m]" is trimmed first: Claude Code normally strips the alias
// before sending, but matching the bare stem costs nothing and means a client
// that does forward it still routes correctly instead of silently landing on
// the default provider.
//
// An unrecognised model resolves to Default. That keeps every current Claude
// model ID — including future ones and dated snapshots — working without this
// table having to know about them; only a provider that claims a prefix
// diverts traffic away from it.
func ForModel(model string) ID {
	m := normalizeModel(model)

	// Aliased IDs are matched FIRST and must be, because an alias deliberately
	// borrows another provider's prefix: "claude-glm-4.7" starts with
	// "claude-", so a naive pass would hand every GLM model to Anthropic.
	for _, p := range registry {
		if p.AdvertisePrefix == "" {
			continue
		}
		for _, prefix := range p.ModelPrefixes {
			if strings.HasPrefix(m, p.AdvertisePrefix+prefix) {
				return p.ID
			}
		}
	}
	// Native IDs. A client addressing "glm-4.7" directly — via
	// ANTHROPIC_DEFAULT_SONNET_MODEL, a raw API call, or a non-Claude-Code
	// client — keeps working unchanged.
	for _, p := range registry {
		for _, prefix := range p.ModelPrefixes {
			if strings.HasPrefix(m, prefix) {
				return p.ID
			}
		}
	}
	return Default
}

func normalizeModel(model string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(model)), "[1m]")
}

// AdvertisedID is how a model is presented in GET /v1/models. It is the
// identity function for providers that need no alias.
func AdvertisedID(id string, p ID) string {
	pr := Get(p)
	if pr.AdvertisePrefix == "" || strings.HasPrefix(strings.ToLower(id), pr.AdvertisePrefix) {
		return id
	}
	return pr.AdvertisePrefix + id
}

// WireModel converts a model ID as the client sent it into the name the
// upstream actually accepts, undoing AdvertisedID.
//
// Anything unrecognised is returned untouched: this must never mangle a model
// name the upstream would have accepted, so it only strips a prefix it can
// prove it added itself (the remainder still matches that provider's own
// prefixes).
func WireModel(model string) string {
	m := normalizeModel(model)
	for _, p := range registry {
		if p.AdvertisePrefix == "" {
			continue
		}
		for _, prefix := range p.ModelPrefixes {
			if strings.HasPrefix(m, p.AdvertisePrefix+prefix) {
				return model[len(p.AdvertisePrefix):]
			}
		}
	}
	return model
}
