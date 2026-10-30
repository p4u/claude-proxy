package proxy

import (
	"bytes"
	"encoding/json"
)

// Claude Code copies the model a response reports into the session record and
// restores it as the session's model on resume. Advertised IDs are aliases
// this proxy invented for the picker (claude-glm-5.2, claude-openai-…), while
// the upstream answers with its own native name (glm-5.2 — or, on one custom
// host, the literal "assistant"), a string the client never offered. The
// session then either falls back to the default model with a "Session model …
// could not be restored" warning, or silently records a model that was never
// picked. Restoring the client's own string keeps the session on the model
// the user chose.
//
// The rewriter touches only the model field — the message_start event of an
// SSE stream, the top-level field of a plain JSON body — and re-emits
// everything else byte-for-byte. Once the model-bearing event has been seen
// (or given up on) it is a no-op pass-through with no per-byte cost, so only
// the head of a response is ever inspected.
//
// The usage capture keeps reading the original bytes, so request_log and the
// prompt captures continue to record the native model name.
type responseModelRewriter struct {
	model string
	sse   bool
	done  bool
	// head buffers the not-yet-decided start of the response.
	head []byte
}

// modelRewriteHead bounds how much of a stream is searched for message_start.
// It is the first event every Anthropic-shaped upstream sends; a stream that
// runs past this without one never had one.
const modelRewriteHead = 64 << 10

func newResponseModelRewriter(model string, sse bool) *responseModelRewriter {
	return &responseModelRewriter{model: model, sse: sse}
}

// filter rewrites the bytes that are still part of the undecided head and
// returns what should be sent to the client — nil while more head is needed.
// After the decision it returns p unchanged.
func (rw *responseModelRewriter) filter(p []byte) []byte {
	if rw.done {
		return p
	}
	rw.head = append(rw.head, p...)
	if rw.sse {
		return rw.filterSSE()
	}
	// A JSON body is decided once, whole, at flush. A body that outruns
	// maxBodyBytes is delivered verbatim from that point on.
	if len(rw.head) > maxBodyBytes {
		rw.done = true
		out := rw.head
		rw.head = nil
		return out
	}
	return nil
}

// flush emits whatever the head still holds, rewritten when its model field
// was found, and ends rewriting.
func (rw *responseModelRewriter) flush() []byte {
	if rw.done {
		return nil
	}
	rw.done = true
	body := rw.head
	rw.head = nil
	if rw.sse {
		// The stream ended without a decidable head (or mid-head): the bytes
		// pass through unchanged.
		return body
	}
	return rewriteJSONModel(body, rw.model)
}

// filterSSE scans buffered events until the model-bearing one is found. Events
// ahead of it (pings, comments) are passed through while the search continues.
func (rw *responseModelRewriter) filterSSE() []byte {
	var out []byte
	for !rw.done {
		raw, rest, found := cutSSEEvent(rw.head)
		if !found {
			if len(rw.head) > modelRewriteHead {
				rw.done = true
				out = append(out, rw.head...)
				rw.head = nil
			}
			return out
		}
		prefix, payload, suffix, ok := sseDataLine(raw)
		if ok {
			switch classifySSE(payload) {
			case sseMessageStart:
				// Whatever happens here, no later event carries a model.
				rw.done = true
				if replaced, ok := rewriteMessageStartModel(payload, rw.model); ok {
					out = append(out, prefix...)
					out = append(out, "data: "...)
					out = append(out, replaced...)
					out = append(out, suffix...)
					out = append(out, rest...)
					rw.head = nil
					return out
				}
				out = append(out, rw.head...)
				rw.head = nil
				return out
			case sseProgression:
				// Content is flowing without a message_start: none will follow.
				rw.done = true
				out = append(out, rw.head...)
				rw.head = nil
				return out
			}
		}
		// ping, error, comment or unparseable: keep it verbatim, keep looking.
		out = append(out, raw...)
		rw.head = rest
	}
	return out
}

// cutSSEEvent splits the first complete SSE event, including its terminating
// blank line, off the front of buf. found is false while no terminator has
// arrived yet.
func cutSSEEvent(buf []byte) (event, rest []byte, found bool) {
	lf := bytes.Index(buf, []byte("\n\n"))
	cr := bytes.Index(buf, []byte("\r\n\r\n"))
	var i int
	switch {
	case lf >= 0 && (cr < 0 || lf <= cr):
		i = lf + 2
	case cr >= 0:
		i = cr + 4
	default:
		return nil, buf, false
	}
	return buf[:i:i], buf[i:], true
}

// sseDataLine locates the payload of an event's single data line, returning
// the bytes around it so a replacement can be spliced in without disturbing
// the rest of the framing. Events with no data line (comments, keep-alives)
// or with several are reported as not found.
func sseDataLine(event []byte) (prefix, payload, suffix []byte, ok bool) {
	dataStart, dataEnd := -1, 0
	for pos := 0; pos < len(event); {
		lineEnd := bytes.IndexByte(event[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(event) - pos
		}
		if bytes.HasPrefix(bytes.TrimSuffix(event[pos:pos+lineEnd], []byte("\r")), []byte("data:")) {
			if dataStart >= 0 {
				return nil, nil, nil, false
			}
			dataStart, dataEnd = pos, pos+lineEnd
		}
		pos += lineEnd + 1
	}
	if dataStart < 0 {
		return nil, nil, nil, false
	}
	line := event[dataStart:dataEnd]
	payload = bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
	return event[:dataStart:dataStart], payload, event[dataEnd:], true
}

type sseKind int

const (
	sseOther        sseKind = iota // ping, error, comment, unparseable: pass through, keep looking
	sseMessageStart                // carries the model field
	sseProgression                 // content_block_*, message_delta, message_stop: no message_start will follow
)

func classifySSE(payload []byte) sseKind {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &env) != nil {
		return sseOther
	}
	switch env.Type {
	case "message_start":
		return sseMessageStart
	case "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop":
		return sseProgression
	}
	return sseOther
}

// rewriteMessageStartModel replaces the model inside a message_start payload.
// ok is false when the payload is not rewritable (not message_start, no model
// field); the caller then leaves the whole event alone.
func rewriteMessageStartModel(payload []byte, model string) ([]byte, bool) {
	var env struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type != "message_start" || len(env.Message) == 0 {
		return nil, false
	}
	replaced, ok := setJSONStringField(env.Message, "model", model)
	if !ok {
		return nil, false
	}
	return setJSONRawField(payload, "message", replaced)
}

// rewriteJSONModel rewrites the top-level model field of a complete JSON
// response body. A body without one, or unparseable, is returned unchanged.
func rewriteJSONModel(body []byte, model string) []byte {
	return setJSONStringFieldOrIdentity(body, "model", model)
}

// setJSONStringField returns obj with field set to the JSON encoding of val,
// preserving every other member byte-for-byte. ok is false when obj is not a
// JSON object or does not carry the field.
func setJSONStringField(obj []byte, field, val string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return nil, false
	}
	if _, ok := m[field]; !ok {
		return nil, false
	}
	return setJSONRawField(obj, field, json.RawMessage(mustJSONString(val)))
}

// setJSONStringFieldOrIdentity is setJSONStringField that returns the original
// bytes when the field is absent or anything fails to parse.
func setJSONStringFieldOrIdentity(obj []byte, field, val string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return obj
	}
	if _, ok := m[field]; !ok {
		return obj
	}
	if cur, ok := m[field]; ok {
		var s string
		if json.Unmarshal(cur, &s) == nil && s == val {
			return obj
		}
	}
	out, ok := setJSONRawField(obj, field, json.RawMessage(mustJSONString(val)))
	if !ok {
		return obj
	}
	return out
}

func setJSONRawField(obj []byte, field string, val json.RawMessage) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return nil, false
	}
	m[field] = val
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

func mustJSONString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return b
}
