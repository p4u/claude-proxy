package webui

import (
	"net/http"
	"strconv"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func (s *Server) handleRoutingEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	var before int64
	if value := r.URL.Query().Get("before"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			writeErr(w, http.StatusBadRequest, "before must be a positive event ID")
			return
		}
		before = parsed
	}
	items, err := store.ListRoutingEvents(r.Context(), s.db, before, limit, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read routing history")
		return
	}
	var next *int64
	if len(items) == limit {
		next = &items[len(items)-1].ID
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"items": items, "next_before": next, "retention_days": store.RoutingRetentionDays})
}
