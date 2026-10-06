package creds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

const ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// TokenURL is a var (not const) so tests can rewrite it.
var TokenURL = "https://platform.claude.com/v1/oauth/token"

type Refresher struct {
	db     *store.DB
	client *http.Client
	mus    sync.Map // id -> *sync.Mutex
}

func NewRefresher(db *store.DB) *Refresher {
	return &Refresher{
		db:     db,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetTokenClient swaps the HTTP client used for refresh requests (test hook).
func SetTokenClient(r *Refresher, c *http.Client) { r.client = c }

// SetTokenURL overrides the global token endpoint (test hook).
func SetTokenURL(u string) { TokenURL = u }

func (r *Refresher) lockFor(id string) *sync.Mutex {
	v, _ := r.mus.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type refreshReq struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

type refreshResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`
	Error        string `json:"error,omitempty"`
}

// RefreshTokens performs a stateless token refresh (no DB record required).
// Used during import to verify a credential is alive before inserting it.
// Returns the new access token, refresh token, and expiry on success.
func RefreshTokens(ctx context.Context, refreshToken string) (accessToken, newRefreshToken string, expiresAt time.Time, err error) {
	body, _ := json.Marshal(refreshReq{
		GrantType:    "refresh_token",
		ClientID:     ClientID,
		RefreshToken: refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, bytes.NewReader(body))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 400 || resp.StatusCode == 401 {
		return "", "", time.Time{}, fmt.Errorf("credential rejected by Anthropic (%d): %s", resp.StatusCode, string(raw))
	}
	if resp.StatusCode != 200 {
		return "", "", time.Time{}, fmt.Errorf("refresh upstream %d: %s", resp.StatusCode, string(raw))
	}

	var rr refreshResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return "", "", time.Time{}, fmt.Errorf("refresh decode: %w", err)
	}
	if rr.AccessToken == "" || rr.RefreshToken == "" {
		return "", "", time.Time{}, fmt.Errorf("refresh missing tokens in response")
	}
	exp := time.Now().Add(time.Duration(rr.ExpiresIn)*time.Second - 5*time.Minute)
	return rr.AccessToken, rr.RefreshToken, exp, nil
}

// Refresh refreshes if the credential is near expiry. Safe to call
// concurrently — serialized per-id. Used by the proactive loop and by manual
// `creds refresh`. If the credential is healthy and far from expiry, it is
// returned unchanged.
func (r *Refresher) Refresh(ctx context.Context, id string) (*Credential, error) {
	return r.refresh(ctx, id, false)
}

// RefreshNow forces a refresh, ignoring the freshness check. Used by the
// reactive 401 path where we know the upstream rejected the current access
// token regardless of what its stored expiry says.
func (r *Refresher) RefreshNow(ctx context.Context, id string) (*Credential, error) {
	return r.refresh(ctx, id, true)
}

func (r *Refresher) refresh(ctx context.Context, id string, force bool) (*Credential, error) {
	mu := r.lockFor(id)
	mu.Lock()
	defer mu.Unlock()

	c, err := Get(ctx, r.db, id)
	if err != nil {
		return nil, err
	}
	if !force && time.Until(c.ExpiresAt) > 5*time.Minute && c.Status == StatusActive {
		return c, nil
	}

	body, _ := json.Marshal(refreshReq{
		GrantType:    "refresh_token",
		ClientID:     ClientID,
		RefreshToken: c.RefreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 400 || resp.StatusCode == 401 {
		// invalid_grant or revoked — but only for the lineage this request
		// used. If the tokens were replaced meanwhile (an Update tokens or a
		// browser reconnect landed while this request was in flight), the
		// rejection describes the old grant, and the credential is healthy.
		revoked, err := revokeIfLineage(ctx, r.db, id, c.RefreshToken)
		if err != nil {
			return nil, err
		}
		if !revoked {
			return Get(ctx, r.db, id)
		}
		return nil, fmt.Errorf("refresh rejected (%d): %s", resp.StatusCode, string(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("refresh upstream %d: %s", resp.StatusCode, string(raw))
	}

	var rr refreshResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("refresh decode: %w", err)
	}
	if rr.AccessToken == "" || rr.RefreshToken == "" {
		return nil, fmt.Errorf("refresh missing tokens: %s", string(raw))
	}
	exp := time.Now().Add(time.Duration(rr.ExpiresIn)*time.Second - 5*time.Minute)
	// Conditional for the same reason: a stale refresh must not overwrite a
	// newer login's tokens with the previous lineage.
	if _, err := replaceTokensIfLineage(ctx, r.db, id, c.RefreshToken, rr.AccessToken, rr.RefreshToken, exp); err != nil {
		return nil, err
	}
	return Get(ctx, r.db, id)
}

// replaceTokensIfLineage writes a refresh result only while the credential
// still holds the refresh token the request was made with.
func replaceTokensIfLineage(ctx context.Context, db *store.DB, id, oldRefresh, access, refresh string, expiresAt time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE credentials SET access_token=?, refresh_token=?, expires_at=?, status='active'
		WHERE id=? AND refresh_token=?`, access, refresh, expiresAt.Unix(), id, oldRefresh)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// revokeIfLineage marks the credential revoked only while it still holds the
// refresh token that was rejected.
func revokeIfLineage(ctx context.Context, db *store.DB, id, oldRefresh string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE credentials SET status=? WHERE id=? AND refresh_token=?`, string(StatusRevoked), id, oldRefresh)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Loop runs proactive refresh in the background until ctx is cancelled.
func (r *Refresher) Loop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Refresher) tick(ctx context.Context) {
	creds, err := List(ctx, r.db)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(5 * time.Minute)
	for _, c := range creds {
		// Static API keys have no refresh token and no real expiry. Attempting
		// an OAuth refresh for one would fail against a token endpoint that
		// never issued it, and would flip a perfectly good key to "revoked".
		if !provider.Get(c.Provider).Refreshable {
			continue
		}
		if c.Status == StatusRevoked || c.Status == StatusDisabled || c.Status == StatusExpired {
			continue
		}
		if c.ExpiresAt.Before(cutoff) {
			_, _ = r.Refresh(ctx, c.ID)
		}
	}
}
