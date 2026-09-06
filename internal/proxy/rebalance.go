package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// rebalanceWriter owns only these two headers. Setting them at WriteHeader,
// after upstream headers have been copied, prevents an upstream from spoofing
// either the notice or its explanation. Model output is never modified.
type rebalanceWriter struct {
	http.ResponseWriter
	state, emitted string
	wrote, failed  bool
}

func (w *rebalanceWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.Header().Del("X-Router-Rebalance")
	w.Header().Del("X-Router-Message")
	if status >= 200 {
		w.wrote = true
		if status < 300 {
			switch w.state {
			case "pending":
				w.Header().Set("X-Router-Rebalance", "pending")
				w.Header().Set("X-Router-Message", "A later message may switch subscriptions after current requests finish, if usage still favors it; prompt cache may rebuild.")
			case "switched":
				w.Header().Set("X-Router-Rebalance", "switched")
				w.Header().Set("X-Router-Message", "This message switched subscriptions to rebalance usage; prompt cache may rebuild.")
			}
			w.emitted = w.state
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *rebalanceWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		w.failed = true
	}
	return n, err
}

func (w *rebalanceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *rebalanceWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.failed = true
	}
}

func (w *rebalanceWriter) pendingEmitted() bool {
	// Emission is not acknowledgment: even a successful Write cannot prove
	// that the client consumed the headers. Known write failures reannounce.
	return w.emitted == "pending" && !w.failed
}

// portableRebalanceRequest excludes account-scoped Files/container/server-tool
// state. Ordinary text, images embedded as base64/URLs, client tools, thinking
// signatures and their history are sent unchanged. Unknown/malformed JSON is
// forwarded normally but never used to initiate an elective account change.
func portableRebalanceRequest(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !json.Valid(body) {
		return false
	}
	// Most turns contain none of these fields. Avoid allocating a second
	// copy of a potentially megabyte-sized conversation tree. Unicode escapes
	// require decoding too, since a field name or tool type could be escaped.
	if !bytes.Contains(body, []byte(`"file_id"`)) && !bytes.Contains(body, []byte(`"container"`)) &&
		!bytes.Contains(body, []byte("server_tool_use")) && !bytes.Contains(body, []byte("code_execution")) &&
		!bytes.Contains(body, []byte(`\u`)) {
		return true
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil || value == nil {
		return false
	}
	return portableRebalanceValue(value)
}

func portableRebalanceValue(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if (key == "file_id" || key == "container") && item != nil && item != "" {
				return false
			}
			if key == "type" {
				if kind, ok := item.(string); ok && (kind == "server_tool_use" || strings.Contains(kind, "code_execution")) {
					return false
				}
			}
			if !portableRebalanceValue(item) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !portableRebalanceValue(item) {
				return false
			}
		}
	}
	return true
}
