package usertoken

import (
	"context"
	"path/filepath"
	"testing"

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

func TestCreateListGetLookup(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	if HasAny(ctx, db) {
		t.Fatal("fresh db should have no user tokens")
	}

	ut, err := Create(ctx, db, "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ut.ID == "" || ut.Token == "" || ut.Status != StatusActive {
		t.Fatalf("unexpected token: %+v", ut)
	}
	if !HasAny(ctx, db) {
		t.Fatal("HasAny should be true after create")
	}

	got, err := Get(ctx, db, ut.ID)
	if err != nil || got.Name != "alice" {
		t.Fatalf("get: %v %+v", err, got)
	}

	byTok, err := LookupByToken(ctx, db, ut.Token)
	if err != nil || byTok.ID != ut.ID {
		t.Fatalf("lookup-by-token: %v %+v", err, byTok)
	}

	list, err := List(ctx, db)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	if _, err := Get(ctx, db, "utok_missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := LookupByToken(ctx, db, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUniqueName(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if _, err := Create(ctx, db, "bob"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Create(ctx, db, "bob"); err == nil {
		t.Fatal("duplicate name should violate UNIQUE constraint")
	}
}

func TestSetStatusRefreshDelete(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, _ := Create(ctx, db, "carol")

	if err := SetStatus(ctx, db, ut.ID, StatusDisabled); err != nil {
		t.Fatalf("set-status: %v", err)
	}
	got, _ := Get(ctx, db, ut.ID)
	if got.Status != StatusDisabled {
		t.Fatalf("status = %q, want disabled", got.Status)
	}
	if err := SetStatus(ctx, db, "utok_missing", StatusActive); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	newTok, err := Refresh(ctx, db, ut.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if newTok == ut.Token {
		t.Fatal("refresh should produce a new token value")
	}
	if _, err := Refresh(ctx, db, "utok_missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// MarkUsed sets last_used_at (best-effort, no error returned).
	MarkUsed(ctx, db, ut.ID)
	got, _ = Get(ctx, db, ut.ID)
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at should be set after MarkUsed")
	}

	if err := Delete(ctx, db, ut.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := Get(ctx, db, ut.ID); err != ErrNotFound {
		t.Fatalf("token should be gone, got %v", err)
	}
	if err := Delete(ctx, db, "utok_missing"); err != ErrNotFound {
		t.Fatalf("delete missing: want ErrNotFound, got %v", err)
	}
}
