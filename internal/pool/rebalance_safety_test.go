package pool

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestRebalanceFreshResetJitter(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	base := rebalanceCandidate{samples: []rebalanceSample{
		{captured: now.Unix(), fhReset: now.Add(time.Hour).Unix(), sdReset: now.Add(72 * time.Hour).Unix(), fh: 20, sd: 20},
		{captured: now.Add(-10 * time.Minute).Unix(), fhReset: now.Add(time.Hour).Unix(), sdReset: now.Add(72 * time.Hour).Unix(), fh: 20, sd: 20},
	}}
	for _, window := range []string{"five-hour", "weekly"} {
		for _, delta := range []int64{-3, -2, -1, 0, 1, 2, 3, 3600} {
			t.Run(fmt.Sprintf("%s/%+ds", window, delta), func(t *testing.T) {
				c := base
				c.samples = append([]rebalanceSample(nil), base.samples...)
				if window == "five-hour" {
					c.samples[0].fhReset += delta
				} else {
					c.samples[0].sdReset += delta
				}
				want := delta >= -2 && delta <= 2
				if got := c.fresh(now); got != want {
					t.Fatalf("fresh=%v for reset jitter %ds, want %v", got, delta, want)
				}
			})
		}
	}
	for _, tc := range []struct {
		name string
		edit func([]rebalanceSample)
		want bool
	}{
		{"both resets absent", func(s []rebalanceSample) {
			for i := range s {
				s[i].fhReset, s[i].sdReset = 0, 0
			}
		}, true},
		{"five-hour unknown to known", func(s []rebalanceSample) { s[1].fhReset = 0 }, false},
		{"five-hour known to unknown", func(s []rebalanceSample) { s[0].fhReset = 0 }, false},
		{"weekly unknown to known", func(s []rebalanceSample) { s[1].sdReset = 0 }, false},
		{"weekly known to unknown", func(s []rebalanceSample) { s[0].sdReset = 0 }, false},
		{"negative reset", func(s []rebalanceSample) { s[0].fhReset, s[1].fhReset = -1, -1 }, false},
		{"latest reset elapsed within jitter", func(s []rebalanceSample) { s[0].fhReset, s[1].fhReset = now.Unix(), now.Unix()+2 }, false},
		{"previous reset elapsed within jitter", func(s []rebalanceSample) { s[0].sdReset, s[1].sdReset = now.Unix()+2, now.Unix() }, false},
		{"latest stale", func(s []rebalanceSample) {
			s[0].captured = now.Add(-16 * time.Minute).Unix()
			s[1].captured = now.Add(-25 * time.Minute).Unix()
		}, false},
		{"previous stale", func(s []rebalanceSample) { s[1].captured = now.Add(-31 * time.Minute).Unix() }, false},
		{"future capture", func(s []rebalanceSample) { s[0].captured++ }, false},
		{"sample gap too short", func(s []rebalanceSample) { s[1].captured = now.Add(-5*time.Minute).Unix() + 1 }, false},
		{"reversed samples", func(s []rebalanceSample) { s[0], s[1] = s[1], s[0] }, false},
		{"negative usage", func(s []rebalanceSample) { s[0].fh = -1 }, false},
		{"saturated usage", func(s []rebalanceSample) { s[1].sd = 100 }, false},
		{"NaN usage", func(s []rebalanceSample) { s[0].sd = math.NaN() }, false},
		{"infinite usage", func(s []rebalanceSample) { s[1].fh = math.Inf(1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.samples = append([]rebalanceSample(nil), base.samples...)
			tc.edit(c.samples)
			if got := c.fresh(now); got != tc.want {
				t.Fatalf("fresh=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestRebalanceSourceAndTargetResetJitter(t *testing.T) {
	for _, who := range []string{"source", "target", "both"} {
		for _, delta := range []int64{-2, 2} {
			t.Run(fmt.Sprintf("%s/%+ds", who, delta), func(t *testing.T) {
				p, db, cs, _ := rebalanceFixture(t)
				for i, c := range cs[:2] {
					if who == "source" && i != 0 || who == "target" && i != 1 {
						continue
					}
					execRebalance(t, db, `UPDATE usage_history SET five_hour_resets_at=five_hour_resets_at+?, seven_day_resets_at=seven_day_resets_at-? WHERE id=(SELECT MAX(id) FROM usage_history WHERE credential_id=?)`, delta, delta, c.ID)
				}
				notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
				if notice.Credential.ID != cs[0].ID || notice.Rebalance != "pending" {
					t.Fatalf("valid jitter prevented announcement: %+v", notice)
				}
				notice.Release(true)
				next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
				if next.Credential.ID != cs[1].ID || next.Rebalance != "switched" {
					t.Fatalf("valid jitter prevented switch: %+v", next)
				}
			})
		}
	}
}

// An elective wait has its own short deadline, independent of the request's
// context. A stream that outlasts it is not interrupted or forcibly migrated.
func TestRebalanceDrainBudgetKeepsOldStream(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, cancel := parkedDrainWaiter(t, p, db)
	defer cancel()
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(false)
	if l := res.lease; l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance != "deferred" || l.RebalanceReason != "drain-timeout" {
		t.Fatalf("bounded drain must defer on current pin: %+v", l)
	}
	if pin, count := longPin(t, db); pin != cs[0].ID || count != 3 {
		t.Fatalf("pin=%s count=%d, want original pin and exactly 3 requests", pin, count)
	}
	if inflight, _ := longSession(t, p); inflight != 2 {
		t.Fatalf("inflight=%d, want old stream plus deferred request", inflight)
	}
	if destinationCount(t, p) != 0 {
		t.Fatal("timed-out wait consumed destination cooldown")
	}
	res.lease.Release(false)
	observe.Release(false)
	next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if next.Credential.ID != cs[1].ID || next.Rebalance != "switched" {
		t.Fatalf("deferred plan did not remain usable after drain: %+v", next)
	}
}

// Turning rebalancing off cancels an already-waiting plan without waiting for
// its earlier leases. The cancellation must not resurrect a replacement plan
// in the same acquisition or interrupt the source stream.
func TestRebalancePlanCancellationWakesWaiter(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, cancel := parkedDrainWaiter(t, p, db)
	defer cancel()
	disabled := acquireRebalance(t, p, RequestOptions{})
	disabled.Release(false)
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(false)
	if l := res.lease; l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance != "cancelled" || l.RebalanceReason != "disabled" {
		t.Fatalf("cancelled waiter did not preserve current pin: %+v", l)
	}
	if inflight, pending := longSession(t, p); inflight != 2 || pending {
		t.Fatalf("inflight=%d pending=%v, want intact old stream and no plan", inflight, pending)
	}
	if pin, count := longPin(t, db); pin != cs[0].ID || count != 4 {
		t.Fatalf("pin=%s count=%d, want original pin and 4 requests", pin, count)
	}
	observe.Release(false)
}

func TestRebalanceInvalidTargetDoesNotHoldWaiter(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, cancel := parkedDrainWaiter(t, p, db)
	defer cancel()
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, cs[1].ID)
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(false)
	if l := res.lease; l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance != "cancelled" || l.RebalanceReason != "no-longer-eligible" {
		t.Fatalf("invalid target must cancel without waiting indefinitely: %+v", l)
	}
	if inflight, pending := longSession(t, p); inflight != 2 || pending {
		t.Fatalf("inflight=%d pending=%v, want two source leases and no plan", inflight, pending)
	}
	observe.Release(false)
}

func TestRebalanceNoticeExpiresDuringDrain(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	observe := announcedDrainSource(t, p)
	p.mu.Lock()
	s := p.sessions["long"]
	s.pending.created = now.Add(-rebalanceNoticeTTL + 100*time.Millisecond)
	s.drainWait = time.Hour // the notice timer, not the wait budget, must wake us
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := goAcquire(p, ctx, RequestOptions{Rebalance: true})
	awaitRequestCount(t, db, 3)
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(false)
	if l := res.lease; l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance != "cancelled" || l.RebalanceReason != "notice-expired" {
		t.Fatalf("expired waiter must keep current pin: %+v", l)
	}
	if inflight, pending := longSession(t, p); inflight != 2 || pending {
		t.Fatalf("inflight=%d pending=%v, want intact stream and no expired plan", inflight, pending)
	}
	if pin, count := longPin(t, db); pin != cs[0].ID || count != 3 {
		t.Fatalf("pin=%s count=%d, want original pin and 3 requests", pin, count)
	}
	observe.Release(false)
}

func TestRebalanceParallelTimeoutsKeepAllSourceLeases(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe := announcedDrainSource(t, p)
	p.mu.Lock()
	p.sessions["long"].drainWait = 250 * time.Millisecond
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const n = 5
	waiters := make([]<-chan acquireResult, n)
	for i := range waiters {
		waiters[i] = goAcquire(p, ctx, RequestOptions{Rebalance: true})
	}
	awaitRequestCount(t, db, n+2)
	for _, waiter := range waiters {
		res := awaitAcquire(t, waiter)
		if res.err != nil {
			t.Fatal(res.err)
		}
		t.Cleanup(func() { res.lease.Release(false) })
		if l := res.lease; l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance != "deferred" || l.RebalanceReason != "drain-timeout" {
			t.Fatalf("parallel timeout moved a live source stream: %+v", l)
		}
	}
	if inflight, pending := longSession(t, p); inflight != n+1 || !pending {
		t.Fatalf("inflight=%d pending=%v, want %d source leases and retained plan", inflight, pending, n+1)
	}
	if pin, count := longPin(t, db); pin != cs[0].ID || count != n+2 {
		t.Fatalf("pin=%s count=%d, want original pin and %d requests", pin, count, n+2)
	}
	if destinationCount(t, p) != 0 {
		t.Fatal("parallel timeouts consumed a destination cooldown")
	}
	observe.Release(false)
}

func TestRebalanceCancelledWaiterNeverAdoptsReplacement(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	observe, waiter, cancel := parkedDrainWaiter(t, p, db)
	defer cancel()
	// Install a new plan under the same mutex, guaranteeing the waiting
	// acquisition sees replacement rather than an intermediate empty state.
	p.mu.Lock()
	s := p.sessions["long"]
	p.finishPlan(s, "disabled")
	replacement := &rebalancePlan{source: cs[0].ID, target: cs[1].ID, created: now, done: make(chan struct{})}
	s.pending = replacement
	p.mu.Unlock()
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(true) // cannot acknowledge a notice this lease never carried
	if l := res.lease; l.Credential.ID != cs[0].ID || l.Rebalance != "cancelled" || l.RebalanceReason != "disabled" {
		t.Fatalf("waiter adopted replacement plan: %+v", l)
	}
	res.lease.Release(true)
	p.mu.Lock()
	replacementIntact := s.pending == replacement && !replacement.notified
	p.mu.Unlock()
	if !replacementIntact {
		t.Fatal("old waiter modified or acknowledged replacement plan")
	}
	observe.Release(false)
}

func TestRebalanceAccountBoundDisablesLaterThinHelpers(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	bound := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true, AccountBound: true})
	if bound.Credential.ID != cs[0].ID || bound.Rebalance != "" {
		t.Fatalf("bound request moved: %+v", bound)
	}
	bound.Release(false)
	// A later request contains no account-bound objects and would otherwise
	// qualify for migration. Eligibility must stay conservative for the session.
	for range 2 {
		thin := acquireRebalance(t, p, RequestOptions{Rebalance: true})
		if thin.Credential.ID != cs[0].ID || thin.Rebalance != "" {
			t.Fatalf("thin helper lost sticky safety: %+v", thin)
		}
		thin.Release(true)
	}
	if pin, count := longPin(t, db); pin != cs[0].ID || count != 3 {
		t.Fatalf("pin=%s count=%d", pin, count)
	}
}

func TestRebalanceAccountBoundCancelsParkedWaiter(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, cancel := parkedDrainWaiter(t, p, db)
	defer cancel()
	bound := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true, AccountBound: true})
	bound.Release(false)
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(false)
	if l := res.lease; l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance != "cancelled" || l.RebalanceReason != "account-bound" {
		t.Fatalf("account-bound cancellation did not wake waiter safely: %+v", l)
	}
	if inflight, pending := longSession(t, p); inflight != 2 || pending {
		t.Fatalf("inflight=%d pending=%v", inflight, pending)
	}
	res.lease.Release(false)
	observe.Release(false)
	thin := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if thin.Credential.ID != cs[0].ID || thin.Rebalance != "" {
		t.Fatalf("draining lost sticky account-bound flag: %+v", thin)
	}
}

func takeRoutingEvents(p *Pool) []store.RoutingEvent {
	var events []store.RoutingEvent
	for {
		select {
		case event := <-p.routingEvents:
			events = append(events, event)
		default:
			return events
		}
	}
}

func TestRebalanceLifecycleAuditDeduplicated(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	for range 3 {
		l := acquireRebalance(t, p, RequestOptions{Rebalance: true})
		if l.Rebalance != "pending" {
			t.Fatalf("expected pending notice: %+v", l)
		}
		l.Release(false)
	}
	events := takeRoutingEvents(p)
	if len(events) != 1 || events[0].Kind != "pending" {
		t.Fatalf("repeated headers must queue one pending event: %+v", events)
	}
	e := events[0]
	if e.Policy != "rebalance" || e.Mode != "live" || e.Reason != "usage-advantage" || e.Conversation != store.ConversationReference("long") || e.SourceID != cs[0].ID || e.TargetID != cs[1].ID || e.TS != now.Unix() {
		t.Fatalf("unexpected pending metadata: %+v", e)
	}
	for range 2 {
		l := acquireRebalance(t, p, RequestOptions{})
		l.Release(false)
	}
	events = takeRoutingEvents(p)
	if len(events) != 1 || events[0].Kind != "cancelled" || events[0].Reason != "disabled" {
		t.Fatalf("cancelled plan must queue exactly one event: %+v", events)
	}
}

func TestRebalanceDeferredAuditThrottled(t *testing.T) {
	p, _, _, now := rebalanceFixture(t)
	observe := announcedDrainSource(t, p)
	takeRoutingEvents(p)
	p.mu.Lock()
	p.sessions["long"].drainWait = time.Millisecond
	p.mu.Unlock()
	for range 3 {
		l := acquireRebalance(t, p, RequestOptions{Rebalance: true})
		if l.Rebalance != "deferred" {
			t.Fatalf("expected deferred: %+v", l)
		}
		l.Release(false)
	}
	events := takeRoutingEvents(p)
	if len(events) != 1 || events[0].Kind != "deferred" || events[0].Reason != "drain-timeout" {
		t.Fatalf("deferred events not throttled per plan: %+v", events)
	}
	p.now = func() time.Time { return now.Add(time.Minute) }
	l := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	l.Release(false)
	events = takeRoutingEvents(p)
	if len(events) != 1 || events[0].Kind != "deferred" || events[0].TS != now.Add(time.Minute).Unix() {
		t.Fatalf("deferred audit throttle did not reopen: %+v", events)
	}
	observe.Release(false)
}

func TestRebalanceAuditFullQueueNeverBlocks(t *testing.T) {
	p, _, cs, _ := rebalanceFixture(t)
	for len(p.routingEvents) < cap(p.routingEvents) {
		p.routingEvents <- store.RoutingEvent{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res := awaitAcquire(t, goAcquire(p, ctx, RequestOptions{Rebalance: true}))
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.lease.Release(false)
	if res.lease.Credential.ID != cs[0].ID || res.lease.Rebalance != "pending" {
		t.Fatalf("full observation queue changed live routing: %+v", res.lease)
	}
}

func TestRebalancePruneAuditsIdlePlanButKeepsWaiters(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("waiting=%v", waiting), func(t *testing.T) {
			p, _, _, now := rebalanceFixture(t)
			notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
			notice.Release(false)
			takeRoutingEvents(p)
			p.mu.Lock()
			s := p.sessions["long"]
			if waiting {
				s.waiting = 1
			}
			p.mu.Unlock()
			p.pruneRebalances(now.Add(rebalancePreviousAge + time.Minute))
			p.mu.Lock()
			retained := p.sessions["long"] == s
			p.mu.Unlock()
			events := takeRoutingEvents(p)
			if waiting {
				if !retained || len(events) != 0 {
					t.Fatalf("prune lost active waiter state: retained=%v events=%+v", retained, events)
				}
			} else if retained || len(events) != 1 || events[0].Kind != "cancelled" || events[0].Reason != "notice-expired" {
				t.Fatalf("idle plan must be cancelled and audited: retained=%v events=%+v", retained, events)
			}
		})
	}
}

func TestRebalanceSwitchAuditAtomic(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%v", reject), func(t *testing.T) {
			p, db, cs, now := rebalanceFixture(t)
			notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
			notice.Release(true)
			if reject {
				execRebalance(t, db, `CREATE TRIGGER reject_switch_audit BEFORE INSERT ON routing_event WHEN NEW.kind='switched' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
			}
			next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
			events, err := store.ListRoutingEvents(context.Background(), db, 0, 100, now)
			if err != nil {
				t.Fatal(err)
			}
			var switched []store.RoutingEvent
			for _, event := range events {
				if event.Kind == "switched" {
					switched = append(switched, event)
				}
			}
			var bound int64
			if err := db.QueryRow(`SELECT bound_at FROM conversations WHERE id='long'`).Scan(&bound); err != nil {
				t.Fatal(err)
			}
			if reject {
				if next.Credential.ID != cs[0].ID || next.IsNew || next.Rebalance != "cancelled" || next.RebalanceReason != "revalidation-failed" {
					t.Fatalf("audit failure must preserve current pin: %+v", next)
				}
				if pin, count := longPin(t, db); pin != cs[0].ID || count != 2 {
					t.Fatalf("failed audit changed pin/count: %s %d", pin, count)
				}
				if bound != now.Add(-2*time.Hour).Unix() || len(switched) != 0 || destinationCount(t, p) != 0 {
					t.Fatalf("audit failure left committed side effects: bound=%d events=%+v", bound, switched)
				}
				return
			}
			if next.Credential.ID != cs[1].ID || next.Rebalance != "switched" || len(switched) != 1 || bound != now.Unix() {
				t.Fatalf("switch and audit must commit together: lease=%+v bound=%d events=%+v", next, bound, switched)
			}
			e := switched[0]
			if e.Policy != "rebalance" || e.Mode != "live" || e.Reason != "usage-advantage" || e.Conversation != store.ConversationReference("long") || e.SourceID != cs[0].ID || e.TargetID != cs[1].ID || e.TS != now.Unix() {
				t.Fatalf("unexpected switch audit metadata: %+v", e)
			}
		})
	}
}

func TestRebalanceAlreadyCancelledContextHasNoLease(t *testing.T) {
	p, _, _, _ := rebalanceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := awaitAcquire(t, goAcquire(p, ctx, RequestOptions{Rebalance: true}))
	if res.err != context.Canceled || res.lease != nil {
		t.Fatalf("cancelled context acquired lease: %+v", res)
	}
	if inflight, _ := longSession(t, p); inflight != 0 {
		t.Fatalf("cancelled context leaked %d leases", inflight)
	}
}
