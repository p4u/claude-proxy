package store

import (
	"context"
	"errors"
	"math/rand"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// retryAttempts is the number of times Retry will run fn before giving up. The
// backoff below tops out well under busyTimeout, so a caller that exhausts all
// attempts has been losing races for long enough that failing is the honest
// answer rather than queueing further.
const retryAttempts = 4

// IsBusy reports whether err is SQLite saying it could not get a lock.
//
// It matches both the plain and the extended result codes. The extended ones
// matter most: SQLITE_BUSY_SNAPSHOT (517) is what a read-then-write transaction
// gets when another connection commits underneath it, and unlike plain
// SQLITE_BUSY it is returned instantly without ever consulting busy_timeout.
// Masking to the low byte catches every such variant without enumerating them.
func IsBusy(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	switch serr.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// Retry runs fn, repeating it while it fails with a lock error. Anything else —
// including success — returns immediately, so application errors are never
// retried and never delayed.
//
// fn must be safe to run more than once. That holds for a self-contained
// transaction, which is what the caller uses: a failed transaction rolls back
// completely, leaving nothing for the next attempt to trip over.
//
// Backoff is randomised. Contending writers that back off on identical
// schedules keep colliding on identical schedules; jitter is what breaks the
// tie.
func Retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := range retryAttempts {
		if err = fn(); !IsBusy(err) {
			return err
		}
		if attempt == retryAttempts-1 {
			break
		}
		// 25ms, 50ms, 100ms, each ±50% jitter.
		base := 25 * time.Millisecond << attempt
		delay := base/2 + time.Duration(rand.Int63n(int64(base)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}
