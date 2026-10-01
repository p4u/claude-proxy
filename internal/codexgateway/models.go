package codexgateway

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// ModelDefinition is the sidecar's static description of one model.
type ModelDefinition struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	ContextLength int64  `json:"context_length"`
}

const modelDefinitionsTTL = 10 * time.Minute

type cachedDefinitions struct {
	defs map[string]ModelDefinition
	at   time.Time
}

// ModelDefinitions returns the channel's model definitions keyed by model ID.
//
// The sidecar's OpenAI-shaped /v1/models — the only shape whose IDs are real;
// the Anthropic-shaped one rewrites them into opaque aliases — carries neither
// display names nor context sizes, so GET /v1/models would otherwise offer
// "gemini-pro-agent" where Antigravity says "Gemini 3.1 Pro (High)". The
// management model-definitions route has both, keyed by the real ID. Cached:
// it changes only when the sidecar refreshes its catalogue (every 3 hours).
func (c *Client) ModelDefinitions(ctx context.Context, ch Channel) (map[string]ModelDefinition, error) {
	if c == nil {
		return nil, nil
	}
	now := time.Now()
	c.quotaMu.Lock()
	cached, ok := c.defsCache[ch.Key]
	c.quotaMu.Unlock()
	if ok && now.Sub(cached.at) < modelDefinitionsTTL {
		return cached.defs, nil
	}
	var resp struct {
		Models []ModelDefinition `json:"models"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/management/model-definitions/"+url.PathEscape(ch.Key), nil, &resp, true); err != nil {
		if ok {
			return cached.defs, nil
		}
		return nil, err
	}
	defs := make(map[string]ModelDefinition, len(resp.Models))
	for _, m := range resp.Models {
		if m.ID != "" {
			defs[m.ID] = m
		}
	}
	c.quotaMu.Lock()
	c.defsCache[ch.Key] = cachedDefinitions{defs: defs, at: now}
	c.quotaMu.Unlock()
	return defs, nil
}
