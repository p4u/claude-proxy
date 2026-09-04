package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/codexgateway"
)

const codexOAuthStartInterval = 3 * time.Second

func (s *Server) handleCodex(w http.ResponseWriter, r *http.Request, rest string) {
	if s.codex == nil {
		if rest == "/accounts" && r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"configured": false, "accounts": []any{}})
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "OpenAI Codex support is not configured")
		return
	}

	switch {
	case rest == "/accounts" && r.Method == http.MethodGet:
		accounts, err := s.codex.Accounts(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		s.decorateCodexAccounts(r.Context(), accounts)
		writeJSON(w, map[string]any{"configured": true, "accounts": accounts})
	case rest == "/oauth/start" && r.Method == http.MethodPost:
		s.startCodexOAuth(w, r)
	case rest == "/oauth/status" && r.Method == http.MethodGet:
		state := strings.TrimSpace(r.URL.Query().Get("state"))
		if err := validateOAuthState(state); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		status, err := s.codex.OAuthStatus(r.Context(), state)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, status)
	case rest == "/oauth/callback" && r.Method == http.MethodPost:
		s.submitCodexCallback(w, r)
	case rest == "/oauth/cancel" && r.Method == http.MethodPost:
		var body struct {
			State string `json:"state"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		body.State = strings.TrimSpace(body.State)
		if err := validateOAuthState(body.State); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.codex.CancelOAuth(r.Context(), body.State); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case rest == "/accounts/status" && r.Method == http.MethodPost:
		s.setCodexAccountStatus(w, r)
	case rest == "/accounts/weight" && r.Method == http.MethodPost:
		s.setCodexAccountWeight(w, r)
	case rest == "/accounts/delete" && r.Method == http.MethodPost:
		s.deleteCodexAccount(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) setCodexAccountWeight(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Weight *int64 `json:"weight"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateAccountRef(body.Name, ""); err != nil || body.Weight == nil {
		if err == nil {
			err = errors.New("weight is required")
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Store the operator's base weight in our DB; the rebalance loop is the
	// only writer to the sidecar's effective weight, computed from base ×
	// pool heuristics. See internal/codexgateway/rebalance.go.
	if err := codexgateway.SetBaseWeight(r.Context(), s.db, strings.TrimSpace(body.Name), *body.Weight); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Kick a single rebalance so the sidecar reflects the change before the
	// next scheduled tick. Detached from the request context: that one is
	// cancelled as soon as this handler returns, which would abort the push
	// and silently leave the sidecar on its old weights until the loop's
	// next 90s pass.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = codexgateway.RebalanceOnce(ctx, s.db, s.codex, nil, time.Now())
	}()
	writeJSON(w, map[string]any{"ok": true, "weight": *body.Weight})
}

// decorateCodexAccounts attaches per-account base_weight (from our DB) and
// effective_weight (what the rebalance loop will push next) so the UI shows
// the same score the sidecar will see. Errors are silent — the raw account
// data is still useful.
func (s *Server) decorateCodexAccounts(ctx context.Context, accounts []codexgateway.Account) {
	base, err := codexgateway.BaseWeightsMap(ctx, s.db)
	if err != nil {
		return
	}
	eff := codexgateway.EffectiveWeights(accounts, base, time.Now())
	for i := range accounts {
		w := base[accounts[i].Name]
		if w < 1 {
			w = 1
		}
		accounts[i].BaseWeight = w
		accounts[i].EffectiveWeight = eff[accounts[i].Name]
	}
}

func (s *Server) startCodexOAuth(w http.ResponseWriter, r *http.Request) {
	s.codexOAuthMu.Lock()
	if wait := codexOAuthStartInterval - time.Since(s.codexOAuthLast); wait > 0 {
		s.codexOAuthMu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "wait a few seconds before starting another OpenAI login")
		return
	}
	s.codexOAuthLast = time.Now()
	s.codexOAuthMu.Unlock()

	started, err := s.codex.StartOAuth(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"url": started.URL, "state": started.State,
		"callback_uri": "http://localhost:1455/auth/callback",
		"callback_uris": []string{
			"http://localhost:1455/auth/callback",
			"http://127.0.0.1:8317/codex/callback",
		},
		"callback_mode": "localhost_or_manual",
	})
}

func (s *Server) submitCodexCallback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State       string `json:"state"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.State = strings.TrimSpace(body.State)
	body.RedirectURL = strings.TrimSpace(body.RedirectURL)
	if err := validateOAuthState(body.State); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCodexRedirect(body.RedirectURL, body.State); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.codex.SubmitCallback(r.Context(), body.State, body.RedirectURL); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) setCodexAccountStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		Disabled  *bool  `json:"disabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateAccountRef(body.Name, body.AuthIndex); err != nil || body.Disabled == nil {
		if err == nil {
			err = errors.New("disabled is required")
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.codex.SetDisabled(r.Context(), strings.TrimSpace(body.Name), strings.TrimSpace(body.AuthIndex), *body.Disabled); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) deleteCodexAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateAccountRef(body.Name, ""); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.codex.DeleteAccount(r.Context(), strings.TrimSpace(body.Name)); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func validateOAuthState(state string) error {
	if state == "" || len(state) > 128 || strings.Contains(state, "..") {
		return errors.New("invalid OAuth state")
	}
	for _, ch := range state {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return errors.New("invalid OAuth state")
	}
	return nil
}

func validateCodexRedirect(raw, wantState string) error {
	if raw == "" || len(raw) > 8192 {
		return errors.New("paste the complete OpenAI callback URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return errors.New("callback must be a CLIProxyAPI loopback callback URL")
	}
	host := strings.ToLower(u.Hostname())
	registeredCallback := host == "localhost" && u.Port() == "1455" && u.Path == "/auth/callback"
	forwardedCallback := host == "127.0.0.1" && u.Port() == "8317" && u.Path == "/codex/callback"
	if !registeredCallback && !forwardedCallback {
		return errors.New("callback must be the localhost:1455 or 127.0.0.1:8317 Codex callback URL")
	}
	q := u.Query()
	if q.Get("state") != wantState {
		return errors.New("callback state does not match this login")
	}
	if strings.TrimSpace(q.Get("code")) == "" && strings.TrimSpace(q.Get("error")) == "" && strings.TrimSpace(q.Get("error_description")) == "" {
		return errors.New("callback URL has no authorization result")
	}
	return nil
}

func validateAccountRef(name, authIndex string) error {
	name, authIndex = strings.TrimSpace(name), strings.TrimSpace(authIndex)
	if name == "" || len(name) > 512 || len(authIndex) > 256 || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(authIndex, "\r\n") {
		return errors.New("invalid Codex account reference")
	}
	return nil
}
