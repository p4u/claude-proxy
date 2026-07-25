package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// maxPromptLen bounds a stored prompt. Anything longer is truncated.
const maxPromptLen = 4096

// maxMessageRunes bounds a single stored conversation_message body. Longer
// content is cut at this many runes and truncMarker is appended.
const maxMessageRunes = 32768

// truncMarker is appended to any conversation_message body cut at the cap.
const truncMarker = "\n\n[truncated by claude-proxy]"

// extractPrompt parses a POST /v1/messages request body and returns the model
// plus the text of the LAST message with role "user". Content may be a plain
// string or an array of content blocks, in which case the first block whose
// type is "text" is used. The prompt is trimmed to maxPromptLen runes. ok is
// false when no user text could be extracted (malformed body, no user message,
// empty text) so the caller can skip the insert.
func extractPrompt(body []byte) (model, prompt string, ok bool) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", false
	}

	// Walk backwards to the last user message.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		text := contentText(req.Messages[i].Content)
		if text == "" {
			return "", "", false
		}
		return req.Model, truncateRunes(text, maxPromptLen), true
	}
	return "", "", false
}

// contentText resolves a message's content, which is either a JSON string or an
// array of content blocks. For the array form it returns the first text block.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String form.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Array-of-blocks form; take the first type:"text" block.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

// contentTextAll resolves a message's content like contentText, but for the
// array form it concatenates EVERY text block (separated by a blank line).
// tool_use / tool_result / image / thinking blocks carry no `text` field and
// are therefore skipped.
func contentTextAll(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// capMessage trims content to maxMessageRunes, marking it when cut.
func capMessage(s string) string {
	if len(s) <= maxMessageRunes { // bytes >= runes, cheap fast path
		return s
	}
	r := []rune(s)
	if len(r) <= maxMessageRunes {
		return s
	}
	return string(r[:maxMessageRunes]) + truncMarker
}

// truncateRunes trims s to at most n runes without splitting a multibyte rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// capturePrompt stores the last user prompt of a /v1/messages request. It is a
// no-op when prompt logging is disabled (retention 0) or nothing extractable is
// present. Failures are swallowed: prompt capture must never affect proxying.
func (h *Handler) capturePrompt(ctx context.Context, convID string, body []byte) {
	if h.PromptRetentionDays <= 0 {
		return
	}
	model, prompt, ok := extractPrompt(body)
	if !ok {
		return
	}
	var userTokenID *string
	if id := usertoken.FromContext(ctx); id != nil && id.UserTokenID != "" {
		userTokenID = &id.UserTokenID
	}
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO prompt_log (user_token_id, conv_id, ts, model, prompt)
		VALUES (?, ?, ?, ?, ?)`,
		userTokenID, convID, time.Now().Unix(), model, prompt); err != nil {
		h.log.Debug("prompt capture insert failed", "err", err, "conv", convID)
	}
}

// fullCaptureEnabled reports whether whole-conversation capture applies to this
// request: the global retention gate must be on AND the authenticated user must
// have opted in.
func (h *Handler) fullCaptureEnabled(ctx context.Context) bool {
	if h.PromptRetentionDays <= 0 {
		return false
	}
	id := usertoken.FromContext(ctx)
	return id != nil && id.FullCapture
}

// captureConversation stores every message of a /v1/messages request body that
// is not yet recorded for convID. The Messages API is stateless, so each
// request re-sends the whole history: seq is simply the message's index in the
// array, which makes the insert idempotent (UNIQUE(conv_id, seq) + INSERT OR
// IGNORE) across repeated and concurrent requests.
//
// It returns the seq the assistant reply to this exchange should occupy (the
// number of messages sent) and whether capture ran at all.
//
// Limitation: server-side context compaction/editing can rewrite the history a
// client re-sends, in which case seq alignment drifts on very long
// conversations.
func (h *Handler) captureConversation(ctx context.Context, convID string, body []byte) (int, bool) {
	if !h.fullCaptureEnabled(ctx) {
		return 0, false
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return 0, false
	}

	var stored int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq)+1, 0) FROM conversation_message WHERE conv_id=?`,
		convID).Scan(&stored); err != nil {
		h.log.Debug("conversation capture: seq lookup failed", "err", err, "conv", convID)
		return 0, false
	}

	now := time.Now().Unix()
	for i := stored; i < len(req.Messages); i++ {
		m := req.Messages[i]
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		text := contentTextAll(m.Content)
		if text == "" {
			continue // tool-only / image-only turn: nothing worth storing
		}
		h.insertMessage(ctx, convID, i, m.Role, capMessage(text), req.Model, now)
	}
	return len(req.Messages), true
}

// captureAssistantReply appends the model's answer as the next message of the
// conversation. No-op on empty text.
func (h *Handler) captureAssistantReply(ctx context.Context, convID string, seq int, model, text string) {
	if text == "" {
		return
	}
	h.insertMessage(ctx, convID, seq, "assistant", capMessage(text), model, time.Now().Unix())
}

// insertMessage writes one conversation_message row. Failures are swallowed:
// capture must never affect proxying.
func (h *Handler) insertMessage(ctx context.Context, convID string, seq int, role, content, model string, ts int64) {
	var userTokenID *string
	if id := usertoken.FromContext(ctx); id != nil && id.UserTokenID != "" {
		userTokenID = &id.UserTokenID
	}
	if _, err := h.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO conversation_message
		  (conv_id, user_token_id, seq, role, content, model, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		convID, userTokenID, seq, role, content, model, ts); err != nil {
		h.log.Debug("conversation capture insert failed", "err", err, "conv", convID, "seq", seq)
	}
}

// PromptJanitor deletes prompt_log and conversation_message rows older than
// retentionDays every hour. It runs only when prompt logging is enabled
// (retentionDays > 0).
func PromptJanitor(ctx context.Context, db *store.DB, retentionDays int, log *slog.Logger) {
	if retentionDays <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	purge := func() {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
		if _, err := db.ExecContext(ctx, `DELETE FROM prompt_log WHERE ts < ?`, cutoff); err != nil {
			log.Warn("prompt_log retention purge failed", "err", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM conversation_message WHERE ts < ?`, cutoff); err != nil {
			log.Warn("conversation_message retention purge failed", "err", err)
		}
	}
	purge() // sweep once at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}
