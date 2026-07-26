package webui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/usertoken"
)

type userView struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	LastUsedAt  *string `json:"last_used_at"`
	FullCapture bool    `json:"full_capture"`

	// Usage limit (v4). 0/0 => unlimited, in which case usage_output_tokens
	// and usage_pct are 0 and blocked_until is null.
	LimitOutputTokens  int64   `json:"limit_output_tokens"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	UsageOutputTokens  int64   `json:"usage_output_tokens"`
	UsagePct           float64 `json:"usage_pct"`
	Blocked            bool    `json:"blocked"`
	// BlockedUntil is non-null only while blocked: a rolling window has no
	// reset instant, so no fake reset time is reported when under the cap.
	BlockedUntil *string `json:"blocked_until"`
}

// handleUsers routes /users and /users/{id}/{action}. `rest` is the path after
// "/users" (e.g. "", "/utok_x/disable", "/utok_x").
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.listUsers(w, r)
	case rest == "" && r.Method == http.MethodPost:
		s.createUser(w, r)
	default:
		trimmed := strings.TrimPrefix(rest, "/")
		id, action, _ := strings.Cut(trimmed, "/")
		if id == "" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		s.userAction(w, r, id, action)
	}
}

func (s *Server) userAction(w http.ResponseWriter, r *http.Request, id, action string) {
	ctx := r.Context()
	var err error
	switch {
	case action == "disable" && r.Method == http.MethodPost:
		err = usertoken.SetStatus(ctx, s.db, id, usertoken.StatusDisabled)
	case action == "enable" && r.Method == http.MethodPost:
		err = usertoken.SetStatus(ctx, s.db, id, usertoken.StatusActive)
	case action == "rotate" && r.Method == http.MethodPost:
		token, rerr := usertoken.Refresh(ctx, s.db, id)
		if rerr != nil {
			s.userErr(w, rerr)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id, "token": token})
		return
	case action == "prompts" && r.Method == http.MethodGet:
		s.listUserPrompts(w, r, id)
		return
	case action == "conversations" && r.Method == http.MethodGet:
		s.listUserConversations(w, r, id)
		return
	case action == "capture" && r.Method == http.MethodPost:
		s.setUserCapture(w, r, id)
		return
	case action == "limit" && r.Method == http.MethodPost:
		s.setUserLimit(w, r, id)
		return
	case action == "usage" && r.Method == http.MethodGet:
		s.userWindowUsage(w, r, id)
		return
	case action == "" && r.Method == http.MethodDelete:
		err = usertoken.Delete(ctx, s.db, id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.userErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (s *Server) userErr(w http.ResponseWriter, err error) {
	if errors.Is(err, usertoken.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := usertoken.List(r.Context(), s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	out := make([]userView, 0, len(list))
	for _, ut := range list {
		v := userView{
			ID:                 ut.ID,
			Name:               ut.Name,
			Status:             string(ut.Status),
			CreatedAt:          ut.CreatedAt.Format(time.RFC3339),
			FullCapture:        ut.FullCapture,
			LimitOutputTokens:  ut.LimitOutputTokens,
			LimitWindowSeconds: ut.LimitWindowSeconds,
		}
		if ut.LastUsedAt != nil {
			s := ut.LastUsedAt.Format(time.RFC3339)
			v.LastUsedAt = &s
		}
		// Skips the query entirely for unlimited users.
		st, err := usertoken.LimitStatus(r.Context(), s.db, ut.ID,
			ut.LimitOutputTokens, ut.LimitWindowSeconds, now)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		v.UsageOutputTokens = st.UsageOutputTokens
		v.UsagePct = st.UsagePct()
		v.Blocked = st.Blocked
		if st.Blocked {
			bu := st.BlockedUntil.UTC().Format(time.RFC3339)
			v.BlockedUntil = &bu
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

type promptView struct {
	TS     string `json:"ts"`
	ConvID string `json:"conv_id"`
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// setUserCapture toggles whole-conversation capture for a user.
func (s *Server) setUserCapture(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Full bool `json:"full"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := usertoken.SetFullCapture(r.Context(), s.db, id, body.Full); err != nil {
		s.userErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "full_capture": body.Full})
}

// setUserLimit configures or clears a user's rolling-window usage cap.
// Both fields zero clears the limit; one zero and one non-zero is rejected,
// since a limit is meaningless without both halves.
func (s *Server) setUserLimit(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		OutputTokens  int64 `json:"output_tokens"`
		WindowSeconds int64 `json:"window_seconds"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.OutputTokens < 0 || body.WindowSeconds < 0 {
		writeErr(w, http.StatusBadRequest, "output_tokens and window_seconds must not be negative")
		return
	}
	if (body.OutputTokens == 0) != (body.WindowSeconds == 0) {
		writeErr(w, http.StatusBadRequest,
			"both output_tokens and window_seconds are required to set a limit; send both as 0 to clear it")
		return
	}
	if err := usertoken.SetLimit(r.Context(), s.db, id, body.OutputTokens, body.WindowSeconds); err != nil {
		s.userErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok":                   true,
		"limit_output_tokens":  body.OutputTokens,
		"limit_window_seconds": body.WindowSeconds,
	})
}

// userWindowUsage reports a user's output tokens over an arbitrary rolling
// window, regardless of whether a limit is configured. The Edit limit modal
// calls it so an operator can see what this user actually produces over the
// window they are about to cap, instead of guessing a number.
func (s *Server) userWindowUsage(w http.ResponseWriter, r *http.Request, id string) {
	secs, err := strconv.ParseInt(r.URL.Query().Get("window_seconds"), 10, 64)
	if err != nil || secs <= 0 {
		writeErr(w, http.StatusBadRequest, "window_seconds must be a positive integer")
		return
	}
	if _, err := usertoken.Get(r.Context(), s.db, id); err != nil {
		s.userErr(w, err)
		return
	}
	total, err := usertoken.OutputTokensInWindow(r.Context(), s.db, id,
		time.Duration(secs)*time.Second, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"id":             id,
		"window_seconds": secs,
		"output_tokens":  total,
	})
}

// pageParams reads limit/offset from the query string, clamped to sane bounds.
func pageParams(r *http.Request, defLimit, maxLimit int) (int, int) {
	limit, offset := defLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			offset = parsed
		}
	}
	return limit, offset
}

// page wraps a paginated payload.
func page(items any, total, limit, offset int) map[string]any {
	return map[string]any{
		"items":    items,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": offset+limit < total,
	}
}

// listUserPrompts returns captured prompts for a user token, newest first,
// paginated over the whole history.
func (s *Server) listUserPrompts(w http.ResponseWriter, r *http.Request, id string) {
	limit, offset := pageParams(r, 50, 500)
	ctx := r.Context()

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM prompt_log WHERE user_token_id = ?`, id).Scan(&total); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, conv_id, model, prompt FROM prompt_log
		WHERE user_token_id = ? ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`, id, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []promptView{}
	for rows.Next() {
		var ts int64
		var pv promptView
		if err := rows.Scan(&ts, &pv.ConvID, &pv.Model, &pv.Prompt); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		pv.TS = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		out = append(out, pv)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, page(out, total, limit, offset))
}

type userConvView struct {
	ConvID   string `json:"conv_id"`
	FirstTS  string `json:"first_ts"`
	LastTS   string `json:"last_ts"`
	Messages int    `json:"messages"`
	Prompts  int    `json:"prompts"`
	Model    string `json:"model"`
	Source   string `json:"source"`
}

// userConvAgg unions the two capture tables so a conversation shows up whether
// it was recorded in full or as prompts only. The substr(MAX(...)) trick picks
// the model of the newest row in each group (SQLite has no "last value" agg).
const userConvAgg = `
	WITH agg AS (
		SELECT conv_id, ts, model, 1 AS m, 0 AS p
		  FROM conversation_message WHERE user_token_id = ?
		UNION ALL
		SELECT conv_id, ts, model, 0 AS m, 1 AS p
		  FROM prompt_log WHERE user_token_id = ?
	)`

// listUserConversations returns the conversations a user has captured rows for.
func (s *Server) listUserConversations(w http.ResponseWriter, r *http.Request, id string) {
	limit, offset := pageParams(r, 50, 500)
	ctx := r.Context()

	var total int
	if err := s.db.QueryRowContext(ctx,
		userConvAgg+` SELECT COUNT(*) FROM (SELECT conv_id FROM agg GROUP BY conv_id)`,
		id, id).Scan(&total); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rows, err := s.db.QueryContext(ctx, userConvAgg+`
		SELECT conv_id, MIN(ts), MAX(ts), SUM(m), SUM(p),
		       COALESCE(substr(MAX(printf('%020d', ts) || model), 21), '')
		FROM agg GROUP BY conv_id ORDER BY MAX(ts) DESC LIMIT ? OFFSET ?`,
		id, id, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []userConvView{}
	for rows.Next() {
		var v userConvView
		var first, last int64
		if err := rows.Scan(&v.ConvID, &first, &last, &v.Messages, &v.Prompts, &v.Model); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		v.FirstTS = time.Unix(first, 0).UTC().Format(time.RFC3339)
		v.LastTS = time.Unix(last, 0).UTC().Format(time.RFC3339)
		v.Source = "prompts"
		if v.Messages > 0 {
			v.Source = "full"
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, page(out, total, limit, offset))
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	ut, err := usertoken.Create(r.Context(), s.db, body.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": ut.ID, "name": ut.Name, "token": ut.Token})
}
