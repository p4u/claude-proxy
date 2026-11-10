package creds

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// AC2: the deadline column exists on fresh and migrated databases, rows that
// predate it read as unknown, and legacy "expired" Anthropic rows are healed.
func TestRefreshDeadlineMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := insertCred(t, db, "legacy")
	if c.RefreshExpiresAt != (time.Time{}) {
		t.Fatalf("fresh insert deadline = %v, want unknown", c.RefreshExpiresAt)
	}
	key, err := InsertKey(ctx, db, provider.GLM, "glm", "", "glm-key", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE credentials SET status='expired'`,
		`ALTER TABLE credentials DROP COLUMN refresh_expires_at`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var cols int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('credentials') WHERE name='refresh_expires_at'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("refresh_expires_at columns = %d, %v", cols, err)
	}
	got, err := Get(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive || !got.RefreshExpiresAt.IsZero() {
		t.Fatalf("legacy row: status=%q deadline=%v, want active and unknown", got.Status, got.RefreshExpiresAt)
	}
	if k, _ := Get(ctx, db, key.ID); k.Status != StatusExpired {
		t.Fatalf("non-Anthropic row healed to %q; only Anthropic rows are", k.Status)
	}
}

func TestRefreshDeadlineStoredAndReplaced(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	deadline := time.Now().Add(27 * 24 * time.Hour).Truncate(time.Second)
	c, err := InsertWithRateLimitTier(ctx, db, "a", "max", "sk-ant-oat-a", "ref-a", time.Now().Add(8*time.Hour), 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(deadline) {
		t.Fatalf("stored deadline = %v, want %v", got.RefreshExpiresAt, deadline)
	}
	newer := deadline.Add(48 * time.Hour)
	if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-b", "ref-b", time.Now().Add(8*time.Hour), "", "", newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(newer) {
		t.Fatalf("reconnect deadline = %v, want the new login's %v", got.RefreshExpiresAt, newer)
	}
	if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-c", "ref-c", time.Now().Add(8*time.Hour), "", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, c.ID); !got.RefreshExpiresAt.IsZero() {
		t.Fatalf("a login without a deadline must store unknown, got %v", got.RefreshExpiresAt)
	}
}

func TestLoginEndsAt(t *testing.T) {
	now := time.Now()
	year, month := now.AddDate(1, 0, 0), now.Add(27*24*time.Hour)
	for _, tt := range []struct {
		name string
		c    Credential
		want time.Time
		ok   bool
	}{
		{"long-lived token", Credential{Provider: provider.Anthropic, ExpiresAt: year, RefreshExpiresAt: month}, year, true},
		{"8-hour token", Credential{Provider: provider.Anthropic, ExpiresAt: now.Add(8 * time.Hour), RefreshExpiresAt: month}, month, true},
		{"unknown deadline", Credential{Provider: provider.Anthropic, ExpiresAt: year}, time.Time{}, false},
		{"api key", Credential{Provider: provider.GLM, ExpiresAt: year, RefreshExpiresAt: month}, time.Time{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.c.LoginEndsAt()
			if ok != tt.ok || !got.Equal(tt.want) {
				t.Fatalf("LoginEndsAt() = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
