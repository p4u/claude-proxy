package pool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
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
	db  *store.DB
	log *slog.Logger
	mu  sync.Mutex // guards selection atomicity
}

func New(db *store.DB) *Pool {
	return &Pool{db: db, log: slog.Default()}
}

func NewWithLogger(db *store.DB, log *slog.Logger) *Pool {
	return &Pool{db: db, log: log}
}

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

// pickActiveLocked selects a credential ID for a new conversation using
// usage-aware weighted-random selection.
//
// Effective score per credential:
//
//	score = weight × blend²
//	blend = 0.6×room_5h + 0.4×room_7d
//	room_X = max(0, 1 − utilization_pct/100)
//
// The blend weights the short-term 5 h window more heavily (immediate
// capacity) while still accounting for long-term 7 d quota. Squaring the
// blend creates a convex penalty that strongly avoids near-saturated
// credentials without hard-excluding them until they actually hit 100 %.
//
// The most recent usage snapshot is always used regardless of age; blend=1
// (full availability) is the fallback only when no snapshot exists at all
// (e.g. newly imported credentials). When all computed scores are zero, the
// credential with the highest configured weight is chosen so traffic always
// has somewhere to go.
func (p *Pool) pickActiveLocked(ctx context.Context, tx *sql.Tx) (string, error) {
	now := time.Now()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, weight FROM credentials
		WHERE status='active'
		  AND (retry_after IS NULL OR retry_after < ?)
		ORDER BY id`, now.Unix())
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var candidates []weightedEntry
	for rows.Next() {
		var e weightedEntry
		if err := rows.Scan(&e.id, &e.weight); err != nil {
			return "", err
		}
		if e.weight < 1 {
			e.weight = 1
		}
		candidates = append(candidates, e)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	// No active credentials — fall back to limited ones so the request
	// reaches Anthropic and gets a real 429 (with Retry-After) instead
	// of a confusing 503 "no active credentials in pool".
	if len(candidates) == 0 {
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
			candidates = append(candidates, e)
		}
		if err := lrows.Err(); err != nil {
			return "", err
		}
	}

	if len(candidates) == 0 {
		return "", ErrNoCredentials
	}

	return p.weightedRandPick(ctx, tx, candidates)
}

type weightedEntry struct {
	id     string
	weight int
}

// weightedRandPick computes a usage-aware effective score for each candidate
// and returns one chosen by weighted-random selection.
//
// Score formula: weight × blend²   where blend = 0.6×room_5h + 0.4×room_7d
//
// The most recent usage snapshot is used regardless of age. blend=1.0 is
// used only when no snapshot exists for a credential (newly imported).
func (p *Pool) weightedRandPick(ctx context.Context, tx *sql.Tx, candidates []weightedEntry) (string, error) {
	type scored struct {
		id     string
		weight int
		fhPct  float64
		sdPct  float64
		blend  float64
		score  float64
	}

	entries := make([]scored, len(candidates))
	bestWeight := 0
	bestIdx := 0
	for i, e := range candidates {
		s := scored{id: e.id, weight: e.weight, blend: 1.0}

		var capturedAt int64
		err := tx.QueryRowContext(ctx, `
			SELECT five_hour_pct, seven_day_pct, captured_at
			FROM usage_history
			WHERE credential_id=?
			ORDER BY captured_at DESC LIMIT 1`, e.id).
			Scan(&s.fhPct, &s.sdPct, &capturedAt)

		if err == nil {
			// Always use the most recent snapshot, regardless of age.
			// Stale data beats assuming 0% usage: if a cred was at 80%
			// thirty minutes ago it is likely still near 80%, not 0%.
			roomFH := math.Max(0, 1-s.fhPct/100)
			roomSD := math.Max(0, 1-s.sdPct/100)
			s.blend = 0.6*roomFH + 0.4*roomSD
		}
		// err == sql.ErrNoRows → no snapshot yet; keep blend=1.0 (bootstrap)

		s.score = float64(e.weight) * s.blend * s.blend
		entries[i] = s

		if e.weight > bestWeight {
			bestWeight = e.weight
			bestIdx = i
		}
	}

	total := 0.0
	for _, s := range entries {
		total += s.score
	}

	// Log scores at debug level so operators can see why a cred was chosen.
	if p.log.Enabled(ctx, slog.LevelDebug) {
		for _, s := range entries {
			pct := 0.0
			if total > 0 {
				pct = s.score / total * 100
			}
			p.log.Debug(
				"pool score",
				"cred", s.id,
				"weight", s.weight,
				"fh_pct", s.fhPct,
				"7d_pct", s.sdPct,
				"blend", fmt.Sprintf("%.4f", s.blend),
				"score", fmt.Sprintf("%.4f", s.score),
				"select_pct", fmt.Sprintf("%.1f", pct),
			)
		}
	}

	if total <= 0 {
		// All credentials are at 100% on both windows. Pick highest weight
		// so traffic still has a destination (will likely get a real 429).
		return candidates[bestIdx].id, nil
	}

	r := rand.Float64() * total
	cumulative := 0.0
	for _, s := range entries {
		cumulative += s.score
		if r < cumulative {
			return s.id, nil
		}
	}
	return entries[len(entries)-1].id, nil
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
