package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func logEntry(t *testing.T, logs []byte, message string) map[string]any {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(logs), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log: %v", err)
		}
		if entry["msg"] == message {
			return entry
		}
	}
	t.Fatalf("log entry %q missing from %s", message, logs)
	return nil
}

type forwardingTransport func(*http.Request) (*http.Response, error)

func (f forwardingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failedResponseBody struct{ body []byte }

func (b *failedResponseBody) Read(p []byte) (int, error) {
	n := copy(p, b.body)
	b.body = b.body[n:]
	return n, io.ErrUnexpectedEOF
}

func (b *failedResponseBody) Close() error { return nil }

func TestForwardReadFailureWithBytesNeverAcknowledgesOrReplays(t *testing.T) {
	h, cs, db, _ := setupProxy(t, nil)
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
	var calls int
	h.client.Transport = forwardingTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := io.NopCloser(strings.NewReader(rebalanceRespJSON))
		if calls == 1 {
			body = &failedResponseBody{body: []byte(rebalanceRespJSON)}
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			ContentLength: int64(len(rebalanceRespJSON)), Body: body,
		}, nil
	})
	var logs bytes.Buffer
	h.log = slog.New(slog.NewJSONHandler(&logs, nil))
	first := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if first.Body.String() != rebalanceRespJSON || calls != 1 {
		t.Fatalf("read failure altered or replayed body: calls %d body %q", calls, first.Body.String())
	}
	forwarded := logEntry(t, logs.Bytes(), "forwarded")
	if forwarded["response_complete"] != false || forwarded["response_model"] != "claude-sonnet-5" {
		t.Errorf("failed read diagnostics = %v", forwarded)
	}
	second := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if second.Header().Get("X-Router-Rebalance") != "pending" {
		t.Fatalf("read failure acknowledged notice: %v", second.Header())
	}
	assertPinnedTo(t, db, cs[0].ID)
}

func TestForwardTransportFailureRetainsRequestedModel(t *testing.T) {
	h, _, db, _ := setupProxy(t, nil)
	h.client.Transport = forwardingTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("local test transport failure")
	})
	rw := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rw.Code)
	}
	var model string
	if err := db.QueryRow(`SELECT model FROM request_log`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-5" {
		t.Errorf("failed transport model = %q", model)
	}
}

func TestForwardSSEErrorDiagnosticsKeepReportedUsage(t *testing.T) {
	const errorEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"private error text\"}}\n\n"
	body := strings.ReplaceAll(sseFixture, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n", errorEvent)
	h, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	})
	var logs bytes.Buffer
	h.log = slog.New(slog.NewJSONHandler(&logs, nil))
	rw := postRebalance(t, h, "/v1/messages", rebalanceStreamReq)
	if rw.Code != http.StatusOK || rw.Body.String() != body {
		t.Fatalf("SSE error relay changed: %d %q", rw.Code, rw.Body.String())
	}
	forwarded := logEntry(t, logs.Bytes(), "forwarded")
	if forwarded["response_complete"] != false || forwarded["error_type"] != "overloaded_error" {
		t.Errorf("SSE error diagnostics = %v", forwarded)
	}
	if strings.Contains(logs.String(), "private error text") {
		t.Error("SSE diagnostic captured free-form error message")
	}
	var model string
	var input, output int64
	if err := db.QueryRow(`SELECT model, input_tokens, output_tokens FROM request_log`).Scan(&model, &input, &output); err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-4" || input != 100 || output != 42 {
		t.Errorf("reported usage lost on interrupted stream: model %q tokens %d/%d", model, input, output)
	}
}

func TestUpstreamErrorTypeIsBoundedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name, encoding string
		body           []byte
		want           string
	}{
		{"known", "", []byte(`{"error":{"type":"rate_limit_error","message":"private"}}`), "rate_limit_error"},
		{"unknown", "", []byte(`{"error":{"type":"private-value","message":"private"}}`), "unknown"},
		{"control characters", "", []byte(`{"error":{"type":"rate_limit_error\nsecret"}}`), "unknown"},
		{"malformed", "", []byte(`{"error":`), "unknown"},
		{"gzip", "gzip", gzipBytes(t, []byte(`{"error":{"type":"overloaded_error"}}`)), "overloaded_error"},
		{"invalid gzip", "gzip", []byte(`{"error":{"type":"overloaded_error"}}`), "unknown"},
		{"gzip expansion cap", "gzip", gzipBytes(t, []byte(`{"error":{"message":"`+strings.Repeat("x", errorBodyCap)+`","type":"rate_limit_error"}}`)), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamErrorType(tc.body, tc.encoding); got != tc.want {
				t.Errorf("error type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestForward429Diagnostics(t *testing.T) {
	const (
		requestedModel = "claude-sonnet-5-gw"
		errorBody      = `{"type":"error","error":{"type":"rate_limit_error","message":"private upstream message"}}`
	)
	for _, tc := range []struct {
		name, retryAfter, source string
		synthesized              bool
	}{
		{"delta seconds", "5", "upstream", false},
		{"HTTP date", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat), "upstream", false},
		{"missing", "", "fallback", true},
		{"invalid", "not-a-delay", "fallback", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, errorBody)
			})
			var logs bytes.Buffer
			h.log = slog.New(slog.NewJSONHandler(&logs, nil))
			body := `{"model":"` + requestedModel + `","messages":[{"role":"user","content":"private request prompt"}]}`
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if rw.Code != http.StatusTooManyRequests || rw.Body.String() != errorBody {
				t.Fatalf("429 relay changed: %d %q", rw.Code, rw.Body.String())
			}
			wantRetry := tc.retryAfter
			if tc.synthesized {
				wantRetry = "30"
			}
			if got := rw.Header().Get("Retry-After"); got != wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, wantRetry)
			}
			var model string
			var status int
			var input, output int64
			if err := db.QueryRow(`SELECT model, status_code, input_tokens, output_tokens FROM request_log`).Scan(&model, &status, &input, &output); err != nil {
				t.Fatal(err)
			}
			if model != requestedModel || status != http.StatusTooManyRequests || input != 0 || output != 0 {
				t.Errorf("failed request log = model %q status %d tokens %d/%d", model, status, input, output)
			}
			limited := logEntry(t, logs.Bytes(), "upstream 429; marked credential limited")
			if limited["retry_after_source"] != tc.source || limited["retry_after_synthesized"] != tc.synthesized {
				t.Errorf("retry provenance = %v", limited)
			}
			if limited["error_type"] != "rate_limit_error" {
				t.Errorf("error type = %v", limited["error_type"])
			}
			forwarded := logEntry(t, logs.Bytes(), "forwarded")
			if forwarded["requested_model"] != requestedModel || forwarded["response_model"] != "" || forwarded["model"] != requestedModel {
				t.Errorf("forwarded model diagnostics = %v", forwarded)
			}
			if strings.Contains(logs.String(), "private request prompt") || strings.Contains(logs.String(), "private upstream message") {
				t.Error("diagnostic logs collected prompt or upstream error message")
			}
		})
	}
}

func TestForward429BodyRemainsByteExactBeyondDiagnosticLimit(t *testing.T) {
	body := `{"type":"error","error":{"type":"rate_limit_error","message":"` + strings.Repeat("x", 8192) + `"}}`
	h, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	})
	rw := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw.Code != http.StatusTooManyRequests || rw.Body.String() != body {
		t.Fatalf("429 body truncated or altered: status %d bytes %d, want %d", rw.Code, rw.Body.Len(), len(body))
	}
	var received int64
	if err := db.QueryRow(`SELECT bytes_received FROM request_log`).Scan(&received); err != nil {
		t.Fatal(err)
	}
	if received != int64(len(body)) {
		t.Errorf("bytes_received = %d, want %d", received, len(body))
	}
}

func TestForwardRequestedAndResponseModelsAreDistinct(t *testing.T) {
	h, _, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, rebalanceRespJSON)
	})
	var logs bytes.Buffer
	h.log = slog.New(slog.NewJSONHandler(&logs, nil))
	body := strings.ReplaceAll(rebalanceReqBody, "claude-sonnet-5", "claude-sonnet-5-gw")
	postRebalance(t, h, "/v1/messages", body)
	forwarded := logEntry(t, logs.Bytes(), "forwarded")
	if forwarded["requested_model"] != "claude-sonnet-5-gw" || forwarded["response_model"] != "claude-sonnet-5" {
		t.Errorf("model diagnostics = %v", forwarded)
	}
	var model string
	if err := db.QueryRow(`SELECT model FROM request_log`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-5" {
		t.Errorf("successful request model = %q, want response model", model)
	}
}
