package proxy

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

const sseFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":5,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}
`

const jsonFixture = `{"id":"msg_2","type":"message","model":"claude-opus-4","usage":{"input_tokens":7,"output_tokens":13,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseSSEUsage(t *testing.T) {
	u := parseSSEUsage(bytes.NewReader([]byte(sseFixture)), false, false).usage
	if u.Model != "claude-sonnet-4" {
		t.Errorf("model = %q, want claude-sonnet-4", u.Model)
	}
	if u.InputTokens != 100 {
		t.Errorf("input = %d, want 100", u.InputTokens)
	}
	if u.OutputTokens != 42 {
		t.Errorf("output = %d, want 42 (from message_delta)", u.OutputTokens)
	}
	if u.CacheCreationTokens != 20 {
		t.Errorf("cache_creation = %d, want 20", u.CacheCreationTokens)
	}
	if u.CacheReadTokens != 5 {
		t.Errorf("cache_read = %d, want 5", u.CacheReadTokens)
	}
}

func TestParseJSONUsage(t *testing.T) {
	u := parseJSONUsage([]byte(jsonFixture), false, false).usage
	if u.Model != "claude-opus-4" {
		t.Errorf("model = %q, want claude-opus-4", u.Model)
	}
	if u.InputTokens != 7 || u.OutputTokens != 13 {
		t.Errorf("in/out = %d/%d, want 7/13", u.InputTokens, u.OutputTokens)
	}
	if u.CacheCreationTokens != 3 || u.CacheReadTokens != 2 {
		t.Errorf("cache creation/read = %d/%d, want 3/2", u.CacheCreationTokens, u.CacheReadTokens)
	}
}

func TestParseMalformed(t *testing.T) {
	// Malformed SSE data lines are skipped, yielding zero usage.
	bad := "event: message_start\ndata: {not json}\n\ndata: \n\nnot-a-data-line\n"
	if u := parseSSEUsage(bytes.NewReader([]byte(bad)), false, false).usage; u != (tokenUsage{}) {
		t.Errorf("expected zero usage on malformed SSE, got %+v", u)
	}
	// Malformed JSON body → zero usage.
	if u := parseJSONUsage([]byte("{broken"), false, false).usage; u != (tokenUsage{}) {
		t.Errorf("expected zero usage on malformed JSON, got %+v", u)
	}
	if u := parseJSONUsage(nil, false, false).usage; u != (tokenUsage{}) {
		t.Errorf("expected zero usage on empty JSON, got %+v", u)
	}
}

func TestUsageCaptureCompletion(t *testing.T) {
	const sseError = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"private message\"}}\n\n"
	for _, tc := range []struct {
		name, contentType, encoding string
		body                        []byte
		complete                    bool
		errorType                   string
	}{
		{"SSE complete", "text/event-stream", "", []byte(sseFixture), true, ""},
		{"SSE missing stop", "text/event-stream", "", []byte(rebalanceSSEHead), false, ""},
		{"SSE error", "text/event-stream", "", []byte(rebalanceSSEHead + sseError), false, "overloaded_error"},
		{"SSE error then stop", "text/event-stream", "", []byte(sseError + sseFixture), false, "overloaded_error"},
		{"SSE error without data type", "text/event-stream", "", []byte("event: error\ndata: {}\n\n" + sseFixture), false, "unknown"},
		{"SSE malformed", "text/event-stream", "", []byte("data: {broken}\n\n" + sseFixture), false, ""},
		{"SSE non-JSON data", "text/event-stream", "", []byte("data: broken\n\n" + sseFixture), false, ""},
		{"SSE done alone", "text/event-stream", "", []byte("data: [DONE]\n\n"), false, ""},
		{"SSE done after stop", "text/event-stream", "", []byte(sseFixture + "\ndata: [DONE]\n\n"), true, ""},
		{"SSE line cap", "text/event-stream", "", []byte(sseFixture + "data: " + strings.Repeat("x", (1<<20)+1)), false, ""},
		{"SSE gzip complete", "text/event-stream", "gzip", gzipBytes(t, []byte(sseFixture)), true, ""},
		{"SSE gzip incomplete", "text/event-stream", "gzip", gzipBytes(t, []byte(rebalanceSSEHead)), false, ""},
		{"SSE bad gzip", "text/event-stream", "gzip", []byte(sseFixture), false, ""},
		{"JSON complete", "application/json", "", []byte(jsonFixture), true, ""},
		{"JSON malformed", "application/json", "", []byte(`{"type":"message"`), false, ""},
		{"JSON error", "application/json", "", []byte(`{"type":"error","error":{"type":"rate_limit_error"}}`), false, "rate_limit_error"},
		{"JSON unknown error", "application/json", "", []byte(`{"type":"error","error":{"type":"private upstream value"}}`), false, "unknown"},
		{"JSON null", "application/json", "", []byte(`null`), false, ""},
		{"JSON cap", "application/json", "", []byte(jsonFixture + strings.Repeat(" ", usageJSONCap)), false, ""},
		{"JSON gzip complete", "application/json", "gzip", gzipBytes(t, []byte(jsonFixture)), true, ""},
		{"JSON gzip bad", "application/json", "gzip", []byte(jsonFixture), false, ""},
		{"JSON gzip expanded cap", "application/json", "gzip", gzipBytes(t, []byte(jsonFixture+strings.Repeat(" ", usageJSONCap))), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newUsageCapture(tc.contentType, tc.encoding, false, false)
			if c.Complete() {
				t.Fatal("capture claims completion before Close")
			}
			c.Write(tc.body)
			c.Close()
			if c.Complete() != tc.complete || c.ErrorType() != tc.errorType {
				t.Fatalf("completion = %v, error type = %q; want %v, %q", c.Complete(), c.ErrorType(), tc.complete, tc.errorType)
			}
		})
	}
}

func TestUsageCaptureGzipTrailerRequiredForCompletion(t *testing.T) {
	for _, tc := range []struct{ contentType, body string }{
		{"text/event-stream", sseFixture},
		{"application/json", jsonFixture},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			raw := gzipBytes(t, []byte(tc.body))
			c := newUsageCapture(tc.contentType, "gzip", false, false)
			c.Write(raw[:len(raw)-4]) // decoded body is present, gzip trailer is not
			c.Close()
			if c.Complete() {
				t.Fatal("truncated gzip body counted as completed response")
			}
		})
	}
}

func TestUsageCaptureSSE(t *testing.T) {
	c := newUsageCapture("text/event-stream; charset=utf-8", "", false, false)
	// Feed in chunks to exercise streaming.
	data := []byte(sseFixture)
	for i := 0; i < len(data); i += 7 {
		end := i + 7
		if end > len(data) {
			end = len(data)
		}
		c.Write(data[i:end])
	}
	u := c.Close()
	if u.Model != "claude-sonnet-4" || u.InputTokens != 100 || u.OutputTokens != 42 {
		t.Errorf("capture SSE = %+v", u)
	}
}

func TestUsageCaptureSSEGzip(t *testing.T) {
	c := newUsageCapture("text/event-stream", "gzip", false, false)
	c.Write(gzipBytes(t, []byte(sseFixture)))
	u := c.Close()
	if u.Model != "claude-sonnet-4" || u.OutputTokens != 42 {
		t.Errorf("capture gzip SSE = %+v", u)
	}
}

func TestUsageCaptureSSEBadGzipDoesNotBlock(t *testing.T) {
	// Content-Encoding says gzip but bytes are plain: parser must drain and
	// return zero usage without blocking Close.
	c := newUsageCapture("text/event-stream", "gzip", false, false)
	c.Write([]byte(sseFixture))
	u := c.Close()
	if u != (tokenUsage{}) {
		t.Errorf("expected zero usage on bad gzip, got %+v", u)
	}
}

func TestUsageCaptureJSON(t *testing.T) {
	c := newUsageCapture("application/json", "", false, false)
	c.Write([]byte(jsonFixture))
	u := c.Close()
	if u.Model != "claude-opus-4" || u.InputTokens != 7 || u.OutputTokens != 13 {
		t.Errorf("capture JSON = %+v", u)
	}
}

func TestUsageCaptureJSONGzip(t *testing.T) {
	c := newUsageCapture("application/json", "gzip", false, false)
	c.Write(gzipBytes(t, []byte(jsonFixture)))
	u := c.Close()
	if u.Model != "claude-opus-4" || u.OutputTokens != 13 {
		t.Errorf("capture gzip JSON = %+v", u)
	}
}

func TestUsageCaptureJSONCap(t *testing.T) {
	// A body larger than the cap should still parse if the model/usage appear
	// early, and must not panic. Here we prepend valid JSON then junk padding.
	c := newUsageCapture("application/json", "", false, false)
	c.Write([]byte(jsonFixture))
	c.Write(bytes.Repeat([]byte("x"), 2<<20)) // exceeds 1 MiB cap; truncated
	u := c.Close()
	// The buffer holds the first (valid) JSON object plus junk, so unmarshal of
	// the combined buffer fails → zero usage. This asserts no panic/blocking.
	_ = u
}
