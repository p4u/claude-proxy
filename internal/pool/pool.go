package pool

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

var (
	ErrNoCredentials      = errors.New("no active credentials in pool")
	ErrCredentialOrphaned = errors.New("conversation pinned to revoked/disabled credential")
)

type Pool struct {
	db *store.DB
	mu sync.Mutex // guards round-robin cursor + selection atomicity
}

func New(db *store.DB) *Pool { return &Pool{db: db} }

// Bind returns the credential to use for this conversation, creating the
// sticky binding on first sight. It also bumps last_seen_at + request_count.
func (p *Pool) Bind(ctx context.Context, convID string) (*creds.Credential, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var credID string
	var newConv bool

	err = tx.QueryRowContext(ctx, `SELECT credential_id FROM conversations WHERE id=?`, convID).Scan(&credID)
	switch {
	case err == sql.ErrNoRows:
		newConv = true
		credID, err = p.pickActiveLocked(ctx, tx)
		if err != nil {
			return nil, false, err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations (id, credential_id, created_at, last_seen_at, request_count, status)
			VALUES (?, ?, ?, ?, 1, 'active')`, convID, credID, now, now); err != nil {
			return nil, false, err
		}
	case err != nil:
		return nil, false, err
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversations SET last_seen_at=?, request_count=request_count+1 WHERE id=?`,
			time.Now().Unix(), convID); err != nil {
			return nil, false, err
		}
	}

	c, err := getCredTx(ctx, tx, credID)
	if err != nil {
		return nil, false, err
	}

	// Sticky semantics:
	//   active, limited → keep the existing pin (caller passes through 429
	//                     for limited, or sends normally for active).
	//   expired, revoked, disabled → permanent failure on this credential.
	//                                Auto-rebind to a healthy active cred so
	//                                the conversation can keep going.
	if !newConv {
		switch c.Status {
		case creds.StatusExpired, creds.StatusRevoked, creds.StatusDisabled:
			newCredID, perr := p.pickActiveLocked(ctx, tx)
			if perr != nil {
				if errors.Is(perr, ErrNoCredentials) {
					return c, false, ErrCredentialOrphaned
				}
				return nil, false, perr
			}
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE conversations SET credential_id=?, last_seen_at=? WHERE id=?`,
				newCredID, time.Now().Unix(), convID); uerr != nil {
				return nil, false, uerr
			}
			rebound, gerr := getCredTx(ctx, tx, newCredID)
			if gerr != nil {
				return nil, false, gerr
			}
			if err := tx.Commit(); err != nil {
				return nil, false, err
			}
			// Surface the rebind to the caller as "new" so it logs accordingly.
			return rebound, true, nil
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return c, newConv, nil
}

// pickActiveLocked returns the next credential ID for a new conversation,
// honoring per-credential weight via interleaved expansion: each round we
// take one slot from every credential that still has weight remaining. This
// gives a smoothly mixed sequence rather than long runs of the same id.
//
//	weights {A:5, B:5}      -> A B A B A B A B A B
//	weights {A:5, B:1}      -> A B A A A A
//	weights {A:5, B:1, C:2} -> A B C A C A A A
func (p *Pool) pickActiveLocked(ctx context.Context, tx *sql.Tx) (string, error) {
	now := time.Now().Unix()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, weight FROM credentials
		WHERE status='active'
		  AND (retry_after IS NULL OR retry_after < ?)
		ORDER BY id`, now)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var pool []weightedEntry
	for rows.Next() {
		var e weightedEntry
		if err := rows.Scan(&e.id, &e.weight); err != nil {
			return "", err
		}
		if e.weight < 1 {
			e.weight = 1
		}
		pool = append(pool, e)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	// No active credentials — fall back to limited ones so the request
	// reaches Anthropic and gets a real 429 (with Retry-After) instead
	// of a confusing 503 "no active credentials in pool".
	if len(pool) == 0 {
		lrows, lerr := tx.QueryContext(ctx, `
			SELECT id, weight FROM credentials
			WHERE status='limited'
			ORDER BY COALESCE(retry_after, 0) ASC, id`)
		if lerr != nil {
			return "", lerr
		}
		defer lrows.Close()
		for lrows.Next() {
			var e weightedEntry
			if err := lrows.Scan(&e.id, &e.weight); err != nil {
				return "", err
			}
			if e.weight < 1 {
				e.weight = 1
			}
			pool = append(pool, e)
		}
		if err := lrows.Err(); err != nil {
			return "", err
		}
	}

	if len(pool) == 0 {
		return "", ErrNoCredentials
	}

	slots := expandInterleaved(pool)

	var cursor int
	if err := tx.QueryRowContext(ctx, `SELECT idx FROM rr_cursor WHERE k=0`).Scan(&cursor); err != nil {
		return "", err
	}
	chosen := slots[cursor%len(slots)]
	next := (cursor + 1) % len(slots)
	if _, err := tx.ExecContext(ctx, `UPDATE rr_cursor SET idx=? WHERE k=0`, next); err != nil {
		return "", err
	}
	return chosen, nil
}

type weightedEntry struct {
	id     string
	weight int
}

func expandInterleaved(pool []weightedEntry) []string {
	total := 0
	for _, e := range pool {
		total += e.weight
	}
	slots := make([]string, 0, total)
	remaining := make([]int, len(pool))
	for i, e := range pool {
		remaining[i] = e.weight
	}
	for {
		progressed := false
		for i, e := range pool {
			if remaining[i] > 0 {
				slots = append(slots, e.id)
				remaining[i]--
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return slots
}

func getCredTx(ctx context.Context, tx *sql.Tx, id string) (*creds.Credential, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, COALESCE(label,''), COALESCE(subscription_type,''),
		       access_token, refresh_token, expires_at, status,
		       retry_after, last_success_at, last_429_at, last_request_at,
		       request_count, success_count, error_count, weight, created_at
		FROM credentials WHERE id=?`, id)
	c := &creds.Credential{}
	var exp, created int64
	var ra, ls, l429, lreq sql.NullInt64
	var status string
	if err := row.Scan(
		&c.ID, &c.Label, &c.SubscriptionType,
		&c.AccessToken, &c.RefreshToken, &exp, &status,
		&ra, &ls, &l429, &lreq,
		&c.RequestCount, &c.SuccessCount, &c.ErrorCount, &c.Weight, &created,
	); err != nil {
		return nil, err
	}
	c.ExpiresAt = time.Unix(exp, 0)
	c.Status = creds.Status(status)
	c.CreatedAt = time.Unix(created, 0)
	if ra.Valid {
		t := time.Unix(ra.Int64, 0)
		c.RetryAfter = &t
	}
	if ls.Valid {
		t := time.Unix(ls.Int64, 0)
		c.LastSuccessAt = &t
	}
	if l429.Valid {
		t := time.Unix(l429.Int64, 0)
		c.Last429At = &t
	}
	if lreq.Valid {
		t := time.Unix(lreq.Int64, 0)
		c.LastRequestAt = &t
	}
	return c, nil
}

// Janitor heals limited→active when retry_after passes, every 30s.
func (p *Pool) Janitor(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().Unix()
			_, _ = p.db.ExecContext(ctx, `
				UPDATE credentials
				SET status='active'
				WHERE status='limited' AND retry_after IS NOT NULL AND retry_after < ?`, now)
		}
	}
}
