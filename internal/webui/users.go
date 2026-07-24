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
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
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
	out := make([]userView, 0, len(list))
	for _, ut := range list {
		v := userView{
			ID:        ut.ID,
			Name:      ut.Name,
			Status:    string(ut.Status),
			CreatedAt: ut.CreatedAt.Format(time.RFC3339),
		}
		if ut.LastUsedAt != nil {
			s := ut.LastUsedAt.Format(time.RFC3339)
			v.LastUsedAt = &s
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

// listUserPrompts returns the newest captured prompts for a user token.
func (s *Server) listUserPrompts(w http.ResponseWriter, r *http.Request, id string) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT ts, conv_id, model, prompt FROM prompt_log
		WHERE user_token_id = ? ORDER BY ts DESC LIMIT ?`, id, limit)
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
	writeJSON(w, out)
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
