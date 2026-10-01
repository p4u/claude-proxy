package codexgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Antigravity publishes no quota response headers (unlike Codex's X-Codex-*),
// so a Gemini account's quota is read from Google's own endpoints. The calls go
// through the sidecar's management api-call tool, which substitutes the
// account's access token server-side ($TOKEN$): this proxy never holds a
// Google token.
const (
	antigravityModelsURL = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
	antigravityTierURL   = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	// Google answers 403 PERMISSION_DENIED without an Antigravity client
	// User-Agent. Any version is accepted — older ones only list fewer models,
	// and quota is reported either way — so a fixed one does not go stale.
	antigravityUserAgent = "antigravity/2.18.1 linux/amd64"

	geminiQuotaTTL = time.Minute
	geminiTierTTL  = time.Hour
)

type cachedQuota struct {
	quota  CodexQuota
	at     time.Time
	tier   string
	tierAt time.Time
}

// geminiQuota returns the account's current quota, cached for a minute so the
// rebalance loop and the dashboard do not each call Google on every pass. A
// failed fetch serves the last good reading rather than a zeroed one: zero
// would read as "unused" and attract traffic to an account that may be spent.
func (c *Client) geminiQuota(ctx context.Context, acc Account, now time.Time) CodexQuota {
	if acc.AuthIndex == "" || acc.Disabled {
		return CodexQuota{}
	}
	c.quotaMu.Lock()
	cached, ok := c.quotaCache[acc.AuthIndex]
	c.quotaMu.Unlock()
	if ok && now.Sub(cached.at) < geminiQuotaTTL {
		return cached.quota
	}

	q, err := c.fetchGeminiQuota(ctx, acc.AuthIndex, now)
	if err != nil {
		if ok {
			return cached.quota
		}
		return CodexQuota{}
	}
	tier, tierAt := cached.tier, cached.tierAt
	if !ok || now.Sub(tierAt) >= geminiTierTTL {
		if t, terr := c.fetchGeminiTier(ctx, acc.AuthIndex); terr == nil {
			tier, tierAt = t, now
		}
	}
	q.PlanType = tier

	c.quotaMu.Lock()
	c.quotaCache[acc.AuthIndex] = cachedQuota{quota: q, at: now, tier: tier, tierAt: tierAt}
	c.quotaMu.Unlock()
	return q
}

func (c *Client) fetchGeminiQuota(ctx context.Context, authIndex string, now time.Time) (CodexQuota, error) {
	body, err := c.antigravityCall(ctx, authIndex, antigravityModelsURL, `{}`)
	if err != nil {
		return CodexQuota{}, err
	}
	q, err := parseGeminiQuota(body)
	if err != nil {
		return CodexQuota{}, err
	}
	q.ObservedAtUnix = now.Unix()
	return q, nil
}

func (c *Client) fetchGeminiTier(ctx context.Context, authIndex string) (string, error) {
	body, err := c.antigravityCall(ctx, authIndex, antigravityTierURL,
		`{"metadata":{"ideType":"ANTIGRAVITY","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`)
	if err != nil {
		return "", err
	}
	return parseGeminiTier(body), nil
}

// antigravityCall POSTs to a Google endpoint as the given account, through the
// sidecar's api-call tool, and returns the upstream body on HTTP 200.
func (c *Client) antigravityCall(ctx context.Context, authIndex, url, data string) ([]byte, error) {
	req := map[string]any{
		"auth_index": authIndex,
		"method":     http.MethodPost,
		"url":        url,
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"User-Agent":    antigravityUserAgent,
		},
		"data": data,
	}
	var out struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := c.do(ctx, http.MethodPost, "/v0/management/api-call", req, &out, true); err != nil {
		return nil, err
	}
	if out.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("antigravity upstream HTTP %d", out.StatusCode)
	}
	return []byte(out.Body), nil
}

// parseGeminiQuota reduces fetchAvailableModels to one reading per account.
//
// Every Gemini model draws on one shared bucket (observed: identical
// remainingFraction and resetTime across all gemini-* models, moving together),
// while Antigravity's claude-* and gpt-oss models draw on another this proxy
// never routes to. The scarcest Gemini model is taken anyway, so a future
// split into per-model buckets errs towards caution.
//
// The response is protobuf JSON, which omits zero values: an exhausted bucket
// arrives as {"resetTime": ...} with no remainingFraction, and must read as
// 0% remaining — not as "no data".
func parseGeminiQuota(body []byte) (CodexQuota, error) {
	var resp struct {
		Models map[string]struct {
			QuotaInfo *struct {
				RemainingFraction *float64 `json:"remainingFraction"`
				ResetTime         string   `json:"resetTime"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return CodexQuota{}, fmt.Errorf("decode fetchAvailableModels: %w", err)
	}
	found := false
	minRemaining := 1.0
	var resetAt int64
	for id, m := range resp.Models {
		if !strings.HasPrefix(id, "gemini-") || m.QuotaInfo == nil {
			continue
		}
		if m.QuotaInfo.RemainingFraction == nil && m.QuotaInfo.ResetTime == "" {
			continue
		}
		remaining := 0.0
		if m.QuotaInfo.RemainingFraction != nil {
			remaining = math.Max(0, math.Min(1, *m.QuotaInfo.RemainingFraction))
		}
		var reset int64
		if t, err := time.Parse(time.RFC3339Nano, m.QuotaInfo.ResetTime); err == nil {
			reset = t.Unix()
		}
		if !found || remaining < minRemaining || (remaining == minRemaining && reset > resetAt) {
			minRemaining, resetAt = remaining, reset
		}
		found = true
	}
	if !found {
		return CodexQuota{}, errors.New("fetchAvailableModels reported no Gemini quota")
	}
	return CodexQuota{
		HasSignals:     true,
		HasFiveHour:    true,
		FiveHourPct:    math.Round((1-minRemaining)*1000) / 10,
		FiveHourResets: resetAt,
	}, nil
}

// parseGeminiTier names the account's Google tier from loadCodeAssist:
// "free-tier" → "free"; paid tiers keep their ID minus the "-tier" suffix.
func parseGeminiTier(body []byte) string {
	var resp struct {
		CurrentTier struct {
			ID string `json:"id"`
		} `json:"currentTier"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(resp.CurrentTier.ID), "-tier")
}
