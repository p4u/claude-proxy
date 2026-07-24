package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// maxPromptLen bounds a stored prompt. Anything longer is truncated.
const maxPromptLen = 4096

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

// PromptJanitor deletes prompt_log rows older than retentionDays every hour. It
// runs only when prompt logging is enabled (retentionDays > 0).
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
