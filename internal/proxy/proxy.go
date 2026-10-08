package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/codexgateway"
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

	// RebalanceSessions enables announced, conservative migration of long-lived
	// Anthropic pins. Emergency failover is independent of this setting.
	RebalanceSessions bool

	// Sidecar, when set, supplies display names and context sizes for the
	// CLIProxyAPI-backed providers' GET /v1/models rows (see
	// enrichFromSidecar). Optional: without it those rows are named by ID.
	Sidecar *codexgateway.Client
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
		Augment1M:         true,
		RebalanceSessions: true,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !shouldForward(r.URL.Path) {
		h.log.Debug("not forwardable", "path", r.URL.Path, "method", r.Method)
		http.NotFound(w, r)
		return
	}
	start := time.Now()
	notice := &rebalanceWriter{ResponseWriter: w}
	w = notice

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

	// Keep the client-selected model even if alias rewriting changes the wire
	// name or an error response has no usage/model fields to parse.
	requestedModel := requestModel(body)

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

	// A custom host declares its own model names, so placing one means asking
	// the database which credential serves it. Only /v1/messages-shaped
	// requests carry a model, and only they can match.
	var custom customMatch
	if prov == provider.Default && needsSticky(r) {
		if list, lerr := creds.List(r.Context(), h.db); lerr == nil {
			if m, ok := matchCustomModel(list, requestModel(body)); ok {
				custom, prov = m, m.Provider
			}
		}
	}

	// Undo the advertising alias before anything else reads the body, so the
	// prompt/conversation capture records the real model rather than the
	// claude-prefixed name this proxy invented for the picker.
	if prov == provider.Custom || prov == provider.CustomOpenAI {
		// The host declared this name, so send exactly that — the alias the
		// picker showed is ours, not something the upstream would recognise.
		if rewritten, ok := setRequestModel(body, custom.Model); ok {
			body = rewritten
		}
		h.log.Debug("custom model resolved", "wire_model", custom.Model, "creds", len(custom.CredIDs))
	} else if rewritten, wire, ok := rewriteModel(body); ok {
		body = rewritten
		h.log.Debug("model alias resolved", "wire_model", wire, "provider", string(prov))
	}
	if stripped, ok := stripSystemText(body, strippedSystemPhrases); ok {
		body = stripped
		h.log.Debug("system prompt phrase removed", "provider", string(prov))
	}

	var (
		cred           *creds.Credential
		convID         string
		convSrc        router.Source
		captureSummary bool
		summaryProof   pool.SummaryProof
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
		// Custom bindings are additionally scoped by model: two custom hosts
		// share the provider but serve disjoint models, so one pin per
		// (conversation, provider) could hand a request to a host that cannot
		// serve it. See pool.BindScoped.
		convID = pool.KeyScoped(dr.ConvID, prov, custom.Model)
		directSubscription := provider.Get(prov).PollsUsage
		// Account affinity is safety state, not an elective-rebalance setting:
		// latch it even while rebalancing is disabled, before emergency binding.
		accountBound := directSubscription && !portableRebalanceRequest(body)
		opts := pool.RequestOptions{
			Rebalance:    h.RebalanceSessions && directSubscription && !accountBound,
			ObserveOnly:  r.URL.Path == "/v1/messages/count_tokens" || strings.Contains(requestModel(body), "haiku"),
			AccountBound: accountBound,
		}
		if opts.Rebalance {
			opts.Compaction, opts.ObserveOnly = classifyCompaction(r, body, dr.Source)
			captureSummary = opts.Compaction.Phase == pool.CompactionSummary && opts.Compaction.Request != ([32]byte{})
		}
		lease, err := h.pool.AcquireScoped(r.Context(), dr.ConvID, prov, custom.Model, custom.CredIDs, opts)
		if err != nil {
			h.log.Warn("bind failed", "err", err, "conv", convID, "src", string(convSrc), "provider", string(prov))
			h.failBind(w, err, prov)
			return
		}
		defer func() {
			if r.Context().Err() != nil {
				summaryProof = pool.SummaryProof{}
			}
			lease.ReleaseSummary(notice.pendingEmitted() && r.Context().Err() == nil, summaryProof)
		}()
		notice.state = lease.Rebalance
		cred = lease.Credential
		h.log.Info("bind",
			"conv", convID, "src", string(convSrc), "new", lease.IsNew,
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

	result := h.forward(w, r, body, cred, true, captureReply, captureSummary)
	result.complete = result.complete && !notice.failed && r.Context().Err() == nil
	notice.complete = result.complete
	if captureSummary && result.status == http.StatusOK && result.complete {
		summaryProof = result.summary
	}
	usage := result.usage
	if usage.Model == "" {
		usage.Model = requestedModel
	}
	latency := time.Since(start)
	h.log.Info("forwarded",
		"cred", cred.ID, "label", cred.Label,
		"conv", convID, "status", result.status,
		"latency_ms", latency.Milliseconds(),
		"bytes_sent", len(body), "bytes_received", result.rxBytes,
		"model", usage.Model, "requested_model", requestedModel, "response_model", result.usage.Model,
		"response_complete", result.complete, "error_type", result.errorType,
		"tokens_in", usage.InputTokens, "tokens_out", usage.OutputTokens)

	if captureReply && result.status == http.StatusOK {
		h.captureAssistantReply(r.Context(), convID, replySeq, usage.Model, result.text)
	}

	h.logRequest(r.Context(), r.URL.Path, convID, cred.ID, result.status, int64(len(body)), result.rxBytes, latency, usage)
}

const errorBodyCap = 4096

// upstreamErrorType extracts only a bounded, recognized error type. Messages
// can contain prompts or credential material; neither they nor arbitrary
// upstream-provided type strings belong in diagnostic logs.
func upstreamErrorType(raw []byte, encoding string) string {
	if isGzip(encoding) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "unknown"
		}
		defer zr.Close()
		raw, err = io.ReadAll(io.LimitReader(zr, errorBodyCap))
		if err != nil {
			return "unknown"
		}
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "unknown"
	}
	return safeErrorType(body.Error.Type)
}

func safeErrorType(kind string) string {
	switch kind {
	case "invalid_request_error", "authentication_error", "permission_error",
		"not_found_error", "request_too_large", "rate_limit_error", "api_error",
		"overloaded_error", "timeout_error", "billing_error":
		return kind
	default:
		return "unknown"
	}
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

// forwardResult separates transport/protocol completion from the HTTP status:
// a 200 stream can still fail after its headers have already reached the client.
// complete requires a successful response read and written in full, plus parser
// confirmation when a Messages response is inspected (including message_stop
// and no error event for SSE). Partial responses are never retried.
type forwardResult struct {
	status    int // upstream status, or -1 on local failure
	rxBytes   int64
	usage     tokenUsage
	text      string            // opt-in assistant text capture only
	summary   pool.SummaryProof // digest only; independent of conversation capture
	complete  bool
	errorType string // sanitized upstream error type, never the error message
}

// forward sends the request upstream and streams the response back. If
// allowRetry is true and upstream returns 401, it refreshes the credential and
// retries once, before any response bytes have been sent to the client.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte, cred *creds.Credential, allowRetry, captureText, captureSummary bool) (result forwardResult) {
	// The upstream is a property of the credential, not of the proxy. GLM's
	// base URL carries a path prefix (/api/anthropic), so this concatenates
	// onto the base rather than swapping a host.
	up := provider.Get(credProvider(cred))
	requestURI := r.URL.RequestURI()
	openAIStream := false
	if up.ID == provider.CustomOpenAI {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			payload, _ := json.Marshal(map[string]any{"input_tokens": approximateAnthropicTokens(body)})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			n, err := w.Write(payload)
			_ = creds.MarkSuccess(r.Context(), h.db, cred.ID)
			return forwardResult{status: http.StatusOK, rxBytes: int64(n), complete: err == nil && n == len(payload)}
		case "/v1/messages":
			translated, stream, err := translateAnthropicToOpenAI(body)
			if err != nil {
				http.Error(w, "translate request: "+err.Error(), http.StatusBadRequest)
				_ = creds.MarkError(r.Context(), h.db, cred.ID)
				return forwardResult{status: http.StatusBadRequest}
			}
			body, openAIStream = translated, stream
			requestURI = "/chat/completions"
		default:
			http.Error(w, "proxy: custom OpenAI hosts support /v1/messages and /v1/messages/count_tokens", http.StatusNotImplemented)
			return forwardResult{status: http.StatusNotImplemented}
		}
	}
	upstreamURL := provider.ResolveBaseURL(up.ID, cred.BaseURL) + requestURI

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build upstream req", http.StatusBadGateway)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		return forwardResult{status: -1}
	}

	copyHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Host = upstreamReq.URL.Host
	if cred.AccessToken != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		upstreamReq.Header.Del("Authorization")
	}
	upstreamReq.Header.Del("X-Api-Key")
	if up.ID == provider.CustomOpenAI {
		upstreamReq.Header.Del("Anthropic-Version")
		upstreamReq.Header.Del("Anthropic-Beta")
		// Translation needs a plain response body. With this header absent Go's
		// transport can negotiate and transparently decompress gzip itself.
		upstreamReq.Header.Del("Accept-Encoding")
	}
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
		return forwardResult{status: -1}
	}
	if up.ID == provider.CustomOpenAI {
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
			_ = resp.Body.Close()
			raw = translateOpenAIError(raw, resp.StatusCode)
			resp.Body = io.NopCloser(bytes.NewReader(raw))
			resp.ContentLength = int64(len(raw))
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
			resp.Header.Del("Content-Encoding")
		} else if openAIStream {
			resp.Body = translateOpenAIStream(r.Context(), resp.Body)
			resp.ContentLength = -1
			resp.Header.Set("Content-Type", "text/event-stream")
			resp.Header.Del("Content-Length")
			resp.Header.Del("Content-Encoding")
		} else {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
			_ = resp.Body.Close()
			if readErr != nil || len(raw) > maxBodyBytes {
				http.Error(w, "translate response: response too large or unreadable", http.StatusBadGateway)
				_ = creds.MarkError(r.Context(), h.db, cred.ID)
				return forwardResult{status: -1}
			}
			translated, translateErr := translateOpenAIJSON(raw)
			if translateErr != nil {
				http.Error(w, "translate response: "+translateErr.Error(), http.StatusBadGateway)
				_ = creds.MarkError(r.Context(), h.db, cred.ID)
				return forwardResult{status: -1}
			}
			resp.Body = io.NopCloser(bytes.NewReader(translated))
			resp.ContentLength = int64(len(translated))
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Set("Content-Length", strconv.Itoa(len(translated)))
			resp.Header.Del("Content-Encoding")
		}
	}

	h.log.Debug(
		"upstream resp",
		"cred", cred.ID, "label", cred.Label, "status", resp.StatusCode,
		"req_id", resp.Header.Get("Request-Id"),
		"rl_tokens_remaining", resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining"),
		"rl_requests_remaining", resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"),
	)

	if resp.StatusCode == http.StatusUnauthorized && up.DelegatedAuth {
		// The gateway already refreshed and retried every account behind it
		// before answering 401; there is nothing to refresh here, and the
		// local credential is not what was rejected. Relay the error as-is.
		h.log.Warn("upstream 401 from delegated-auth gateway; credential status unchanged",
			"cred", cred.ID, "label", cred.Label, "provider", string(up.ID))
	}

	if resp.StatusCode == http.StatusUnauthorized && allowRetry && !up.Refreshable && !up.DelegatedAuth {
		// Static API key: a 401 means the key is wrong or revoked, not stale.
		// Retrying after a "refresh" would be a no-op that burns a second
		// request and reports a misleading OAuth failure, so fail immediately
		// and say what actually needs fixing.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		errorType := upstreamErrorType(raw, resp.Header.Get("Content-Encoding"))
		_ = creds.SetStatus(r.Context(), h.db, cred.ID, creds.StatusRevoked)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		h.log.Error("upstream 401 on API key; marked revoked",
			"cred", cred.ID, "label", cred.Label, "provider", string(up.ID),
			"error_type", errorType)
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"proxy: `+up.Name+` API key rejected; replace it"}}`, http.StatusBadGateway)
		return forwardResult{status: http.StatusUnauthorized, errorType: errorType}
	}

	if resp.StatusCode == http.StatusUnauthorized && allowRetry && !up.DelegatedAuth {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		errorType := upstreamErrorType(raw, resp.Header.Get("Content-Encoding"))
		h.log.Warn("upstream 401; attempting refresh",
			"cred", cred.ID,
			"error_type", errorType)
		fresh, rerr := h.refresher.RefreshNow(r.Context(), cred.ID)
		if rerr != nil {
			_ = creds.SetStatus(r.Context(), h.db, cred.ID, creds.StatusExpired)
			_ = creds.MarkError(r.Context(), h.db, cred.ID)
			h.log.Error("refresh failed after 401; marked expired", "cred", cred.ID, "err", rerr)
			http.Error(w, "proxy: credential expired", http.StatusBadGateway)
			return forwardResult{status: http.StatusUnauthorized, errorType: errorType}
		}
		h.log.Info("refresh succeeded; retrying upstream", "cred", cred.ID)
		return h.forward(w, r, body, fresh, false, captureText, captureSummary)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAt, retrySource := parseRetryAfter(resp.Header.Get("Retry-After"))
		retryIn := time.Until(retryAt).Round(time.Second)
		// Keep an upstream value unchanged, even if it was unparseable locally.
		// The provenance describes the local delay; synthesized describes the
		// client-facing header. These differ for an invalid upstream value.
		synthesized := resp.Header.Get("Retry-After") == ""
		if synthesized {
			resp.Header.Set("Retry-After", strconv.Itoa(int(retryIn.Seconds())))
		}
		_ = creds.MarkLimited(r.Context(), h.db, cred.ID, retryAt)
		_ = creds.MarkError(r.Context(), h.db, cred.ID)
		// Observe the body during the normal relay, never consume/close it to
		// peek. A diagnostic size cap must not truncate the client's response.
		defer func() {
			h.log.Warn(
				"upstream 429; marked credential limited",
				"cred", cred.ID,
				"retry_in", retryIn.String(),
				"retry_after", retryAt.Format(time.RFC3339),
				"retry_after_source", retrySource, "retry_after_synthesized", synthesized,
				"error_type", result.errorType,
				"rl_tokens_remaining", resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining"),
				"rl_tokens_reset", resp.Header.Get("Anthropic-Ratelimit-Tokens-Reset"),
				"rl_requests_remaining", resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"),
				"rl_requests_reset", resp.Header.Get("Anthropic-Ratelimit-Requests-Reset"),
			)
		}()
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
		cap = newUsageCapture(resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), captureText, captureSummary)
	}

	result.status = resp.StatusCode
	var errorBody []byte
	defer func() {
		if cap != nil {
			result.usage = cap.Close()
			result.text = cap.Text()
			result.summary = cap.Summary()
			result.complete = result.complete && cap.Complete()
			result.errorType = cap.ErrorType()
		} else if resp.StatusCode >= 400 {
			result.errorType = upstreamErrorType(errorBody, resp.Header.Get("Content-Encoding"))
		}
	}()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if resp.StatusCode >= 400 && len(errorBody) < errorBodyCap {
				errorBody = append(errorBody, buf[:min(n, errorBodyCap-len(errorBody))]...)
			}
			nw, werr := w.Write(buf[:n])
			if werr == nil && nw != n {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				h.log.Warn("client write error", "err", werr, "cred", cred.ID, "streamed", result.rxBytes)
				return result
			}
			if cap != nil {
				cap.Write(buf[:n])
			}
			result.rxBytes += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			h.log.Debug("stream done",
				"cred", cred.ID, "label", cred.Label, "status", resp.StatusCode, "bytes", result.rxBytes)
			result.complete = resp.StatusCode >= 200 && resp.StatusCode < 300 && r.Context().Err() == nil &&
				(resp.ContentLength < 0 || result.rxBytes == resp.ContentLength)
			return result
		}
		if rerr != nil {
			h.log.Warn("upstream stream error", "err", rerr, "cred", cred.ID, "streamed", result.rxBytes)
			return result
		}
	}
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

// parseRetryAfter handles both delta-seconds and HTTP-date forms and reports
// whether the local retry time came from upstream or the existing 30s fallback.
// Missing/malformed headers retain that short-term burst-limit fallback.
func parseRetryAfter(v string) (time.Time, string) {
	v = strings.TrimSpace(v)
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Now().Add(time.Duration(secs) * time.Second), "upstream"
	}
	if t, err := http.ParseTime(v); err == nil {
		return t, "upstream"
	}
	return time.Now().Add(30 * time.Second), "fallback"
}
