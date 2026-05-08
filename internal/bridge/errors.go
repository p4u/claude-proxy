package bridge

import (
	"encoding/json"
	"net/http"
)

// writeError writes an Anthropic-shaped error body and sets the HTTP status.
// For streaming responses, the caller should emit an SSE error event instead.
func writeError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(AnthropicError{
		Type:  "error",
		Error: ErrorDetail{Type: errType, Message: msg},
	})
}

// sseError writes an Anthropic SSE error event to an already-started SSE stream.
func sseError(w http.ResponseWriter, errType, msg string) {
	data, _ := json.Marshal(AnthropicError{
		Type:  "error",
		Error: ErrorDetail{Type: errType, Message: msg},
	})
	writeSSEEvent(w, "error", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
