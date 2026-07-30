package ingest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// verifyClient is a package var so tests can point verification at an httptest
// server instead of the real upstream.
var verifyClient = &http.Client{Timeout: 20 * time.Second}

// VerifyKey checks that an API key is accepted by its provider, by issuing the
// cheapest authenticated call the Anthropic-compatible surface offers:
// GET /v1/models. It costs no tokens and no quota.
//
// This mirrors the discipline Import already applies to OAuth credentials
// (insertVerified refreshes the token before storing it): a credential that
// cannot authenticate must never enter the pool, because once it is in, the
// selector will hand live conversations to it and every one of them fails.
func VerifyKey(ctx context.Context, p provider.ID, apiKey string) error {
	pr := provider.Get(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pr.BaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := verifyClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", pr.Name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s rejected the API key (HTTP %d)", pr.Name, resp.StatusCode)
	default:
		return fmt.Errorf("%s returned HTTP %d: %s", pr.Name, resp.StatusCode,
			strings.TrimSpace(string(body)))
	}
}

// ImportKey verifies a static API key and adds it to the pool.
//
// plan is a free-form tier label (e.g. "pro", "max") shown in listings; it does
// not affect routing. Unlike OAuth subscription types it cannot be derived from
// the credential itself, since an API key carries no metadata.
func ImportKey(ctx context.Context, db *store.DB, p provider.ID, label, plan, apiKey string, weight int) (*creds.Credential, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("API key is empty")
	}
	if !provider.Valid(p) {
		return nil, fmt.Errorf("unknown provider %q", p)
	}
	if provider.Get(p).Refreshable {
		return nil, fmt.Errorf("%s uses OAuth credentials, not API keys — use `creds import` instead",
			provider.Get(p).Name)
	}

	dup, err := creds.HasAccessToken(ctx, db, apiKey)
	if err != nil {
		return nil, fmt.Errorf("duplicate check: %w", err)
	}
	if dup {
		return nil, fmt.Errorf("that API key is already in the pool")
	}

	if err := VerifyKey(ctx, p, apiKey); err != nil {
		return nil, fmt.Errorf("key is not usable: %w", err)
	}

	if label == "" {
		label = string(p)
	}
	return creds.InsertKey(ctx, db, p, label, plan, apiKey, weight)
}
