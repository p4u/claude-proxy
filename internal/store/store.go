package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if _, err := sdb.Exec(schema); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, alter := range []string{
		`ALTER TABLE credentials ADD COLUMN subscription_type TEXT`,
		`ALTER TABLE credentials ADD COLUMN last_request_at INTEGER`,
		`ALTER TABLE credentials ADD COLUMN request_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE credentials ADD COLUMN success_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE credentials ADD COLUMN error_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE credentials ADD COLUMN weight INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE request_log ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_log ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_log ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_log ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_log ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := sdb.Exec(alter); err != nil && !isDuplicateColumn(err) {
			_ = sdb.Close()
			return nil, fmt.Errorf("migrate %q: %w", alter, err)
		}
	}
	return &DB{sdb}, nil
}

func isDuplicateColumn(err error) bool {
	s := err.Error()
	return contains(s, "duplicate column name") || contains(s, "already exists")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
