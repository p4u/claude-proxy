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

import (
	"fmt"
	"strings"
)

// ID identifies an upstream. It is stored verbatim in credentials.provider.
type ID string

const (
	// Anthropic is the default for every credential that predates
	// multi-provider support, which is why its value must never change.
	Anthropic ID = "anthropic"
	GLM       ID = "glm"
	MiMo      ID = "mimo"
	// Codex is an OpenAI ChatGPT/Codex subscription served through the private
	// CLIProxyAPI sidecar. The sidecar owns OAuth refresh and account selection;
	// this proxy sees one internal gateway credential.
	Codex ID = "codex"
	// Custom is any self-hosted or third-party Anthropic-compatible endpoint.
	// Unlike the others it has no fixed base URL and no fixed model list: both
	// live on the credential, because each custom host is its own upstream.
	Custom ID = "custom"
	// CustomOpenAI is any self-hosted or third-party endpoint implementing the
	// OpenAI Chat Completions API. Requests and responses are translated at the
	// proxy boundary so downstream clients continue to speak Anthropic.
	CustomOpenAI ID = "custom_openai"
)

// Endpoint is one selectable base URL for a provider.
//
// Some providers run regional clusters and a key is bound to exactly one of
// them: a MiMo Token Plan key issued for Singapore authenticates against
// token-plan-sgp and returns "Invalid API Key" on token-plan-ams, with no hint
// that the region is the problem. The endpoint is therefore chosen per
// credential when the key is added, not fixed per provider.
type Endpoint struct {
	Name string // short selector, e.g. "sgp"
	Desc string // human label for pickers
	URL  string
}

// StaticModel is a catalogue entry for providers with no /v1/models endpoint.
type StaticModel struct {
	ID          string
	DisplayName string
}

// Provider is the per-upstream behaviour table.
type Provider struct {
	ID   ID
	Name string // human-readable, for CLI/TUI/web UI display

	// BaseURL is the default endpoint, prefixed to the incoming request path.
	// GLM's and MiMo's include a path segment (/api/anthropic, /anthropic), so
	// callers must join rather than swap the host. A credential may override
	// this with its own endpoint — see Endpoints and ResolveBaseURL.
	BaseURL string

	// Endpoints are the selectable base URLs offered when adding a credential.
	// Empty means the provider has exactly one, BaseURL.
	Endpoints []Endpoint

	// HasModelsAPI is false when the upstream serves no GET /v1/models. MiMo
	// returns nginx 404 there, so its catalogue cannot be discovered live and
	// StaticModels is advertised instead.
	HasModelsAPI bool

	// StaticModels is the advertised catalogue for providers without a models
	// API. Ignored when HasModelsAPI is true.
	StaticModels []StaticModel

	// VerifyModel is the model used for the authentication probe when a key is
	// added. Only meaningful for key-based providers.
	VerifyModel string

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
		HasModelsAPI:  true,
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
		HasModelsAPI:    true,
		VerifyModel:     "glm-4.7",
		Endpoints: []Endpoint{
			{Name: "global", Desc: "Global (api.z.ai)", URL: "https://api.z.ai/api/anthropic"},
			{Name: "cn", Desc: "China (open.bigmodel.cn)", URL: "https://open.bigmodel.cn/api/anthropic"},
		},
	},
	{
		ID:              Custom,
		Name:            "Custom Anthropic API Host",
		BaseURL:         "",  // supplied per credential; there is no default
		ModelPrefixes:   nil, // routed by the credential's declared model list
		Refreshable:     false,
		PollsUsage:      false,
		Augment1M:       false,
		AdvertisePrefix: "claude-",
		HasModelsAPI:    false, // probed per credential
	},
	{
		ID:            CustomOpenAI,
		Name:          "Custom OpenAI API Host",
		BaseURL:       "",  // supplied per credential, normally ending in /v1
		ModelPrefixes: nil, // routed by the credential's declared model list
		Refreshable:   false,
		PollsUsage:    false,
		Augment1M:     false,
		// A protocol-specific alias avoids collisions when Anthropic- and
		// OpenAI-compatible hosts happen to publish the same native model ID.
		AdvertisePrefix: "claude-openai-",
		HasModelsAPI:    false, // discovered and stored per credential
	},
	{
		ID:              MiMo,
		Name:            "Xiaomi MiMo",
		BaseURL:         "https://token-plan-sgp.xiaomimimo.com/anthropic",
		ModelPrefixes:   []string{"mimo-"},
		Refreshable:     false,
		PollsUsage:      false,
		Augment1M:       false,
		AdvertisePrefix: "claude-",
		// MiMo serves no /v1/models (nginx 404 on every cluster), so the
		// catalogue is static. Both IDs verified live; every other plausible
		// name probed returned "Unsupported model".
		HasModelsAPI: false,
		StaticModels: []StaticModel{
			{ID: "mimo-v2.5-pro", DisplayName: "MiMo v2.5 Pro"},
			{ID: "mimo-v2.5", DisplayName: "MiMo v2.5"},
		},
		VerifyModel: "mimo-v2.5-pro",
		// A Token Plan key is bound to one cluster and returns a bare
		// "Invalid API Key" on the others, so the operator must pick.
		Endpoints: []Endpoint{
			{Name: "sgp", Desc: "Token Plan — Singapore", URL: "https://token-plan-sgp.xiaomimimo.com/anthropic"},
			{Name: "ams", Desc: "Token Plan — Amsterdam", URL: "https://token-plan-ams.xiaomimimo.com/anthropic"},
			{Name: "cn", Desc: "Token Plan — China", URL: "https://token-plan-cn.xiaomimimo.com/anthropic"},
			{Name: "payg", Desc: "Pay-as-you-go (api.xiaomimimo.com)", URL: "https://api.xiaomimimo.com/anthropic"},
		},
	},
	{
		ID:              Codex,
		Name:            "OpenAI Codex",
		BaseURL:         "", // supplied by the internal gateway credential
		ModelPrefixes:   []string{"gpt-"},
		Refreshable:     false, // CLIProxyAPI refreshes the OAuth credentials
		PollsUsage:      false,
		Augment1M:       false,
		AdvertisePrefix: "claude-",
		HasModelsAPI:    true,
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

// ResolveBaseURL returns the endpoint a credential should talk to: its own
// override when set, otherwise the provider default. Stored overrides are
// trusted as-is so a cluster added by the vendor after this build still works
// without a registry edit.
func ResolveBaseURL(p ID, credBaseURL string) string {
	if u := strings.TrimSpace(credBaseURL); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	return Get(p).BaseURL
}

// EndpointName returns the preset name matching a credential's resolved base
// URL, or "" when it is a custom URL the registry does not know.
func EndpointName(p ID, credBaseURL string) string {
	url := ResolveBaseURL(p, credBaseURL)
	for _, e := range Get(p).Endpoints {
		if strings.EqualFold(e.URL, url) {
			return e.Name
		}
	}
	return ""
}

// ResolveEndpoint maps an operator's endpoint choice to a base URL. It accepts
// either a short name from the provider's Endpoints ("sgp") or a full URL, so
// a cluster this build has never heard of is still reachable. Empty selects
// the provider default.
func ResolveEndpoint(p ID, choice string) (string, error) {
	c := strings.TrimSpace(choice)
	if c == "" {
		return Get(p).BaseURL, nil
	}
	for _, e := range Get(p).Endpoints {
		if strings.EqualFold(e.Name, c) {
			return e.URL, nil
		}
	}
	if strings.HasPrefix(c, "https://") || strings.HasPrefix(c, "http://") {
		return strings.TrimSuffix(c, "/"), nil
	}
	names := make([]string, 0, len(Get(p).Endpoints))
	for _, e := range Get(p).Endpoints {
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%s has no selectable endpoints; pass a full https:// URL", Get(p).Name)
	}
	return "", fmt.Errorf("unknown %s endpoint %q — use one of %s, or a full https:// URL",
		Get(p).Name, c, strings.Join(names, ", "))
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
