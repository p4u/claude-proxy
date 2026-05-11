package users

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p4u/claude-proxy/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAndLookup(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	u, raw, err := s.Create(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "alice" {
		t.Fatalf("name: got %q want alice", u.Name)
	}
	if !strings.HasPrefix(raw, "cp_") {
		t.Fatalf("token must start with cp_, got %q", raw)
	}
	if len(raw) < 10 {
		t.Fatalf("token too short: %q", raw)
	}
	if u.TokenSHA256 == "" {
		t.Fatal("TokenSHA256 must not be empty after Create")
	}

	got, err := s.Lookup(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID || got.Name != u.Name {
		t.Fatalf("lookup mismatch: got %+v want id=%d name=%q", got, u.ID, u.Name)
	}
}

func TestLookupUnknown(t *testing.T) {
	s := NewStore(openTestDB(t))
	_, err := s.Lookup(context.Background(), "cp_doesnotexist")
	if err == nil {
		t.Fatal("expected error for unknown token")
	}
}

func TestDuplicateName(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()
	if _, _, err := s.Create(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Create(ctx, "bob")
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
}

func TestEmptyName(t *testing.T) {
	s := NewStore(openTestDB(t))
	_, _, err := s.Create(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestRevokeByName(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	_, raw, err := s.Create(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, raw); err == nil {
		t.Fatal("expected error after revoke")
	}
	if err := s.Revoke(ctx, "carol"); err == nil {
		t.Fatal("expected error revoking non-existent user")
	}
}

func TestRevokeByID(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	u, raw, err := s.Create(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, fmt.Sprintf("%d", u.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, raw); err == nil {
		t.Fatal("expected error after revoke by id")
	}
}

func TestList(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}

	if _, _, err := s.Create(ctx, "eve"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, "frank"); err != nil {
		t.Fatal(err)
	}

	list, err = s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 users, got %d", len(list))
	}
	if list[0].Name != "eve" || list[1].Name != "frank" {
		t.Fatalf("unexpected names: %q %q", list[0].Name, list[1].Name)
	}
	if list[0].TokenSHA256 == "" || list[1].TokenSHA256 == "" {
		t.Fatal("TokenSHA256 must be populated by List")
	}
}

func TestTouchLastUsed(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	u, _, err := s.Create(ctx, "grace")
	if err != nil {
		t.Fatal(err)
	}
	if !u.LastUsedAt.IsZero() {
		t.Fatal("expected zero LastUsedAt before touch")
	}
	if err := s.TouchLastUsed(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[0].LastUsedAt.IsZero() {
		t.Fatal("expected non-zero LastUsedAt after touch")
	}
}

func TestTokenFingerprint(t *testing.T) {
	fp := TokenFingerprint("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	if fp != "abcdef12" {
		t.Fatalf("fingerprint: got %q want abcdef12", fp)
	}
	if got := TokenFingerprint("short"); got != "short" {
		t.Fatalf("short fingerprint: got %q want short", got)
	}
}

// TestCLIFlow exercises the add → list → revoke → list cycle used by the CLI.
func TestCLIFlow(t *testing.T) {
	s := NewStore(openTestDB(t))
	ctx := context.Background()

	// add
	u, raw, err := s.Create(ctx, "cli-user")
	if err != nil {
		t.Fatal("create:", err)
	}

	// list → 1 user
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal("list:", err)
	}
	if len(list) != 1 || list[0].Name != "cli-user" {
		t.Fatalf("list after add: %+v", list)
	}

	// valid lookup
	if _, err := s.Lookup(ctx, raw); err != nil {
		t.Fatal("lookup before revoke:", err)
	}

	// revoke
	if err := s.Revoke(ctx, u.Name); err != nil {
		t.Fatal("revoke:", err)
	}

	// list → empty
	list, err = s.List(ctx)
	if err != nil {
		t.Fatal("list after revoke:", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list after revoke, got %d", len(list))
	}

	// lookup after revoke → error
	if _, err := s.Lookup(ctx, raw); err == nil {
		t.Fatal("expected error after revoke")
	}
}
