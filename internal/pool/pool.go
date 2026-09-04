package pool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
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
//
// A lock error here is the one database failure the proxy reports to the client
// as a 502, so it is retried rather than surfaced. p.mu only serialises this
// process; the request logger, the capture writers, the usage poller and any
// concurrently running CLI or TUI all write to the same file, so losing a race
// is normal and transient. bindOnce is a self-contained transaction that rolls
// back whole, which is what makes re-running it safe.
// The credential is chosen from prov's credentials only. Callers pass the
// provider that serves the request's model; see Key for how the two combine.
func (p *Pool) Bind(ctx context.Context, convID string, prov provider.ID) (*creds.Credential, bool, error) {
	return p.BindScoped(ctx, convID, prov, "", nil)
}

// BindScoped is Bind with two extra constraints, both needed by custom hosts:
//
//   - scope further qualifies the sticky key. Custom credentials each serve
//     their own model list, so a conversation that switches between two custom
//     models must not reuse a binding to a credential that cannot serve the
//     new one; scoping by model gives each its own stable pin, for the same
//     reason Key scopes by provider.
//   - allowed restricts the candidate set to specific credential IDs. Provider
//     alone is not a fine enough filter for custom hosts: two of them are both
//     provider "custom" while serving disjoint models, so selecting on
//     provider would hand a request to a credential that does not serve it.
//     nil means no constraint, which is every registry provider.
func (p *Pool) BindScoped(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string) (*creds.Credential, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if prov == "" {
		prov = provider.Default
	}
	var (
		c     *creds.Credential
		isNew bool
	)
	err := store.Retry(ctx, func() error {
		var err error
		c, isNew, err = p.bindOnce(ctx, convID, prov, scope, allowed)
		return err
	})
	return c, isNew, err
}

// Key returns the storage key for a conversation's sticky binding on a given
// provider.
//
// A single Claude Code session sends its main-model turns and its background
// haiku calls under the same derived conversation ID. Once those can land on
// different providers, one key per conversation is not enough: each request
// would find the other provider's credential pinned and migrate the binding,
// so the two would fight and neither would ever be sticky.
//
// Scoping the key by provider gives each its own stable pin. Anthropic keeps
// the bare ID so existing rows, existing bindings and existing dashboard links
// are untouched; other providers are prefixed.
func Key(convID string, prov provider.ID) string { return KeyScoped(convID, prov, "") }

// KeyScoped adds a further qualifier to the sticky key — see BindScoped.
func KeyScoped(convID string, prov provider.ID, scope string) string {
	if convID == "" {
		return convID
	}
	key := convID
	if prov != "" && prov != provider.Default {
		key = string(prov) + ":" + key
	}
	if scope != "" {
		key = scope + ":" + key
	}
	return key
}

func (p *Pool) bindOnce(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string) (*creds.Credential, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var credID string
	var newConv bool

	// Sticky bindings are per (conversation, provider) — see Key.
	convKey := KeyScoped(convID, prov, scope)

	err = tx.QueryRowContext(ctx, `SELECT credential_id FROM conversations WHERE id=?`, convKey).Scan(&credID)
	switch {
	case err == sql.ErrNoRows:
		newConv = true
		credID, err = p.pickActiveLocked(ctx, tx, prov, allowed)
		if err != nil {
			return nil, false, err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations (id, credential_id, created_at, last_seen_at, request_count, status)
			VALUES (?, ?, ?, ?, 1, 'active')`, convKey, credID, now, now); err != nil {
			return nil, false, err
		}
	case err != nil:
		return nil, false, err
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversations SET last_seen_at=?, request_count=request_count+1 WHERE id=?`,
			time.Now().Unix(), convKey); err != nil {
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
			newCredID, perr := p.pickActiveLocked(ctx, tx, prov, allowed)
			if perr != nil {
				if errors.Is(perr, ErrNoCredentials) {
					return c, false, ErrCredentialOrphaned
				}
				return nil, false, perr
			}
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE conversations SET credential_id=?, last_seen_at=? WHERE id=?`,
				newCredID, time.Now().Unix(), convKey); uerr != nil {
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
				newCredID, perr := p.pickActiveLocked(ctx, tx, prov, allowed)
				switch {
				case errors.Is(perr, ErrNoCredentials):
					// No healthy alternative — keep the sticky (saturated) pin
					// so the request still reaches Anthropic for a real 429.
				case perr != nil:
					return nil, false, perr
				case newCredID != credID:
					if _, uerr := tx.ExecContext(ctx,
						`UPDATE conversations SET credential_id=?, last_seen_at=? WHERE id=?`,
						newCredID, time.Now().Unix(), convKey); uerr != nil {
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
// Provider is a hard pre-filter, applied before any scoring: a request for a
// GLM model can only ever be served by a GLM credential, so an Anthropic
// subscription is not a fallback for it (it cannot serve that model at all) and
// vice versa. Weights and usage scores therefore only ever compare credentials
// within one provider.
func (p *Pool) pickActiveLocked(ctx context.Context, tx *sql.Tx, prov provider.ID, allowed []string) (string, error) {
	if prov == "" {
		prov = provider.Default
	}
	now := time.Now()
	inClause, inArgs := idFilter(allowed)
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.weight FROM credentials c
		WHERE c.status='active'
		  AND COALESCE(c.provider,'anthropic') = ?
		  AND (c.retry_after IS NULL OR c.retry_after < ?)`+inClause+`
		  AND NOT EXISTS (
		    SELECT 1 FROM usage_history u
		    WHERE u.credential_id = c.id
		      AND u.captured_at = (
		        SELECT MAX(captured_at) FROM usage_history WHERE credential_id = c.id
		      )
		      AND (u.five_hour_pct >= 100 OR u.seven_day_pct >= 100)
		  )
		ORDER BY c.id`, append([]any{string(prov), now.Unix()}, inArgs...)...)
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
		limitedIn, limitedArgs := idFilter(allowed)
		lrows, lerr := tx.QueryContext(ctx, `
			SELECT id, weight FROM credentials
			WHERE status='limited'
			  AND COALESCE(provider,'anthropic') = ?`+limitedIn+`
			ORDER BY COALESCE(retry_after, 0) ASC, id`,
			append([]any{string(prov)}, limitedArgs...)...)
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

// Window lengths for the two Anthropic quota windows the poller tracks. Used
// by Urgency to translate a resets_at instant into "remaining fraction of the
// window".
const (
	FiveHourWindow = 5 * time.Hour
	SevenDayWindow = 7 * 24 * time.Hour
)

// minRemainingFraction floors the remaining share of the 7-day window when
// computing Urgency, so the term stays finite as a reset approaches. One hour
// of a week: an unspent window in its final hour gets at most ~167× the score
// of one that is exactly on pace.
const minRemainingFraction = 1.0 / (7 * 24)

// Urgency is the 7-day "use it or lose it" term of the selection score:
//
//	urgency = max(0, room_7d / remaining_fraction_7d − 1)
//
// room_7d / remaining_fraction is the burn rate (in "whole windows per
// window") needed to consume the remaining weekly allowance before it resets;
// 1 is the even-pace baseline. So a credential that is on or ahead of pace
// contributes 0, and one that is behind grows hyperbolically as the reset
// nears: 0% used with 1h45m left is ~95, 0% used with 2.5 days left is
// ~1.8. That ordering is the point — quota in a window about to reset is
// free to spend, whereas a window with days left will still be there
// tomorrow.
//
// Only the 7-day window counts. Spending inside an about-to-reset 5-hour
// window still charges the weekly allowance, so it is not "free"; the 5-hour
// window is a ceiling (handled by room_5h) rather than an expiring budget.
//
// Zero when the reset time is unknown or in the past (the poller has not
// refreshed since the window rolled over, so the snapshot describes the
// previous window). Callers pass now separately so tests are deterministic
// and one scoring pass sees one clock.
func Urgency(sdPct float64, sdResetsUnix int64, now time.Time) float64 {
	if sdResetsUnix <= 0 {
		return 0
	}
	remaining := time.Unix(sdResetsUnix, 0).Sub(now)
	if remaining <= 0 || remaining >= SevenDayWindow {
		return 0
	}
	f := math.Max(float64(remaining)/float64(SevenDayWindow), minRemainingFraction)
	return math.Max(0, Room(sdPct)/f-1)
}

// EffectiveScore is what the picker actually weighs:
//
//	Score(weight, fh%, sd%) × (1 + Urgency(sd%, sd_resets_at, now))
//
// Shared by the pool, the web UI's selection view and the Codex rebalance
// loop so no surface can show a different number from the one used to route.
func EffectiveScore(weight int, fhPct, sdPct float64, sdResetsUnix int64, now time.Time) float64 {
	return Score(weight, fhPct, sdPct) * (1 + Urgency(sdPct, sdResetsUnix, now))
}

// weightedRandPick computes a usage-aware effective score for each candidate
// and returns one chosen by weighted-random selection.
//
// Score formula: weight × room_5h × room_7d^sevenDayExp × (1 + urgency),
// room = 1 − util. The two windows are independent ceilings, so their
// remaining room is multiplied rather than averaged: saturation on either
// window drives the score toward zero. Urgency (see Urgency) favours weekly
// windows about to reset with allowance unspent. See pickActiveLocked for
// the full rationale.
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
		boost  float64
		score  float64
	}

	now := time.Now()
	entries := make([]scored, len(candidates))
	bestWeight := 0
	bestIdx := 0
	for i, e := range candidates {
		s := scored{id: e.id, weight: e.weight, head: 1.0}

		var capturedAt int64
		var sdReset sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT five_hour_pct, seven_day_pct, captured_at, seven_day_resets_at
			FROM usage_history
			WHERE credential_id=?
			ORDER BY captured_at DESC LIMIT 1`, e.id).
			Scan(&s.fhPct, &s.sdPct, &capturedAt, &sdReset)

		if err == nil {
			// Always use the most recent snapshot, regardless of age.
			// Stale data beats assuming 0% usage: if a cred was at 80%
			// thirty minutes ago it is likely still near 80%, not 0%.
			//
			// Multiply the two windows' remaining room (independent ceilings)
			// and penalise low 7d room harder via the exponent.
			s.head = Room(s.fhPct) * math.Pow(Room(s.sdPct), SevenDayExp)
			s.boost = Urgency(s.sdPct, sdReset.Int64, now)
		}
		// err == sql.ErrNoRows → no snapshot yet; keep head=1.0, boost=0
		// (bootstrap). Only credentials with a real snapshot carry a reset
		// time, so unmeasurable providers (GLM, MiMo, custom) never get an
		// urgency boost — they compete on weight alone within their provider.

		// Same formula the web UI and the Codex rebalance loop use.
		s.score = EffectiveScore(e.weight, s.fhPct, s.sdPct, sdReset.Int64, now)
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
				"urgency", fmt.Sprintf("%.4f", s.boost),
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

// idFilter renders an optional "AND id IN (...)" restriction. Empty allowed
// means no restriction, which is the registry-provider path.
func idFilter(allowed []string) (string, []any) {
	if len(allowed) == 0 {
		return "", nil
	}
	ph := make([]string, len(allowed))
	args := make([]any, len(allowed))
	for i, id := range allowed {
		ph[i], args[i] = "?", id
	}
	return " AND c.id IN (" + strings.Join(ph, ",") + ")", args
}

// getCredTx reads one credential inside the caller's transaction.
//
// It deliberately reuses creds.SelectCols and creds.ScanCred rather than
// re-spelling the query: this function previously carried its own copy of the
// column list, which is exactly the kind of duplication that turns "add a
// column" into a silent scan mismatch here while every other read path works.
func getCredTx(ctx context.Context, tx *sql.Tx, id string) (*creds.Credential, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+creds.SelectCols+` FROM credentials WHERE id=?`, id)
	return creds.ScanCred(row)
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
