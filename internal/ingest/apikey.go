package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// verifyClient is a package var so tests can point verification at an httptest
// server instead of the real upstream.
var verifyClient = &http.Client{Timeout: 20 * time.Second}

// VerifyKey checks that an API key is accepted by its provider at the given
// endpoint, by issuing the smallest request that actually authenticates.
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

// UpdateKeyEndpoint re-points an existing API-key credential at another
// endpoint, verifying the key against the new one before committing.
//
// Verification is the point: the common reason to change an endpoint is that
// the key was added against the wrong cluster, where it authenticated as
// "Invalid API Key" and was marked revoked. Proving it works at the new
// endpoint both prevents swapping one broken endpoint for another and lets the
// credential be healed back to active in the same step.
func UpdateKeyEndpoint(ctx context.Context, db *store.DB, id, endpoint string) (*creds.Credential, error) {
	c, err := creds.Get(ctx, db, id)
	if err != nil {
		return nil, err
	}
	p := c.Provider
	if p == "" {
		p = provider.Default
	}
	if provider.Get(p).Refreshable {
		return nil, fmt.Errorf("%s credentials are OAuth and have a fixed endpoint", provider.Get(p).Name)
	}

	baseURL, err := provider.ResolveEndpoint(p, endpoint)
	if err != nil {
		return nil, err
	}
	if err := VerifyKey(ctx, p, c.AccessToken, baseURL); err != nil {
		return nil, fmt.Errorf("key is not usable at that endpoint: %w", err)
	}

	stored := baseURL
	if stored == provider.Get(p).BaseURL {
		stored = ""
	}
	if err := creds.SetBaseURL(ctx, db, id, stored); err != nil {
		return nil, err
	}
	// The key demonstrably works now, so a status set by the old endpoint's
	// rejections is stale. Heal it rather than leaving a working credential
	// parked outside the selection pool.
	switch c.Status {
	case creds.StatusRevoked, creds.StatusExpired, creds.StatusLimited:
		if err := creds.SetStatus(ctx, db, id, creds.StatusActive); err != nil {
			return nil, err
		}
	}
	return creds.Get(ctx, db, id)
}

// Probe is what a single interrogation of a custom Anthropic-compatible host
// discovered. Everything here is observed, never assumed: a field left zero
// means the host did not tell us, not that the value is zero.
type Probe struct {
	BaseURL        string        `json:"base_url"`
	OK             bool          `json:"ok"`
	Error          string        `json:"error,omitempty"`
	AuthRequired   bool          `json:"auth_required"`
	HasModelsAPI   bool          `json:"has_models_api"`
	HasCountTokens bool          `json:"has_count_tokens"`
	Models         []creds.Model `json:"models"`
	// ReportedModel is the model name the host answered with, which may differ
	// from the one requested — translation shims commonly ignore the request's
	// model and answer as whatever they front. That name is what the catalogue
	// should carry, since it is what the host actually serves.
	ReportedModel string `json:"reported_model,omitempty"`
}

// ProbeCustomHost interrogates an Anthropic-compatible endpoint and reports
// what it supports, so the operator does not have to know or type any of it.
//
// The order matters. GET /v1/models is tried first because a host that serves
// it gives the whole catalogue including context windows — the only way those
// are ever discoverable. Failing that, one max_tokens:1 completion both proves
// the key works and reveals the model name the host answers with.
func ProbeCustomHost(ctx context.Context, baseURL, apiKey, modelHint string) Probe {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	p := Probe{BaseURL: base}
	if base == "" {
		p.Error = "base URL is required"
		return p
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		p.Error = "base URL must start with http:// or https://"
		return p
	}

	// Catalogue, when the host publishes one.
	if entries, ok := probeModelsAPI(ctx, base, apiKey); ok {
		p.HasModelsAPI = true
		p.Models = entries
	}

	// Liveness + auth + the host's own idea of its model name. Run even when
	// /v1/models answered, because that endpoint is frequently unauthenticated
	// (api.z.ai returns 200 for a garbage token) and so proves nothing about
	// the key.
	model := strings.TrimSpace(modelHint)
	if model == "" && len(p.Models) > 0 {
		model = p.Models[0].ID
	}
	if model == "" {
		// Hosts that ignore the requested model accept anything; hosts that
		// validate it will say so, and the error is surfaced to the operator.
		model = "default"
	}
	reported, err := probeMessages(ctx, base, apiKey, model)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.OK = true
	p.ReportedModel = reported
	p.AuthRequired = probeAuthRequired(ctx, base, model)
	p.HasCountTokens = probeCountTokens(ctx, base, apiKey, model)

	// With no catalogue, the host's own answer is the catalogue. Context window
	// stays unset: nothing short of binary-searching the input size could find
	// it, and a guess would misconfigure the client's context management.
	if len(p.Models) == 0 && reported != "" {
		p.Models = []creds.Model{{ID: reported, DisplayName: reported}}
	}
	return p
}

func probeModelsAPI(ctx context.Context, base, apiKey string) ([]creds.Model, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := verifyClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var env struct {
		Data []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			MaxInputTokens int    `json:"max_input_tokens"`
			MaxTokens      int    `json:"max_tokens"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return nil, false
	}
	out := make([]creds.Model, 0, len(env.Data))
	for _, e := range env.Data {
		if strings.TrimSpace(e.ID) == "" {
			continue
		}
		out = append(out, creds.Model{
			ID: e.ID, DisplayName: e.DisplayName,
			ContextWindow: e.MaxInputTokens, MaxOutput: e.MaxTokens,
		})
	}
	return out, len(out) > 0
}

// probeMessages returns the model name the host reports, or an error the
// operator can act on.
func probeMessages(ctx context.Context, base, apiKey, model string) (string, error) {
	body := fmt.Sprintf(
		`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := verifyClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting %s: %w", base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("host rejected the API key (HTTP %d)", resp.StatusCode)
	default:
		return "", fmt.Errorf("host returned HTTP %d: %s", resp.StatusCode,
			strings.TrimSpace(string(raw)))
	}
	var out struct {
		Model string `json:"model"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("host did not return an Anthropic message: %s",
			strings.TrimSpace(string(raw)))
	}
	return out.Model, nil
}

// probeAuthRequired checks whether the host actually enforces the key. A host
// that answers without one is worth flagging: the operator may have exposed an
// unauthenticated endpoint without realising.
func probeAuthRequired(ctx context.Context, base, model string) bool {
	body := fmt.Sprintf(
		`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return true
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := verifyClient.Do(req)
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

func probeCountTokens(ctx context.Context, base, apiKey, model string) bool {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/v1/messages/count_tokens", strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := verifyClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// ImportCustomHost probes a custom Anthropic-compatible endpoint and adds it.
// models, when non-empty, overrides whatever the probe discovered.
func ImportCustomHost(ctx context.Context, db *store.DB, label, baseURL, apiKey string, models []creds.Model, weight int) (*creds.Credential, Probe, error) {
	apiKey = strings.TrimSpace(apiKey)
	p := ProbeCustomHost(ctx, baseURL, apiKey, "")
	if !p.OK {
		return nil, p, fmt.Errorf("host is not usable: %s", p.Error)
	}
	if len(models) > 0 {
		p.Models = models
	}
	if len(p.Models) == 0 {
		return nil, p, fmt.Errorf("no models discovered; supply at least one model name")
	}
	if apiKey != "" {
		dup, err := creds.HasAccessToken(ctx, db, apiKey)
		if err != nil {
			return nil, p, fmt.Errorf("duplicate check: %w", err)
		}
		if dup {
			return nil, p, fmt.Errorf("that API key is already in the pool")
		}
	}
	if label == "" {
		label = hostLabel(p.BaseURL)
	}
	c, err := creds.InsertCustomKey(ctx, db, label, apiKey, p.BaseURL, p.Models, weight)
	return c, p, err
}

// hostLabel derives a readable default label from a URL's host.
func hostLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "custom"
	}
	return u.Host
}
