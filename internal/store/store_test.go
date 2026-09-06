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

func TestMigrateLegacyConversationBoundAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the previous conversations shape in this synthetic database.
	if _, err := db.Exec(`ALTER TABLE conversations DROP COLUMN bound_at;
		INSERT INTO credentials (id,access_token,refresh_token,expires_at,status,created_at) VALUES ('test','fake','fake',4100000000,'active',1700000000);
		INSERT INTO conversations (id,credential_id,created_at,last_seen_at) VALUES ('old','test',1700000000,1700000100)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		var created, bound int64
		err = db.QueryRow(`SELECT credential_id,created_at,bound_at FROM conversations WHERE id='old'`).Scan(&id, &created, &bound)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if id != "test" || created != 1700000000 || bound != 0 {
			t.Fatalf("legacy row changed: %s %d %d", id, created, bound)
		}
	}
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
