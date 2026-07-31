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
func VerifyKey(ctx context.Context, p provider.ID, apiKey, baseURL string) error {
	pr := provider.Get(p)
	// A one-token /v1/messages call, NOT GET /v1/models.
	//
	// /v1/models looks cheaper but does not authenticate: api.z.ai returns 200
	// for a garbage bearer token, so verifying against it would wave through
	// any string and let a dead credential into the pool — precisely the
	// failure this check exists to prevent. MiMo has no /v1/models at all.
	// A max_tokens:1 completion is the smallest request both providers accept
	// and both reject with 401 when the key is wrong.
	body := fmt.Sprintf(
		`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
		pr.VerifyModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		provider.ResolveBaseURL(p, baseURL)+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := verifyClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", pr.Name, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// Regional providers answer a wrong-cluster key with a bare "Invalid
		// API Key", so name the endpoint that actually rejected it.
		return fmt.Errorf("%s rejected the API key at %s (HTTP %d) — check the key and that the endpoint matches the one it was issued for",
			pr.Name, provider.ResolveBaseURL(p, baseURL), resp.StatusCode)
	default:
		return fmt.Errorf("%s returned HTTP %d: %s", pr.Name, resp.StatusCode,
			strings.TrimSpace(string(respBody)))
	}
}

// ImportKey verifies a static API key and adds it to the pool.
//
// plan is a free-form tier label (e.g. "pro", "max") shown in listings; it does
// not affect routing. Unlike OAuth subscription types it cannot be derived from
// the credential itself, since an API key carries no metadata.
func ImportKey(ctx context.Context, db *store.DB, p provider.ID, label, plan, apiKey, endpoint string, weight int) (*creds.Credential, error) {
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

	baseURL, err := provider.ResolveEndpoint(p, endpoint)
	if err != nil {
		return nil, err
	}
	if err := VerifyKey(ctx, p, apiKey, baseURL); err != nil {
		return nil, fmt.Errorf("key is not usable: %w", err)
	}

	if label == "" {
		label = string(p)
	}
	// Store the resolved URL only when it differs from the provider default, so
	// a default-endpoint credential keeps following the registry if it moves.
	stored := baseURL
	if stored == provider.Get(p).BaseURL {
		stored = ""
	}
	return creds.InsertKey(ctx, db, p, label, plan, apiKey, stored, weight)
}
