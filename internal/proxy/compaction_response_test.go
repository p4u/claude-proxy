package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/pool"
)

func summaryProofFor(text string) pool.SummaryProof {
	return pool.SummaryProof{Digest: sha256.Sum256([]byte(text)), Bytes: len(text)}
}

func TestSummaryFingerprintNormalization(t *testing.T) {
	for _, tc := range []struct{ name, text, normalized string }{
		{"plain", "A plain summary.", "A plain summary."},
		{"trim", " \t\nRésumé 世界\n\r ", "Résumé 世界"},
		{"tags", "<analysis>private\nreasoning</analysis>\n<summary>\n  Kept text. \n</summary>", "Summary:\nKept text."},
		{"first tags only", "<analysis>drop</analysis><analysis>keep</analysis><summary>first</summary><summary>second</summary>", "<analysis>keep</analysis>Summary:\nfirst<summary>second</summary>"},
		{"newlines", "\n\n\nfirst\n\n\n\nsecond\n\n\n", "first\n\nsecond"},
		{"CRLF preserved", "first\r\n\r\n\r\nsecond", "first\r\n\r\n\r\nsecond"},
		{"ECMAScript whitespace", string(rune(0xfeff)) + "  <summary>" + string(rune(0xfeff)) + " body 　</summary>" + string(rune(0xfeff)), "Summary:\nbody"},
		{"NEL not JS whitespace", "\u0085body\u0085", "\u0085body\u0085"},
		{"unclosed tags unchanged", "<analysis>unfinished <summary>unfinished", "<analysis>unfinished <summary>unfinished"},
		{"empty summary tag", "<summary> \n </summary>", "Summary:"},
		{"empty", "", ""},
		{"whitespace only", " \t\n", ""},
		{"analysis only", "<analysis>not visible</analysis>", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := pool.SummaryProof{}
			if tc.normalized != "" {
				want = summaryProofFor(tc.normalized)
			}
			if got := summaryFingerprint(tc.text); got != want {
				t.Fatalf("fingerprint = %+v, want %+v", got, want)
			}
		})
	}
	if got := summaryFingerprint(strings.Repeat("x", (1<<20)+1)); got != (pool.SummaryProof{}) {
		t.Fatal("over-cap text produced a fingerprint")
	}
	if got := summaryFingerprint(string([]byte{0xff})); got != (pool.SummaryProof{}) {
		t.Fatal("invalid UTF-8 produced a fingerprint")
	}
}

func summaryJSONResponse(t *testing.T, text string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type": "message", "role": "assistant", "model": "claude-test",
		"content":     []map[string]string{{"type": "text", "text": text}},
		"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 7, "output_tokens": 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

const summaryStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":7}}}\n\n"
const summaryBlockStop = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
const summaryDeltaStop = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":9}}\n\n"
const summaryMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func summaryTextStart(text string) string {
	raw, _ := json.Marshal(text)
	return fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":%s}}\n\n", raw)
}

func summaryTextDelta(text string) string {
	raw, _ := json.Marshal(text)
	return fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", raw)
}

func summarySSEBody(initial, delta string) string {
	return summaryStart + summaryTextStart(initial) + summaryTextDelta(delta) + summaryBlockStop + summaryDeltaStop + summaryMessageStop
}

func TestParseJSONSummary(t *testing.T) {
	text := "<analysis>reasoning</analysis>\n<summary>  Adopted résumé.\n\n\nNext.  </summary>"
	body := summaryJSONResponse(t, text)
	got := parseJSONUsage(body, false, true)
	want := summaryProofFor("Summary:\nAdopted résumé.\n\nNext.")
	if !got.complete || got.summary != want || got.text != "" {
		t.Fatalf("complete=%v, summary=%+v, text=%q", got.complete, got.summary, got.text)
	}
	disabled := parseJSONUsage(body, false, false)
	if disabled.summary != (pool.SummaryProof{}) || disabled.usage != got.usage || disabled.complete != got.complete {
		t.Fatal("summary opt-in altered usage/completion or leaked a disabled proof")
	}
	if full := parseJSONUsage(body, true, true); full.text != text || full.summary != want {
		t.Fatal("summary normalization altered full capture text")
	}
}

func TestParseJSONSummaryRejectedShapes(t *testing.T) {
	valid := string(summaryJSONResponse(t, "summary"))
	for _, tc := range []struct{ name, body string }{
		{"missing role", strings.Replace(valid, `"role":"assistant",`, "", 1)},
		{"wrong role", strings.Replace(valid, `"assistant"`, `"user"`, 1)},
		{"wrong type", strings.Replace(valid, `"type":"message"`, `"type":"other"`, 1)},
		{"missing stop", strings.Replace(valid, `"stop_reason":"end_turn",`, "", 1)},
		{"null stop", strings.Replace(valid, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1)},
		{"max tokens", strings.Replace(valid, `"end_turn"`, `"max_tokens"`, 1)},
		{"refusal", strings.Replace(valid, `"end_turn"`, `"refusal"`, 1)},
		{"tool stop", strings.Replace(valid, `"end_turn"`, `"tool_use"`, 1)},
		{"empty text", string(summaryJSONResponse(t, ""))},
		{"normalized empty", string(summaryJSONResponse(t, "<analysis>hidden</analysis>"))},
		{"tool block", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"summary"},{"type":"tool_use","id":"tool_1","name":"x","input":{}}]}`},
		{"refusal block", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"refusal","refusal":"no"}]}`},
		{"multiple text blocks", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}`},
		{"duplicate content merge", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"summary"}],"content":[{"type":"text"}]}`},
		{"missing text", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text"}]}`},
		{"unsigned thinking", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"reasoning","signature":""},{"type":"text","text":"summary"}]}`},
		{"redacted thinking", `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"summary"}]}`},
		{"error", `{"type":"error","error":{"type":"overloaded_error"}}`},
		{"malformed", valid[:len(valid)-1]},
		{"trailing data", valid + "{}"},
		{"over cap", string(summaryJSONResponse(t, strings.Repeat("x", (1<<20)+1)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseJSONUsage([]byte(tc.body), false, true)
			if got.summary != (pool.SummaryProof{}) || got.text != "" {
				t.Fatalf("rejected shape yielded proof/text: %+v, %q", got.summary, got.text)
			}
			without := parseJSONUsage([]byte(tc.body), false, false)
			if got.usage != without.usage || got.complete != without.complete || got.errorType != without.errorType {
				t.Fatal("proof rejection changed ordinary usage/completion/error handling")
			}
		})
	}
}

func TestParseSSESummary(t *testing.T) {
	for _, tc := range []struct{ name, initial, delta, normalized string }{
		{"delta", "", "summary", "summary"},
		{"initial and delta", "Summary: ", "世界", "Summary: 世界"},
		{"initial only", "summary", "", "summary"},
		{"split tags", "<analysis>hidden</analysis>\n<sum", "mary>\n summary \n</summary>", "Summary:\nsummary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := summarySSEBody(tc.initial, tc.delta)
			got := parseSSEUsage(strings.NewReader(body), false, true)
			if !got.complete || got.summary != summaryProofFor(tc.normalized) || got.text != "" {
				t.Fatalf("complete=%v, summary=%+v, text=%q", got.complete, got.summary, got.text)
			}
			without := parseSSEUsage(strings.NewReader(body), false, false)
			if without.summary != (pool.SummaryProof{}) || got.usage != without.usage || got.complete != without.complete {
				t.Fatal("summary opt-in changed ordinary parsing")
			}
		})
	}
}

func TestParseSummaryThinking(t *testing.T) {
	jsonBody := `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"private reasoning","signature":"signed"},{"type":"text","text":"summary"}]}`
	thinking := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"initial\",\"signature\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private reasoning\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"signed\"}}\n\n" + summaryBlockStop
	text := strings.ReplaceAll(summaryTextStart("sum")+summaryTextDelta("mary")+summaryBlockStop, `"index":0`, `"index":1`)
	sseBody := summaryStart + thinking + text + summaryDeltaStop + summaryMessageStop
	for name, got := range map[string]capturedResponse{
		"JSON": parseJSONUsage([]byte(jsonBody), false, true),
		"SSE":  parseSSEUsage(strings.NewReader(sseBody), false, true),
	} {
		if got.summary != summaryProofFor("summary") || got.text != "" {
			t.Fatalf("%s thinking proof=%+v, text=%q", name, got.summary, got.text)
		}
	}
}

func TestParseSSESummaryRejectedShapes(t *testing.T) {
	valid := summarySSEBody("", "summary")
	for _, tc := range []struct{ name, body string }{
		{"missing start", strings.TrimPrefix(valid, summaryStart)},
		{"wrong role", strings.Replace(valid, `"role":"assistant"`, `"role":"user"`, 1)},
		{"wrong message type", strings.Replace(valid, `"type":"message"`, `"type":"other"`, 1)},
		{"missing block start", strings.Replace(valid, summaryTextStart(""), "", 1)},
		{"missing block stop", strings.Replace(valid, summaryBlockStop, "", 1)},
		{"missing delta stop", strings.Replace(valid, summaryDeltaStop, "", 1)},
		{"duplicate delta merge", strings.Replace(valid, `"delta":{"stop_reason":"end_turn","stop_sequence":null}`, `"delta":{"stop_reason":"end_turn","stop_sequence":null},"delta":{}`, 1)},
		{"missing message stop", strings.Replace(valid, summaryMessageStop, "", 1)},
		{"max tokens", strings.Replace(valid, `"end_turn"`, `"max_tokens"`, 1)},
		{"refusal", strings.Replace(valid, `"end_turn"`, `"refusal"`, 1)},
		{"null stop", strings.Replace(valid, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1)},
		{"tool block", strings.Replace(valid, `"type":"text","text":""`, `"type":"tool_use","id":"tool_1","name":"x","input":{}`, 1)},
		{"multiple blocks", strings.Replace(valid, summaryDeltaStop, strings.ReplaceAll(summaryTextStart("other")+summaryBlockStop, `"index":0`, `"index":1`)+summaryDeltaStop, 1)},
		{"bad index", strings.Replace(valid, `"index":0`, `"index":1`, 1)},
		{"missing index", strings.Replace(valid, `"index":0,`, "", 1)},
		{"missing text", strings.Replace(valid, `"type":"text","text":""`, `"type":"text"`, 1)},
		{"wrong delta", strings.Replace(valid, `"text_delta"`, `"input_json_delta"`, 1)},
		{"duplicate start", summaryStart + valid},
		{"duplicate stop", valid + summaryMessageStop},
		{"delta after stop", valid + summaryTextDelta("more")},
		{"malformed", "data: {broken}\n\n" + valid},
		{"non JSON", "data: broken\n\n" + valid},
		{"error then stop", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n" + valid},
		{"error event only", "event: error\n\n" + valid},
		{"unknown event", "event: future\ndata: {\"type\":\"future\"}\n\n" + valid},
		{"mismatched event", strings.Replace(valid, "event: message_start", "event: ping", 1)},
		{"malformed SSE field", "bad-line\n\n" + valid},
		{"multi data frame", strings.Replace(valid, "event: message_stop\n", "event: message_stop\ndata: {}\n", 1)},
		{"unterminated final frame", strings.TrimSuffix(valid, "\n")},
		{"partial trailing event", valid + "event: content_block_delta\ndata: {"},
		{"DONE extension", valid + "data: [DONE]\n\n"},
		{"empty text", summarySSEBody("", "")},
		{"over cap", summaryStart + summaryTextStart("") + summaryTextDelta(strings.Repeat("x", 600<<10)) + summaryTextDelta(strings.Repeat("y", 600<<10)) + summaryBlockStop + summaryDeltaStop + summaryMessageStop},
		{"line cap", valid + "data: " + strings.Repeat("x", (1<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSSEUsage(strings.NewReader(tc.body), false, true)
			if got.summary != (pool.SummaryProof{}) || got.text != "" {
				t.Fatalf("rejected shape yielded proof/text: %+v, %q", got.summary, got.text)
			}
			without := parseSSEUsage(strings.NewReader(tc.body), false, false)
			if got.usage != without.usage || got.complete != without.complete || got.errorType != without.errorType {
				t.Fatal("proof rejection changed ordinary usage/completion/error handling")
			}
		})
	}
}

func TestParseSSESummaryPingAndComments(t *testing.T) {
	body := ": keepalive\n\n" + summaryStart + "event: ping\ndata: {\"type\":\"ping\"}\n\n" + summaryTextStart("summary") + summaryBlockStop + summaryDeltaStop + summaryMessageStop
	if got := parseSSEUsage(strings.NewReader(body), false, true); got.summary != summaryProofFor("summary") {
		t.Fatalf("ping/comments rejected: %+v", got.summary)
	}
}

type summaryFailedReader struct{}

func (summaryFailedReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestParseSSESummaryReadFailure(t *testing.T) {
	r := io.MultiReader(strings.NewReader(summarySSEBody("", "summary")), summaryFailedReader{})
	if got := parseSSEUsage(r, false, true); got.complete || got.summary != (pool.SummaryProof{}) {
		t.Fatal("read error after message_stop produced proof/completion")
	}
}

func TestUsageCaptureSummary(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body string }{
		{"JSON", "application/json", string(summaryJSONResponse(t, "summary"))},
		{"SSE", "text/event-stream", summarySSEBody("sum", "mary")},
	} {
		for _, encoding := range []string{"", "gzip"} {
			t.Run(tc.name+"/"+encoding, func(t *testing.T) {
				body := []byte(tc.body)
				if encoding == "gzip" {
					body = gzipBytes(t, body)
				}
				c := newUsageCapture(tc.contentType, encoding, false, true)
				if c.Summary() != (pool.SummaryProof{}) {
					t.Fatal("proof present before response")
				}
				for start := 0; start < len(body); start += 17 {
					c.Write(body[start:min(start+17, len(body))])
				}
				if c.Summary() != (pool.SummaryProof{}) {
					t.Fatal("proof present before Close")
				}
				usage := c.Close()
				if !c.Complete() || c.Summary() != summaryProofFor("summary") || c.Text() != "" {
					t.Fatalf("complete=%v, summary=%+v, text=%q", c.Complete(), c.Summary(), c.Text())
				}
				if usage.Model != "claude-test" || usage.InputTokens != 7 || usage.OutputTokens != 9 {
					t.Fatalf("usage changed: %+v", usage)
				}
				if c.buf.Len() != 0 {
					t.Fatal("raw summary retained in capture buffer after Close")
				}
			})
		}
	}
}

func TestUsageCaptureSummaryFailures(t *testing.T) {
	jsonBody := summaryJSONResponse(t, "summary")
	sseBody := []byte(summarySSEBody("", "summary"))
	for _, tc := range []struct {
		name, contentType, encoding string
		body                        []byte
	}{
		{"JSON buffer cap", "application/json", "", append(bytes.Clone(jsonBody), bytes.Repeat([]byte(" "), usageJSONCap)...)},
		{"JSON expanded cap", "application/json", "gzip", gzipBytes(t, append(bytes.Clone(jsonBody), bytes.Repeat([]byte(" "), usageJSONCap)...))},
		{"JSON invalid gzip", "application/json", "gzip", jsonBody},
		{"SSE invalid gzip", "text/event-stream", "gzip", sseBody},
		{"JSON gzip trailer", "application/json", "gzip", gzipBytes(t, jsonBody)},
		{"SSE gzip trailer", "text/event-stream", "gzip", gzipBytes(t, sseBody)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if strings.Contains(tc.name, "trailer") {
				body = body[:len(body)-4]
			}
			c := newUsageCapture(tc.contentType, tc.encoding, false, true)
			c.Write(body)
			c.Close()
			if c.Complete() || c.Summary() != (pool.SummaryProof{}) || c.Text() != "" {
				t.Fatalf("failed capture complete=%v, summary=%+v, text=%q", c.Complete(), c.Summary(), c.Text())
			}
		})
	}
}
