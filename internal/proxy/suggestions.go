package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

// suggestionMarker opens the prompt Claude Code appends when it wants to guess
// what the user will type next. In the CLI bundle it is a single compile-time
// constant sent as one extra user message on a forked copy of the whole
// conversation (querySource:"prompt_suggestion"), so the request carries the
// entire history and costs a real round trip while producing nothing the user
// asked for.
//
// Matching only the opening marker, rather than the whole prompt, survives
// upstream rewording of the body. If Anthropic renames the marker itself the
// match stops firing and requests are forwarded exactly as before — the safe
// direction to fail, since a miss costs one wasted request while a false
// positive would silently swallow a real prompt.
const suggestionMarker = "[SUGGESTION MODE:"

// isSuggestionRequest reports whether body is one of those requests.
//
// The marker must open the LAST message and that message must be from the
// user, which is exactly the shape the CLI produces. A prompt that merely
// quotes the marker further in — someone pasting the block into a question
// about it, say — is left alone.
func isSuggestionRequest(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(contentText(last.Content)), suggestionMarker)
}

// blockSuggestion answers a prompt-suggestion request locally when the
// authenticated user has opted out of them, returning true once it has written
// the response and the caller must stop.
//
// It runs before metering and before pool.Bind, so a suppressed request costs
// no quota, binds no credential, and is never stored as a prompt — that last
// part matters as much as the tokens, since these requests otherwise bury the
// user's real prompts in the capture log.
//
// Users without the option set incur one cheap body check and nothing else.
func (h *Handler) blockSuggestion(w http.ResponseWriter, r *http.Request, body []byte, start time.Time) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		return false
	}
	id := usertoken.FromContext(r.Context())
	if id == nil || !id.BlockSuggestions {
		return false
	}
	if !isSuggestionRequest(body) {
		return false
	}

	msgID := "msg_proxy_suggestion_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h.log.Debug("prompt suggestion blocked",
		"user", id.UserTokenID, "name", id.UserName, "bytes", len(body))
	writeEmptySuggestion(w, body, msgID)

	// Logged with no credential or conversation and zero tokens: it never
	// reached Anthropic, so attributing it to one would misreport usage. The
	// row is what makes the saving visible in per-user stats.
	h.logRequest(r.Context(), r.URL.Path, "", "", http.StatusOK,
		int64(len(body)), 0, time.Since(start), tokenUsage{Model: requestedModel(body)})
	return true
}

// wantsStream reports whether the request asked for an SSE response, so the
// synthetic reply matches the shape the client is parsing for.
func wantsStream(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &req) == nil && req.Stream
}

// requestedModel echoes back the model the client asked for. Claude Code reads
// it off the response, and an empty string would be conspicuous.
func requestedModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		return "claude-proxy-suggestion"
	}
	return req.Model
}

// writeEmptySuggestion answers a suppressed suggestion request locally with a
// well-formed but empty completion.
//
// Returning nothing is a path Claude Code already handles: it looks for a text
// block, finds none, and records the outcome as "suppressed" — the same branch
// it takes when the model's own answer is filtered for being too long or too
// evaluative. No error surfaces and nothing is retried.
//
// Usage is reported as zero tokens, which is the truth: nothing was sent
// upstream, so nothing was spent.
func writeEmptySuggestion(w http.ResponseWriter, body []byte, id string) {
	model := requestedModel(body)
	w.Header().Set("X-Router-Reason", "suggestion-blocked")

	if !wantsStream(body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []map[string]any{{"type": "text", "text": ""}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	for _, ev := range []struct{ name, data string }{
		{"message_start", string(start)},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}
