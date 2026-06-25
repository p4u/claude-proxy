package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

type oauthBlock struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // milliseconds
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
}

type credFile struct {
	ClaudeAiOauth oauthBlock `json:"claudeAiOauth"`
}

// parseCredBytes validates the raw JSON of a Claude Code .credentials.json and
// returns the embedded OAuth block. who labels the source in error messages
// (e.g. a file path, or "pasted credentials").
func parseCredBytes(raw []byte, who string) (oauthBlock, error) {
	var f credFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return oauthBlock{}, fmt.Errorf("parse %s: %w", who, err)
	}
	o := f.ClaudeAiOauth
	if o.AccessToken == "" || o.RefreshToken == "" || o.ExpiresAt == 0 {
		return oauthBlock{}, fmt.Errorf("%s: missing claudeAiOauth fields", who)
	}
	if !creds.HasOATMarker(o.AccessToken) {
		return oauthBlock{}, fmt.Errorf("%s: access token does not look like a Claude Code OAuth token (no sk-ant-oat marker)", who)
	}
	return o, nil
}

// parseCredFile reads and validates a .credentials.json from disk. It warns
// (but does not fail) on loose file perms.
func parseCredFile(path string) (oauthBlock, error) {
	st, err := os.Stat(path)
	if err != nil {
		return oauthBlock{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s mode is %#o, expected 0600\n", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return oauthBlock{}, err
	}
	return parseCredBytes(raw, path)
}

// insertVerified runs the shared liveness-check + insert path used by Import and
// ImportFromJSON: it refreshes the token to confirm the credential is alive
// (yielding a fresh access token + authoritative expiry), rejects duplicates,
// and inserts. label defaults to the subscription type when empty.
func insertVerified(ctx context.Context, db *store.DB, o oauthBlock, label string, weight int) (*creds.Credential, error) {
	dup, err := creds.HasRefreshToken(ctx, db, o.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("duplicate check: %w", err)
	}
	if dup {
		return nil, fmt.Errorf("credential already imported (refresh token already exists in the pool)")
	}

	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
	accessToken, refreshToken, expires, err := creds.RefreshTokens(ctx, o.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("credential is not alive: %w", err)
	}

	if label == "" {
		label = o.SubscriptionType
	}
	return creds.Insert(ctx, db, label, o.SubscriptionType, accessToken, refreshToken, expires, weight)
}

// Import reads a Claude Code .credentials.json and inserts it into the pool.
// If weight <= 0, the default weight for the subscription tier is used.
func Import(ctx context.Context, db *store.DB, path, label string, weight int) (*creds.Credential, error) {
	o, err := parseCredFile(path)
	if err != nil {
		return nil, err
	}
	return insertVerified(ctx, db, o, label, weight)
}

// ImportFromJSON imports a credential from the raw bytes of a .credentials.json
// (e.g. pasted into the TUI) rather than a file on disk. Same validation,
// liveness check, and duplicate rejection as Import.
func ImportFromJSON(ctx context.Context, db *store.DB, raw []byte, label string, weight int) (*creds.Credential, error) {
	o, err := parseCredBytes(raw, "pasted credentials")
	if err != nil {
		return nil, err
	}
	return insertVerified(ctx, db, o, label, weight)
}

// UpdateFromFile re-points an existing credential at a freshly re-logged-in
// .credentials.json — used when a subscription's OAuth lineage was reset and
// the stored refresh token no longer works. It verifies the new token is alive
// (which also yields a fresh access token + authoritative expiry) and writes it
// over the existing credential's tokens, healing its status back to active.
//
// Unlike Import it does not run the duplicate-refresh-token check: a credential
// legitimately re-uses its own lineage, and the file may even carry the same
// refresh token that is already stored.
func UpdateFromFile(ctx context.Context, db *store.DB, id, path string) (*creds.Credential, error) {
	if _, err := creds.Get(ctx, db, id); err != nil {
		return nil, err
	}
	o, err := parseCredFile(path)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
	accessToken, refreshToken, expires, err := creds.RefreshTokens(ctx, o.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("credential is not alive: %w", err)
	}
	if err := creds.UpdateTokens(ctx, db, id, accessToken, refreshToken, expires); err != nil {
		return nil, err
	}
	return creds.Get(ctx, db, id)
}
