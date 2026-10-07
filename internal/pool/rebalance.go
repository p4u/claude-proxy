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
	rebalanceResetJitter = 2 * time.Second
	rebalanceDrainWait   = 2 * time.Second
)

// RequestOptions gates elective migration and identifies account-bound history.
// Emergency binding runs first, respecting AccountBound. ObserveOnly requests
// participate in leases but cannot announce or switch.
type RequestOptions struct {
	Rebalance   bool
	ObserveOnly bool
	// AccountBound permanently disables elective handoffs for this binding,
	// including later requests that omit account-scoped objects.
	AccountBound bool
}

type rebalancePlan struct {
	source, target string
	created        time.Time
	notified       bool
	conversation   string // hashed reference only; never a raw client-supplied key
	lastDeferred   time.Time
	done           chan struct{} // closed under p.mu when replaced, cancelled, or switched
	reason         string        // completion reason, also guarded by p.mu
}

type sessionState struct {
	inflight     int
	waiting      int
	drained      chan struct{}
	pending      *rebalancePlan
	lastCheck    time.Time
	lastUsed     time.Time
	drainWait    time.Duration // zero uses rebalanceDrainWait; test-only override
	accountBound bool          // sticky; bindLocked hydrates/persists the durable flag
}

// finishPlan wakes every waiter, even when the source still has live streams.
// The caller holds p.mu, including when inspecting the completed plan's reason.
func (p *Pool) finishPlan(s *sessionState, reason string) {
	if plan := s.pending; plan != nil {
		// A successful switch was audited in the pin-update transaction. Other
		// outcomes are nonblocking, best-effort observations, emitted once.
		if reason != "switched" {
			p.recordRouting(plan.routingEvent("cancelled", reason, p.now()))
		}
		plan.reason = reason
		close(plan.done)
		s.pending = nil
	}
}

func (plan *rebalancePlan) routingEvent(kind, reason string, now time.Time) store.RoutingEvent {
	return store.RoutingEvent{TS: now.Unix(), Policy: "rebalance", Mode: "live", Kind: kind, Reason: reason,
		Conversation: plan.conversation, SourceID: plan.source, TargetID: plan.target}
}

// Lease holds a credential for one entire upstream request, including streaming.
// Call Release on EVERY exit path. All requests sharing a Pool must acquire a
// lease for in-flight protection; leases do not coordinate separate processes.
type Lease struct {
	Credential      *creds.Credential
	IsNew           bool
	Rebalance       string // empty, "pending", "switched", "deferred", or "cancelled"
	RebalanceReason string // bounded, account-neutral diagnostic for deferred/cancelled

	pool  *Pool
	state *sessionState
	plan  *rebalancePlan
	once  sync.Once
}

// Release reports whether the pending notice was emitted on a completed,
// successful response without a detected upstream, parser, or client write
// failure. It is NOT a client acknowledgment: HTTP cannot prove the client read
// the headers. Idempotent.
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
// Once a notice has completed, new generation requests briefly wait for earlier
// leases to drain. The wait holds neither a mutex nor a transaction, and wakes on
// cancellation, notice expiry, or its bounded budget. A timed-out request keeps
// the usable current pin; it never interrupts or forcibly moves an old stream.
// Count-only requests never initiate or execute a migration.
func (p *Pool) AcquireScoped(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, opts RequestOptions) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := KeyScoped(convID, prov, scope)
	s := p.sessions[key]
	if s == nil {
		s = &sessionState{}
		p.sessions[key] = s
	}
	// Latch before binding so the emergency path can apply the same safety
	// policy. bindLocked may also restore this flag from durable storage.
	if opts.AccountBound {
		s.accountBound = true
	}
	if s.accountBound {
		p.finishPlan(s, "account-bound")
	}
	touch := true
	var waited *rebalancePlan
	var deadline time.Time
	budgetExpired := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, isNew, err := p.bindLocked(ctx, convID, prov, scope, allowed, touch)
		if err != nil {
			return nil, err
		}
		touch = false // a drain wait must not count the request twice
		now := p.now()
		s.lastUsed = now
		eligible := opts.Rebalance && !opts.ObserveOnly && !s.accountBound && provider.Get(prov).PollsUsage && scope == "" && allowed == nil
		switch {
		case s.accountBound:
			p.finishPlan(s, "account-bound")
		case isNew || s.pending != nil && s.pending.source != c.ID:
			p.finishPlan(s, "pin-changed")
		case !opts.Rebalance:
			p.finishPlan(s, "disabled")
		case c.Status != creds.StatusActive:
			p.finishPlan(s, "source-unusable")
		case s.pending != nil && now.Sub(s.pending.created) >= rebalanceNoticeTTL:
			p.finishPlan(s, "notice-expired")
		}
		l := &Lease{Credential: c, IsNew: isNew, pool: p, state: s}
		// A waiter belongs to one plan, never a replacement. Other waiters may
		// already have switched the pin; in that case just join the new pin.
		planEnded := waited != nil && s.pending != waited
		if planEnded && c.ID == waited.source {
			l.Rebalance, l.RebalanceReason = "cancelled", waited.reason
		}
		if waited != nil && !time.Now().Before(deadline) {
			budgetExpired = true
		}
		if eligible && !planEnded && !budgetExpired && s.pending != nil && s.pending.notified && s.inflight > 0 {
			if waited == nil {
				waited = s.pending
				budget := s.drainWait
				if budget <= 0 {
					budget = rebalanceDrainWait
				}
				deadline = time.Now().Add(budget)
			}
			reason := p.waitForDrain(ctx, s, waited, deadline, now)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if reason == "notice-expired" && s.pending == waited {
				p.finishPlan(s, reason)
			} else if reason == "drain-timeout" {
				budgetExpired = true
			}
			continue // re-run emergency failover and revalidate after every wake
		}
		if eligible && !planEnded && !isNew && c.Status == creds.StatusActive &&
			(s.pending != nil || now.Sub(s.lastCheck) >= rebalanceCheckEvery) {
			s.lastCheck = now
			var target *creds.Credential
			err := store.Retry(ctx, func() error {
				var err error
				target, err = p.rebalanceOnce(ctx, key, c, s.pending, now, !budgetExpired)
				return err
			})
			if err != nil {
				// An optional optimization must not turn a working pin into a 502.
				p.log.Warn("session rebalance check failed", "conv", key, "err", err)
				if s.pending != nil {
					l.Rebalance, l.RebalanceReason = "cancelled", "revalidation-failed"
				}
				p.finishPlan(s, "revalidation-failed")
			} else if target == nil {
				if s.pending != nil {
					l.Rebalance, l.RebalanceReason = "cancelled", "no-longer-eligible"
				}
				p.finishPlan(s, "no-longer-eligible")
			} else if s.pending != nil && s.pending.notified {
				if budgetExpired {
					l.Rebalance, l.RebalanceReason = "deferred", "drain-timeout"
					if s.pending.lastDeferred.IsZero() || now.Sub(s.pending.lastDeferred) >= time.Minute {
						p.recordRouting(s.pending.routingEvent("deferred", "drain-timeout", now))
						s.pending.lastDeferred = now
					}
				} else {
					l.Credential, l.IsNew, l.Rebalance = target, true, "switched"
					p.destinations[target.ID] = now
					p.finishPlan(s, "switched")
					p.log.Info("session rebalanced", "conv", key, "from", c.ID, "to", target.ID)
				}
			} else {
				if s.pending == nil {
					s.pending = &rebalancePlan{source: c.ID, target: target.ID, created: now,
						conversation: store.ConversationReference(key), done: make(chan struct{})}
					p.recordRouting(s.pending.routingEvent("pending", "usage-advantage", now))
					p.log.Info("session rebalance pending", "conv", key, "from", c.ID, "to", target.ID)
				}
				l.Rebalance, l.plan = "pending", s.pending
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.inflight == 0 {
			s.drained = make(chan struct{})
		}
		s.inflight++
		return l, nil
	}
}

// waitForDrain temporarily releases p.mu. One stopped timer covers both the
// total request wait budget (never reset by a wake) and the notice's remaining
// lifetime. No polling, background goroutine, or transaction spans this wait.
func (p *Pool) waitForDrain(ctx context.Context, s *sessionState, plan *rebalancePlan, deadline, now time.Time) string {
	delay, reason := time.Until(deadline), "drain-timeout"
	if ttl := plan.created.Add(rebalanceNoticeTTL).Sub(now); ttl <= delay {
		delay, reason = ttl, "notice-expired"
	}
	timer := time.NewTimer(max(0, delay))
	defer timer.Stop()
	drained := s.drained
	s.waiting++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		s.waiting--
	}()
	select {
	case <-ctx.Done():
		return ""
	case <-drained:
		return ""
	case <-plan.done:
		return ""
	case <-timer.C:
		return reason
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
		!sameRebalanceWindow(a.fhReset, b.fhReset, now.Unix()) ||
		!sameRebalanceWindow(a.sdReset, b.sdReset, now.Unix()) {
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
		if age < 0 || age > ageLimit || !(s.fh >= 0 && s.fh < 100 && s.sd >= 0 && s.sd < 100) {
			return false
		}
	}
	return true
}

// A known, still-future reset may move a couple of seconds between polls.
// Missing resets agree only with other missing resets; never hide a rollover,
// elapsed window, or invalid timestamp behind the jitter allowance.
func sameRebalanceWindow(a, b, now int64) bool {
	if a == 0 && b == 0 {
		return true
	}
	if a <= 0 || b <= 0 || a <= now || b <= now {
		return false
	}
	return max(a, b)-min(a, b) <= int64(rebalanceResetJitter/time.Second)
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
func (p *Pool) rebalanceOnce(ctx context.Context, key string, current *creds.Credential, plan *rebalancePlan, now time.Time, allowSwitch bool) (*creds.Credential, error) {
	// Discovery and timeout validation need no SQLite write lock. Only an
	// acknowledged plan with a drained source may change the pin, rechecking
	// everything inside an immediate transaction.
	var reader rebalanceReader = p.db
	var tx *sql.Tx
	if allowSwitch && plan != nil && plan.notified {
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
		if err := store.AppendRoutingEvent(ctx, tx, store.RoutingEvent{
			TS: now.Unix(), Policy: "rebalance", Mode: "live", Kind: "switched", Reason: "usage-advantage",
			Conversation: store.ConversationReference(key), SourceID: current.ID, TargetID: target.ID,
		}); err != nil {
			return nil, err // an unauditable elective move must roll back
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
		if s.inflight == 0 && s.waiting == 0 && now.Sub(s.lastUsed) > rebalancePreviousAge {
			p.finishPlan(s, "notice-expired")
			delete(p.sessions, key)
		}
	}
	for id, at := range p.destinations {
		if now.Sub(at) >= rebalanceDestination {
			delete(p.destinations, id)
		}
	}
}
