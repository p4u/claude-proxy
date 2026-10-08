package creds

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// insertCred is a small helper for tests that need a credential to exist.
func insertCred(t *testing.T, db *store.DB, label string) *Credential {
	t.Helper()
	c, err := Insert(context.Background(), db, label, "max",
		"sk-ant-oat-access", "sk-ant-ort-refresh", time.Now().Add(time.Hour), 5)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return c
}

func TestInsertGetList(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	c := insertCred(t, db, "acct-A")
	if c.ID == "" || c.Status != StatusActive || c.Weight != 5 {
		t.Fatalf("unexpected credential: %+v", c)
	}

	got, err := Get(ctx, db, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "acct-A" || got.AccessToken != "sk-ant-oat-access" {
		t.Fatalf("get mismatch: %+v", got)
	}

	list, err := List(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 credential, got %d", len(list))
	}

	if _, err := Get(ctx, db, "cred_missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRateLimitTierPersistence(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c, err := InsertWithRateLimitTier(ctx, db, "tiered", "max", "sk-ant-oat-access", "ref", time.Now().Add(time.Hour), 0, " default_claude_max_20x ")
	if err != nil {
		t.Fatal(err)
	}
	if c.RateLimitTier != "default_claude_max_20x" || c.Weight != 5 {
		t.Fatalf("inserted tier=%q weight=%d", c.RateLimitTier, c.Weight)
	}
	got, err := Get(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := List(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimitTier != c.RateLimitTier || len(list) != 1 || list[0].RateLimitTier != c.RateLimitTier {
		t.Fatal("tier not preserved by get/list")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["rate_limit_tier"]) != `"default_claude_max_20x"` {
		t.Fatalf("JSON tier = %s", fields["rate_limit_tier"])
	}
	legacy := insertCred(t, db, "unknown")
	got, err = Get(ctx, db, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.RateLimitTier != "" || got.RateLimitTier != "" {
		t.Fatal("plain Insert must leave the tier unknown")
	}
}

func TestSetRateLimitTier(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "manual")
	if err := SetStatus(ctx, db, c.ID, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ input, want string }{
		{"20x", "20x"},
		{" 5x ", "5x"},
		{" future:Plan/未定 ", "future:Plan/未定"},
		{strings.Repeat("é", 64), strings.Repeat("é", 64)}, // 128 UTF-8 bytes
		{"", ""},
		{"  ", ""},
	} {
		if err := SetRateLimitTier(ctx, db, c.ID, tt.input); err != nil {
			t.Fatalf("set %q: %v", tt.input, err)
		}
		got, err := Get(ctx, db, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RateLimitTier != tt.want || got.Weight != 5 || got.Status != StatusDisabled || got.SubscriptionType != "max" {
			t.Fatalf("set %q: tier=%q weight=%d status=%q plan=%q", tt.input, got.RateLimitTier, got.Weight, got.Status, got.SubscriptionType)
		}
	}
	if err := SetRateLimitTier(ctx, db, "cred_missing", "20x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ID: %v", err)
	}
	if err := SetRateLimitTier(ctx, db, "cred_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ID clear: %v", err)
	}
}

func TestRateLimitTierRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "existing")
	if err := SetRateLimitTier(ctx, db, c.ID, "5x"); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []string{
		strings.Repeat("a", 129), strings.Repeat("é", 65),
		"bad\x00tier", "20x\n", "\t20x", "bad\x7ftier", "bad\u0085tier", string([]byte{0xff}),
	} {
		if err := SetRateLimitTier(ctx, db, c.ID, tier); !errors.Is(err, ErrInvalidRateLimitTier) {
			t.Fatalf("set invalid %q: %v", tier, err)
		}
		if _, err := InsertWithRateLimitTier(ctx, db, "invalid", "max", "fake", "fake", time.Now(), 5, tier); !errors.Is(err, ErrInvalidRateLimitTier) {
			t.Fatalf("insert invalid %q: %v", tier, err)
		}
	}
	list, err := List(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].RateLimitTier != "5x" {
		t.Fatal("invalid tier mutated credentials")
	}
}

func TestSubscriptionTypeClearsStaleTier(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "plan")
	if err := SetRateLimitTier(ctx, db, c.ID, "20x"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ plan, wantTier string }{{"max", "20x"}, {"pro", ""}} {
		if err := SetSubscriptionType(ctx, db, c.ID, tt.plan); err != nil {
			t.Fatal(err)
		}
		got, err := Get(ctx, db, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SubscriptionType != tt.plan || got.RateLimitTier != tt.wantTier || got.Weight != 5 {
			t.Fatalf("plan=%q tier=%q weight=%d", got.SubscriptionType, got.RateLimitTier, got.Weight)
		}
	}
}

func TestMigrateLegacyRateLimitTier(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := insertCred(t, db, "legacy")
	for _, stmt := range []string{
		`ALTER TABLE credentials DROP COLUMN rate_limit_tier`,
		`DROP INDEX idx_request_log_cred_ts`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The second reopen checks duplicate-column handling and preservation of a
	// tier set after migration. Legacy rows themselves must start unknown.
	for i := range 2 {
		db, err = store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		got, err := Get(ctx, db, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantTier := ""
		if i == 1 {
			wantTier = "20x"
		}
		if got.RateLimitTier != wantTier || got.SubscriptionType != "max" || got.Weight != 5 {
			t.Fatalf("migrated tier=%q plan=%q weight=%d", got.RateLimitTier, got.SubscriptionType, got.Weight)
		}
		var columns string
		if err := db.QueryRow(`SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_index_info('idx_request_log_cred_ts') ORDER BY seqno)`).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if columns != "credential_id,ts" {
			t.Fatalf("request index columns = %q", columns)
		}
		if _, err := db.Exec(`UPDATE credentials SET rate_limit_tier=NULL WHERE id=?`, c.ID); err == nil {
			t.Fatal("rate_limit_tier must be NOT NULL")
		}
		if err := SetRateLimitTier(ctx, db, c.ID, "20x"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsertDefaultWeight(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	// weight 0 -> derive from subscription tier.
	c, err := Insert(ctx, db, "pro-acct", "pro", "sk-ant-oat-x", "ref", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if c.Weight != 1 {
		t.Fatalf("pro default weight = %d, want 1", c.Weight)
	}
}

func TestSetWeight(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "a")

	if err := SetWeight(ctx, db, c.ID, 9); err != nil {
		t.Fatalf("set-weight: %v", err)
	}
	got, _ := Get(ctx, db, c.ID)
	if got.Weight != 9 {
		t.Fatalf("weight = %d, want 9", got.Weight)
	}

	if err := SetWeight(ctx, db, c.ID, 0); err == nil {
		t.Fatal("set-weight 0 should error")
	}
	if err := SetWeight(ctx, db, "cred_missing", 3); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSetStatusAndUpdateTokens(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "a")
	if err := SetRateLimitTier(ctx, db, c.ID, "20x"); err != nil {
		t.Fatal(err)
	}

	if err := SetStatus(ctx, db, c.ID, StatusDisabled); err != nil {
		t.Fatalf("set-status: %v", err)
	}
	got, _ := Get(ctx, db, c.ID)
	if got.Status != StatusDisabled {
		t.Fatalf("status = %q, want disabled", got.Status)
	}

	exp := time.Now().Add(2 * time.Hour)
	if err := UpdateTokens(ctx, db, c.ID, "sk-ant-oat-new", "ref-new", exp); err != nil {
		t.Fatalf("update-tokens: %v", err)
	}
	got, _ = Get(ctx, db, c.ID)
	if got.AccessToken != "sk-ant-oat-new" || got.RefreshToken != "ref-new" {
		t.Fatalf("tokens not updated: %+v", got)
	}
	if got.Status != StatusActive {
		t.Fatalf("UpdateTokens should heal status to active, got %q", got.Status)
	}
	if got.RateLimitTier != "20x" {
		t.Fatalf("UpdateTokens lost tier: %q", got.RateLimitTier)
	}
	if got.ExpiresAt.Unix() != exp.Unix() {
		t.Fatalf("expiry = %v, want %v", got.ExpiresAt, exp)
	}
}

func TestUpdateTokensWithSubscriptionMissing(t *testing.T) {
	err := UpdateTokensWithSubscription(context.Background(), testDB(t), "cred_missing",
		"sk-ant-oat-new", "ref-new", time.Now().Add(time.Hour), "max", "20x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ID: %v", err)
	}
}

func TestMarkCounters(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "a")

	if err := MarkRequest(ctx, db, c.ID); err != nil {
		t.Fatalf("mark-request: %v", err)
	}
	if err := MarkError(ctx, db, c.ID); err != nil {
		t.Fatalf("mark-error: %v", err)
	}
	if err := MarkLimited(ctx, db, c.ID, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("mark-limited: %v", err)
	}
	got, _ := Get(ctx, db, c.ID)
	if got.Status != StatusLimited || got.RetryAfter == nil {
		t.Fatalf("expected limited with retry-after, got %+v", got)
	}

	// MarkSuccess heals a limited credential back to active and clears retry_after.
	if err := MarkSuccess(ctx, db, c.ID); err != nil {
		t.Fatalf("mark-success: %v", err)
	}
	got, _ = Get(ctx, db, c.ID)
	if got.Status != StatusActive || got.RetryAfter != nil {
		t.Fatalf("MarkSuccess should heal to active and clear retry_after, got %+v", got)
	}
	if got.RequestCount != 1 || got.ErrorCount != 1 || got.SuccessCount != 1 {
		t.Fatalf("counters req=%d err=%d ok=%d, want 1/1/1", got.RequestCount, got.ErrorCount, got.SuccessCount)
	}
}

func TestHasRefreshToken(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	insertCred(t, db, "a") // refresh token "sk-ant-ort-refresh"

	ok, err := HasRefreshToken(ctx, db, "sk-ant-ort-refresh")
	if err != nil {
		t.Fatalf("has-refresh: %v", err)
	}
	if !ok {
		t.Fatal("expected refresh token to be found")
	}
	ok, _ = HasRefreshToken(ctx, db, "nope")
	if ok {
		t.Fatal("did not expect to find unknown refresh token")
	}
}

// TestDeleteWithBindings is the regression test for the FK-787 bug: deleting a
// credential that has conversation bindings and usage_history rows must succeed
// (it previously failed with FOREIGN KEY constraint failed because conversations
// had no ON DELETE CASCADE).
func TestDeleteWithBindings(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	c := insertCred(t, db, "a")

	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO conversations (id, credential_id, created_at, last_seen_at, request_count, status)
		VALUES (?, ?, ?, ?, 0, 'active')`, "conv_1", c.ID, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_history (credential_id, captured_at, five_hour_pct, seven_day_pct)
		VALUES (?, ?, 12.5, 30.0)`, c.ID, now); err != nil {
		t.Fatalf("insert usage_history: %v", err)
	}

	if err := Delete(ctx, db, c.ID); err != nil {
		t.Fatalf("delete with bindings failed (FK-787 regression): %v", err)
	}

	// Credential and its dependent rows are gone.
	if _, err := Get(ctx, db, c.ID); err != ErrNotFound {
		t.Fatalf("credential still present after delete: %v", err)
	}
	for _, tbl := range []string{"conversations", "usage_history"} {
		var n int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+tbl+" WHERE credential_id=?", c.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("%s still has %d rows for deleted credential", tbl, n)
		}
	}

	if err := Delete(ctx, db, "cred_missing"); err != ErrNotFound {
		t.Fatalf("delete missing: want ErrNotFound, got %v", err)
	}
}
