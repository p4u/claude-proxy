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

// parseSSEUsage scans an Anthropic SSE stream and extracts token usage from the
// `message_start` (model + input/cache tokens) and `message_delta` (final
// output_tokens) events. When captureText is set it also concatenates the
// assistant's visible output from `content_block_delta` events of type
// `text_delta` (thinking and tool deltas are ignored). Malformed lines are
// skipped; it always drains r.
func parseSSEUsage(r io.Reader, captureText bool) (tokenUsage, string) {
	var u tokenUsage
	var text strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // up to 1 MiB per SSE data line
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
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
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			u = u.apply(ev.Message.Model, ev.Message.Usage, true)
		case "message_delta":
			u = u.apply("", ev.Usage, false)
		case "content_block_delta":
			if captureText && ev.Delta.Type == "text_delta" && text.Len() < captureTextCap {
				text.WriteString(ev.Delta.Text)
			}
		}
	}
	return u, text.String()
}

// parseJSONUsage parses a non-stream Anthropic Messages response body and
// extracts top-level model + usage, plus (when captureText is set) the
// concatenated `content[]` text blocks. Malformed bodies yield a zero
// tokenUsage and empty text.
func parseJSONUsage(b []byte, captureText bool) (tokenUsage, string) {
	var body struct {
		Model   string     `json:"model"`
		Usage   usageBlock `json:"usage"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return tokenUsage{}, ""
	}
	var text string
	if captureText {
		var parts []string
		for _, c := range body.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		text = strings.Join(parts, "\n\n")
	}
	return tokenUsage{}.apply(body.Model, body.Usage, true), text
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
	stream bool
	buf    bytes.Buffer
	gzip   bool

	// captureText accumulates the assistant's visible output text; only set
	// when the request's user opted into full conversation capture.
	captureText bool

	usage tokenUsage
	text  string
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
	c.usage, c.text = parseSSEUsage(r, c.captureText)
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
	// Buffered JSON path, capped.
	if remaining := usageJSONCap - c.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		c.buf.Write(p)
	}
}

// Text returns the assistant output text accumulated during Close. It is empty
// unless the capture was built with captureText.
func (c *usageCapture) Text() string { return c.text }

// Close finalizes parsing and returns the extracted usage. Safe to call once.
func (c *usageCapture) Close() tokenUsage {
	if c.stream {
		_ = c.pw.Close()
		<-c.done
		return c.usage
	}
	raw := c.buf.Bytes()
	if c.gzip {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			if d, derr := io.ReadAll(io.LimitReader(zr, usageJSONCap)); derr == nil {
				raw = d
			}
			zr.Close()
		}
	}
	u, text := parseJSONUsage(raw, c.captureText)
	c.text = text
	return u
}
