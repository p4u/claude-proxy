package pool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

// recordRouting never waits for SQLite while a request holds the pool mutex.
// This queue is observational, not a reservation or a handoff acknowledgement.
func (p *Pool) recordRouting(event store.RoutingEvent) {
	select {
	case p.routingEvents <- event:
	default:
		p.routingDropped.Add(1)
	}
}

// RoutingLoop persists lifecycle observations even with expiry forecasts off.
// Actual elective switch events are written transactionally in rebalanceOnce.
// Call exactly once per serving pool. Shutdown may lose queued observations,
// but cannot lose a committed switch event or affect routing.
func (p *Pool) RoutingLoop(ctx context.Context, expiryShadow bool) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	type lastObservation struct {
		reason string
		at     time.Time
	}
	last := make(map[string]lastObservation)
	persist := func(parent context.Context, e store.RoutingEvent) bool {
		writeCtx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
		defer cancel()
		if err := store.AppendRoutingEvent(writeCtx, p.db, e); err != nil {
			p.log.Warn("routing observation not recorded", "kind", e.Kind, "error", err)
			return false
		}
		return true
	}
	observe := func() {
		if ctx.Err() != nil {
			return
		}
		now := p.now()
		if n := p.routingDropped.Swap(0); n > 0 {
			p.log.Warn("routing observation queue overflow", "dropped", n)
			evidence, _ := json.Marshal(map[string]uint64{"dropped": n})
			persist(ctx, store.RoutingEvent{TS: now.Unix(), Policy: "rebalance", Mode: "live", Kind: "coverage_gap", Reason: "queue-overflow", Evidence: evidence})
		}
		if expiryShadow {
			events, err := p.ObserveExpiry(ctx)
			if err != nil {
				p.log.Warn("expiry shadow observation incomplete", "error", err)
			}
			// One batch deadline prevents shadow writes from monopolizing the
			// consumer for 64 separate busy timeouts. Live events stay queued.
			batchCtx, cancelBatch := context.WithTimeout(ctx, time.Second)
			seen := make(map[string]bool)
			for i, e := range events {
				if i >= 64 || batchCtx.Err() != nil {
					break
				}
				seen[e.TargetID] = true
				old := last[e.TargetID]
				// Reason changes are immediate; steady assessments are sampled at
				// most once per 15 minutes rather than filling history each tick.
				if old.reason == e.Reason && now.Sub(old.at) < 15*time.Minute {
					continue
				}
				if persist(batchCtx, e) {
					last[e.TargetID] = lastObservation{e.Reason, now}
				}
			}
			cancelBatch()
			for id := range last {
				if !seen[id] {
					delete(last, id)
				}
			}
		}
		purgeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if err := store.PurgeRoutingEvents(purgeCtx, p.db, now); err != nil {
			p.log.Warn("routing history retention failed", "error", err)
		}
	}
	observe()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-p.routingEvents:
			persist(ctx, event)
		case <-ticker.C:
			observe()
		}
	}
}
