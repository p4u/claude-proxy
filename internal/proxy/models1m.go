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

// augmentModels appends "[1m]" picker entries to a /v1/models response body.
// Returns nil when the body can't be parsed (caller passes the original through).
func augmentModels(raw []byte) []byte {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	var entries []map[string]any
	if err := json.Unmarshal(envelope["data"], &entries); err != nil {
		return nil
	}

	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		if id, ok := e["id"].(string); ok {
			existing[id] = true
		}
	}

	// The [1m] variant is inserted BEFORE its base model: Claude Code's /model
	// picker collapses long gateway lists behind a "+N models" tail, and models
	// without a built-in picker row (e.g. Fable) are only reachable through
	// these gateway rows — putting the 1M variant first makes it the visible,
	// default pick instead of the 200K bare entry.
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

	data, err := json.Marshal(out)
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
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, start time.Time) {
	now := time.Now()
	if body, ct, ok := h.modelsCache.get(now); ok {
		h.log.Debug("models served from cache", "bytes", len(body))
		writeModels(w, body, ct)
		return
	}

	cred, err := h.pickAny(r.Context())
	if err != nil {
		// No usable credential: serve a stale copy if we have one so client
		// discovery still succeeds.
		if body, ct, ok := h.modelsCache.getStale(); ok {
			h.log.Warn("models: no credential, serving stale cache", "err", err)
			writeModels(w, body, ct)
			return
		}
		h.failBind(w, err)
		return
	}

	// Without Accept-Encoding, Go's transport negotiates gzip itself and
	// transparently decompresses, so the buffered body is plain JSON.
	r.Header.Del("Accept-Encoding")

	rec := newBufferedRW()
	status, rxBytes, _ := h.forward(rec, r, nil, cred, true)
	latency := time.Since(start)
	h.log.Info("models discovery",
		"cred", cred.ID, "label", cred.Label, "status", status,
		"latency_ms", latency.Milliseconds(), "bytes_received", rxBytes)
	h.logRequest(r.Context(), r.URL.Path, "", cred.ID, status, 0, rxBytes, latency, tokenUsage{})

	if status != http.StatusOK {
		// Serve stale cache on upstream/transport failures; pass 4xx through.
		if status < 0 || status >= 500 {
			if body, ct, ok := h.modelsCache.getStale(); ok {
				h.log.Warn("models: upstream failure, serving stale cache", "status", status)
				writeModels(w, body, ct)
				return
			}
		}
		replayBuffered(w, rec)
		return
	}

	body := rec.body.Bytes()
	if augmented := augmentModels(body); augmented != nil {
		body = augmented
	} else {
		h.log.Warn("models: response not augmentable, passing through", "bytes", len(body))
	}
	ct := rec.header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	h.modelsCache.set(body, ct, now)
	writeModels(w, body, ct)
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
