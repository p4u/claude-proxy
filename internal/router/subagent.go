package router

import "strings"

// AgentIDHeader carries the id of the Claude Code subagent that sent a
// request. The main thread never sends it.
const AgentIDHeader = "X-Claude-Code-Agent-Id"

// maxAgentIDLen bounds an accepted agent id. Claude Code's own ids are at most
// 81 characters.
const maxAgentIDLen = 128

// ValidAgentID reports whether s is 1 to 128 characters of [0-9A-Za-z_-].
// Anything else is treated as if the header were absent.
func ValidAgentID(s string) bool {
	if len(s) == 0 || len(s) > maxAgentIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// SubagentKey returns a subagent's sticky key under its session's key:
// parent + "/a_" + the shortened agent id, so every subagent of a session
// sorts in the range [parent+"/", parent+"0"). It returns "" when agentID is
// invalid, or parent is empty or already contains "/" (a short raw
// metadata.user_id); such a request keeps the parent key.
func SubagentKey(parent, agentID string) string {
	if parent == "" || strings.Contains(parent, "/") || !ValidAgentID(agentID) {
		return ""
	}
	return parent + "/a_" + shorten(agentID)
}
