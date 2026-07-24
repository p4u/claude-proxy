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
	//                     for limited, or sends normally for active) UNLESS the
	//                     credential's latest usage snapshot is saturated
	//                     (≥100% on either window), in which case migrate the
	//                     conversation onto a fresh credential — the same ≥100%
	//                     cutoff pickActiveLocked applies to new bindings.
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
		case creds.StatusActive, creds.StatusLimited:
			saturated, serr := credSaturatedLocked(ctx, tx, credID)
			if serr != nil {
				return nil, false, serr
			}
			if saturated {
				newCredID, perr := p.pickActiveLocked(ctx, tx)
				switch {
				case errors.Is(perr, ErrNoCredentials):
					// No healthy alternative — keep the sticky (saturated) pin
					// so the request still reaches Anthropic for a real 429.
				case perr != nil:
					return nil, false, perr
				case newCredID != credID:
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
					// Surface the rebind as "new" so callers log the migration.
					return rebound, true, nil
				}
			}
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
//	score   = weight × headroom
//	headroom = room_5h × room_7d^sevenDayExp
//	room_X   = max(0, 1 − utilization_pct/100)
//
// The two windows are independent ceilings — a request is rejected the moment
// it hits EITHER limit — so their remaining room is multiplied, not averaged.
// Multiplying means a credential that is saturated on one window scores near
// zero even if the other window is wide open, which the old additive blend
// failed to capture (it would keep routing to a credential whose 7 d quota was
// already spent). Raising room_7d to a power >1 penalises consumption of the
// slow-resetting weekly quota harder than the cheap 5 h window.
//
// The most recent usage snapshot is always used regardless of age; headroom=1
// (full availability) is the fallback only when no snapshot exists at all
// (e.g. newly imported credentials). When all computed scores are zero, the
// credential with the highest configured weight is chosen so traffic always
// has somewhere to go.
//
// Hard saturation cutoff: a credential whose most recent snapshot reports
// EITHER window at ≥100 % utilization is excluded from the active set entirely,
// before scoring — a maxed-out subscription is never selected for a new
// conversation. Only the limited fallback below can still reach a saturated
// credential, and only as the last resort to obtain a real 429 + Retry-After.
func (p *Pool) pickActiveLocked(ctx context.Context, tx *sql.Tx) (string, error) {
	now := time.Now()
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.weight FROM credentials c
		WHERE c.status='active'
		  AND (c.retry_after IS NULL OR c.retry_after < ?)
		  AND NOT EXISTS (
		    SELECT 1 FROM usage_history u
		    WHERE u.credential_id = c.id
		      AND u.captured_at = (
		        SELECT MAX(captured_at) FROM usage_history WHERE credential_id = c.id
		      )
		      AND (u.five_hour_pct >= 100 OR u.seven_day_pct >= 100)
		  )
		ORDER BY c.id`, now.Unix())
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

// SevenDayExp controls how hard the slow-resetting 7-day quota is protected
// relative to the 5-hour window. >1 makes a low 7d room shrink the score
// faster, so the pool prefers to spend the cheap 5h window (which resets in
// hours) over the expensive weekly one (which resets slowly).
//
// Exported so the web UI can surface the exact selection score without
// re-deriving (and drifting from) the pool's formula.
const SevenDayExp = 1.5

// Room returns the remaining fraction (0..1) of a utilization window given its
// utilization percentage. room = max(0, 1 − pct/100).
func Room(pct float64) float64 {
	return math.Max(0, 1-pct/100)
}

// Score is the pool's usage-aware effective selection score for one credential:
//
//	score = weight × room_5h × room_7d^SevenDayExp
//
// It is the single source of truth for selection weighting, shared between the
// pool's picker and the web UI's usage view so the two can never diverge.
func Score(weight int, fhPct, sdPct float64) float64 {
	if weight < 1 {
		weight = 1
	}
	return float64(weight) * Room(fhPct) * math.Pow(Room(sdPct), SevenDayExp)
}

// Saturated reports whether a credential's latest snapshot maxes out EITHER
// window (≥100%). Saturated credentials are excluded from new bindings and
// trigger rebinding of existing ones.
func Saturated(fhPct, sdPct float64) bool {
	return fhPct >= 100 || sdPct >= 100
}

// weightedRandPick computes a usage-aware effective score for each candidate
// and returns one chosen by weighted-random selection.
//
// Score formula: weight × room_5h × room_7d^sevenDayExp   (room = 1 − util).
// The two windows are independent ceilings, so their remaining room is
// multiplied rather than averaged: saturation on either window drives the
// score toward zero. See pickActiveLocked for the full rationale.
//
// The most recent usage snapshot is used regardless of age. headroom=1.0 is
// used only when no snapshot exists for a credential (newly imported).
func (p *Pool) weightedRandPick(ctx context.Context, tx *sql.Tx, candidates []weightedEntry) (string, error) {
	type scored struct {
		id     string
		weight int
		fhPct  float64
		sdPct  float64
		head   float64
		score  float64
	}

	entries := make([]scored, len(candidates))
	bestWeight := 0
	bestIdx := 0
	for i, e := range candidates {
		s := scored{id: e.id, weight: e.weight, head: 1.0}

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
			//
			// Multiply the two windows' remaining room (independent ceilings)
			// and penalise low 7d room harder via the exponent.
			s.head = Room(s.fhPct) * math.Pow(Room(s.sdPct), SevenDayExp)
		}
		// err == sql.ErrNoRows → no snapshot yet; keep head=1.0 (bootstrap)

		s.score = Score(e.weight, s.fhPct, s.sdPct)
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
				"headroom", fmt.Sprintf("%.4f", s.head),
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

// credSaturatedLocked reports whether a credential's most recent usage snapshot
// maxes out either window (≥100%). No snapshot ⇒ not saturated (bootstrap).
func credSaturatedLocked(ctx context.Context, tx *sql.Tx, credID string) (bool, error) {
	var fhPct, sdPct float64
	err := tx.QueryRowContext(ctx, `
		SELECT five_hour_pct, seven_day_pct
		FROM usage_history
		WHERE credential_id=?
		ORDER BY captured_at DESC LIMIT 1`, credID).Scan(&fhPct, &sdPct)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return Saturated(fhPct, sdPct), nil
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
