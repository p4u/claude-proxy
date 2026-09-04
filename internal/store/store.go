package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// maxOpenConns bounds how many connections may contend for the single write
// lock. SQLite serialises writers regardless, so an unbounded pool does not buy
// throughput — it just deepens the queue every writer must outwait within
// busyTimeout. The cap sits comfortably above the number of concurrent readers
// the dashboard's aggregation queries need.
const maxOpenConns = 8

// busyTimeout is how long a blocked writer retries before returning
// SQLITE_BUSY. It must exceed the longest single write in the system; the
// retention janitor deletes in bounded batches specifically so that holds.
const busyTimeout = 15 * time.Second

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	// _txlock=immediate makes every explicit transaction take the write lock at
	// BEGIN. Without it, a transaction that reads before it writes (pool.Bind,
	// creds.Delete) pins a read snapshot, and any commit by another connection
	// in the gap makes the later write fail with SQLITE_BUSY_SNAPSHOT —
	// immediately and unconditionally, because no amount of waiting can rescue
	// a stale snapshot. busy_timeout is never consulted for that error, so
	// taking the lock up front is the only fix.
	//
	// synchronous=NORMAL is the standard pairing for WAL: commits stop fsyncing
	// individually (durability narrows to "the last commits may be lost on
	// power loss", never corruption), which shortens every write-lock hold and
	// so shrinks the window in which anyone else can collide.
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate"+
			"&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)"+
			"&_pragma=busy_timeout(%d)"+
			"&_pragma=foreign_keys(1)",
		path, busyTimeout.Milliseconds())
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(maxOpenConns)
	// Idle count matches the open cap: every new connection re-runs the DSN
	// pragmas, so recycling connections is pure overhead here.
	sdb.SetMaxIdleConns(maxOpenConns)
	sdb.SetConnMaxIdleTime(5 * time.Minute)
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
		`ALTER TABLE user_tokens ADD COLUMN full_capture INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_tokens ADD COLUMN limit_output_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_tokens ADD COLUMN limit_window_seconds INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_tokens ADD COLUMN block_suggestions INTEGER NOT NULL DEFAULT 0`,
		// Multi-provider support. The DEFAULT is what migrates existing rows:
		// every credential that predates this column is an Anthropic OAuth
		// subscription, so no backfill is needed.
		`ALTER TABLE credentials ADD COLUMN provider TEXT NOT NULL DEFAULT 'anthropic'`,
		// Per-credential endpoint override. Empty = the provider default; set
		// when a provider runs regional clusters and a key is bound to one.
		`ALTER TABLE credentials ADD COLUMN base_url TEXT NOT NULL DEFAULT ''`,
		// Per-credential model catalogue, JSON, for custom Anthropic- or
		// OpenAI-compatible hosts. Empty for registry providers.
		`ALTER TABLE credentials ADD COLUMN models TEXT NOT NULL DEFAULT ''`,
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
