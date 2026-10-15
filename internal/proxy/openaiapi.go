package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/router"
)

// OpenAIBasePath is the base URL path of the OpenAI-compatible API, for
// clients (Pi, opencode, any OpenAI SDK) that speak Chat Completions rather
// than Anthropic Messages: https://<host>/openai/v1. It is its own prefix, not
// /v1, because Claude Code already owns /v1/models with an Anthropic-shaped,
// alias-laden list.
//
// Requests pass through untranslated, so only providers whose upstream speaks
// Chat Completions natively are offered (provider.OpenAIChat): Privatemode,
// whose sidecar encrypts /v1/chat/completions exactly like /v1/messages; the
// Codex and Gemini subscriptions behind CLIProxyAPI; and custom OpenAI hosts.
// Authentication, per-user limits, sticky credential binding, request logging
// and the 401/429 credential handling are the /v1/messages ones.
const OpenAIBasePath = "/openai/v1"

// IsOpenAIPath reports whether p belongs to the OpenAI-compatible API.
func IsOpenAIPath(p string) bool {
	return p == OpenAIBasePath || strings.HasPrefix(p, OpenAIBasePath+"/")
}

// openAIModelID names a model on the OpenAI surface: "<provider>/<native ID>".
// There is no Claude Code picker filter to satisfy here, so the claude- alias
// is dropped for a name that says where the model runs.
func openAIModelID(p provider.ID, native string) string { return string(p) + "/" + native }

// openAIRoute resolves an OpenAI-surface model to its provider and the name
// the upstream accepts. "<provider>/<model>" is the advertised form; the
// claude-* aliases Claude Code uses are accepted too, so one model name works
// with either client.
func openAIRoute(list []*creds.Credential, model string) (provider.ID, string, customMatch, bool) {
	model = strings.TrimSpace(model)
	if pfx, rest, found := strings.Cut(model, "/"); found && rest != "" {
		p := provider.ID(strings.ToLower(pfx))
		if !provider.Valid(p) || !provider.Get(p).OpenAIChat {
			return "", "", customMatch{}, false
		}
		if p == provider.CustomOpenAI {
			m, ok := matchCustomModel(list, provider.AdvertisedID(rest, provider.CustomOpenAI))
			if !ok || m.Provider != provider.CustomOpenAI {
				return "", "", customMatch{}, false
			}
			return p, m.Model, m, true
		}
		return p, rest, customMatch{}, true
	}
	if m, ok := matchCustomModel(list, model); ok {
		if m.Provider != provider.CustomOpenAI {
			return "", "", customMatch{}, false
		}
		return m.Provider, m.Model, m, true
	}
	p := provider.ForModel(model)
	if !provider.Get(p).OpenAIChat {
		return "", "", customMatch{}, false
	}
	return p, stripPickerAlias(provider.WireModel(model)), customMatch{}, true
}

// openAIError writes an OpenAI-shaped error body.
func openAIError(w http.ResponseWriter, status int, kind, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": kind, "code": code},
	})
}

// serveOpenAI dispatches the OpenAI-compatible API.
func (h *Handler) serveOpenAI(w http.ResponseWriter, r *http.Request, start time.Time, body []byte) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == OpenAIBasePath+"/models":
		h.serveOpenAIModels(w, r)
	case r.Method == http.MethodPost && r.URL.Path == OpenAIBasePath+"/chat/completions":
		h.serveOpenAIChat(w, r, start, body)
	default:
		openAIError(w, http.StatusNotFound, "invalid_request_error", "unknown_url",
			"proxy: the OpenAI-compatible API serves GET "+OpenAIBasePath+"/models and POST "+OpenAIBasePath+"/chat/completions")
	}
}

// serveOpenAIModels lists, in OpenAI's shape, the models of the shared
// catalogue (see GetCatalogue) that the OpenAI surface can serve. Picker-only
// rows ("[1m]" twins, "-gw" aliases) are dropped: they exist for Claude Code's
// context accounting and model picker, and an OpenAI client sends the window it
// wants itself.
func (h *Handler) serveOpenAIModels(w http.ResponseWriter, r *http.Request) {
	entries, _, ok := h.GetCatalogue()
	if !ok {
		h.refreshCatalogueOnce(r.Context())
		entries, _, _ = h.GetCatalogue()
	}
	list, _ := creds.List(r.Context(), h.db)
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		if id == "" || strings.HasSuffix(id, "[1m]") || stripPickerAlias(id) != id {
			continue
		}
		p, wire, _, routable := openAIRoute(list, id)
		if !routable {
			continue
		}
		oid := openAIModelID(p, wire)
		if seen[oid] {
			continue
		}
		seen[oid] = true
		m := map[string]any{"id": oid, "object": "model", "created": 0, "owned_by": string(p)}
		if name, _ := e["display_name"].(string); name != "" {
			m["name"] = name
		}
		if n, has := maxInputTokens(e); has && n > 0 {
			m["context_length"] = n
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
}

// serveOpenAIChat forwards one Chat Completions request to the provider that
// serves its model, through the same binding, metering and logging as a
// /v1/messages request.
func (h *Handler) serveOpenAIChat(w http.ResponseWriter, r *http.Request, start time.Time, body []byte) {
	requested := requestModel(body)
	if requested == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "proxy: the request names no model")
		return
	}
	list, err := creds.List(r.Context(), h.db)
	if err != nil {
		openAIError(w, http.StatusBadGateway, "api_error", "", "proxy: "+err.Error())
		return
	}
	prov, wire, custom, ok := openAIRoute(list, requested)
	if !ok {
		openAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"proxy: model "+requested+" is not served by the OpenAI-compatible API; see GET "+OpenAIBasePath+"/models")
		return
	}
	if h.enforceUserLimit(w, r, start, int64(len(body))) {
		return
	}
	if rewritten, changed := setRequestModel(body, wire); changed {
		body = rewritten
	}
	body = withStreamUsage(body)

	dr := router.Derive(r, body)
	convID := pool.KeyScoped(dr.ConvID, prov, custom.Model)
	lease, err := h.pool.AcquireScoped(r.Context(), dr.ConvID, prov, custom.Model, custom.CredIDs, pool.RequestOptions{})
	if err != nil {
		h.log.Warn("bind failed", "err", err, "conv", convID, "provider", string(prov), "api", "openai")
		h.failBind(w, err, prov)
		return
	}
	defer lease.Release(false)
	cred := lease.Credential
	h.log.Info("bind", "conv", convID, "src", string(dr.Source), "new", lease.IsNew,
		"provider", string(prov), "cred", cred.ID, "label", cred.Label, "path", r.URL.Path, "api", "openai")
	if err := creds.MarkRequest(r.Context(), h.db, cred.ID); err != nil {
		h.log.Error("counter bump", "err", err, "cred", cred.ID)
	}

	result := h.forward(w, r, body, cred, true, false, false)
	usage := result.usage
	if usage.Model == "" {
		usage.Model = wire
	}
	latency := time.Since(start)
	h.log.Info("forwarded", "cred", cred.ID, "label", cred.Label, "conv", convID,
		"status", result.status, "latency_ms", latency.Milliseconds(), "api", "openai",
		"model", usage.Model, "requested_model", requested,
		"tokens_in", usage.InputTokens, "tokens_out", usage.OutputTokens)
	h.logRequest(r.Context(), r.URL.Path, convID, cred.ID, result.status, int64(len(body)), result.rxBytes, latency, usage)
}

// withStreamUsage asks a streaming Chat Completions upstream to report usage.
// OpenAI streams carry no usage unless stream_options.include_usage is set, and
// without it every streamed request would be metered as zero tokens — which
// per-user limits would read as free. The extra final chunk (empty choices,
// usage set) is part of the standard protocol. Non-stream requests and bodies
// that already decide include_usage are left alone.
func withStreamUsage(body []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	var stream bool
	if json.Unmarshal(obj["stream"], &stream) != nil || !stream {
		return body
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := obj["stream_options"]; ok {
		if json.Unmarshal(raw, &opts) != nil {
			return body
		}
		if _, set := opts["include_usage"]; set {
			return body
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	obj["stream_options"] = encoded
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
