package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/claudeoauth"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mockToken points creds.RefreshTokens at a fake endpoint returning fresh tokens.
func mockToken(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":3600}`))
	}))
	prev := creds.TokenURL
	creds.SetTokenURL(srv.URL)
	t.Cleanup(func() { creds.SetTokenURL(prev); srv.Close() })
}

// writeCredFile writes a synthetic .credentials.json with mode 0600.
func writeCredFile(t *testing.T, access, refresh string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"` + refresh +
		`","expiresAt":9999999999000,"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cred file: %v", err)
	}
	return path
}

func future() time.Time { return time.Now().Add(time.Hour) }

func TestImportSuccess(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	mockToken(t)

	path := writeCredFile(t, "sk-ant-oat-orig", "ref-orig")
	c, err := Import(ctx, db, path, "acct-A", 0)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// Tokens come from the liveness refresh, not the file.
	if c.AccessToken != "sk-ant-oat-fresh" || c.RefreshToken != "ref-fresh" {
		t.Fatalf("expected refreshed tokens, got %+v", c)
	}
	if c.Label != "acct-A" || c.SubscriptionType != "max" || c.RateLimitTier != "default_claude_max_20x" {
		t.Fatalf("unexpected metadata: label=%q plan=%q tier=%q", c.Label, c.SubscriptionType, c.RateLimitTier)
	}
	got, err := creds.Get(ctx, db, c.ID)
	if err != nil || got.RateLimitTier != c.RateLimitTier {
		t.Fatalf("stored tier mismatch: %v", err)
	}
}

func TestImportTierSources(t *testing.T) {
	ctx := context.Background()
	mockToken(t)
	for _, source := range []string{"json", "oauth"} {
		for _, tier := range []string{"", "default_claude_max_5x", " future/raw:Tier "} {
			t.Run(source+"/"+tier, func(t *testing.T) {
				db := testDB(t)
				var c *creds.Credential
				var err error
				if source == "oauth" {
					c, err = ImportOAuth(ctx, db, &claudeoauth.Tokens{
						AccessToken: "sk-ant-oat-new", RefreshToken: "ref-new", ExpiresAt: future(),
						SubscriptionType: "max", RateLimitTier: tier,
					}, "new", 7)
				} else {
					c, err = ImportFromJSON(ctx, db, tierJSON(t, "max", tier), "new", 7)
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := creds.Get(ctx, db, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				if c.RateLimitTier != strings.TrimSpace(tier) || got.RateLimitTier != c.RateLimitTier || got.Weight != 7 {
					t.Fatalf("tier=%q stored=%q weight=%d", c.RateLimitTier, got.RateLimitTier, got.Weight)
				}
			})
		}
	}
}

func tierJSON(t *testing.T, plan, tier string) []byte {
	t.Helper()
	block := map[string]any{
		"accessToken": "sk-ant-oat-input", "refreshToken": "ref-input", "expiresAt": 9999999999000,
	}
	if plan != "" {
		block["subscriptionType"] = plan
	}
	if tier != "" {
		block["rateLimitTier"] = tier
	}
	b, err := json.Marshal(map[string]any{"claudeAiOauth": block})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReconnectTierMetadata(t *testing.T) {
	ctx := context.Background()
	mockToken(t)
	for _, source := range []string{"json", "oauth"} {
		for _, tt := range []struct {
			name, plan, tier, wantPlan, wantTier string
		}{
			{"same plan missing tier", "max", "", "max", "20x"},
			{"same plan updated tier", "max", "default_claude_max_5x", "max", "default_claude_max_5x"},
			{"changed plan missing tier", "pro", "", "pro", ""},
			{"changed plan updated tier", "pro", "future-pro", "pro", "future-pro"},
			{"missing metadata", "", "", "max", "20x"},
			{"missing plan supplied tier", "", "future-max", "max", "future-max"},
		} {
			t.Run(source+"/"+tt.name, func(t *testing.T) {
				db := testDB(t)
				c, err := creds.InsertWithRateLimitTier(ctx, db, "old", "max", "sk-ant-oat-old", "ref-old", future(), 11, "20x")
				if err != nil {
					t.Fatal(err)
				}
				if err := creds.SetStatus(ctx, db, c.ID, creds.StatusRevoked); err != nil {
					t.Fatal(err)
				}
				var updated *creds.Credential
				if source == "oauth" {
					updated, err = UpdateFromOAuth(ctx, db, c.ID, &claudeoauth.Tokens{
						AccessToken: "sk-ant-oat-fresh", RefreshToken: "ref-fresh", ExpiresAt: future(),
						SubscriptionType: tt.plan, RateLimitTier: tt.tier,
					})
				} else {
					updated, err = UpdateFromJSON(ctx, db, c.ID, tierJSON(t, tt.plan, tt.tier))
				}
				if err != nil {
					t.Fatal(err)
				}
				if updated.RateLimitTier != tt.wantTier || updated.SubscriptionType != tt.wantPlan {
					t.Fatalf("tier=%q plan=%q, want %q/%q", updated.RateLimitTier, updated.SubscriptionType, tt.wantTier, tt.wantPlan)
				}
				if updated.Weight != 11 || updated.Label != "old" || updated.Status != creds.StatusActive || updated.AccessToken != "sk-ant-oat-fresh" {
					t.Fatal("reconnect must preserve label/weight and heal tokens/status")
				}
			})
		}
	}
}

func TestImportInvalidTierBeforeRefresh(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	prev := creds.TokenURL
	creds.SetTokenURL(srv.URL)
	t.Cleanup(func() { creds.SetTokenURL(prev); srv.Close() })
	c, err := creds.InsertWithRateLimitTier(ctx, db, "old", "max", "sk-ant-oat-old", "ref-old", future(), 5, "20x")
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range []string{"bad\ntier", strings.Repeat("é", 65)} {
		raw := tierJSON(t, "pro", tier)
		if _, err := ImportFromJSON(ctx, db, raw, "new", 0); !errors.Is(err, creds.ErrInvalidRateLimitTier) {
			t.Fatalf("import invalid tier: %v", err)
		}
		if _, err := UpdateFromJSON(ctx, db, c.ID, raw); !errors.Is(err, creds.ErrInvalidRateLimitTier) {
			t.Fatalf("update invalid tier: %v", err)
		}
		tok := &claudeoauth.Tokens{AccessToken: "sk-ant-oat-new", RefreshToken: "ref-new", ExpiresAt: future(), SubscriptionType: "pro", RateLimitTier: tier}
		if _, err := ImportOAuth(ctx, db, tok, "new", 0); !errors.Is(err, creds.ErrInvalidRateLimitTier) {
			t.Fatalf("OAuth import invalid tier: %v", err)
		}
		if _, err := UpdateFromOAuth(ctx, db, c.ID, tok); !errors.Is(err, creds.ErrInvalidRateLimitTier) {
			t.Fatalf("OAuth update invalid tier: %v", err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid metadata caused %d refresh attempts", calls)
	}
	list, err := creds.List(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AccessToken != c.AccessToken || list[0].RateLimitTier != "20x" || list[0].SubscriptionType != "max" {
		t.Fatal("invalid tier changed stored tokens or metadata")
	}
}

func TestImportRejectsNonOAT(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	path := writeCredFile(t, "not-a-real-token", "ref")
	if _, err := Import(ctx, db, path, "x", 0); err == nil {
		t.Fatal("expected rejection of token without sk-ant-oat marker")
	}
}

func TestImportRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	mockToken(t)

	// Pre-seed a credential whose refresh token matches the file's.
	if _, err := creds.Insert(ctx, db, "existing", "max",
		"sk-ant-oat-x", "dup-ref", future(), 5); err != nil {
		t.Fatalf("seed: %v", err)
	}
	path := writeCredFile(t, "sk-ant-oat-orig", "dup-ref")
	if _, err := Import(ctx, db, path, "x", 0); err == nil {
		t.Fatal("expected duplicate refresh-token rejection")
	}
}

func TestUpdateFromFile(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	mockToken(t)

	c, err := creds.Insert(ctx, db, "acct-A", "max",
		"sk-ant-oat-old", "ref-old", future(), 5)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Revoke it to prove UpdateFromFile heals status back to active.
	if err := creds.SetStatus(ctx, db, c.ID, creds.StatusRevoked); err != nil {
		t.Fatalf("set-status: %v", err)
	}

	path := writeCredFile(t, "sk-ant-oat-orig", "ref-new-lineage")
	updated, err := UpdateFromFile(ctx, db, c.ID, path)
	if err != nil {
		t.Fatalf("update-from-file: %v", err)
	}
	if updated.AccessToken != "sk-ant-oat-fresh" || updated.RefreshToken != "ref-fresh" {
		t.Fatalf("tokens not replaced: %+v", updated)
	}
	if updated.Status != creds.StatusActive {
		t.Fatalf("status = %q, want active", updated.Status)
	}

	// Unknown id -> ErrNotFound.
	if _, err := UpdateFromFile(ctx, db, "cred_missing", path); err != creds.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
