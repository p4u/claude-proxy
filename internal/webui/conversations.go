package webui

import (
	"net/http"
	"strconv"
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
