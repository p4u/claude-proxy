package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/ingest"
)

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

// handleCredentials routes /credentials and /credentials/{id}/{action}. `rest`
// is the path after "/credentials" (e.g. "", "/cred_x/disable", "/cred_x").
func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.listCreds(w, r)
	case rest == "" && r.Method == http.MethodPost:
		s.importCred(w, r)
	default:
		// /{id} or /{id}/{action}
		trimmed := strings.TrimPrefix(rest, "/")
		id, action, _ := strings.Cut(trimmed, "/")
		if id == "" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		s.credAction(w, r, id, action)
	}
}

func (s *Server) credAction(w http.ResponseWriter, r *http.Request, id, action string) {
	ctx := r.Context()
	switch {
	case action == "disable" && r.Method == http.MethodPost:
		if err := creds.SetStatus(ctx, s.db, id, creds.StatusDisabled); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	case action == "enable" && r.Method == http.MethodPost:
		if err := creds.SetStatus(ctx, s.db, id, creds.StatusActive); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	case action == "refresh" && r.Method == http.MethodPost:
		c, err := s.refresher.RefreshNow(ctx, id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id, "expires_at": c.ExpiresAt.UTC().Format(time.RFC3339)})
	case action == "weight" && r.Method == http.MethodPost:
		var body struct {
			Weight int `json:"weight"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := creds.SetWeight(ctx, s.db, id, body.Weight); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id, "weight": body.Weight})
	case action == "tokens" && r.Method == http.MethodPut:
		var body struct {
			CredentialsJSON string `json:"credentials_json"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		c, err := ingest.UpdateFromJSON(ctx, s.db, id, []byte(body.CredentialsJSON))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": c.ID, "status": string(c.Status)})
	case action == "" && r.Method == http.MethodDelete:
		if err := creds.Delete(ctx, s.db, id); err != nil {
			if errors.Is(err, creds.ErrNotFound) {
				writeErr(w, http.StatusNotFound, err.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) listCreds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := creds.List(ctx, s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
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
		_ = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM conversations WHERE credential_id=? AND status='active'`, c.ID).Scan(&n)
		v.ActiveConversations = n
		out = append(out, v)
	}
	writeJSON(w, out)
}

func (s *Server) importCred(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CredentialsJSON string `json:"credentials_json"`
		Label           string `json:"label"`
		Weight          int    `json:"weight"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := ingest.ImportFromJSON(r.Context(), s.db, []byte(body.CredentialsJSON), body.Label, body.Weight)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "id": c.ID, "label": c.Label,
		"subscription_type": c.SubscriptionType, "weight": c.Weight,
	})
}

// decodeJSON decodes a bounded request body into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}
