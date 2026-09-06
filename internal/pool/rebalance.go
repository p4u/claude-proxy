package pool

import (
	"context"
	"database/sql"
	"math/rand"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

const (
	rebalanceDwell       = time.Hour
	rebalanceCheckEvery  = time.Minute
	rebalanceNoticeTTL   = 15 * time.Minute
	rebalanceDestination = 5 * time.Minute
	rebalanceLatestAge   = 15 * time.Minute
	rebalancePreviousAge = 30 * time.Minute
	rebalanceSampleGap   = 5 * time.Minute
)

// RequestOptions controls elective migration only. Emergency failover always
// runs first. ObserveOnly participates in leases but cannot announce or switch.
type RequestOptions struct {
	Rebalance   bool
	ObserveOnly bool
}

type rebalancePlan struct {
	source, target string
	created        time.Time
	notified       bool
}

type sessionState struct {
	inflight  int
	drained   chan struct{}
	pending   *rebalancePlan
	lastCheck time.Time
	lastUsed  time.Time
}

// Lease holds a credential for one entire upstream request, including streaming.
// Call Release on EVERY exit path. All requests sharing a Pool must acquire a
// lease for in-flight protection; leases do not coordinate separate processes.
type Lease struct {
	Credential *creds.Credential
	IsNew      bool
	Rebalance  string // empty, "pending", or "switched"

	pool  *Pool
	state *sessionState
	plan  *rebalancePlan
	once  sync.Once
}

// Release reports whether the pending notice was emitted on a successful
// response without a detected client write failure. It is NOT a client
// acknowledgment: HTTP cannot prove the client read the headers. Idempotent.
func (l *Lease) Release(notified bool) {
	l.once.Do(func() {
		p := l.pool
		p.mu.Lock()
		defer p.mu.Unlock()
		s := l.state
		if notified && l.plan != nil && s.pending == l.plan {
			s.pending.notified = true
		}
		s.inflight--
		s.lastUsed = p.now()
		if s.inflight == 0 {
			close(s.drained)
		}
	})
}

// AcquireScoped is BindScoped with request-lifetime tracking and optional
// announced rebalancing. Selection and lease registration share p.mu; registering
// after Bind would allow a switch in the gap before the request was counted.
//
// Once a notice has completed, new generation requests wait for earlier leases
// to drain. The wait is cancellable and holds neither a mutex nor a transaction.
// This prevents continuous overlapping generation requests from deferring a
// switch forever. Count-only requests never initiate or execute a migration.
func (p *Pool) AcquireScoped(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, opts RequestOptions) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := KeyScoped(convID, prov, scope)
	s := p.sessions[key]
	if s == nil {
		s = &sessionState{}
		p.sessions[key] = s
	}
	eligible := opts.Rebalance && !opts.ObserveOnly && provider.Get(prov).PollsUsage && scope == "" && allowed == nil
	touch := true
	for {
		c, isNew, err := p.bindLocked(ctx, convID, prov, scope, allowed, touch)
		if err != nil {
			return nil, err
		}
		touch = false // a drain wait must not count the request twice
		now := p.now()
		s.lastUsed = now
		if isNew || !opts.Rebalance || c.Status != creds.StatusActive ||
			(s.pending != nil && (s.pending.source != c.ID || now.Sub(s.pending.created) >= rebalanceNoticeTTL)) {
			s.pending = nil
		}
		if eligible && s.pending != nil && s.pending.notified && s.inflight > 0 {
			drained := s.drained
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				p.mu.Lock()
				return nil, ctx.Err()
			case <-drained:
				p.mu.Lock()
				continue // re-run emergency failover and revalidate after waiting
			}
		}
		l := &Lease{Credential: c, IsNew: isNew, pool: p, state: s}
		if eligible && !isNew && c.Status == creds.StatusActive &&
			(s.pending != nil || now.Sub(s.lastCheck) >= rebalanceCheckEvery) {
			s.lastCheck = now
			var target *creds.Credential
			err := store.Retry(ctx, func() error {
				var err error
				target, err = p.rebalanceOnce(ctx, key, c, s.pending, now)
				return err
			})
			if err != nil {
				// An optional optimization must not turn a working pin into a 502.
				p.log.Warn("session rebalance check failed", "conv", key, "err", err)
				s.pending = nil
			} else if target == nil {
				s.pending = nil
			} else if s.pending != nil && s.pending.notified {
				l.Credential, l.IsNew, l.Rebalance = target, true, "switched"
				p.destinations[target.ID] = now
				s.pending = nil
				p.log.Info("session rebalanced", "conv", key, "from", c.ID, "to", target.ID)
			} else {
				if s.pending == nil {
					s.pending = &rebalancePlan{source: c.ID, target: target.ID, created: now}
					p.log.Info("session rebalance pending", "conv", key, "from", c.ID, "to", target.ID)
				}
				l.Rebalance, l.plan = "pending", s.pending
			}
		}
		if s.inflight == 0 {
			s.drained = make(chan struct{})
		}
		s.inflight++
		return l, nil
	}
}

type rebalanceSample struct {
	captured, fhReset, sdReset int64
	fh, sd                     float64
}

type rebalanceCandidate struct {
	id, baseURL string
	weight      int
	samples     []rebalanceSample // newest first
}

func (c rebalanceCandidate) fresh(now time.Time) bool {
	if len(c.samples) != 2 {
		return false
	}
	a, b := c.samples[0], c.samples[1]
	if a.captured-b.captured < int64(rebalanceSampleGap/time.Second) ||
		a.fhReset != b.fhReset || a.sdReset != b.sdReset {
		return false
	}
	for i, s := range c.samples {
		ageLimit := rebalanceLatestAge
		if i == 1 {
			ageLimit = rebalancePreviousAge
		}
		age := now.Sub(time.Unix(s.captured, 0))
		// Comparisons also reject NaN/Inf; missing reset timestamps do not
		// invent urgency, while elapsed windows are never treated as empty.
		if age < 0 || age > ageLimit || !(s.fh >= 0 && s.fh < 100 && s.sd >= 0 && s.sd < 100) ||
			(s.fhReset > 0 && s.fhReset <= now.Unix()) || (s.sdReset > 0 && s.sdReset <= now.Unix()) {
			return false
		}
	}
	return true
}

func (c rebalanceCandidate) betterThan(source rebalanceCandidate, now time.Time) bool {
	for i, target := range c.samples {
		old := source.samples[i]
		if target.fh > 60 || target.sd > 60 {
			return false
		}
		// Confirm the advantage at both observations, not merely now when a
		// nearing reset could make one transient sample look attractive.
		at := time.Unix(min(target.captured, old.captured), 0)
		if !rebalanceAdvantage(source.weight, old, c.weight, target, at) ||
			!rebalanceAdvantage(source.weight, old, c.weight, target, now) {
			return false
		}
	}
	return true
}

func rebalanceAdvantage(oldWeight int, old rebalanceSample, weight int, target rebalanceSample, at time.Time) bool {
	oldScore := EffectiveScore(oldWeight, old.fh, old.sd, old.sdReset, at)
	newScore := EffectiveScore(weight, target.fh, target.sd, target.sdReset, at)
	roomGain := Score(1, target.fh, target.sd) >= 2*Score(1, old.fh, old.sd)
	urgencyGain := 1+Urgency(target.sd, target.sdReset, at) >= 4*(1+Urgency(old.sd, old.sdReset, at))
	return oldScore > 0 && newScore >= 4*oldScore && (roomGain || urgencyGain)
}

// rebalanceOnce revalidates an announced destination in the same short write
// transaction that changes the pin. Unlike normal new-binding selection, it has
// no limited-credential fallback and never bootstraps missing usage as 0%.
func (p *Pool) rebalanceOnce(ctx context.Context, key string, current *creds.Credential, plan *rebalancePlan, now time.Time) (*creds.Credential, error) {
	// Discovery only announces intent, so it needs no SQLite write lock. An
	// acknowledged plan rechecks everything inside an immediate transaction.
	var reader rebalanceReader = p.db
	var tx *sql.Tx
	if plan != nil && plan.notified {
		var err error
		tx, err = p.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		reader = tx
	}
	var stored string
	var bound int64
	if err := reader.QueryRowContext(ctx, `SELECT credential_id, CASE WHEN bound_at>0 THEN bound_at ELSE created_at END FROM conversations WHERE id=?`, key).Scan(&stored, &bound); err != nil {
		return nil, err
	}
	if stored != current.ID || now.Sub(time.Unix(bound, 0)) < rebalanceDwell {
		return nil, nil
	}
	candidates, err := rebalanceCandidates(ctx, reader, current.Provider, now)
	if err != nil {
		return nil, err
	}
	source, ok := candidates[current.ID]
	if !ok || !source.fresh(now) {
		return nil, nil
	}
	var selected string
	var total float64
	for id, candidate := range candidates {
		if id == current.ID || (plan != nil && id != plan.target) ||
			now.Sub(p.destinations[id]) < rebalanceDestination ||
			provider.ResolveBaseURL(current.Provider, candidate.baseURL) != provider.ResolveBaseURL(current.Provider, source.baseURL) ||
			!candidate.fresh(now) || !candidate.betterThan(source, now) {
			continue
		}
		s := candidate.samples[0]
		score := EffectiveScore(candidate.weight, s.fh, s.sd, s.sdReset, now)
		total += score
		if rand.Float64()*total < score {
			selected = id
		}
	}
	if selected == "" {
		return nil, nil
	}
	target, err := creds.ScanCred(reader.QueryRowContext(ctx, `SELECT `+creds.SelectCols+` FROM credentials WHERE id=?`, selected))
	if err != nil {
		return nil, err
	}
	if tx != nil {
		result, err := tx.ExecContext(ctx, `UPDATE conversations SET credential_id=?, bound_at=? WHERE id=? AND credential_id=?`, selected, now.Unix(), key, current.ID)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return target, nil
}

// Both *store.DB (notice discovery) and *sql.Tx (switch revalidation) provide
// these reads; only the latter needs to serialize with other SQLite writers.
type rebalanceReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func rebalanceCandidates(ctx context.Context, reader rebalanceReader, prov provider.ID, now time.Time) (map[string]rebalanceCandidate, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT c.id, c.base_url, c.weight, u.captured_at, u.five_hour_pct, u.seven_day_pct,
		       COALESCE(u.five_hour_resets_at,0), COALESCE(u.seven_day_resets_at,0)
		FROM credentials c JOIN usage_history u ON u.id IN (
		    SELECT h.id FROM usage_history h WHERE h.credential_id=c.id
		    ORDER BY h.captured_at DESC, h.id DESC LIMIT 2)
		WHERE c.provider=? AND c.status='active' AND c.expires_at>?
		  AND (c.retry_after IS NULL OR c.retry_after<=?)
		ORDER BY c.id, u.captured_at DESC, u.id DESC`, string(prov), now.Add(5*time.Minute).Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]rebalanceCandidate)
	for rows.Next() {
		var c rebalanceCandidate
		var s rebalanceSample
		if err := rows.Scan(&c.id, &c.baseURL, &c.weight, &s.captured, &s.fh, &s.sd, &s.fhReset, &s.sdReset); err != nil {
			return nil, err
		}
		c.samples = append(result[c.id].samples, s)
		result[c.id] = c
	}
	return result, rows.Err()
}

func (p *Pool) pruneRebalances(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, s := range p.sessions {
		if s.inflight == 0 && now.Sub(s.lastUsed) > rebalancePreviousAge {
			delete(p.sessions, key)
		}
	}
	for id, at := range p.destinations {
		if now.Sub(at) >= rebalanceDestination {
			delete(p.destinations, id)
		}
	}
}
