package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// strippedSystemPhrases are removed from every request's system prompt, for
// every provider, before it goes upstream. This is what makes headless
// `claude -p` (and Agent SDK apps) behave like interactive Claude Code through
// the proxy.
//
// Claude Agent SDK's identity line is what headless clients send where
// interactive Claude Code sends "You are Claude Code, Anthropic's official CLI
// for Claude." Google's Antigravity backend answers any request carrying it
// with 429 RESOURCE_EXHAUSTED regardless of quota (observed 2026-10-01), so
// `claude -p` on a Gemini model failed every time. Removing it everywhere —
// not only for Gemini — keeps one request shape for every provider; Anthropic
// subscriptions were verified to accept requests with and without it.
var strippedSystemPhrases = []string{
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
}

// stripSystemText removes each phrase from the request's system prompt and
// reports whether the body changed. It handles both shapes of "system": a
// plain string, and an array of content blocks, where a text block emptied by
// the removal is dropped (an empty text block is invalid upstream) and every
// other block is kept exactly as sent. A body without any of the phrases is
// returned untouched, byte for byte; so is anything that does not parse.
func stripSystemText(body []byte, phrases []string) ([]byte, bool) {
	if !containsAnyBytes(body, phrases) {
		return body, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	raw, ok := obj["system"]
	if !ok {
		return body, false
	}

	var newSystem json.RawMessage
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		cleaned, changed := removePhrases(asString, phrases)
		if !changed {
			return body, false
		}
		if cleaned == "" {
			delete(obj, "system")
		} else if newSystem, err = json.Marshal(cleaned); err != nil {
			return body, false
		}
	} else {
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return body, false
		}
		kept := make([]map[string]json.RawMessage, 0, len(blocks))
		changed := false
		for _, b := range blocks {
			var text string
			if json.Unmarshal(b["text"], &text) != nil {
				kept = append(kept, b)
				continue
			}
			cleaned, did := removePhrases(text, phrases)
			if !did {
				kept = append(kept, b)
				continue
			}
			changed = true
			if cleaned == "" {
				continue
			}
			enc, err := json.Marshal(cleaned)
			if err != nil {
				return body, false
			}
			b["text"] = enc
			kept = append(kept, b)
		}
		if !changed {
			return body, false
		}
		if len(kept) == 0 {
			delete(obj, "system")
		} else if newSystem, err = json.Marshal(kept); err != nil {
			return body, false
		}
	}
	if newSystem != nil {
		obj["system"] = newSystem
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// removePhrases deletes every occurrence of each phrase and trims the
// whitespace the removal leaves at the ends.
func removePhrases(s string, phrases []string) (string, bool) {
	out := s
	for _, p := range phrases {
		out = strings.ReplaceAll(out, p, "")
	}
	if out == s {
		return s, false
	}
	return strings.TrimSpace(out), true
}

func containsAnyBytes(body []byte, phrases []string) bool {
	for _, p := range phrases {
		if bytes.Contains(body, []byte(p)) {
			return true
		}
	}
	return false
}
