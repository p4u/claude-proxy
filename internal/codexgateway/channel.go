package codexgateway

import "github.com/p4u/claude-proxy/internal/provider"

// Channel is one provider served through the shared CLIProxyAPI sidecar.
//
// The sidecar holds every account of every channel side by side in one auth
// directory; a channel selects its own accounts by the sidecar's provider key,
// starts logins on its own management route, and is fronted in this proxy by
// its own synthetic gateway credential, so each provider's routing, sticky
// bindings and 401 handling stay independent even though the upstream process
// is shared.
type Channel struct {
	// Key is the sidecar's provider/type for the channel's auth files.
	Key string
	// Provider is this proxy's provider the channel serves.
	Provider provider.ID
	// Name is the human label used in error messages.
	Name string
	// CredentialID is the synthetic credential that routes requests to the
	// sidecar. It carries only the sidecar URL and internal API key.
	CredentialID string

	authURLPath string
	// RegisteredCallback is the loopback redirect the upstream OAuth client is
	// registered with; ForwardedCallback is where the sidecar's callback
	// forwarder sends the browser next (its own port, 8317). A user pasting
	// the URL from either hop must be accepted.
	RegisteredCallback callbackShape
	ForwardedCallback  callbackShape

	// quotaFromAPI marks channels whose quota is not carried by response
	// headers and must be read from the upstream (see geminiQuota).
	quotaFromAPI bool
}

type callbackShape struct {
	Host, Port, Path string
}

// URL renders the callback for display ("http://localhost:1455/auth/callback").
func (c callbackShape) URL() string { return "http://" + c.Host + ":" + c.Port + c.Path }

var (
	// CodexChannel is OpenAI ChatGPT/Codex. OpenAI's registered redirect is
	// localhost:1455.
	CodexChannel = Channel{
		Key:                "codex",
		Provider:           provider.Codex,
		Name:               "OpenAI Codex",
		CredentialID:       "gateway_codex",
		authURLPath:        "/v0/management/codex-auth-url?is_webui=true",
		RegisteredCallback: callbackShape{"localhost", "1455", "/auth/callback"},
		ForwardedCallback:  callbackShape{"127.0.0.1", "8317", "/codex/callback"},
	}
	// GeminiChannel is a Google account signed in through Antigravity's OAuth
	// client, whose registered redirect is localhost:51121.
	GeminiChannel = Channel{
		Key:                "antigravity",
		Provider:           provider.Gemini,
		Name:               "Google Gemini",
		CredentialID:       "gateway_gemini",
		authURLPath:        "/v0/management/antigravity-auth-url?is_webui=true",
		RegisteredCallback: callbackShape{"localhost", "51121", "/oauth-callback"},
		ForwardedCallback:  callbackShape{"127.0.0.1", "8317", "/antigravity/callback"},
		quotaFromAPI:       true,
	}

	// Channels lists every sidecar-backed provider, in display order.
	Channels = []Channel{CodexChannel, GeminiChannel}
)

// GatewayCredentialID is the Codex gateway credential, kept for callers that
// predate Gemini. New code should use IsGatewayCredential.
const GatewayCredentialID = "gateway_codex"

// IsGatewayCredential reports whether id is one of the synthetic sidecar
// credentials, which are internal hops rather than anyone's subscription.
func IsGatewayCredential(id string) bool {
	for _, ch := range Channels {
		if ch.CredentialID == id {
			return true
		}
	}
	return false
}

// ChannelFor returns the sidecar channel serving a provider, if any.
func ChannelFor(p provider.ID) (Channel, bool) {
	for _, ch := range Channels {
		if ch.Provider == p {
			return ch, true
		}
	}
	return Channel{}, false
}
