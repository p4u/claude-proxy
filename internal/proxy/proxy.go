package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/router"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// maxBodyBytes is a hard ceiling on the buffered request body.
const maxBodyBytes = 16 << 20 // 16 MiB

type Handler struct {
	db        *store.DB
	pool      *pool.Pool
	refresher *creds.Refresher
	log       *slog.Logger
	client    *http.Client

	// Augment1M appends "[1m]" variant entries to GET /v1/models responses so
	// Claude Code's gateway model discovery can offer the 1M-context models
	// (see models1m.go). Enabled by default; disable via CLAUDE_PROXY_MODELS_1M=0.
	Augment1M   bool
	modelsCache modelsCache

	// PromptRetentionDays gates prompt capture: when >0 the LAST user prompt of
	// each POST /v1/messages request is stored in prompt_log (see
	// promptcapture.go); 0 disables capture entirely. Set from
	// CLAUDE_PROXY_PROMPT_RETENTION_DAYS at startup.
	PromptRetentionDays int
}

func New(db *store.DB, p *pool.Pool, r *creds.Refresher, log *slog.Logger) *Handler {
	return &Handler{
		db:        db,
		pool:      p,
		refresher: r,
		log:       log,
		client: &http.Client{
			Timeout: 0, // streaming responses; rely on context
		},
		Augment1M: true,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !shouldForward(r.URL.Path) {
		h.log.Debug("not forwardable", "path", r.URL.Path, "method", r.Method)
		http.NotFound(w, r)
		return
	}
	start := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	r.Body.Close()
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if h.Augment1M && r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		h.serveModels(w, r, start)
		return
	}
	h.log.Debug("request",
		"method", r.Method, "path", r.URL.Path,
		"remote", r.RemoteAddr, "bytes", len(body),
		"client_auth", maskBearer(r.Header.Get("Authorization")),
		"ua", r.Header.Get("User-Agent"),
		"beta", r.Header.Get("Anthropic-Beta"))

	// Prompt-suggestion traffic the user opted out of: answered locally, before
	// metering and before any credential is bound (see suggestions.go).
	if h.blockSuggestion(w, r, body, start) {
		return
	}

	// Per-user usage cap: checked before any credential is bound so a blocked
	// request never touches credential state (see userlimit.go).
	if h.enforceUserLimit(w, r, start, int64(len(body))) {
		return
	}

	// Which upstream serves this request is decided by the model the client
	// asked for, before any credential is chosen — a GLM model can only be
	// served by a GLM key, and vice versa.
	prov := providerFor(r, body)

	var (
		cred    *creds.Credential
		convID  string
		convSrc router.Source
	)
	if needsSticky(r) {
		dr := router.Derive(r, body)
		convSrc = dr.Source
		// The stored conversation key is provider-scoped so a session that
		// mixes providers (a GLM main model alongside Claude Code's built-in
		// haiku background calls) keeps one stable binding per provider
		// instead of the two fighting over a single pin. Logged and recorded
		// in the qualified form so request_log, prompt_log and conversations
		// all agree on one identifier.
		convID = pool.Key(dr.ConvID, prov)
		c, isNew, err := h.pool.Bind(r.Context(), dr.ConvID, prov)
		if err != nil {
			h.log.Warn("bind failed", "err", err, "conv", convID, "src", string(convSrc), "provider", string(prov))
			h.failBind(w, err, prov)
			return
		}
		cred = c
		h.log.Info("bind",
			"conv", convID, "src", string(convSrc), "new", isNew,
			"provider", string(prov),
			"cred", cred.ID, "label", cred.Label,
			"sub", cred.SubscriptionType, "weight", cred.Weight,
			"status", string(cred.Status),
			"req_count", cred.RequestCount, "path", r.URL.Path)
	} else {
		c, err := h.pickForProvider(r.Context(), prov)
		if err != nil {
			h.failBind(w, err, prov)
			return
		}
		cred = c
		h.log.Info("non-sticky pick",
			"cred", cred.ID, "label", cred.Label, "provider", string(prov),
			"sub", cred.SubscriptionType, "path", r.URL.Path)
	}

	if err := creds.MarkRequest(r.Context(), h.db, cred.ID); err != nil {
		h.log.Error("counter bump", "err", err, "cred", cred.ID)
	}

	// Capture the user's prompt for POST /v1/messages when prompt logging is
	// enabled, plus the full message history for users opted into full capture.
	// No-op otherwise.
	replySeq, captureReply := -1, false
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
		h.capturePrompt(r.Context(), convID, body)
		if seq, ok := h.captureConversation(r.Context(), convID, body); ok {
			replySeq, captureReply = seq, true
		}
	}

	status, rxBytes, usage, reply := h.forward(w, r, body, cred, true, captureReply)
	latency := time.Since(start)
	h.log.Info("forwarded",
		"cred", cred.ID, "label", cred.Label,
		"conv", convID, "status", status,
		"latency_ms", latency.Milliseconds(),
		"bytes_sent", len(body), "bytes_received", rxBytes,
		"model", usage.Model,
		"tokens_in", usage.InputTokens, "tokens_out", usage.OutputTokens)

	if captureReply && status == http.StatusOK {
		h.captureAssistantReply(r.Context(), convID, replySeq, usage.Model, reply)
	}

	h.logRequest(r.Context(), r.URL.Path, convID, cred.ID, status, int64(len(body)), rxBytes, latency, usage)
}

// decodeBodySnippet decompresses a body if it was sent gzip/deflate, then
// trims/normalizes for log display. Returns a printable string.
func decodeBodySnippet(raw []byte, encoding string) string {
	body := raw
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			if d, derr := io.ReadAll(io.LimitReader(zr, 4096)); derr == nil {
				body = d
			}
			zr.Close()
		}
	}
	s := string(body)
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	// Strip trailing whitespace; collapse newlines so log line stays one row.
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func maskBearer(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 16 {
		return "***"
	}
	return v[:10] + "…" + v[len(v)-4:]
}

func shouldForward(p string) bool {
	switch {
	case p == "/v1/messages",
		p == "/v1/messages/count_tokens",
		p == "/v1/models",
		strings.HasPrefix(p, "/v1/"):
		return true
	}
	return false
}

func needsSticky(r *http.Request) bool {
	return r.Method == "POST" && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens")
}

func (h *Handler) failBind(w http.ResponseWriter, err error, prov provider.ID) {
	switch {
	case errors.Is(err, pool.ErrNoCredentials):
		// Name the provider. With more than one upstream configured, "no active
		// credentials" is ambiguous and actively misleading: the pool may be
		// full of healthy Anthropic subscriptions and still have nothing that
		// can serve a GLM model. The operator needs to know which one to fix.
		p := provider.Get(prov)
		w.Header().Set("X-Router-Reason", "provider-unavailable")
		http.Error(w, `{"type":"error","error":{"type":"overloaded_error","message":"proxy: no active `+p.Name+` credentials"}}`, http.StatusServiceUnavailable)
	case errors.Is(err, pool.ErrCredentialOrphaned):
		w.Header().Set("X-Router-Reason", "credential-orphaned")
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"proxy: pinned credential revoked; re-import"}}`, http.StatusServiceUnavailable)
	default:
		http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
	}
}

// forward sends the request upstream and streams the response back. Returns the
// upstream HTTP status (or -1 on local failure), bytes received from upstream,
// the parsed token usage, and — when captureText is set — the assistant's
// visible output text (for full conversation capture). If allowRetry is true
// and upstream returns 401, the proxy refreshes the credential and retries once.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte, cred *creds.Credential, allowRetry, captureText bool) (int, int64, tokenUsage, string) {
	// The upstream is a property of the credential, not of the proxy. GLM's
	// base URL carries a path prefix (/api/anthropic), so this concatenates
	// onto the base rather than swapping a host.
	up := provider.Get(credProvider(cred))
	upstreamURL := up.BaseURL + r.URL.RequestURI()

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build upstream req", http.StatusBadGateway)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		return -1, 0, tokenUsage{}, ""
	}

	copyHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Host = upstreamReq.URL.Host
	upstreamReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	upstreamReq.Header.Del("X-Api-Key")
	for k := range upstreamReq.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-router-") {
			upstreamReq.Header.Del(k)
		}
	}

	h.log.Debug("upstream send",
		"cred", cred.ID, "label", cred.Label,
		"auth", maskBearer("Bearer "+cred.AccessToken))

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		h.log.Error("upstream transport error", "cred", cred.ID, "err", err)
		return -1, 0, tokenUsage{}, ""
	}

	h.log.Debug(
		"upstream resp",
		"cred", cred.ID, "label", cred.Label, "status", resp.StatusCode,
		"req_id", resp.Header.Get("Request-Id"),
		"rl_tokens_remaining", resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining"),
		"rl_requests_remaining", resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"),
	)

	if resp.StatusCode == http.StatusUnauthorized && allowRetry && !up.Refreshable {
		// Static API key: a 401 means the key is wrong or revoked, not stale.
		// Retrying after a "refresh" would be a no-op that burns a second
		// request and reports a misleading OAuth failure, so fail immediately
		// and say what actually needs fixing.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		_ = creds.SetStatus(r.Context(), h.db, cred.ID, creds.StatusRevoked)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		h.log.Error("upstream 401 on API key; marked revoked",
			"cred", cred.ID, "label", cred.Label, "provider", string(up.ID),
			"snippet", decodeBodySnippet(raw, resp.Header.Get("Content-Encoding")))
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"proxy: `+up.Name+` API key rejected; replace it"}}`, http.StatusBadGateway)
		return 401, 0, tokenUsage{}, ""
	}

	if resp.StatusCode == http.StatusUnauthorized && allowRetry {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		h.log.Warn("upstream 401; attempting refresh",
			"cred", cred.ID,
			"snippet", decodeBodySnippet(raw, resp.Header.Get("Content-Encoding")))
		fresh, rerr := h.refresher.RefreshNow(r.Context(), cred.ID)
		if rerr != nil {
			_ = creds.SetStatus(r.Context(), h.db, cred.ID, creds.StatusExpired)
			_ = creds.MarkError(r.Context(), h.db, cred.ID)
			h.log.Error("refresh failed after 401; marked expired", "cred", cred.ID, "err", rerr)
			http.Error(w, "proxy: credential expired", http.StatusBadGateway)
			return 401, 0, tokenUsage{}, ""
		}
		h.log.Info("refresh succeeded; retrying upstream", "cred", cred.ID)
		return h.forward(w, r, body, fresh, false, captureText)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		// Peek at the body so we can log the error message, then replay it for the client.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), strings.NewReader("")))

		retryAt := parseRetryAfter(resp.Header.Get("Retry-After"))
		retryIn := time.Until(retryAt).Round(time.Second)
		// Synthesize Retry-After when upstream omitted it so the client knows when to retry.
		if resp.Header.Get("Retry-After") == "" {
			resp.Header.Set("Retry-After", strconv.Itoa(int(retryIn.Seconds())))
		}
		_ = creds.MarkLimited(r.Context(), h.db, cred.ID, retryAt)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		h.log.Warn(
			"upstream 429; marked credential limited",
			"cred", cred.ID,
			"retry_in", retryIn.String(),
			"retry_after", retryAt.Format(time.RFC3339),
			"body", decodeBodySnippet(raw, resp.Header.Get("Content-Encoding")),
			"rl_tokens_remaining", resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining"),
			"rl_tokens_reset", resp.Header.Get("Anthropic-Ratelimit-Tokens-Reset"),
			"rl_requests_remaining", resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"),
			"rl_requests_reset", resp.Header.Get("Anthropic-Ratelimit-Requests-Reset"),
		)
	} else if resp.StatusCode == http.StatusOK {
		_ = creds.MarkSuccess(r.Context(), h.db, cred.ID)
	} else if resp.StatusCode >= 400 {
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
	}

	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Tee the response body into a usage parser only for successful message
	// responses; other statuses carry no billable usage worth parsing.
	var cap *usageCapture
	if resp.StatusCode == http.StatusOK {
		cap = newUsageCapture(resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), captureText)
	}

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var rxBytes int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				h.log.Warn("client write error", "err", werr, "cred", cred.ID, "streamed", rxBytes)
				u, text := closeUsage(cap)
				return resp.StatusCode, rxBytes, u, text
			}
			if cap != nil {
				cap.Write(buf[:n])
			}
			rxBytes += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			h.log.Debug("stream done",
				"cred", cred.ID, "label", cred.Label, "status", resp.StatusCode, "bytes", rxBytes)
			u, text := closeUsage(cap)
			return resp.StatusCode, rxBytes, u, text
		}
		if rerr != nil {
			h.log.Warn("upstream stream error", "err", rerr, "cred", cred.ID, "streamed", rxBytes)
			u, text := closeUsage(cap)
			return resp.StatusCode, rxBytes, u, text
		}
	}
}

// closeUsage finalizes a capture (nil-safe) and returns the parsed usage plus
// any accumulated assistant text.
func closeUsage(c *usageCapture) (tokenUsage, string) {
	if c == nil {
		return tokenUsage{}, ""
	}
	u := c.Close()
	return u, c.Text()
}

// logRequest inserts one row into request_log for dashboard aggregation.
func (h *Handler) logRequest(ctx context.Context, path, convID, credID string, status int, txBytes, rxBytes int64, latency time.Duration, usage tokenUsage) {
	id := usertoken.FromContext(ctx)
	var userTokenID *string
	if id != nil && id.UserTokenID != "" {
		userTokenID = &id.UserTokenID
	}
	_, _ = h.db.ExecContext(ctx, `
		INSERT INTO request_log
		  (user_token_id, credential_id, conv_id, ts, path, status_code,
		   bytes_sent, bytes_received, latency_ms,
		   model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userTokenID, credID, convID, time.Now().Unix(), path, status,
		txBytes, rxBytes, latency.Milliseconds(),
		usage.Model, usage.InputTokens, usage.OutputTokens,
		usage.CacheCreationTokens, usage.CacheReadTokens)
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		if strings.EqualFold(k, "Authorization") {
			continue // we set this ourselves
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func isHopByHop(k string) bool {
	return hopByHop[http.CanonicalHeaderKey(k)]
}

// parseRetryAfter handles both delta-seconds and HTTP-date forms.
// Default when the header is absent: 30s — short-term burst limits clear quickly.
func parseRetryAfter(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Now().Add(30 * time.Second)
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Now().Add(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return t
	}
	return time.Now().Add(30 * time.Second)
}
