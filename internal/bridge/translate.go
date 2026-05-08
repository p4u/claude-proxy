package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// writeSSEEvent writes a single SSE frame: "event: <name>\ndata: <data>\n\n".
func writeSSEEvent(w http.ResponseWriter, name string, data []byte) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}

// writePing emits an Anthropic-style ping SSE event to keep the connection alive.
func writePing(w http.ResponseWriter) {
	writeSSEEvent(w, "ping", []byte(`{"type":"ping"}`))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// initSSE sets the response headers for an SSE stream.
func initSSE(w http.ResponseWriter, _ string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// indexTracker maps claude's content block indices (which include swallowed
// internal tool blocks) to the indices we emit to the client.
type indexTracker struct {
	claudeToOut map[int]int // claude idx → out idx; -1 = swallowed
	nextOut     int
}

func newIndexTracker() *indexTracker {
	return &indexTracker{claudeToOut: make(map[int]int)}
}

// register allocates (or swallows) a claude content block index.
// Returns (outIdx, false) when emitted, or (0, true) when swallowed.
func (t *indexTracker) register(claudeIdx int, swallow bool) (int, bool) {
	if swallow {
		t.claudeToOut[claudeIdx] = -1
		return 0, true
	}
	out := t.nextOut
	t.claudeToOut[claudeIdx] = out
	t.nextOut++
	return out, false
}

// translate converts a claude stream_event's `event` field (already in
// Anthropic SSE format but with unswizzled indices) into an SSE event.
// Returns ("", nil) if the event should be suppressed entirely.
func translateStreamEvent(raw json.RawMessage, tracker *indexTracker, clientTools map[string]bool) (evtName string, data []byte, stop bool, stopReason string) {
	var ev map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "", nil, false, ""
	}

	var typStr string
	if err := json.Unmarshal(ev["type"], &typStr); err != nil {
		return "", nil, false, ""
	}

	switch typStr {
	case "message_start":
		// Suppress — our handler emits its own message_start with the
		// correct model and message ID.
		return "", nil, false, ""

	case "content_block_start":
		var idx int
		_ = json.Unmarshal(ev["index"], &idx)

		var cb map[string]json.RawMessage
		_ = json.Unmarshal(ev["content_block"], &cb)
		var cbType string
		_ = json.Unmarshal(cb["type"], &cbType)

		swallow := false
		if cbType == "tool_use" {
			var name string
			_ = json.Unmarshal(cb["name"], &name)
			if !clientTools[name] {
				swallow = true // internal tool: Read, Grep, etc.
			}
		}

		outIdx, sw := tracker.register(idx, swallow)
		if sw {
			return "", nil, false, ""
		}

		// Rebuild event with corrected index.
		out := map[string]any{
			"type":          "content_block_start",
			"index":         outIdx,
			"content_block": json.RawMessage(ev["content_block"]),
		}
		d, _ := json.Marshal(out)
		return "content_block_start", d, false, ""

	case "content_block_delta":
		var idx int
		_ = json.Unmarshal(ev["index"], &idx)
		outIdx, ok := tracker.claudeToOut[idx]
		if !ok || outIdx == -1 {
			return "", nil, false, "" // swallowed block
		}
		out := map[string]any{
			"type":  "content_block_delta",
			"index": outIdx,
			"delta": json.RawMessage(ev["delta"]),
		}
		d, _ := json.Marshal(out)
		return "content_block_delta", d, false, ""

	case "content_block_stop":
		var idx int
		_ = json.Unmarshal(ev["index"], &idx)
		outIdx, ok := tracker.claudeToOut[idx]
		if !ok || outIdx == -1 {
			return "", nil, false, "" // swallowed block
		}
		out := map[string]any{"type": "content_block_stop", "index": outIdx}
		d, _ := json.Marshal(out)
		return "content_block_stop", d, false, ""

	case "message_delta":
		var md struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(raw, &md)
		d, _ := json.Marshal(map[string]any{
			"type":  "message_delta",
			"delta": json.RawMessage(ev["delta"]),
			"usage": json.RawMessage(ev["usage"]),
		})
		// Do NOT stop here; message_stop is the proper end-of-stream signal.
		return "message_delta", d, false, md.Delta.StopReason

	case "message_stop":
		d, _ := json.Marshal(map[string]any{"type": "message_stop"})
		// message_stop is the authoritative end-of-stream event per the Anthropic spec.
		return "message_stop", d, true, ""
	}

	return "", nil, false, ""
}

// buildMessageStart constructs the Anthropic message_start SSE payload.
func buildMessageStart(msgID, model string, inputTokens int) []byte {
	d, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  inputTokens,
				"output_tokens": 0,
			},
		},
	})
	return d
}

// newMsgID mints an Anthropic-style message ID (msg_<26 chars>).
func newMsgID() string {
	// Use timestamp + nano for uniqueness without extra deps.
	return fmt.Sprintf("msg_%d", time.Now().UnixNano())
}

// extractUserMessage returns the content of the last user message in msgs
// as a JSON raw message (array of content blocks, or a plain string).
// Returns nil if there is no user message.
func extractUserMessage(msgs []Message) json.RawMessage {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return nil
}

// extractToolResults returns all tool_result blocks from the last user message.
func extractToolResults(msgs []Message) []ContentBlock {
	content := extractUserMessage(msgs)
	if content == nil {
		return nil
	}
	// Try array of blocks.
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var results []ContentBlock
	for _, b := range blocks {
		if b.Type == "tool_result" {
			results = append(results, b)
		}
	}
	return results
}

// hasOnlyToolResults reports whether the last user message contains ONLY
// tool_result blocks (no new text to send to the agent via stdin).
func hasOnlyToolResults(msgs []Message) bool {
	content := extractUserMessage(msgs)
	if content == nil {
		return false
	}
	// If content is a plain string, it has text.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return false
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}

// extractNewUserContent returns the content portion (array or string) to send
// via stdin, filtering out tool_result blocks (those go through MCP).
func extractNewUserContent(msgs []Message) json.RawMessage {
	content := extractUserMessage(msgs)
	if content == nil {
		return nil
	}
	// Plain string: send as-is.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return content
	}
	// Array: filter out tool_result blocks.
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return content
	}
	var filtered []ContentBlock
	for _, b := range blocks {
		if b.Type != "tool_result" {
			filtered = append(filtered, b)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	d, _ := json.Marshal(filtered)
	return d
}

// ResolveToolName finds the tool name for the given tool_use_id by scanning
// the assistant messages in the conversation. A tool_result block only carries
// tool_use_id, not the name; the name lives in the corresponding tool_use block
// in the preceding assistant message.
func ResolveToolName(msgs []Message, toolUseID string) string {
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		var blocks []ContentBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID == toolUseID {
				return b.Name
			}
		}
	}
	return ""
}

// toolResultContent extracts the content string from a tool_result block's
// content field (which can be a string or [{type:"text",text:"..."}]).
func toolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
