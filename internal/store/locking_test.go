package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// openRaw opens a scratch database with a near-zero busy timeout so contention
// surfaces immediately instead of after the production timeout. txlock selects
// BEGIN DEFERRED or IMMEDIATE, which is the behaviour under test.
func openRaw(t *testing.T, txlock string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_txlock=%s&_pragma=journal_mode(WAL)&_pragma=busy_timeout(1)",
		filepath.Join(t.TempDir(), "raw.db"), txlock)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// plainBusyError returns a genuine SQLITE_BUSY: one connection holds the write
// lock while another tries to write. The driver's Error fields are unexported,
// so the only way to obtain one is to cause it.
func plainBusyError(t *testing.T) error {
	t.Helper()
	db := openRaw(t, "immediate")
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE t SET v=2 WHERE id=1`); err != nil {
		t.Fatalf("tx write: %v", err)
	}

	_, err = db.ExecContext(ctx, `UPDATE t SET v=3 WHERE id=1`)
	if err == nil {
		t.Fatal("expected a busy error from the blocked writer, got nil")
	}
	return err
}

// snapshotBusyError returns a genuine SQLITE_BUSY_SNAPSHOT: a deferred
// transaction reads, someone else commits, and the deferred transaction's
// later write can no longer succeed.
func snapshotBusyError(t *testing.T) error {
	t.Helper()
	db := openRaw(t, "deferred")
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, `SELECT v FROM t WHERE id=1`).Scan(&v); err != nil {
		t.Fatalf("tx read: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE t SET v=v+1 WHERE id=1`); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	_, err = tx.ExecContext(ctx, `UPDATE t SET v=9 WHERE id=1`)
	if err == nil {
		t.Fatal("expected a snapshot busy error from the deferred transaction, got nil")
	}
	return err
}

// The hazard this package guards against: a deferred transaction that reads
// before it writes cannot survive another connection committing in the gap.
// The read pins a snapshot and the later write fails with SQLITE_BUSY_SNAPSHOT
// — instantly, without consulting busy_timeout, because waiting can never make
// a stale snapshot valid. pool.Bind has exactly this shape, and the failure
// reached clients as "502 proxy: database is locked".
//
// If this test ever stops failing, the driver changed its behaviour and the
// _txlock=immediate in Open's DSN deserves a fresh look.
func TestDeferredTxFailsOnConcurrentCommit(t *testing.T) {
	if err := snapshotBusyError(t); !IsBusy(err) {
		t.Fatalf("deferred tx write after concurrent commit: err = %v, want a busy error", err)
	}
}

// The fix, exercised the way production actually runs: many short read-then-
// write transactions racing plain writes on other connections. Every one must
// succeed. Under BEGIN DEFERRED this fails in seconds.
func TestConcurrentBindShapedWritesSucceed(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO credentials (id, access_token, refresh_token, expires_at, status, created_at)
		VALUES ('cred-1', 'tok', 'ref', 0, 'active', 0)`); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO conversations (id, credential_id, created_at, last_seen_at, request_count, status)
		VALUES ('conv-1', 'cred-1', 0, 0, 0, 'active')`); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	const (
		binders    = 6
		loggers    = 4
		iterations = 25
	)
	errCh := make(chan error, (binders+loggers)*iterations)
	var wg sync.WaitGroup

	// pool.Bind's shape: begin, read the pinned credential, write, commit.
	bind := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var credID string
		if err := tx.QueryRowContext(ctx,
			`SELECT credential_id FROM conversations WHERE id='conv-1'`).Scan(&credID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversations SET last_seen_at=?, request_count=request_count+1 WHERE id='conv-1'`,
			time.Now().Unix()); err != nil {
			return err
		}
		return tx.Commit()
	}

	for range binders {
		wg.Go(func() {
			for range iterations {
				if err := bind(); err != nil {
					errCh <- fmt.Errorf("bind: %w", err)
				}
			}
		})
	}
	// Everything else that writes while requests are in flight: the request
	// logger, the capture writers, the usage poller, a running CLI or TUI.
	for range loggers {
		wg.Go(func() {
			for range iterations {
				if _, err := db.ExecContext(ctx,
					`UPDATE credentials SET request_count=request_count+1 WHERE id='cred-1'`); err != nil {
					errCh <- fmt.Errorf("log: %w", err)
				}
			}
		})
	}
	wg.Wait()
	close(errCh)

	var failures int
	for err := range errCh {
		if failures < 5 {
			t.Errorf("concurrent write failed: %v", err)
		}
		failures++
	}
	if failures > 0 {
		t.Fatalf("%d of %d concurrent writes failed", failures, (binders+loggers)*iterations)
	}

	// All the bind transactions landed.
	var count int
	if err := db.QueryRow(`SELECT request_count FROM conversations WHERE id='conv-1'`).Scan(&count); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if want := binders * iterations; count != want {
		t.Fatalf("request_count = %d, want %d", count, want)
	}
}

func TestIsBusy(t *testing.T) {
	t.Run("plain busy", func(t *testing.T) {
		if err := plainBusyError(t); !IsBusy(err) {
			t.Fatalf("IsBusy(%v) = false, want true", err)
		}
	})
	t.Run("snapshot busy", func(t *testing.T) {
		if err := snapshotBusyError(t); !IsBusy(err) {
			t.Fatalf("IsBusy(%v) = false, want true", err)
		}
	})
	t.Run("wrapped busy", func(t *testing.T) {
		err := fmt.Errorf("bind conversation: %w", plainBusyError(t))
		if !IsBusy(err) {
			t.Fatalf("IsBusy(%v) = false, want true", err)
		}
	})
	t.Run("nil", func(t *testing.T) {
		if IsBusy(nil) {
			t.Fatal("IsBusy(nil) = true, want false")
		}
	})
	t.Run("non-sqlite error", func(t *testing.T) {
		if IsBusy(errors.New("no rows")) {
			t.Fatal("IsBusy(plain error) = true, want false")
		}
	})
	t.Run("other sqlite error", func(t *testing.T) {
		// A constraint violation must not be mistaken for contention, or Retry
		// would pointlessly repeat a write that can never succeed.
		db := openRaw(t, "immediate")
		_, err := db.Exec(`INSERT INTO t VALUES (1, 1)`)
		if err == nil {
			t.Fatal("expected a constraint violation")
		}
		if IsBusy(err) {
			t.Fatalf("IsBusy(%v) = true, want false", err)
		}
	})
}

func TestRetryRepeatsOnlyBusyErrors(t *testing.T) {
	ctx := context.Background()
	busy := plainBusyError(t)

	t.Run("succeeds after transient busy", func(t *testing.T) {
		calls := 0
		err := Retry(ctx, func() error {
			calls++
			if calls < 3 {
				return busy
			}
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry other errors", func(t *testing.T) {
		calls := 0
		sentinel := errors.New("constraint violated")
		err := Retry(ctx, func() error {
			calls++
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want %v", err, sentinel)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (application errors must not be retried)", calls)
		}
	})

	t.Run("gives up and returns the busy error", func(t *testing.T) {
		calls := 0
		err := Retry(ctx, func() error {
			calls++
			return busy
		})
		if !IsBusy(err) {
			t.Fatalf("err = %v, want a busy error", err)
		}
		if calls != retryAttempts {
			t.Fatalf("calls = %d, want %d", calls, retryAttempts)
		}
	})

	t.Run("honours context cancellation", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Retry(cctx, func() error { return busy })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestOpenSetsBusyTimeoutSynchronousAndPoolLimit(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var ms int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&ms); err != nil {
		t.Fatalf("pragma busy_timeout: %v", err)
	}
	if want := int(busyTimeout / time.Millisecond); ms != want {
		t.Fatalf("busy_timeout = %d, want %d", ms, want)
	}

	var syncMode int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	if syncMode != 1 { // 1 = NORMAL
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", syncMode)
	}

	if got := db.Stats().MaxOpenConnections; got != maxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, maxOpenConns)
	}
}
