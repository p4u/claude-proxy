package webui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type convView struct {
	ID           string `json:"id"`
	CredentialID string `json:"credential_id"`
	CreatedAt    string `json:"created_at"`
	LastSeenAt   string `json:"last_seen_at"`
	RequestCount int    `json:"request_count"`
	Status       string `json:"status"`
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, credential_id, created_at, last_seen_at, request_count, status
		FROM conversations ORDER BY last_seen_at DESC LIMIT ?`, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []convView{}
	for rows.Next() {
		var v convView
		var created, seen int64
		if err := rows.Scan(&v.ID, &v.CredentialID, &created, &seen, &v.RequestCount, &v.Status); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		v.CreatedAt = time.Unix(created, 0).Format(time.RFC3339)
		v.LastSeenAt = time.Unix(seen, 0).Format(time.RFC3339)
		out = append(out, v)
	}
	writeJSON(w, out)
}

type messageView struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Model   string `json:"model"`
	TS      string `json:"ts"`
}

type convUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// exportMaxMessages bounds a markdown export so one request can never buffer an
// unbounded conversation.
const exportMaxMessages = 5000

// handleConversationSub routes the per-conversation sub-resources
// (/conversations/{convID}/messages and /conversations/{convID}/export.md).
// Conversation IDs may themselves contain "/" (they can be caller-supplied
// metadata.user_id values), so the action is split off the END of the path.
func (s *Server) handleConversationSub(w http.ResponseWriter, r *http.Request, rest string) {
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	convID, action := rest[:i], rest[i+1:]
	if convID == "" || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	switch action {
	case "messages":
		s.listConvMessages(w, r, convID)
	case "export.md":
		s.exportConv(w, r, convID)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// loadConvMessages returns a page of a conversation's messages. When the
// conversation has no conversation_message rows (user not opted into full
// capture) it falls back to that conversation's prompt_log rows rendered as
// user turns, so the viewer works in both modes.
func (s *Server) loadConvMessages(ctx context.Context, convID string, limit, offset int) (items []messageView, total int, source string, err error) {
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversation_message WHERE conv_id=?`, convID).Scan(&total); err != nil {
		return nil, 0, "", err
	}
	source = "full"
	query := `SELECT seq, role, content, model, ts FROM conversation_message
	          WHERE conv_id=? ORDER BY seq LIMIT ? OFFSET ?`
	if total == 0 {
		source = "prompts"
		if err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM prompt_log WHERE conv_id=?`, convID).Scan(&total); err != nil {
			return nil, 0, "", err
		}
		query = `SELECT ROW_NUMBER() OVER (ORDER BY ts, id) - 1, 'user', prompt, model, ts
		         FROM prompt_log WHERE conv_id=? ORDER BY ts, id LIMIT ? OFFSET ?`
	}

	rows, err := s.db.QueryContext(ctx, query, convID, limit, offset)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()
	items = []messageView{}
	for rows.Next() {
		var m messageView
		var ts int64
		if err = rows.Scan(&m.Seq, &m.Role, &m.Content, &m.Model, &ts); err != nil {
			return nil, 0, "", err
		}
		m.TS = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		items = append(items, m)
	}
	return items, total, source, rows.Err()
}

// convOwner resolves the user a conversation was captured for. Unattributed
// conversations yield a zero-value convUser rather than an error.
func (s *Server) convOwner(ctx context.Context, convID string) convUser {
	var u convUser
	_ = s.db.QueryRowContext(ctx, `
		SELECT u.id, u.name FROM user_tokens u WHERE u.id = (
		  SELECT user_token_id FROM (
		    SELECT user_token_id, ts FROM conversation_message
		      WHERE conv_id=? AND user_token_id IS NOT NULL
		    UNION ALL
		    SELECT user_token_id, ts FROM prompt_log
		      WHERE conv_id=? AND user_token_id IS NOT NULL
		  ) ORDER BY ts DESC LIMIT 1)`, convID, convID).Scan(&u.ID, &u.Name)
	return u
}

func (s *Server) listConvMessages(w http.ResponseWriter, r *http.Request, convID string) {
	limit, offset := pageParams(r, 50, 500)
	items, total, source, err := s.loadConvMessages(r.Context(), convID, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := page(items, total, limit, offset)
	resp["source"] = source
	resp["conv_id"] = convID
	resp["user"] = s.convOwner(r.Context(), convID)
	writeJSON(w, resp)
}

// exportConv renders a conversation as a downloadable markdown document.
func (s *Server) exportConv(w http.ResponseWriter, r *http.Request, convID string) {
	ctx := r.Context()
	items, total, source, err := s.loadConvMessages(ctx, convID, exportMaxMessages, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if total == 0 {
		writeErr(w, http.StatusNotFound, "conversation not found")
		return
	}
	md := renderConvMarkdown(convID, source, s.convOwner(ctx, convID), items, total)

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="conversation-`+shortConvID(convID)+`.md"`)
	_, _ = io.WriteString(w, md)
}

// shortConvID produces a filename-safe 8-char handle for a conversation.
func shortConvID(convID string) string {
	var b strings.Builder
	for _, r := range convID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() >= 8 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

const mdTime = "2006-01-02 15:04:05 MST"

// renderConvMarkdown formats a conversation for a markdown viewer. Message
// bodies are NOT fenced (they usually contain code fences already); turns are
// separated by a rule and a heading carrying index, role and timestamp.
func renderConvMarkdown(convID, source string, user convUser, items []messageView, total int) string {
	parse := func(s string) time.Time {
		t, _ := time.Parse(time.RFC3339, s)
		return t.UTC()
	}
	var first, last, model string
	if len(items) > 0 {
		first = parse(items[0].TS).Format(mdTime)
		last = parse(items[len(items)-1].TS).Format(mdTime)
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].Model != "" {
				model = items[i].Model
				break
			}
		}
	}
	sourceLabel := "full conversation"
	if source != "full" {
		sourceLabel = "user prompts only"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Conversation %s\n\n", shortConvID(convID))
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| **User** | %s |\n", mdCell(user.Name))
	fmt.Fprintf(&b, "| **Model** | %s |\n", mdCell(model))
	fmt.Fprintf(&b, "| **Messages** | %d |\n", total)
	fmt.Fprintf(&b, "| **Started** | %s |\n", first)
	fmt.Fprintf(&b, "| **Last activity** | %s |\n", last)
	fmt.Fprintf(&b, "| **Exported** | %s |\n", time.Now().UTC().Format(mdTime))
	fmt.Fprintf(&b, "| **Source** | %s |\n", sourceLabel)
	if source != "full" {
		b.WriteString("\n> Assistant replies were not captured for this conversation.\n")
	}
	if total > len(items) {
		fmt.Fprintf(&b, "\n> Export truncated to the first %d messages.\n", len(items))
	}
	for i, m := range items {
		fmt.Fprintf(&b, "\n---\n\n### %d - %s - %s\n\n%s\n",
			i+1, roleLabel(m.Role), parse(m.TS).Format(mdTime), m.Content)
	}
	return b.String()
}

func roleLabel(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	default:
		if role == "" {
			return "Unknown"
		}
		return strings.ToUpper(role[:1]) + role[1:]
	}
}

// mdCell keeps a value from breaking the metadata table.
func mdCell(v string) string {
	if v == "" {
		return "—"
	}
	return strings.ReplaceAll(strings.ReplaceAll(v, "|", "\\|"), "\n", " ")
}
