package store

import (
	"path/filepath"
	"testing"
)

func TestOpenAppliesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Schema applied: all expected tables exist.
	for _, tbl := range []string{"credentials", "conversations", "rr_cursor", "user_tokens", "request_log", "usage_history"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", tbl, err)
		}
	}
	_ = db.Close()

	// Re-opening the same file must succeed (schema CREATE IF NOT EXISTS +
	// ADD COLUMN migrations are no-ops the second time).
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
}

func TestForeignKeysEnabled(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var on int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys = %d, want 1", on)
	}
}
