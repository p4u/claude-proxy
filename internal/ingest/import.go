package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

type credFile struct {
	ClaudeAiOauth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"` // milliseconds
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// Import reads a Claude Code .credentials.json and inserts it into the pool.
// If weight <= 0, the default weight for the subscription tier is used.
func Import(ctx context.Context, db *store.DB, path, label string, weight int) (*creds.Credential, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	mode := st.Mode().Perm()
	if mode&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s mode is %#o, expected 0600\n", path, mode)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f credFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	o := f.ClaudeAiOauth
	if o.AccessToken == "" || o.RefreshToken == "" || o.ExpiresAt == 0 {
		return nil, fmt.Errorf("%s: missing claudeAiOauth fields", path)
	}
	if !creds.HasOATMarker(o.AccessToken) {
		return nil, fmt.Errorf("%s: access token does not look like a Claude Code OAuth token (no sk-ant-oat marker)", path)
	}

	// expiresAt is milliseconds.
	expires := time.UnixMilli(o.ExpiresAt)

	if label == "" {
		label = o.SubscriptionType
	}

	c, err := creds.Insert(ctx, db, label, o.SubscriptionType, o.AccessToken, o.RefreshToken, expires, weight)
	if err != nil {
		return nil, err
	}
	return c, nil
}
