package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// feed pushes a stream through the rewriter in small chunks, so tests exercise
// the buffering of an event split across reads, and appends the flush.
func feed(rw *responseModelRewriter, stream string) string {
	var out []byte
	for i := 0; i < len(stream); i += 7 {
		end := min(i+7, len(stream))
		out = append(out, rw.filter([]byte(stream[i:end]))...)
	}
	return string(append(out, rw.flush()...))
}

const sseHead = "event: message_start\ndata: {\"message\":{\"id\":\"msg_1\",\"model\":\"glm-4.7\",\"role\":\"assistant\"},\"type\":\"message_start\"}\n\n"

func TestResponseModelRewriterRewritesSSEHead(t *testing.T) {
	stream := "event: ping\ndata: {\"type\":\"ping\"}\n\n" + sseHead +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	rw := newResponseModelRewriter("claude-glm-4.7", true)
	got := feed(rw, stream)
	want := "event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_start\ndata: {\"message\":{\"id\":\"msg_1\",\"model\":\"claude-glm-4.7\",\"role\":\"assistant\"},\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	if got != want {
		t.Fatalf("rewritten stream:\n got %s\nwant %s", got, want)
	}
}

// A model that already matches is emitted byte-for-byte, not re-encoded.
func TestResponseModelRewriterLeavesMatchingModel(t *testing.T) {
	stream := strings.Replace(sseHead, "glm-4.7", "claude-glm-4.7", 1)
	if got := feed(newResponseModelRewriter("claude-glm-4.7", true), stream); got != stream {
		t.Fatalf("matching model was touched:\n got %s\nwant %s", got, stream)
	}
}

// A stream already delivering content has no message_start ahead: verbatim.
func TestResponseModelRewriterGivesUpWithoutMessageStart(t *testing.T) {
	stream := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n" + sseHead
	if got := feed(newResponseModelRewriter("claude-glm-4.7", true), stream); got != stream {
		t.Fatalf("stream without message_start was touched:\n got %s\nwant %s", got, stream)
	}
}

// CRLF framing splits events the same way.
func TestResponseModelRewriterHandlesCRLF(t *testing.T) {
	stream := "event: message_start\r\ndata: {\"message\":{\"model\":\"assistant\"},\"type\":\"message_start\"}\r\n\r\n"
	got := feed(newResponseModelRewriter("claude-openai-qwen38-27b", true), stream)
	if !strings.Contains(got, `"model":"claude-openai-qwen38-27b"`) || strings.Contains(got, `"assistant"`) {
		t.Fatalf("CRLF stream not rewritten: %s", got)
	}
}

// A plain JSON body is buffered and rewritten whole at flush.
func TestResponseModelRewriterJSONBody(t *testing.T) {
	rw := newResponseModelRewriter("claude-glm-4.7", false)
	if out := rw.filter([]byte(`{"id":"msg_1","model":"glm-4.7","role"`)); out != nil {
		t.Fatalf("partial body emitted early: %s", out)
	}
	got := string(append(rw.filter([]byte(`:"assistant"}`)), rw.flush()...))
	want := `{"id":"msg_1","model":"claude-glm-4.7","role":"assistant"}`
	if got != want {
		t.Fatalf("JSON body:\n got %s\nwant %s", got, want)
	}
}

// Bodies without a model field (count_tokens) or unparseable ones pass through.
func TestResponseModelRewriterJSONWithoutModel(t *testing.T) {
	for _, body := range []string{`{"input_tokens":8}`, `not json at all`} {
		rw := newResponseModelRewriter("claude-glm-4.7", false)
		if got := feed(rw, body); got != body {
			t.Fatalf("body %q rewritten to %q", body, got)
		}
	}
}

// The rewriter is inert once its decision is made: later chunks are returned
// as-is, so a long stream pays nothing after its first event.
func TestResponseModelRewriterPassThroughAfterDecision(t *testing.T) {
	rw := newResponseModelRewriter("claude-glm-4.7", true)
	_ = feed(rw, sseHead)
	tail := []byte("event: message_delta\ndata: {\"type\":\"message_delta\"}\n\n")
	if got := rw.filter(tail); string(got) != string(tail) {
		t.Fatalf("tail chunk copied: %q", got)
	}
	if rw.flush() != nil {
		t.Fatal("flush after decision emitted bytes")
	}
}

// End to end: the client picks the advertised alias, the upstream answers with
// its native name, and the response the client receives names the alias —
// while the request that reached Z.AI and the request_log row keep the native
// model.
func TestAliasedResponseCarriesClientModel(t *testing.T) {
	sse := sseHead +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var upstreamModel string
	h, db, seen := glmSetup(t, nil, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamModel = requestModel(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	})

	rec := postMessages(t, h, "claude-glm-4.7")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"claude-glm-4.7"`) {
		t.Errorf("client response missing its own model id:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"model":"glm-4.7"`) {
		t.Errorf("native model leaked to the client:\n%s", rec.Body.String())
	}
	if upstreamModel != "glm-4.7" {
		t.Errorf("upstream saw model %q, want the native glm-4.7", upstreamModel)
	}
	if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], "api.z.ai/") {
		t.Fatalf("request did not reach Z.AI: %v", seen)
	}
	// The usage capture reads the original stream, so the log keeps the
	// native model the upstream really served.
	var logged string
	if err := db.QueryRowContext(context.Background(), `SELECT model FROM request_log`).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != "glm-4.7" {
		t.Errorf("request_log model = %q, want the native glm-4.7", logged)
	}
}

// Same contract for a non-streaming JSON response.
func TestAliasedJSONResponseCarriesClientModel(t *testing.T) {
	h, _, _ := glmSetup(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	})
	rec := postMessages(t, h, "claude-glm-4.7")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"claude-glm-4.7"`) || strings.Contains(rec.Body.String(), `"model":"glm-4.7"`) {
		t.Errorf("JSON response model not restored:\n%s", rec.Body.String())
	}
}

// Anthropic passthrough must stay byte-for-byte: no alias applies, so neither
// the request nor the response is touched — including dated model variants.
func TestAnthropicResponseNotRewritten(t *testing.T) {
	sse := "event: message_start\ndata: {\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-5-20261001\",\"role\":\"assistant\"},\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	h, _, _ := glmSetup(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}, nil)
	rec := postMessages(t, h, "claude-sonnet-5")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != sse {
		t.Fatalf("Anthropic SSE was modified:\n got %s\nwant %s", rec.Body.String(), sse)
	}
}
