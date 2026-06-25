package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		`","expiresAt":9999999999000,"scopes":["user:inference"],"subscriptionType":"max"}}`
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
	if c.Label != "acct-A" || c.SubscriptionType != "max" {
		t.Fatalf("unexpected metadata: %+v", c)
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
