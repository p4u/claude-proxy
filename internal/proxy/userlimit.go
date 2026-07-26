package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

// enforceUserLimit rejects a request when the authenticated user is over their
// configured rolling-window usage cap. It returns true when it has written the
// response and the caller must stop.
//
// It runs after the identity is known and BEFORE pool.Bind, so a blocked
// request never touches a credential: no binding, no counters, no status
// change. Only POST /v1/messages is metered (count_tokens is free), the admin
// token is exempt, and users without an active limit incur zero extra queries.
//
// Known limitation: token counts are only known once a response completes, so a
// single large response can overshoot the cap; the NEXT request is what gets
// blocked. The client's max_tokens is deliberately left untouched.
func (h *Handler) enforceUserLimit(w http.ResponseWriter, r *http.Request, start time.Time, txBytes int64) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		return false
	}
	id := usertoken.FromContext(r.Context())
	if id == nil || id.IsAdmin || id.UserTokenID == "" {
		return false
	}
	if !usertoken.HasLimit(id.LimitOutputTokens, id.LimitWindowSeconds) {
		return false
	}

	now := time.Now()
	st, err := usertoken.LimitStatus(r.Context(), h.db, id.UserTokenID,
		id.LimitOutputTokens, id.LimitWindowSeconds, now)
	if err != nil {
		// Fail open: a metering hiccup must not take the proxy down.
		h.log.Error("usage limit check failed; allowing request",
			"user", id.UserTokenID, "err", err)
		return false
	}
	if !st.Blocked {
		return false
	}

	retryAfter := st.RetryAfterSeconds(now)
	msg := st.QuotaMessage(now)
	h.log.Warn("user over usage limit; blocked",
		"user", id.UserTokenID, "name", id.UserName,
		"limit_output_tokens", st.LimitOutputTokens, "usage_output_tokens", st.UsageOutputTokens,
		"window", usertoken.FormatWindow(st.Window),
		"retry_after_s", retryAfter,
		"blocked_until", st.BlockedUntil.UTC().Format(time.RFC3339))

	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "rate_limit_error",
			"message": msg,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	// Distinguishes a proxy-side quota block from an upstream 429; unlike the
	// upstream case, no credential is marked limited.
	w.Header().Set("X-Router-Reason", "user-quota")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)

	// Log the blocked attempt with no credential or conversation attribution so
	// it shows up as an error in per-user stats while contributing no usage.
	h.logRequest(r.Context(), r.URL.Path, "", "", http.StatusTooManyRequests,
		txBytes, 0, time.Since(start), tokenUsage{})
	return true
}
