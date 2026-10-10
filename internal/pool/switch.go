package pool

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

var (
	// ErrNotBound means the conversation has no sticky binding (never seen,
	// or its credential was deleted).
	ErrNotBound = errors.New("conversation has no credential binding")
	// ErrNoAlternative means the picker has no other eligible credential for
	// the conversation's provider and allowed set.
	ErrNoAlternative = errors.New("no alternative credential for conversation")
	// ErrAccountBound means the conversation uses account-scoped resources
	// (files, containers, server tools) and must keep its account.
	ErrAccountBound = errors.New("conversation is bound to its account")
)

// SwitchPlan describes a scheduled user-requested switch. From is the current
// pin; To is the credential the plan will move to, nil when it can no longer
// be determined (deleted since the plan was made).
type SwitchPlan struct {
	From, To *creds.Credential
}

// RequestSwitch schedules a user-requested move of a conversation's binding to
// another credential, chosen by the normal picker (same provider, allowed set,
// health and saturation rules) with the current pin excluded.
//
// Nothing moves here. The plan is executed by the conversation's next
// generation request through AcquireScoped, after in-flight requests on the
// old credential drain, exactly like an acknowledged elective plan; that lease
// reports Rebalance "switched". While a user plan is pending, repeated calls
// return it unchanged. A pending elective plan is superseded.
func (p *Pool) RequestSwitch(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string) (SwitchPlan, error) {
	if prov == "" {
		prov = provider.Default
	}
	key := KeyScoped(convID, prov, scope)
	if key == "" {
		return SwitchPlan{}, ErrNotBound
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var credID string
	var accountBound bool
	err := p.db.QueryRowContext(ctx, `SELECT credential_id, account_bound FROM conversations WHERE id=?`, key).Scan(&credID, &accountBound)
	if errors.Is(err, sql.ErrNoRows) {
		return SwitchPlan{}, ErrNotBound
	}
	if err != nil {
		return SwitchPlan{}, err
	}
	from, err := creds.Get(ctx, p.db, credID)
	if errors.Is(err, creds.ErrNotFound) {
		return SwitchPlan{}, ErrNotBound
	}
	if err != nil {
		return SwitchPlan{}, err
	}
	s := p.sessions[key]
	if s == nil {
		s = &sessionState{}
		p.sessions[key] = s
	}
	if accountBound || s.accountBound {
		return SwitchPlan{From: from}, ErrAccountBound
	}
	now := p.now()
	s.lastUsed = now // keep the plan across janitor sweeps until the next request
	if plan := s.pending; plan != nil && plan.user && plan.source == from.ID {
		to, err := creds.Get(ctx, p.db, plan.target)
		if err != nil && !errors.Is(err, creds.ErrNotFound) {
			return SwitchPlan{}, err
		}
		return SwitchPlan{From: from, To: to}, nil
	}

	candidates, err := activeCandidates(ctx, p.db, prov, allowed, from.ID, time.Now())
	if err != nil {
		return SwitchPlan{}, err
	}
	if len(candidates) == 0 {
		return SwitchPlan{From: from}, ErrNoAlternative
	}
	targetID, err := p.weightedRandPick(ctx, p.db, candidates)
	if err != nil {
		return SwitchPlan{}, err
	}
	to, err := creds.Get(ctx, p.db, targetID)
	if err != nil {
		return SwitchPlan{}, err
	}
	p.finishPlan(s, "user-requested")
	s.pending = &rebalancePlan{source: from.ID, target: to.ID, created: now, notified: true, user: true,
		conversation: store.ConversationReference(key), done: make(chan struct{})}
	p.recordRouting(s.pending.routingEvent("pending", "user", now))
	p.log.Info("session switch requested", "conv", key, "provider", string(prov), "from", from.ID, "to", to.ID)
	return SwitchPlan{From: from, To: to}, nil
}

// executeUserSwitch runs the pending user plan for lease l under p.mu, after
// AcquireScoped's drain wait. A drain timeout defers the move to a later
// request; the source keeps serving and no in-flight request is touched.
func (p *Pool) executeUserSwitch(ctx context.Context, l *Lease, s *sessionState, key string, c *creds.Credential, prov provider.ID, allowed []string, budgetExpired bool, now time.Time) {
	plan := s.pending
	if budgetExpired {
		l.Rebalance, l.RebalanceReason = "deferred", "drain-timeout"
		if plan.lastDeferred.IsZero() || now.Sub(plan.lastDeferred) >= time.Minute {
			p.recordRouting(plan.routingEvent("deferred", "drain-timeout", now))
			plan.lastDeferred = now
		}
		return
	}
	var target *creds.Credential
	err := store.Retry(ctx, func() error {
		var err error
		target, err = p.userSwitchOnce(ctx, key, c, plan, prov, allowed, now)
		return err
	})
	switch {
	case err != nil:
		p.log.Warn("session switch failed", "conv", key, "err", err)
		l.Rebalance, l.RebalanceReason = "cancelled", "revalidation-failed"
		p.finishPlan(s, "revalidation-failed")
	case target == nil:
		l.Rebalance, l.RebalanceReason = "cancelled", "no-alternative"
		p.finishPlan(s, "no-alternative")
	default:
		l.Credential, l.IsNew, l.Rebalance, l.RebalanceReason = target, true, "switched", "user"
		p.finishPlan(s, "switched")
		p.log.Info("session switched on user request", "conv", key, "from", c.ID, "to", target.ID)
	}
}

// userSwitchOnce moves the pin in one immediate transaction. The pin must
// still be the plan's source and not account-bound. The planned target is
// kept while the picker would still accept it; otherwise the picker chooses
// again among the remaining eligible credentials. nil means none is left.
func (p *Pool) userSwitchOnce(ctx context.Context, key string, current *creds.Credential, plan *rebalancePlan, prov provider.ID, allowed []string, now time.Time) (*creds.Credential, error) {
	if prov == "" {
		prov = provider.Default
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var stored string
	var accountBound bool
	if err := tx.QueryRowContext(ctx, `SELECT credential_id, account_bound FROM conversations WHERE id=?`, key).Scan(&stored, &accountBound); err != nil {
		return nil, err
	}
	if stored != current.ID || accountBound {
		return nil, nil
	}
	candidates, err := activeCandidates(ctx, tx, prov, allowed, current.ID, time.Now())
	if err != nil {
		return nil, err
	}
	var selected string
	for _, c := range candidates {
		if c.id == plan.target {
			selected = c.id
		}
	}
	if selected == "" && len(candidates) > 0 {
		if selected, err = p.weightedRandPick(ctx, tx, candidates); err != nil {
			return nil, err
		}
	}
	if selected == "" {
		return nil, nil
	}
	target, err := getCredTx(ctx, tx, selected)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversations SET credential_id=?, bound_at=? WHERE id=? AND credential_id=?`, selected, now.Unix(), key, current.ID)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return nil, err
	}
	if err := store.AppendRoutingEvent(ctx, tx, store.RoutingEvent{
		TS: now.Unix(), Policy: "rebalance", Mode: "live", Kind: "switched", Reason: "user",
		Conversation: store.ConversationReference(key), SourceID: current.ID, TargetID: target.ID,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return target, nil
}
