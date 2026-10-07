package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
)

// tokenUsage holds the token counters parsed out of an Anthropic Messages
// response. Zero values mean "unknown / not reported".
type tokenUsage struct {
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// usageBlock mirrors the Anthropic `usage` object as seen on both SSE events
// and non-stream responses. Pointers distinguish "absent" from "zero".
type usageBlock struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	CacheCreationInput *int64 `json:"cache_creation_input_tokens"`
	CacheReadInput     *int64 `json:"cache_read_input_tokens"`
}

func (u tokenUsage) apply(model string, b usageBlock, isStart bool) tokenUsage {
	if model != "" {
		u.Model = model
	}
	// Anthropic puts input + cache counters at message_start, while translated
	// OpenAI streams can only learn exact usage in their final chunk. Accept
	// those fields whenever present so the latter can correct its initial zero.
	if isStart || b.InputTokens != nil || b.CacheCreationInput != nil || b.CacheReadInput != nil {
		if b.InputTokens != nil {
			u.InputTokens = *b.InputTokens
		}
		if b.CacheCreationInput != nil {
			u.CacheCreationTokens = *b.CacheCreationInput
		}
		if b.CacheReadInput != nil {
			u.CacheReadTokens = *b.CacheReadInput
		}
	}
	if b.OutputTokens != nil {
		u.OutputTokens = *b.OutputTokens
	}
	return u
}

// capturedResponse keeps usage independently of completion: an interrupted
// generation can report billable tokens without finishing successfully.
type capturedResponse struct {
	usage     tokenUsage
	text      string
	complete  bool
	errorType string
}

// parseSSEUsage extracts usage and opt-in visible text from an Anthropic SSE
// stream. Malformed data is skipped for usage, but cannot establish successful
// completion. Only message_stop with no error event/read/parse failure does.
func parseSSEUsage(r io.Reader, captureText bool) capturedResponse {
	var result capturedResponse
	var text strings.Builder
	var failed, errorEvent bool
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // up to 1 MiB per SSE data line
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			errorEvent = false
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			errorEvent = string(bytes.TrimSpace(line[len("event:"):])) == "error"
			if errorEvent {
				failed, result.errorType = true, "unknown"
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if payload[0] != '{' {
			failed = true
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string     `json:"model"`
				Usage usageBlock `json:"usage"`
			} `json:"message"`
			Usage usageBlock `json:"usage"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			failed = true
			continue
		}
		if ev.Type == "error" || errorEvent {
			failed, result.errorType = true, safeErrorType(ev.Error.Type)
		}
		switch ev.Type {
		case "message_start":
			result.usage = result.usage.apply(ev.Message.Model, ev.Message.Usage, true)
		case "message_delta":
			result.usage = result.usage.apply("", ev.Usage, false)
		case "message_stop":
			result.complete = true
		case "content_block_delta":
			if captureText && ev.Delta.Type == "text_delta" && text.Len() < captureTextCap {
				text.WriteString(ev.Delta.Text)
			}
		}
	}
	result.complete = result.complete && !failed && sc.Err() == nil
	result.text = text.String()
	return result
}

// parseJSONUsage extracts model, usage and opt-in visible text. Successful
// completion requires a valid JSON object rather than a truncated/error body.
func parseJSONUsage(b []byte, captureText bool) capturedResponse {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return capturedResponse{}
	}
	var body struct {
		Type    string     `json:"type"`
		Model   string     `json:"model"`
		Usage   usageBlock `json:"usage"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return capturedResponse{}
	}
	result := capturedResponse{
		usage:    tokenUsage{}.apply(body.Model, body.Usage, true),
		complete: body.Type != "error" && body.Error == nil,
	}
	if !result.complete {
		result.errorType = "unknown"
		if body.Error != nil {
			result.errorType = safeErrorType(body.Error.Type)
		}
	}
	if captureText {
		var parts []string
		for _, c := range body.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		result.text = strings.Join(parts, "\n\n")
	}
	return result
}

const usageJSONCap = 1 << 20 // 1 MiB cap on buffered non-stream bodies

// captureTextCap bounds the assistant text accumulated in memory while
// streaming. It is generous relative to the per-message rune cap so the stored
// message is cut by capMessage, not by this safety valve.
const captureTextCap = 4 << 20 // 4 MiB

// usageCapture tees a response body (fed via Write) into a usage parser without
// affecting the client stream. It handles gzip transparently on the parse side.
// Parse errors are silent and can never block or break the caller's writes.
type usageCapture struct {
	pw     *io.PipeWriter
	done   chan struct{}
	broken bool // pipe reader gone; stop feeding

	// non-stream JSON path (buffered, no goroutine)
	stream    bool
	buf       bytes.Buffer
	gzip      bool
	truncated bool
	closed    bool

	// captureText accumulates the assistant's visible output text; only set
	// when the request's user opted into full conversation capture.
	captureText bool

	result capturedResponse
}

// isEventStream reports whether the content type is an SSE stream.
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func isGzip(contentEncoding string) bool {
	return strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip")
}

// newUsageCapture builds a capture for the given response headers. For SSE it
// spins up a background parser reading through an io.Pipe (so gzip can stream);
// for non-stream JSON it buffers up to 1 MiB and parses on Close.
func newUsageCapture(contentType, contentEncoding string, captureText bool) *usageCapture {
	c := &usageCapture{gzip: isGzip(contentEncoding), captureText: captureText}
	if isEventStream(contentType) {
		c.stream = true
		pr, pw := io.Pipe()
		c.pw = pw
		c.done = make(chan struct{})
		go c.runSSE(pr)
	}
	return c
}

func (c *usageCapture) runSSE(pr *io.PipeReader) {
	defer close(c.done)
	// Always drain to EOF so Write never blocks, even on gzip/parse failure.
	defer func() { _, _ = io.Copy(io.Discard, pr) }()

	var r io.Reader = pr
	if c.gzip {
		zr, err := gzip.NewReader(pr)
		if err != nil {
			return
		}
		defer zr.Close()
		r = zr
	}
	c.result = parseSSEUsage(r, c.captureText)
}

// Write feeds response bytes to the parser. It never returns an error to the
// caller and never blocks the client path beyond the parser's drain.
func (c *usageCapture) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	if c.stream {
		if c.broken {
			return
		}
		if _, err := c.pw.Write(p); err != nil {
			c.broken = true
		}
		return
	}
	// Buffered JSON path, capped. A valid prefix at the cap must not be
	// mistaken for a completely inspected response.
	remaining := usageJSONCap - c.buf.Len()
	if len(p) > remaining {
		c.truncated = true
		p = p[:remaining]
	}
	c.buf.Write(p)
}

// Text returns the assistant output text accumulated during Close. It is empty
// unless the capture was built with captureText.
func (c *usageCapture) Text() string { return c.result.text }

// Complete reports protocol completion after Close. Transport, context and
// client-write success are separate requirements checked by the relay.
func (c *usageCapture) Complete() bool { return c.closed && c.result.complete }

// ErrorType returns only a sanitized error type observed during parsing.
func (c *usageCapture) ErrorType() string { return c.result.errorType }

// Close finalizes parsing and returns the extracted usage. Safe to call once.
func (c *usageCapture) Close() tokenUsage {
	defer func() { c.closed = true }()
	if c.stream {
		_ = c.pw.Close()
		<-c.done
		return c.result.usage
	}
	raw := c.buf.Bytes()
	if c.gzip {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return tokenUsage{}
		}
		defer zr.Close()
		raw, err = io.ReadAll(io.LimitReader(zr, usageJSONCap+1))
		if err != nil {
			return tokenUsage{}
		}
		if len(raw) > usageJSONCap {
			c.truncated = true
			raw = raw[:usageJSONCap]
		}
	}
	c.result = parseJSONUsage(raw, c.captureText)
	c.result.complete = c.result.complete && !c.truncated
	return c.result.usage
}
