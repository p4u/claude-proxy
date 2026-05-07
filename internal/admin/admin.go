package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

type Handler struct {
	db *store.DB
}

func New(db *store.DB) *Handler { return &Handler{db: db} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/admin/credentials" && r.Method == "GET":
		h.listCreds(w, r)
	case r.URL.Path == "/admin/conversations" && r.Method == "GET":
		h.listConvs(w, r)
	case r.URL.Path == "/admin/stats" && r.Method == "GET":
		h.stats(w, r)
	case strings.HasPrefix(r.URL.Path, "/admin/credentials/") && strings.HasSuffix(r.URL.Path, "/disable") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/credentials/"), "/disable")
		_ = creds.SetStatus(r.Context(), h.db, id, creds.StatusDisabled)
		writeJSON(w, map[string]any{"ok": true, "id": id})
	case strings.HasPrefix(r.URL.Path, "/admin/credentials/") && r.Method == "DELETE":
		id := strings.TrimPrefix(r.URL.Path, "/admin/credentials/")
		if err := creds.Delete(r.Context(), h.db, id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	default:
		http.NotFound(w, r)
	}
}

type credView struct {
	ID                  string `json:"id"`
	Label               string `json:"label,omitempty"`
	SubscriptionType    string `json:"subscription_type,omitempty"`
	Status              string `json:"status"`
	ExpiresAt           string `json:"expires_at"`
	RetryAfter          string `json:"retry_after,omitempty"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	Last429At           string `json:"last_429_at,omitempty"`
	LastRequestAt       string `json:"last_request_at,omitempty"`
	RequestCount        int64  `json:"request_count"`
	SuccessCount        int64  `json:"success_count"`
	ErrorCount          int64  `json:"error_count"`
	Weight              int    `json:"weight"`
	ActiveConversations int    `json:"active_conversations"`
}

func (h *Handler) listCreds(w http.ResponseWriter, r *http.Request) {
	list, err := creds.List(r.Context(), h.db)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	out := make([]credView, 0, len(list))
	for _, c := range list {
		v := credView{
			ID: c.ID, Label: c.Label,
			SubscriptionType: c.SubscriptionType,
			Status:           string(c.Status),
			ExpiresAt:        c.ExpiresAt.Format(time.RFC3339),
			RequestCount:     c.RequestCount,
			SuccessCount:     c.SuccessCount,
			ErrorCount:       c.ErrorCount,
			Weight:           c.Weight,
		}
		if c.LastRequestAt != nil {
			v.LastRequestAt = c.LastRequestAt.Format(time.RFC3339)
		}
		if c.RetryAfter != nil {
			v.RetryAfter = c.RetryAfter.Format(time.RFC3339)
		}
		if c.LastSuccessAt != nil {
			v.LastSuccessAt = c.LastSuccessAt.Format(time.RFC3339)
		}
		if c.Last429At != nil {
			v.Last429At = c.Last429At.Format(time.RFC3339)
		}
		var n int
		_ = h.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM conversations WHERE credential_id=? AND status='active'`, c.ID).Scan(&n)
		v.ActiveConversations = n
		out = append(out, v)
	}
	writeJSON(w, out)
}

type convView struct {
	ID           string `json:"id"`
	CredentialID string `json:"credential_id"`
	CreatedAt    string `json:"created_at"`
	LastSeenAt   string `json:"last_seen_at"`
	RequestCount int    `json:"request_count"`
	Status       string `json:"status"`
}

func (h *Handler) listConvs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, credential_id, created_at, last_seen_at, request_count, status
		FROM conversations ORDER BY last_seen_at DESC LIMIT 200`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	out := []convView{}
	for rows.Next() {
		var v convView
		var created, seen int64
		if err := rows.Scan(&v.ID, &v.CredentialID, &created, &seen, &v.RequestCount, &v.Status); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		v.CreatedAt = time.Unix(created, 0).Format(time.RFC3339)
		v.LastSeenAt = time.Unix(seen, 0).Format(time.RFC3339)
		out = append(out, v)
	}
	writeJSON(w, out)
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]any{}
	var n int
	_ = h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM conversations`).Scan(&n)
	stats["conversations_total"] = n
	_ = h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM credentials WHERE status='active'`).Scan(&n)
	stats["credentials_active"] = n
	_ = h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM credentials`).Scan(&n)
	stats["credentials_total"] = n

	dist := map[string]int{}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT credential_id, COUNT(*) FROM conversations GROUP BY credential_id`)
	if err == nil {
		for rows.Next() {
			var id string
			var c int
			_ = rows.Scan(&id, &c)
			dist[id] = c
		}
		rows.Close()
	}
	stats["rr_distribution"] = dist
	writeJSON(w, stats)
}

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
