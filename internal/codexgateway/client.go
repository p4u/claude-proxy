// Package codexgateway connects claude-proxy to a private CLIProxyAPI sidecar.
// The sidecar owns OpenAI OAuth credentials; this package deliberately exposes
// only the small, sanitized management surface needed by the web UI.
package codexgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

const GatewayCredentialID = "gateway_codex"

const maxResponseBytes = 1 << 20

type Config struct {
	BaseURL       string
	APIKey        string
	ManagementKey string
}

func (c Config) Enabled() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.ManagementKey) != ""
}

type Client struct {
	baseURL       string
	apiKey        string
	managementKey string
	http          *http.Client
}

func New(cfg Config) (*Client, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid Codex gateway base URL")
	}
	return &Client{
		baseURL:       u.String(),
		apiKey:        strings.TrimSpace(cfg.APIKey),
		managementKey: strings.TrimSpace(cfg.ManagementKey),
		http:          &http.Client{Timeout: 20 * time.Second},
	}, nil
}

func (c *Client) Configured() bool { return c != nil }
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}
func (c *Client) APIKey() string {
	if c == nil {
		return ""
	}
	return c.apiKey
}

type Account struct {
	Name          string `json:"name"`
	AuthIndex     string `json:"auth_index,omitempty"`
	Email         string `json:"email,omitempty"`
	Label         string `json:"label,omitempty"`
	Status        string `json:"status,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
	AccountType   string `json:"account_type,omitempty"`
	Account       string `json:"account,omitempty"`
	Disabled      bool   `json:"disabled"`
	Unavailable   bool   `json:"unavailable"`
	Success       int64  `json:"success"`
	Failed        int64  `json:"failed"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	Weight        *int64 `json:"weight,omitempty"`
}

type rawAccount struct {
	Account
	Type     string `json:"type"`
	Provider string `json:"provider"`
}

func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var response struct {
		Files []rawAccount `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/management/auth-files", nil, &response, true); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(response.Files))
	for _, file := range response.Files {
		if !strings.EqualFold(file.Type, "codex") && !strings.EqualFold(file.Provider, "codex") {
			continue
		}
		accounts = append(accounts, file.Account)
	}
	return accounts, nil
}

type OAuthStart struct {
	Status string `json:"status"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

func (c *Client) StartOAuth(ctx context.Context) (OAuthStart, error) {
	var out OAuthStart
	err := c.do(ctx, http.MethodGet, "/v0/management/codex-auth-url?is_webui=true", nil, &out, true)
	if err == nil && (out.URL == "" || out.State == "") {
		err = errors.New("codex gateway returned an incomplete OAuth session")
	}
	return out, err
}

type OAuthStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (c *Client) OAuthStatus(ctx context.Context, state string) (OAuthStatus, error) {
	var out OAuthStatus
	path := "/v0/management/get-auth-status?state=" + url.QueryEscape(state)
	err := c.do(ctx, http.MethodGet, path, nil, &out, true)
	return out, err
}

func (c *Client) CancelOAuth(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodDelete, "/v0/management/oauth-session?state="+url.QueryEscape(state), nil, nil, true)
}

func (c *Client) SubmitCallback(ctx context.Context, state, redirectURL string) error {
	body := map[string]string{"provider": "codex", "state": state, "redirect_url": redirectURL}
	var out OAuthStatus
	return c.do(ctx, http.MethodPost, "/v0/management/oauth-callback", body, &out, true)
}

func (c *Client) SetDisabled(ctx context.Context, name, authIndex string, disabled bool) error {
	body := map[string]any{"name": name, "disabled": disabled}
	if authIndex != "" {
		body["auth_index"] = authIndex
	}
	return c.do(ctx, http.MethodPatch, "/v0/management/auth-files/status", body, nil, true)
}

func (c *Client) SetWeight(ctx context.Context, name string, weight int64) error {
	if weight < 1 || weight > 1_000_000 {
		return errors.New("weight must be between 1 and 1000000")
	}
	body := map[string]any{"name": name, "weight": weight}
	return c.do(ctx, http.MethodPatch, "/v0/management/auth-files/fields", body, nil, true)
}

func (c *Client) DeleteAccount(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(name), nil, nil, true)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, management bool) error {
	if c == nil {
		return errors.New("OpenAI Codex support is not configured")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Codex gateway request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build Codex gateway request: %w", err)
	}
	key := c.apiKey
	if management {
		key = c.managementKey
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("codex gateway unavailable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Codex gateway response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return errors.New("codex gateway response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		message := strings.TrimSpace(apiErr.Error)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("codex gateway: %s", message)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode Codex gateway response: %w", err)
		}
	}
	return nil
}

// ReconcileCredential creates or refreshes the one local credential that
// represents the private sidecar. OAuth tokens never enter this database.
func ReconcileCredential(ctx context.Context, db *store.DB, c *Client) error {
	if c == nil {
		_, err := db.ExecContext(ctx, `UPDATE credentials SET status='disabled' WHERE id=?`, GatewayCredentialID)
		return err
	}
	now := time.Now()
	_, err := db.ExecContext(ctx, `
		INSERT INTO credentials
		  (id, label, subscription_type, provider, base_url, models,
		   access_token, refresh_token, expires_at, status, weight, created_at)
		VALUES (?, 'OpenAI Codex gateway', 'oauth-sidecar', ?, ?, '', ?, '', ?, 'active', 1, ?)
		ON CONFLICT(id) DO UPDATE SET
		  label=excluded.label, subscription_type=excluded.subscription_type,
		  provider=excluded.provider, base_url=excluded.base_url,
		  access_token=excluded.access_token, refresh_token='',
		  expires_at=excluded.expires_at, status='active', weight=1`,
		GatewayCredentialID, string(provider.Codex), c.BaseURL(), c.APIKey(),
		now.AddDate(100, 0, 0).Unix(), now.Unix())
	return err
}
