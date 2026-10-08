package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/p4u/claude-proxy/internal/pool"
)

const summaryTextCap = 1 << 20 // transient text only; never persisted or logged

var summaryBlankLines = regexp.MustCompile(`\n{3,}`)

// summaryFingerprint mirrors Claude Code 2.1.280's Dlr normalization, not the
// raw model output. The adopted compaction wrapper contains this normalized
// text. Only the first analysis and summary tags are rewritten, in that order.
func summaryFingerprint(text string) pool.SummaryProof {
	if len(text) > summaryTextCap || !utf8.ValidString(text) {
		return pool.SummaryProof{}
	}
	if start, end := firstSummaryTag(text, "analysis"); start >= 0 {
		text = text[:start] + text[end:]
	}
	if start, end := firstSummaryTag(text, "summary"); start >= 0 {
		inner := text[start+len("<summary>") : end-len("</summary>")]
		text = text[:start] + "Summary:\n" + strings.TrimFunc(inner, summaryTrimSpace) + text[end:]
	}
	text = summaryBlankLines.ReplaceAllString(text, "\n\n")
	text = strings.TrimFunc(text, summaryTrimSpace)
	if text == "" {
		return pool.SummaryProof{}
	}
	return pool.SummaryProof{Digest: sha256.Sum256([]byte(text)), Bytes: len(text)}
}

func firstSummaryTag(text, tag string) (int, int) {
	open, close := "<"+tag+">", "</"+tag+">"
	start := strings.Index(text, open)
	if start < 0 {
		return -1, -1
	}
	end := strings.Index(text[start+len(open):], close)
	if end < 0 {
		return -1, -1
	}
	return start, start + len(open) + end + len(close)
}

// ECMAScript trim includes BOM but excludes NEL, unlike strings.TrimSpace.
func summaryTrimSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	default:
		return r >= 0x2000 && r <= 0x200a
	}
}

type summaryContentBlock struct {
	Type      string  `json:"type"`
	Text      *string `json:"text"`
	Thinking  *string `json:"thinking"`
	Signature *string `json:"signature"`
}

type summaryMessage struct {
	Type         string                `json:"type"`
	Role         string                `json:"role"`
	Content      []summaryContentBlock `json:"content"`
	StopReason   *string               `json:"stop_reason"`
	StopSequence *string               `json:"stop_sequence"`
}

func (m *summaryMessage) assistant() bool {
	return m != nil && m.Type == "message" && m.Role == "assistant" && m.Content != nil
}

// parseJSONSummary deliberately accepts only one visible text block, optionally
// preceded by signed thinking blocks. Tools, redacted thinking, refusals and
// future block types fall back to the existing account without a handoff proof.
func parseJSONSummary(raw []byte) pool.SummaryProof {
	if len(raw) > usageJSONCap || !utf8.Valid(raw) {
		return pool.SummaryProof{}
	}
	var message summaryMessage
	if !unmarshalSummaryJSON(raw, &message) || !message.assistant() ||
		message.StopReason == nil || *message.StopReason != "end_turn" || message.StopSequence != nil {
		return pool.SummaryProof{}
	}
	var text *string
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			if text != nil || block.Text == nil {
				return pool.SummaryProof{}
			}
			text = block.Text
		case "thinking":
			if text != nil || block.Thinking == nil || block.Signature == nil || *block.Signature == "" {
				return pool.SummaryProof{}
			}
		default:
			return pool.SummaryProof{}
		}
	}
	if text == nil {
		return pool.SummaryProof{}
	}
	return summaryFingerprint(*text)
}

// encoding/json merges duplicate objects into reused structs, whereas the CLI's
// JSON.parse replaces them. Reject duplicate members rather than prove a hybrid
// message that the client never received. The ordinary usage parser stays lenient.
func unmarshalSummaryJSON(raw []byte, dst any) bool {
	if json.Unmarshal(raw, dst) != nil {
		return false
	}
	return uniqueSummaryMembers(json.NewDecoder(bytes.NewReader(raw)))
}

func uniqueSummaryMembers(dec *json.Decoder) bool {
	token, err := dec.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for dec.More() {
			key, err := dec.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !uniqueSummaryMembers(dec) {
				return false
			}
		}
		token, err = dec.Token()
		return err == nil && token == json.Delim('}')
	case json.Delim('['):
		for dec.More() {
			if !uniqueSummaryMembers(dec) {
				return false
			}
		}
		token, err = dec.Token()
		return err == nil && token == json.Delim(']')
	default:
		return true // the initial Unmarshal already validated JSON syntax
	}
}

type summarySSEEvent struct {
	Type    string               `json:"type"`
	Message *summaryMessage      `json:"message"`
	Index   *int                 `json:"index"`
	Block   *summaryContentBlock `json:"content_block"`
	Delta   *struct {
		Type         string  `json:"type"`
		Text         *string `json:"text"`
		Thinking     *string `json:"thinking"`
		Signature    *string `json:"signature"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
}

// summarySSE validates independently of the permissive usage parser. The narrow
// SSE subset is one JSON data line per event, optional matching event fields,
// sequential content indexes, signed thinking before exactly one text block,
// end_turn and a dispatched message_stop. Ping/comments are harmless. Unknown
// shapes fail closed for handoffs without changing usage or relay completion.
// The zero value is ready to consume lines; allocate one only for summary calls.
type summarySSE struct {
	invalid   bool
	eventName string
	hasEvent  bool
	hasData   bool
	pending   summarySSEEvent

	started    bool
	ended      bool
	stopped    bool
	block      string
	nextIndex  int
	textBlocks int
	signed     bool
	text       []byte
}

func (s *summarySSE) reject() {
	s.invalid = true
	s.pending = summarySSEEvent{}
	clear(s.text)
	s.text = nil
}

func (s *summarySSE) line(line []byte) {
	if s.invalid {
		return
	}
	if !utf8.Valid(line) {
		s.reject()
		return
	}
	if len(line) == 0 {
		if s.hasData {
			if s.hasEvent && s.eventName != s.pending.Type {
				s.reject()
			} else {
				s.event(s.pending)
			}
		} else if s.hasEvent {
			s.reject()
		}
		s.eventName, s.hasEvent, s.hasData = "", false, false
		s.pending = summarySSEEvent{}
		return
	}
	if line[0] == ':' {
		return
	}
	field, value, ok := bytes.Cut(line, []byte(":"))
	if !ok {
		s.reject()
		return
	}
	value = bytes.TrimPrefix(value, []byte(" ")) // SSE removes one leading space
	switch string(field) {
	case "event":
		if s.hasEvent || s.hasData || len(value) == 0 {
			s.reject()
			return
		}
		s.eventName, s.hasEvent = string(value), true
	case "data":
		if s.hasData || !unmarshalSummaryJSON(value, &s.pending) || s.pending.Type == "" {
			s.reject()
			return
		}
		s.hasData = true
	default:
		s.reject()
	}
}

func (s *summarySSE) event(ev summarySSEEvent) {
	if s.stopped {
		s.reject()
		return
	}
	switch ev.Type {
	case "ping":
		return
	case "message_start":
		if s.started || !ev.Message.assistant() || len(ev.Message.Content) != 0 || ev.Message.StopReason != nil || ev.Message.StopSequence != nil {
			s.reject()
			return
		}
		s.started = true
	case "content_block_start":
		if !s.started || s.ended || s.block != "" || ev.Index == nil || *ev.Index != s.nextIndex || ev.Block == nil {
			s.reject()
			return
		}
		s.block, s.signed = ev.Block.Type, false
		switch s.block {
		case "text":
			if s.textBlocks != 0 || ev.Block.Text == nil {
				s.reject()
				return
			}
			s.textBlocks++
			s.appendText(*ev.Block.Text)
		case "thinking":
			if s.textBlocks != 0 || ev.Block.Thinking == nil {
				s.reject()
				return
			}
			s.signed = ev.Block.Signature != nil && *ev.Block.Signature != ""
		default:
			s.reject()
		}
	case "content_block_delta":
		if s.block == "" || ev.Index == nil || *ev.Index != s.nextIndex || ev.Delta == nil {
			s.reject()
			return
		}
		switch {
		case s.block == "text" && ev.Delta.Type == "text_delta" && ev.Delta.Text != nil:
			s.appendText(*ev.Delta.Text)
		case s.block == "thinking" && ev.Delta.Type == "thinking_delta" && ev.Delta.Thinking != nil && !s.signed:
			// Thinking is validated but never accumulated for the proof.
		case s.block == "thinking" && ev.Delta.Type == "signature_delta" && ev.Delta.Signature != nil:
			s.signed = s.signed || *ev.Delta.Signature != ""
		default:
			s.reject()
		}
	case "content_block_stop":
		if s.block == "" || ev.Index == nil || *ev.Index != s.nextIndex || (s.block == "thinking" && !s.signed) {
			s.reject()
			return
		}
		s.block = ""
		s.nextIndex++
	case "message_delta":
		if !s.started || s.ended || s.block != "" || s.textBlocks != 1 || ev.Delta == nil ||
			ev.Delta.StopReason == nil || *ev.Delta.StopReason != "end_turn" || ev.Delta.StopSequence != nil {
			s.reject()
			return
		}
		s.ended = true
	case "message_stop":
		if !s.ended || s.block != "" {
			s.reject()
			return
		}
		s.stopped = true
	default:
		s.reject()
	}
}

func (s *summarySSE) appendText(text string) {
	if len(text) > summaryTextCap-len(s.text) {
		s.reject()
		return
	}
	if needed := len(s.text) + len(text); needed > cap(s.text) {
		// Bound capacity as well as length, and discard prior text copies.
		buf := make([]byte, len(s.text), min(summaryTextCap, max(needed, 2*cap(s.text))))
		copy(buf, s.text)
		clear(s.text)
		s.text = buf
	}
	s.text = append(s.text, text...)
}

func (s *summarySSE) finish(complete bool) pool.SummaryProof {
	defer func() {
		clear(s.text)
		s.text = nil
		s.pending = summarySSEEvent{}
	}()
	if !complete || s.invalid || !s.stopped || s.hasEvent || s.hasData {
		return pool.SummaryProof{}
	}
	return summaryFingerprint(string(s.text))
}
