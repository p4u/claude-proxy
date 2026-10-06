package webui

import (
	"net/http"
	"strings"

	"github.com/p4u/claude-proxy/internal/codexgateway"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/ingest"
)

// handleClaudeLogin serves /api/credentials/oauth/*: an Anthropic subscription
// sign-in run from the browser instead of pasting a .credentials.json. The
// flow is Claude Code's manual-code login — the browser approves access on
// claude.com, Anthropic displays an authentication code, and the operator
// pastes it here. The PKCE verifier stays in this process (claudeoauth.Flow);
// the browser only ever holds the opaque session ID and the pasted code.
func (s *Server) handleClaudeLogin(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "/start" && r.Method == http.MethodPost:
		id, url, err := s.claudeLogin.Start()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"session": id, "url": url})
	case rest == "/cancel" && r.Method == http.MethodPost:
		var body struct {
			Session string `json:"session"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.claudeLogin.Cancel(strings.TrimSpace(body.Session))
		writeJSON(w, map[string]any{"ok": true})
	case rest == "/exchange" && r.Method == http.MethodPost:
		s.exchangeClaudeLogin(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// exchangeClaudeLogin finishes a sign-in. Without credential_id it adds a new
// subscription; with one it replaces that credential's tokens, keeping its
// identity, weight and history (the OAuth counterpart of PUT /tokens).
func (s *Server) exchangeClaudeLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session      string `json:"session"`
		Code         string `json:"code"`
		Label        string `json:"label"`
		Weight       int    `json:"weight"`
		CredentialID string `json:"credential_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	target := strings.TrimSpace(body.CredentialID)
	if codexgateway.IsGatewayCredential(target) {
		writeErr(w, http.StatusConflict, "sidecar gateways are managed through their accounts panels")
		return
	}
	ctx := r.Context()
	if target != "" {
		// Fail before consuming the single-use code on a typo'd ID.
		if _, err := creds.Get(ctx, s.db, target); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
	}
	t, err := s.claudeLogin.Exchange(ctx, strings.TrimSpace(body.Session), body.Code)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var c *creds.Credential
	if target != "" {
		c, err = ingest.UpdateFromOAuth(ctx, s.db, target, t)
	} else {
		c, err = ingest.ImportOAuth(ctx, s.db, t, strings.TrimSpace(body.Label), body.Weight)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "id": c.ID, "label": c.Label, "status": string(c.Status),
		"subscription_type": c.SubscriptionType, "weight": c.Weight,
	})
}
