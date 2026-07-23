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
	// input + cache counters are authoritative at message_start (SSE) or on the
	// single non-stream body; message_delta only carries output_tokens.
	if isStart {
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
// output_tokens) events. Malformed lines are skipped; it always drains r.
func parseSSEUsage(r io.Reader) tokenUsage {
	var u tokenUsage
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
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			u = u.apply(ev.Message.Model, ev.Message.Usage, true)
		case "message_delta":
			u = u.apply("", ev.Usage, false)
		}
	}
	return u
}

// parseJSONUsage parses a non-stream Anthropic Messages response body and
// extracts top-level model + usage. Malformed bodies yield a zero tokenUsage.
func parseJSONUsage(b []byte) tokenUsage {
	var body struct {
		Model string     `json:"model"`
		Usage usageBlock `json:"usage"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return tokenUsage{}
	}
	return tokenUsage{}.apply(body.Model, body.Usage, true)
}

const usageJSONCap = 1 << 20 // 1 MiB cap on buffered non-stream bodies

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

	usage tokenUsage
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
func newUsageCapture(contentType, contentEncoding string) *usageCapture {
	c := &usageCapture{gzip: isGzip(contentEncoding)}
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
	c.usage = parseSSEUsage(r)
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
	return parseJSONUsage(raw)
}
