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
		// invalid_grant or revoked
		_ = SetStatus(ctx, r.db, id, StatusRevoked)
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
	if err := UpdateTokens(ctx, r.db, id, rr.AccessToken, rr.RefreshToken, exp); err != nil {
		return nil, err
	}
	return Get(ctx, r.db, id)
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
		if c.Status == StatusRevoked || c.Status == StatusDisabled || c.Status == StatusExpired {
			continue
		}
		if c.ExpiresAt.Before(cutoff) {
			_, _ = r.Refresh(ctx, c.ID)
		}
	}
}
