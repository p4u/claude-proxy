package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/router"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// subagentChild returns the child key of a subagent candidate, or "" for any
// other request: an Anthropic, non-custom request keyed by metadata.user_id,
// from a caller with spreading on, carrying a valid X-Claude-Code-Agent-Id.
// The caller has already checked needsSticky.
func subagentChild(r *http.Request, dr router.Result, prov provider.ID, customModel string) string {
	if prov != provider.Anthropic || customModel != "" || dr.Source != router.SourceMetadataUID ||
		!usertoken.SpreadSubagentsFor(usertoken.FromContext(r.Context())) {
		return ""
	}
	return router.SubagentKey(dr.ConvID, r.Header.Get(router.AgentIDHeader))
}

// noAssistantTurn reports whether body's messages hold no assistant turn, as
// in the first request of a fresh subagent. Forks, and subagents already
// running before their child binding existed, carry assistant turns.
func noAssistantTurn(body []byte) bool {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			return false
		}
	}
	return true
}
