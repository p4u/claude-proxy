package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Source identifies which signal produced the conversation key (for logging).
type Source string

const (
	SourceHeader      Source = "header"
	SourceMetadataUID Source = "metadata.user_id"
	SourceContentHash Source = "content_hash"
	SourceFallback    Source = "fallback"
)

type Result struct {
	ConvID string
	Source Source
}

// Derive returns a stable conversation key from request headers + buffered body.
// body may be nil/empty (e.g. GET /v1/models); in that case we fall back to a
// per-request key (no stickiness), but the caller can choose to skip sticky
// routing entirely for those endpoints.
func Derive(r *http.Request, body []byte) Result {
	if v := strings.TrimSpace(r.Header.Get("X-Router-Conversation-ID")); v != "" {
		return Result{ConvID: shorten(v), Source: SourceHeader}
	}

	if len(body) > 0 {
		if uid := metadataUserID(body); uid != "" {
			return Result{ConvID: "u_" + shorten(uid), Source: SourceMetadataUID}
		}
		if h := contentHash(body); h != "" {
			return Result{ConvID: "c_" + h, Source: SourceContentHash}
		}
	}

	// Fallback: client IP + body prefix hash.
	h := sha256.New()
	h.Write([]byte(remoteAddr(r)))
	if len(body) > 0 {
		end := 4096
		if end > len(body) {
			end = len(body)
		}
		h.Write(body[:end])
	}
	return Result{
		ConvID: "f_" + hex.EncodeToString(h.Sum(nil)[:8]),
		Source: SourceFallback,
	}
}

func shorten(s string) string {
	if len(s) <= 32 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func remoteAddr(r *http.Request) string {
	if r == nil {
		return ""
	}
	a := r.RemoteAddr
	if i := strings.LastIndex(a, ":"); i > 0 {
		return a[:i]
	}
	return a
}

// metadataUserID extracts $.metadata.user_id from a JSON body without
// fully unmarshaling — uses json.Decoder for robustness on big bodies.
func metadataUserID(body []byte) string {
	type meta struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	var m meta
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Metadata.UserID)
}

// contentHash hashes the system prompt + first user message text. Stable across
// turns of a single Claude Code session because the system block (Claude Code's
// preamble + project CLAUDE.md) stays identical and the first user message
// rarely changes once the session has started.
func contentHash(body []byte) string {
	type message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	type req struct {
		System   json.RawMessage `json:"system"`
		Messages []message       `json:"messages"`
	}
	var r req
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}

	h := sha256.New()
	writeSystem(h, r.System)

	// First user message text.
	for _, m := range r.Messages {
		if m.Role != "user" {
			continue
		}
		writeContent(h, m.Content)
		break
	}

	sum := h.Sum(nil)
	if isZero(sum) {
		return ""
	}
	return hex.EncodeToString(sum[:8])
}

func writeSystem(h hashWriter, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	// system can be a string OR an array of {type:"text", text:"..."}.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		_, _ = h.Write([]byte(s))
		return
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				_, _ = h.Write([]byte(b.Text))
			}
		}
	}
}

func writeContent(h hashWriter, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		_, _ = h.Write([]byte(s))
		return
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				_, _ = h.Write([]byte(b.Text))
			}
		}
	}
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
