package creds

import (
	"context"
	"path/filepath"
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
	if got.ExpiresAt.Unix() != exp.Unix() {
		t.Fatalf("expiry = %v, want %v", got.ExpiresAt, exp)
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
