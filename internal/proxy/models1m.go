package proxy

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
)

// Claude Code's gateway model discovery (CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1)
// issues GET /v1/models at startup and reads only `id` and `display_name` per
// entry — context-window sizes are pinned client-side per model ID, and the 1M
// window is selected with the client-side "[1m]" alias suffix. Anthropic's
// /v1/models never lists those variants, so the proxy appends "<id>[1m]"
// entries for 1M-capable models, making them selectable in the /model picker
// without per-client env overrides.

// oneMillionModels are the model IDs (aliases) with a 1M-token context window
// for which Claude Code understands the "[1m]" suffix. Dated snapshot IDs are
// matched by prefix (e.g. "claude-sonnet-4-6-20260101").
var oneMillionModels = []string{
	"claude-fable-5",
	"claude-mythos-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
}

const modelsCacheTTL = 5 * time.Minute

func has1MVariant(id string) bool {
	for _, m := range oneMillionModels {
		if id == m || strings.HasPrefix(id, m+"-2") {
			return true
		}
	}
	return false
}

// parseModelEntries extracts the "data" array from a /v1/models response.
// Returns ok=false when the body is not a models envelope.
func parseModelEntries(raw []byte) ([]map[string]any, bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}
	data, present := envelope["data"]
	if !present {
		return nil, false
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// augment1M returns entries with "[1m]" picker rows inserted.
//
// The variant goes BEFORE its base model: Claude Code's /model picker collapses
// long gateway lists behind a "+N models" tail, and models without a built-in
// picker row (e.g. Fable) are only reachable through these gateway rows — so
// putting the 1M variant first makes it the visible, default pick instead of
// the 200K bare entry.
func augment1M(entries []map[string]any) []map[string]any {
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		if id, ok := e["id"].(string); ok {
			existing[id] = true
		}
	}

	out := make([]map[string]any, 0, len(entries)*2)
	for _, e := range entries {
		id, ok := e["id"].(string)
		if ok && has1MVariant(id) && !existing[id+"[1m]"] {
			variant := maps.Clone(e)
			variant["id"] = id + "[1m]"
			if dn, ok := e["display_name"].(string); ok {
				variant["display_name"] = dn + " (1M context)"
			}
			out = append(out, variant)
		}
		out = append(out, e)
	}
	return out
}

// advertise rewrites entry IDs into the form clients are offered.
//
// For providers with no AdvertisePrefix this is a no-op. For GLM it prefixes
// each ID so Claude Code's client-side filter (which drops any gateway model
// not matching /^(claude|anthropic)/i) lets it through, and tags the display
// name so the picker still shows plainly which upstream serves it — the
// prefix is a transport detail, not a claim that this is a Claude model.
func advertise(entries []map[string]any, p provider.Provider) []map[string]any {
	if p.AdvertisePrefix == "" {
		return entries
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		id, ok := e["id"].(string)
		if !ok {
			continue
		}
		clone := maps.Clone(e)
		clone["id"] = provider.AdvertisedID(id, p.ID)
		name, _ := e["display_name"].(string)
		if name == "" {
			name = id
		}
		clone["display_name"] = name + " (" + p.Name + ")"
		out = append(out, clone)
	}
	return out
}

// modelsEnvelope renders merged entries as an Anthropic /v1/models response.
//
// The envelope is rebuilt rather than carried over from any one upstream: the
// pagination fields describe the merged list, not whichever provider happened
// to answer first, and Z.AI spells them camelCase (firstId/lastId) while
// Anthropic uses first_id/last_id. Claude Code reads only id and display_name
// per entry, but emitting a coherent envelope keeps the response honest for
// anything else that consumes it.
func modelsEnvelope(entries []map[string]any) ([]byte, error) {
	if entries == nil {
		entries = []map[string]any{}
	}
	env := map[string]any{
		"data":     entries,
		"has_more": false,
	}
	if len(entries) > 0 {
		if id, ok := entries[0]["id"].(string); ok {
			env["first_id"] = id
		}
		if id, ok := entries[len(entries)-1]["id"].(string); ok {
			env["last_id"] = id
		}
	}
	return json.Marshal(env)
}

// augmentModels appends "[1m]" picker entries to a single /v1/models response
// body. Returns nil when the body can't be parsed (caller passes the original
// through). Retained for its tests; the merge path composes the helpers above.
func augmentModels(raw []byte) []byte {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	entries, ok := parseModelEntries(raw)
	if !ok {
		return nil
	}
	data, err := json.Marshal(augment1M(entries))
	if err != nil {
		return nil
	}
	envelope["data"] = data
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return body
}

// modelsCache holds the last augmented /v1/models response so discovery
// requests answer well inside Claude Code's 3-second timeout, and so a stale
// copy can be served when upstream is unreachable.
type modelsCache struct {
	mu   sync.Mutex
	body []byte
	ct   string
	exp  time.Time
}

func (c *modelsCache) get(now time.Time) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body == nil || now.After(c.exp) {
		return nil, "", false
	}
	return c.body, c.ct, true
}

func (c *modelsCache) getStale() ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body, c.ct, c.body != nil
}

func (c *modelsCache) set(body []byte, ct string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body, c.ct, c.exp = body, ct, now.Add(modelsCacheTTL)
}

// bufferedRW captures a forward() response instead of streaming it to the client.
type bufferedRW struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedRW() *bufferedRW {
	return &bufferedRW{header: make(http.Header), status: http.StatusOK}
}

func (b *bufferedRW) Header() http.Header         { return b.header }
func (b *bufferedRW) WriteHeader(status int)      { b.status = status }
func (b *bufferedRW) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedRW) Flush()                      {}

// serveModels handles GET /v1/models: forward upstream via the regular path
// (401 refresh, credential accounting), augment the JSON with [1m] variants,
// and cache the result.
// serveModels answers GET /v1/models with the union of every provider that can
// currently serve traffic.
//
// A provider contributes rows only if it has a usable credential and answers
// successfully. That is the whole mechanism behind "unusable models are not
// offered": with no GLM key configured (or every GLM key revoked), no glm-*
// entry is advertised, so Claude Code never shows a model the proxy would have
// to reject. One provider being down degrades the list rather than failing
// discovery, since a partial picker beats no picker at all.
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, start time.Time) {
	now := time.Now()
	if body, ct, ok := h.modelsCache.get(now); ok {
		h.log.Debug("models served from cache", "bytes", len(body))
		writeModels(w, body, ct)
		return
	}

	list, err := creds.List(r.Context(), h.db)
	if err != nil {
		h.log.Error("models: list credentials", "err", err)
		if body, ct, ok := h.modelsCache.getStale(); ok {
			writeModels(w, body, ct)
			return
		}
		h.failBind(w, err, provider.Default)
		return
	}

	// Without Accept-Encoding, Go's transport negotiates gzip itself and
	// transparently decompresses, so the buffered body is plain JSON.
	r.Header.Del("Accept-Encoding")

	// The client's Anthropic-Version travels with every forward; on providers
	// that translate to Anthropic (the CLIProxyAPI sidecar for Codex) its
	// presence flips the response into an Anthropic-shaped, obfuscated
	// catalogue that hides the real OpenAI model IDs. Strip it here and
	// re-inject only for the Anthropic provider itself (which does need it,
	// or upstream 400s and its catalogue drops out of the merged list).
	originalVersion := r.Header.Get("Anthropic-Version")
	r.Header.Del("Anthropic-Version")

	var (
		merged    []map[string]any
		answered  int
		lastErr   = pool.ErrNoCredentials
		lastRec   *bufferedRW
		lastState int
	)
	for _, p := range provider.All() {
		// Custom hosts have no single catalogue: each credential declares its
		// own, so they are merged from the credential list rather than fetched.
		if p.ID == provider.Custom || p.ID == provider.CustomOpenAI {
			entries := customModels(list, p.ID)
			if len(entries) == 0 {
				continue
			}
			merged = append(merged, advertise(entries, p)...)
			answered++
			lastErr = nil
			h.log.Info("models custom catalogue", "models", len(entries))
			continue
		}
		cred, perr := pickFrom(list, p.ID)
		if perr != nil {
			h.log.Debug("models: provider has no usable credential, omitting its models",
				"provider", string(p.ID))
			continue
		}

		// Providers with no /v1/models (MiMo answers nginx 404) contribute
		// their static catalogue instead. Advertising it is still gated on
		// having a usable credential, so the "unusable models are never
		// offered" guarantee holds identically for them.
		if !p.HasModelsAPI {
			entries := make([]map[string]any, 0, len(p.StaticModels))
			for _, m := range p.StaticModels {
				entries = append(entries, map[string]any{
					"id": m.ID, "display_name": m.DisplayName, "type": "model",
				})
			}
			merged = append(merged, advertise(entries, p)...)
			answered++
			lastErr = nil
			h.log.Info("models static catalogue",
				"provider", string(p.ID), "cred", cred.ID, "models", len(entries))
			continue
		}

		if p.ID == provider.Anthropic {
			v := originalVersion
			if v == "" {
				v = "2023-06-01"
			}
			r.Header.Set("Anthropic-Version", v)
		} else {
			r.Header.Del("Anthropic-Version")
		}

		rec := newBufferedRW()
		status, rxBytes, _, _ := h.forward(rec, r, nil, cred, true, false)
		h.logRequest(r.Context(), r.URL.Path, "", cred.ID, status, 0, rxBytes, time.Since(start), tokenUsage{})

		if status != http.StatusOK {
			h.log.Warn("models: provider fetch failed, omitting its models",
				"provider", string(p.ID), "cred", cred.ID, "status", status)
			lastRec, lastState = rec, status
			continue
		}
		entries, ok := parseModelEntries(rec.body.Bytes())
		if !ok {
			h.log.Warn("models: provider response not parseable, omitting its models",
				"provider", string(p.ID), "bytes", rec.body.Len())
			continue
		}
		if p.Augment1M {
			entries = augment1M(entries)
		}
		entries = advertise(entries, p)
		merged = append(merged, entries...)
		answered++
		lastErr = nil
		h.log.Info("models discovery",
			"provider", string(p.ID), "cred", cred.ID, "label", cred.Label,
			"models", len(entries), "bytes_received", rxBytes)
	}

	if answered == 0 {
		// Nothing usable anywhere: prefer a stale copy so client discovery
		// still succeeds, then the upstream's own error, then a clean 503.
		if body, ct, ok := h.modelsCache.getStale(); ok {
			h.log.Warn("models: no provider answered, serving stale cache")
			writeModels(w, body, ct)
			return
		}
		if lastRec != nil && lastState > 0 && lastState < 500 {
			replayBuffered(w, lastRec)
			return
		}
		h.failBind(w, lastErr, provider.Default)
		return
	}

	body, merr := modelsEnvelope(merged)
	if merr != nil {
		h.log.Error("models: encode merged envelope", "err", merr)
		h.failBind(w, merr, provider.Default)
		return
	}
	h.log.Info("models merged",
		"providers", answered, "models", len(merged),
		"latency_ms", time.Since(start).Milliseconds())

	h.modelsCache.set(body, "application/json", now)
	writeModels(w, body, "application/json")
}

func writeModels(w http.ResponseWriter, body []byte, ct string) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func replayBuffered(w http.ResponseWriter, rec *bufferedRW) {
	for k, vs := range rec.header {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
}
