// Package claudioapi implements the /v1/claudio/* HTTP endpoints for the
// claudio session manager.
//
// The entire /v1/claudio subtree is claimed locally: requests are never
// forwarded upstream, never written to request_log, and never bound to a
// credential. Requests are authenticated through the same AuthMiddleware as
// all other /v1/* traffic, so a valid user token or admin token is required
// when the proxy is configured with auth.
//
// Routes
//
//	GET /v1/claudio            — discovery ({version, capabilities})
//	GET /v1/claudio/config     — recommended Claude Code env vars
//	GET /v1/claudio/models     — augmented model catalogue
//	GET /v1/claudio/me/stats   — per-user request/token statistics
//	GET /v1/claudio/pool/health — coarse per-provider availability
//
// Security isolation
//
//   - /me/stats requires a non-empty, non-admin UserTokenID; anonymous
//     callers and admin tokens receive 403.
//   - /pool/health exposes no credential IDs, labels, percentages or counts —
//     only a coarse status string per provider name.
//   - All caches are keyed by identity so cross-user data is impossible.
//   - A per-identity in-memory token bucket limits query rates to prevent
//     enumeration and abuse.
package claudioapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// Handler mounts all /v1/claudio/* endpoints on the provided mux.
// It holds no upstream connections and never forwards traffic.
type Handler struct {
	db  *store.DB
	cat CatalogueSource

	// poolHealth cache
	poolMu          sync.Mutex
	poolHealthCache []providerHealth
	poolCachedAt    time.Time

	// per-identity rate limiter
	rateMu   sync.Mutex
	rateBkts map[string]*bucket
}

// CatalogueSource is the subset of the proxy.Handler interface that the
// claudio API needs: a way to read the augmented model catalogue.
type CatalogueSource interface {
	// GetCatalogue returns the augmented model entries and the time at which
	// they were last refreshed from upstream. ok is false when no catalogue
	// has been fetched yet (fresh proxy start before the first /v1/models
	// call); callers should treat an empty slice as a valid response.
	GetCatalogue() (entries []map[string]any, refreshedAt time.Time, ok bool)
}

// New creates a Handler and registers all /v1/claudio routes on mux.
//
// mux must be the same http.ServeMux that the proxy already uses so that
// /v1/claudio and /v1/claudio/ are resolved before the /v1/ catch-all.
func New(mux *http.ServeMux, db *store.DB, cat CatalogueSource) *Handler {
	h := &Handler{
		db:       db,
		cat:      cat,
		rateBkts: make(map[string]*bucket),
	}
	// Register both the bare prefix and the slash-terminated prefix.
	// ServeMux routes /v1/claudio and everything under /v1/claudio/ to h,
	// which means the forwarding handler on /v1/ never sees these paths.
	mux.Handle("/v1/claudio", h)
	mux.Handle("/v1/claudio/", h)
	return h
}

// ServeHTTP dispatches to the appropriate sub-handler or returns 404/405.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip trailing slash for matching (GET /v1/claudio/ == GET /v1/claudio).
	path := strings.TrimRight(r.URL.Path, "/")

	// Per-identity rate limiting (1 req/s, burst 10).
	id := identityKey(r)
	if !h.allow(id) {
		writeJSON(w, http.StatusTooManyRequests, apiError("rate_limit_error", "claudio: too many requests"))
		return
	}

	switch path {
	case "/v1/claudio":
		h.methodOnly(w, r, http.MethodGet, h.serveRoot)
	case "/v1/claudio/config":
		h.methodOnly(w, r, http.MethodGet, h.serveConfig)
	case "/v1/claudio/models":
		h.methodOnly(w, r, http.MethodGet, h.serveModels)
	case "/v1/claudio/me/stats":
		h.methodOnly(w, r, http.MethodGet, h.serveStats)
	case "/v1/claudio/pool/health":
		h.methodOnly(w, r, http.MethodGet, h.servePoolHealth)
	default:
		writeJSON(w, http.StatusNotFound, apiError("not_found", "claudio: unknown endpoint "+r.URL.Path))
	}
}

// methodOnly calls fn when the method matches, otherwise writes 405.
func (h *Handler) methodOnly(w http.ResponseWriter, r *http.Request, method string, fn http.HandlerFunc) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeJSON(w, http.StatusMethodNotAllowed, apiError("method_not_allowed", "claudio: method not allowed"))
		return
	}
	fn(w, r)
}

// ---------------------------------------------------------------------------
// GET /v1/claudio
// ---------------------------------------------------------------------------

// rootResponse is returned by GET /v1/claudio.
type rootResponse struct {
	Version      int      `json:"version"`
	Capabilities []string `json:"capabilities"`
}

func (h *Handler) serveRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, rootResponse{
		Version:      1,
		Capabilities: []string{"config", "models", "me/stats", "pool/health"},
	})
}

// ---------------------------------------------------------------------------
// GET /v1/claudio/config
// ---------------------------------------------------------------------------

// configResponse is returned by GET /v1/claudio/config.
type configResponse struct {
	Version           int               `json:"version"`
	Env               map[string]string `json:"env"`
	ModelsRefreshedAt *string           `json:"models_refreshed_at,omitempty"`
}

// defaultEnv is the fixed set of env vars recommended for every claudio session.
// The ANTHROPIC_DEFAULT_*_MODEL values are filled from the live catalogue.
var defaultEnv = map[string]string{
	"CLAUDE_CODE_USE_GATEWAY":                    "1",
	"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
	"CLAUDE_CODE_GATEWAY_HINT_HEADERS":           "1",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":   "1",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW":            "1000000",
	"API_TIMEOUT_MS":                             "30000",
}

// modelFamilyEnvKey maps a detected family name to the env var that sets it.
var modelFamilyEnvKey = map[string]string{
	"fable":  "ANTHROPIC_DEFAULT_FABLE_MODEL",
	"opus":   "ANTHROPIC_DEFAULT_OPUS_MODEL",
	"sonnet": "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"haiku":  "ANTHROPIC_DEFAULT_HAIKU_MODEL",
}

func (h *Handler) serveConfig(w http.ResponseWriter, r *http.Request) {
	env := make(map[string]string, len(defaultEnv)+4)
	for k, v := range defaultEnv {
		env[k] = v
	}

	entries, refreshedAt, ok := h.cat.GetCatalogue()
	var refreshedAtStr *string
	if ok {
		s := refreshedAt.UTC().Format(time.RFC3339)
		refreshedAtStr = &s
		// Pick the first [1m] entry per family; the catalogue is ordered so
		// that the most recently published model comes first (see augment1M
		// in models1m.go, which inserts [1m] rows before their base models,
		// preserving upstream ordering which is newest-first on Anthropic).
		for _, e := range entries {
			id, _ := e["id"].(string)
			if !strings.HasSuffix(id, "[1m]") {
				continue
			}
			fam := modelFamily(id)
			if fam == "" {
				continue
			}
			envKey := modelFamilyEnvKey[fam]
			if _, already := env[envKey]; already {
				continue // keep the first (newest) entry per family
			}
			env[envKey] = id
		}
	}

	writeJSON(w, http.StatusOK, configResponse{
		Version:           1,
		Env:               env,
		ModelsRefreshedAt: refreshedAtStr,
	})
}

// modelFamily extracts the family name (fable/opus/sonnet/haiku) from a model
// ID. The [1m] suffix is stripped before matching. Returns "" for unrecognised
// IDs.
func modelFamily(id string) string {
	base := strings.TrimSuffix(id, "[1m]")
	switch {
	case strings.Contains(base, "-fable-") || strings.Contains(base, "-mythos-"):
		return "fable"
	case strings.Contains(base, "-opus-"):
		return "opus"
	case strings.Contains(base, "-sonnet-"):
		return "sonnet"
	case strings.Contains(base, "-haiku-"):
		return "haiku"
	}
	return ""
}

// ---------------------------------------------------------------------------
// GET /v1/claudio/models
// ---------------------------------------------------------------------------

// modelEntry is one row in the models response.
type modelEntry struct {
	ID                 string `json:"id"`
	DisplayName        string `json:"display_name"`
	Provider           string `json:"provider"`
	Family             string `json:"family,omitempty"`
	MaxInputTokens     int64  `json:"max_input_tokens,omitempty"`
	RecommendedDefault bool   `json:"recommended_default"`
}

// modelsResponse is returned by GET /v1/claudio/models.
type modelsResponse struct {
	Version     int          `json:"version"`
	Data        []modelEntry `json:"data"`
	RefreshedAt *string      `json:"refreshed_at,omitempty"`
}

func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	entries, refreshedAt, ok := h.cat.GetCatalogue()

	// Determine the recommended defaults (first [1m] per family).
	defaults := pickDefaults(entries)

	data := make([]modelEntry, 0, len(entries))
	for _, e := range entries {
		id, _ := e["id"].(string)
		if id == "" {
			continue
		}
		dn, _ := e["display_name"].(string)
		prov := ""
		if p := providerFromID(id); p != "" {
			prov = p
		}
		var maxIn int64
		if n, hasN := maxInputTokensEntry(e); hasN {
			maxIn = n
		}
		data = append(data, modelEntry{
			ID:                 id,
			DisplayName:        dn,
			Provider:           prov,
			Family:             modelFamily(id),
			MaxInputTokens:     maxIn,
			RecommendedDefault: defaults[id],
		})
	}

	var refreshedAtStr *string
	if ok {
		s := refreshedAt.UTC().Format(time.RFC3339)
		refreshedAtStr = &s
	}

	writeJSON(w, http.StatusOK, modelsResponse{
		Version:     1,
		Data:        data,
		RefreshedAt: refreshedAtStr,
	})
}

// pickDefaults returns the set of model IDs that are the recommended default
// for their family: the first [1m] entry per family in catalogue order.
func pickDefaults(entries []map[string]any) map[string]bool {
	seen := make(map[string]bool)
	defaults := make(map[string]bool)
	for _, e := range entries {
		id, _ := e["id"].(string)
		if !strings.HasSuffix(id, "[1m]") {
			continue
		}
		fam := modelFamily(id)
		if fam == "" || seen[fam] {
			continue
		}
		seen[fam] = true
		defaults[id] = true
	}
	return defaults
}

// providerFromID guesses a provider string from the model ID prefix or content.
func providerFromID(id string) string {
	switch {
	case strings.HasPrefix(id, "claude-"):
		return "anthropic"
	case strings.HasPrefix(id, "glm-") || strings.HasPrefix(id, "anthropic/glm-"):
		return "glm"
	case strings.HasPrefix(id, "mimo-") || strings.HasPrefix(id, "anthropic/mimo-"):
		return "mimo"
	case strings.Contains(id, "gemini"):
		return "gemini"
	case strings.Contains(id, "gpt") || strings.Contains(id, "codex"):
		return "codex"
	}
	return ""
}

// maxInputTokensEntry reads max_input_tokens from a catalogue entry.
// JSON numbers decode as float64 through map[string]any.
func maxInputTokensEntry(e map[string]any) (int64, bool) {
	switch v := e["max_input_tokens"].(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// GET /v1/claudio/me/stats
// ---------------------------------------------------------------------------

// statsResponse is returned by GET /v1/claudio/me/stats.
type statsResponse struct {
	Version  int          `json:"version"`
	UserName string       `json:"user_name"`
	Period   string       `json:"period"`
	Totals   statsTotals  `json:"totals"`
	ByModel  []statsModel `json:"by_model"`
	Limit    *limitInfo   `json:"limit,omitempty"`
}

type statsTotals struct {
	Requests      int64 `json:"requests"`
	Errors        int64 `json:"errors"`
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

type statsModel struct {
	Model         string `json:"model"`
	Requests      int64  `json:"requests"`
	Errors        int64  `json:"errors"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheRead     int64  `json:"cache_read"`
	CacheCreation int64  `json:"cache_creation"`
}

type limitInfo struct {
	OutputTokens     int64   `json:"output_tokens"`
	WindowSeconds    int64   `json:"window_seconds"`
	UsedOutputTokens int64   `json:"used_output_tokens"`
	UsedPct          float64 `json:"used_pct"`
	Blocked          bool    `json:"blocked"`
	BlockedUntil     *string `json:"blocked_until,omitempty"`
}

// parsePeriod parses the ?period= query parameter into a time.Duration.
// Accepted values: 24h (default), 7d, 30d.
func parsePeriod(s string) (time.Duration, string, bool) {
	switch s {
	case "", "24h":
		return 24 * time.Hour, "24h", true
	case "7d":
		return 7 * 24 * time.Hour, "7d", true
	case "30d":
		return 30 * 24 * time.Hour, "30d", true
	}
	return 0, "", false
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	id := usertoken.FromContext(r.Context())
	// Admin, anonymous (nil identity), or empty UserTokenID → 403.
	if id == nil || id.IsAdmin || id.UserTokenID == "" {
		writeJSON(w, http.StatusForbidden, apiError("authentication_error",
			"claudio: /me/stats requires a user token identity (admin and anonymous callers not allowed)"))
		return
	}

	period, periodStr, ok := parsePeriod(r.URL.Query().Get("period"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, apiError("invalid_request_error",
			"claudio: period must be 24h, 7d, or 30d"))
		return
	}

	now := time.Now()
	since := now.Add(-period)

	totals, err := queryTotals(r.Context(), h.db, id.UserTokenID, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("api_error", "claudio: stats query failed"))
		return
	}

	byModel, err := queryByModel(r.Context(), h.db, id.UserTokenID, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("api_error", "claudio: by-model stats query failed"))
		return
	}

	resp := statsResponse{
		Version:  1,
		UserName: id.UserName,
		Period:   periodStr,
		Totals:   totals,
		ByModel:  byModel,
	}

	if usertoken.HasLimit(id.LimitOutputTokens, id.LimitWindowSeconds) {
		ls, lerr := usertoken.LimitStatus(r.Context(), h.db, id.UserTokenID,
			id.LimitOutputTokens, id.LimitWindowSeconds, now)
		if lerr == nil && ls.Active {
			li := &limitInfo{
				OutputTokens:     ls.LimitOutputTokens,
				WindowSeconds:    id.LimitWindowSeconds,
				UsedOutputTokens: ls.UsageOutputTokens,
				UsedPct:          ls.UsagePct(),
				Blocked:          ls.Blocked,
			}
			if ls.Blocked {
				s := ls.BlockedUntil.UTC().Format(time.RFC3339)
				li.BlockedUntil = &s
			}
			resp.Limit = li
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// queryTotals aggregates request_log for one user since `since`.
func queryTotals(ctx context.Context, db *store.DB, userID string, since time.Time) (statsTotals, error) {
	var t statsTotals
	err := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status_code>=400 OR status_code=-1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0)
		FROM request_log
		WHERE user_token_id=? AND ts>=?`,
		userID, since.Unix()).
		Scan(&t.Requests, &t.Errors, &t.InputTokens, &t.OutputTokens, &t.CacheRead, &t.CacheCreation)
	return t, err
}

// queryByModel aggregates request_log grouped by model for one user since `since`.
func queryByModel(ctx context.Context, db *store.DB, userID string, since time.Time) ([]statsModel, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			model,
			COUNT(*),
			COALESCE(SUM(CASE WHEN status_code>=400 OR status_code=-1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0)
		FROM request_log
		WHERE user_token_id=? AND ts>=?
		GROUP BY model
		ORDER BY SUM(output_tokens) DESC`,
		userID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []statsModel
	for rows.Next() {
		var m statsModel
		if err := rows.Scan(&m.Model, &m.Requests, &m.Errors,
			&m.InputTokens, &m.OutputTokens, &m.CacheRead, &m.CacheCreation); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// GET /v1/claudio/pool/health
// ---------------------------------------------------------------------------

// Pool health status thresholds.
//
// These match the pool package's saturation semantics: a credential is
// saturated when either its 5-hour or 7-day usage percentage hits 100%.
// For the aggregate per-provider status, the thresholds below apply to the
// fraction of saturated active credentials:
//
//   - ok:          fewer than saturatedBusyThreshold of active creds saturated
//   - busy:        at or above saturatedBusyThreshold but not all saturated
//   - saturated:   every active credential is saturated
//   - unavailable: no active credential exists for this provider
const (
	// saturatedBusyThreshold: when ≥50% of active credentials are saturated,
	// the provider is reported as "busy" rather than "ok".
	saturatedBusyThreshold = 0.5
	// poolHealthCacheTTL is how long the health response is cached.
	poolHealthCacheTTL = 30 * time.Second
)

type providerHealth struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type poolHealthResponse struct {
	Version   int              `json:"version"`
	Providers []providerHealth `json:"providers"`
}

func (h *Handler) servePoolHealth(w http.ResponseWriter, r *http.Request) {
	h.poolMu.Lock()
	if time.Since(h.poolCachedAt) < poolHealthCacheTTL && h.poolHealthCache != nil {
		cached := h.poolHealthCache
		h.poolMu.Unlock()
		writeJSON(w, http.StatusOK, poolHealthResponse{Version: 1, Providers: cached})
		return
	}
	h.poolMu.Unlock()

	providers := h.computePoolHealth(r.Context())

	h.poolMu.Lock()
	h.poolHealthCache = providers
	h.poolCachedAt = time.Now()
	h.poolMu.Unlock()

	writeJSON(w, http.StatusOK, poolHealthResponse{Version: 1, Providers: providers})
}

// computePoolHealth queries the DB for per-provider credential health using
// the same saturation signals the pool uses (latest usage_history entry per
// credential). Only provider names are exposed; credential IDs, labels,
// emails and raw percentages are not included in the result.
func (h *Handler) computePoolHealth(ctx context.Context) []providerHealth {
	// For each well-known provider, count active credentials and how many are
	// saturated according to their most recent usage_history snapshot.
	type provStats struct {
		active    int
		saturated int
	}
	stats := make(map[provider.ID]*provStats)
	for _, p := range provider.All() {
		stats[p.ID] = &provStats{}
	}

	rows, err := h.db.QueryContext(ctx, `
		SELECT c.provider,
		       COALESCE(u.five_hour_pct, 0),
		       COALESCE(u.seven_day_pct, 0)
		FROM credentials c
		LEFT JOIN usage_history u
		  ON u.credential_id = c.id
		  AND u.captured_at = (
		        SELECT MAX(captured_at) FROM usage_history WHERE credential_id = c.id
		      )
		WHERE c.status IN ('active', 'limited')`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var provID provider.ID
			var fhPct, sdPct float64
			if err := rows.Scan(&provID, &fhPct, &sdPct); err != nil {
				continue
			}
			ps := stats[provID]
			if ps == nil {
				continue
			}
			ps.active++
			if pool.Saturated(fhPct, sdPct) {
				ps.saturated++
			}
		}
	}

	out := make([]providerHealth, 0, len(provider.All()))
	for _, p := range provider.All() {
		ps := stats[p.ID]
		var status string
		switch {
		case ps == nil || ps.active == 0:
			status = "unavailable"
		case ps.saturated == ps.active:
			status = "saturated"
		case float64(ps.saturated)/float64(ps.active) >= saturatedBusyThreshold:
			status = "busy"
		default:
			status = "ok"
		}
		out = append(out, providerHealth{Name: string(p.ID), Status: status})
	}
	return out
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// bucket is a simple token-bucket rate limiter. Each identity gets 10 tokens
// initially and refills at 1 token per second, capped at 10.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

const (
	ratePerSecond = 1.0
	rateBurst     = 10.0
)

func (b *bucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens = min(rateBurst, b.tokens+elapsed*ratePerSecond)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allow reports whether the identity is within its rate limit.
func (h *Handler) allow(key string) bool {
	h.rateMu.Lock()
	b, ok := h.rateBkts[key]
	if !ok {
		b = &bucket{tokens: rateBurst, last: time.Now()}
		h.rateBkts[key] = b
	}
	h.rateMu.Unlock()
	return b.take()
}

// identityKey returns a stable string identifying the caller.
func identityKey(r *http.Request) string {
	id := usertoken.FromContext(r.Context())
	if id == nil {
		return "anon"
	}
	if id.IsAdmin {
		return "admin"
	}
	if id.UserTokenID != "" {
		return "user:" + id.UserTokenID
	}
	return "anon"
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// apiError returns a JSON error envelope in the Anthropic style.
func apiError(errType, msg string) map[string]any {
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	}
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"json marshal failed"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
