// Package prettylog provides a tty-friendly slog.Handler with per-credential
// color highlighting. The credential ID (key "cred") is hashed into a stable
// color so log lines from the same credential are visually grouped, and the
// label (key "label") is rendered next to it as a human-friendly tag.
package prettylog

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"strconv"
	"sync"
)

const (
	reset    = "\x1b[0m"
	dim      = "\x1b[2m"
	red      = "\x1b[31m"
	green    = "\x1b[32m"
	yellow   = "\x1b[33m"
	blue     = "\x1b[34m"
	magenta  = "\x1b[35m"
	cyan     = "\x1b[36m"
	bRed     = "\x1b[91m"
	bGreen   = "\x1b[92m"
	bYellow  = "\x1b[93m"
	bBlue    = "\x1b[94m"
	bMagenta = "\x1b[95m"
	bCyan    = "\x1b[96m"
)

// Distinct, well-spaced palette for credentials. Order is chosen so the first
// few credentials in any session get visually distinct colors.
var credPalette = []string{cyan, magenta, green, yellow, blue, bCyan, bMagenta, bGreen, bYellow, bBlue}

// Options configures a Handler.
type Options struct {
	Level slog.Level
	Color bool // if false, ANSI codes are omitted
}

// Handler is a slog.Handler that renders one line per record:
//
//	HH:MM:SS.mmm LVL [credShort label] message key=value key=value
type Handler struct {
	out    io.Writer
	level  slog.Level
	color  bool
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

// New returns a Handler writing to out.
func New(out io.Writer, opts *Options) *Handler {
	if opts == nil {
		opts = &Options{}
	}
	return &Handler{
		out:   out,
		level: opts.Level,
		color: opts.Color,
		mu:    &sync.Mutex{},
	}
}

// Enabled reports whether the given level is enabled.
func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

// Handle renders one line.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)

	// Timestamp
	if h.color {
		buf = append(buf, dim...)
	}
	buf = r.Time.AppendFormat(buf, "15:04:05.000")
	if h.color {
		buf = append(buf, reset...)
	}
	buf = append(buf, ' ')

	// Level tag
	buf = append(buf, h.levelTag(r.Level)...)
	buf = append(buf, ' ')

	// Find cred + label across both pre-bound attrs and record attrs.
	var credID, label string
	scan := func(a slog.Attr) {
		switch a.Key {
		case "cred":
			credID = a.Value.String()
		case "label":
			label = a.Value.String()
		}
	}
	for _, a := range h.attrs {
		scan(a)
	}
	r.Attrs(func(a slog.Attr) bool { scan(a); return true })

	// Credential prefix
	if credID != "" {
		col := ""
		if h.color {
			col = credColor(credID)
			buf = append(buf, col...)
		}
		buf = append(buf, '[')
		buf = append(buf, shortCred(credID)...)
		if label != "" {
			buf = append(buf, ' ')
			buf = append(buf, label...)
		}
		buf = append(buf, ']')
		if h.color {
			buf = append(buf, reset...)
		}
		buf = append(buf, ' ')
	}

	// Message
	if h.color {
		buf = appendBold(buf, r.Message)
	} else {
		buf = append(buf, r.Message...)
	}

	// Remaining attrs (skip cred/label — already rendered as prefix).
	appendAttr := func(a slog.Attr) {
		if a.Key == "cred" || a.Key == "label" {
			return
		}
		buf = append(buf, ' ')
		if h.color {
			buf = append(buf, dim...)
		}
		buf = append(buf, a.Key...)
		buf = append(buf, '=')
		if h.color {
			buf = append(buf, reset...)
		}
		buf = appendValue(buf, a.Value, h.color)
	}
	for _, a := range h.attrs {
		appendAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool { appendAttr(a); return true })

	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(buf)
	return err
}

// WithAttrs returns a child handler that pre-applies the given attributes.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	out := *h
	out.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &out
}

// WithGroup is a no-op (groups are not rendered).
func (h *Handler) WithGroup(name string) slog.Handler {
	out := *h
	out.groups = append(append([]string{}, h.groups...), name)
	return &out
}

func (h *Handler) levelTag(l slog.Level) string {
	var c, name string
	switch {
	case l < slog.LevelInfo:
		c, name = dim, "DBG"
	case l < slog.LevelWarn:
		c, name = bCyan, "INF"
	case l < slog.LevelError:
		c, name = bYellow, "WRN"
	default:
		c, name = bRed, "ERR"
	}
	if h.color {
		return c + name + reset
	}
	return name
}

func credColor(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return credPalette[int(h.Sum32()%uint32(len(credPalette)))]
}

func shortCred(id string) string {
	// Strip "cred_" prefix for compactness, then keep first 6 + last 4 hex chars.
	core := id
	if len(core) > 5 && core[:5] == "cred_" {
		core = core[5:]
	}
	if len(core) <= 10 {
		return core
	}
	return core[:6] + "…" + core[len(core)-4:]
}

func appendBold(buf []byte, s string) []byte {
	buf = append(buf, "\x1b[1m"...)
	buf = append(buf, s...)
	buf = append(buf, reset...)
	return buf
}

func appendValue(buf []byte, v slog.Value, color bool) []byte {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if needsQuote(s) {
			return strconv.AppendQuote(buf, s)
		}
		return append(buf, s...)
	case slog.KindInt64:
		return strconv.AppendInt(buf, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(buf, v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.AppendFloat(buf, v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.AppendBool(buf, v.Bool())
	case slog.KindDuration:
		return append(buf, v.Duration().String()...)
	case slog.KindTime:
		return v.Time().AppendFormat(buf, "15:04:05.000")
	default:
		// Fall back to %v.
		s := fmt.Sprintf("%v", v.Any())
		if needsQuote(s) {
			return strconv.AppendQuote(buf, s)
		}
		return append(buf, s...)
	}
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, c := range s {
		if c == ' ' || c == '"' || c == '\t' || c < 32 {
			return true
		}
	}
	return false
}

// Compile-time check.
var _ slog.Handler = (*Handler)(nil)

// Avoid unused-import warnings if the palette is trimmed.
var _ = []string{red, green, blue, magenta, yellow, bRed, bGreen, bBlue, bMagenta, bYellow}
