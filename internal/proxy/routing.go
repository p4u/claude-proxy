package proxy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
)

// requestModel pulls the "model" field out of a Messages API request body.
// Returns "" when the body is absent, unparseable, or names no model — callers
// treat that as "use the default provider" rather than as an error, because
// rejecting a request the upstream might well accept is the worse failure.
func requestModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.Model
}

// providerFor decides which upstream serves this request.
//
// The model named in the body is the only signal: it is what the user picked in
// Claude Code's /model picker, and it is the one thing that genuinely cannot be
// served by the wrong upstream. Paths that carry no model (/v1/models
// discovery, anything else under /v1/) fall to the default provider,
// preserving pre-GLM behaviour exactly.
func providerFor(r *http.Request, body []byte) provider.ID {
	if r.Method != http.MethodPost {
		return provider.Default
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/messages/count_tokens":
		return provider.ForModel(requestModel(body))
	}
	return provider.Default
}

// rewriteModel replaces the body's "model" with the name the upstream accepts,
// undoing the advertising alias (see provider.AdvertisePrefix). Returns the
// body unchanged — the same slice — when no rewrite is needed, which is every
// Anthropic request and every request that already names a native model.
//
// The envelope is decoded into json.RawMessage values so every other field is
// re-encoded byte-for-byte: this must not normalise, reorder or lose anything
// in a request body it does not own. Only the model string is touched.
func rewriteModel(body []byte) ([]byte, string, bool) {
	model := requestModel(body)
	if model == "" {
		return body, "", false
	}
	wire := provider.WireModel(model)
	if wire == model {
		return body, model, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, model, false
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return body, model, false
	}
	obj["model"] = encoded
	out, err := json.Marshal(obj)
	if err != nil {
		return body, model, false
	}
	return out, wire, true
}

// pickForProvider returns a credential belonging to prov for non-sticky paths.
// Prefers active; falls back to limited so the request still reaches the
// upstream and earns a real 429 (with Retry-After) instead of a proxy-invented
// 503. Mirrors pickAny, but scoped to one provider.
func (h *Handler) pickForProvider(ctx context.Context, prov provider.ID) (*creds.Credential, error) {
	list, err := creds.List(ctx, h.db)
	if err != nil {
		return nil, err
	}
	return pickFrom(list, prov)
}

// pickFrom applies the active-then-limited preference to an already-loaded
// credential list. Split out so callers that list once (model discovery walks
// every provider) do not re-query per provider.
func pickFrom(list []*creds.Credential, prov provider.ID) (*creds.Credential, error) {
	if prov == "" {
		prov = provider.Default
	}
	var fallback *creds.Credential
	for _, c := range list {
		if credProvider(c) != prov {
			continue
		}
		if c.Status == creds.StatusActive {
			return c, nil
		}
		if c.Status == creds.StatusLimited && fallback == nil {
			fallback = c
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, pool.ErrNoCredentials
}

// credProvider reads a credential's provider, defaulting empty to Anthropic so
// rows written before the column existed route exactly as they always did.
func credProvider(c *creds.Credential) provider.ID {
	if c.Provider == "" {
		return provider.Default
	}
	return c.Provider
}
