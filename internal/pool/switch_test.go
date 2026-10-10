package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// switchFixture pins a young conversation "conv" to cs[0]. The pin is fresh,
// so no elective rebalance could ever move it: anything that does is the
// user-requested plan.
func switchFixture(t *testing.T) (*Pool, *store.DB, []*creds.Credential) {
	t.Helper()
	db, cs := setup(t)
	now := time.Now().Unix()
	execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES ('conv',?,?,?,?)`, cs[0].ID, now, now, now)
	return New(db), db, cs
}

func acquireSwitch(t *testing.T, p *Pool, opts RequestOptions) *Lease {
	t.Helper()
	l, err := p.AcquireScoped(context.Background(), "conv", provider.Anthropic, "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Release(false) })
	return l
}

func requestSwitch(t *testing.T, p *Pool, allowed []string) SwitchPlan {
	t.Helper()
	plan, err := p.RequestSwitch(context.Background(), "conv", provider.Anthropic, "", allowed)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func pinnedTo(t *testing.T, db *store.DB, key string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT credential_id FROM conversations WHERE id=?`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUserSwitchExcludesCurrentAndSwitchesOnNextRequest(t *testing.T) {
	p, db, cs := switchFixture(t)
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, cs[2].ID)

	plan := requestSwitch(t, p, nil)
	if plan.From.ID != cs[0].ID || plan.To == nil || plan.To.ID != cs[1].ID {
		t.Fatalf("plan = from %v to %v", plan.From, plan.To)
	}
	if got := pinnedTo(t, db, "conv"); got != cs[0].ID {
		t.Fatalf("request moved the pin by itself: %s", got)
	}
	// Elective rebalancing is off for this request; a user plan still runs.
	l := acquireSwitch(t, p, RequestOptions{})
	if l.Credential.ID != cs[1].ID || l.Rebalance != "switched" || l.RebalanceReason != "user" || !l.IsNew {
		t.Fatalf("lease = %+v", l)
	}
	l.Release(false)
	if got := pinnedTo(t, db, "conv"); got != cs[1].ID {
		t.Fatalf("pin = %s, want %s", got, cs[1].ID)
	}
	var reason string
	if err := db.QueryRow(`SELECT reason FROM routing_event WHERE kind='switched' AND source_id=? AND target_id=?`, cs[0].ID, cs[1].ID).Scan(&reason); err != nil || reason != "user" {
		t.Fatalf("switch not audited: reason=%q err=%v", reason, err)
	}
	next := acquireSwitch(t, p, RequestOptions{})
	if next.Credential.ID != cs[1].ID || next.Rebalance != "" {
		t.Fatalf("new pin not sticky: %+v", next)
	}
}

func TestUserSwitchRespectsEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		allowed     func(cs []*creds.Credential) []string
	}{
		{name: "others disabled", query: `UPDATE credentials SET status='disabled' WHERE label<>'A'`},
		{name: "others saturated", query: `INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct) SELECT id, strftime('%s','now'), 100, 10 FROM credentials WHERE label<>'A'`},
		{name: "others cooling down", query: `UPDATE credentials SET retry_after=strftime('%s','now')+3600 WHERE label<>'A'`},
		{name: "others limited", query: `UPDATE credentials SET status='limited' WHERE label<>'A'`},
		{name: "others another provider", query: `UPDATE credentials SET provider='glm' WHERE label<>'A'`},
		{name: "allowed set", allowed: func(cs []*creds.Credential) []string { return []string{cs[0].ID} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, db, cs := switchFixture(t)
			if tc.query != "" {
				execRebalance(t, db, tc.query)
			}
			var allowed []string
			if tc.allowed != nil {
				allowed = tc.allowed(cs)
			}
			plan, err := p.RequestSwitch(context.Background(), "conv", provider.Anthropic, "", allowed)
			if !errors.Is(err, ErrNoAlternative) {
				t.Fatalf("err = %v, plan = %+v", err, plan)
			}
			l := acquireSwitch(t, p, RequestOptions{})
			if l.Credential.ID != cs[0].ID || l.Rebalance != "" {
				t.Fatalf("refused switch still moved: %+v", l)
			}
		})
	}
}

func TestUserSwitchHonoursAllowedSet(t *testing.T) {
	p, _, cs := switchFixture(t)
	// C is healthy but outside the allowed set (a custom host that does not
	// serve the session's model): only B may be chosen.
	plan := requestSwitch(t, p, []string{cs[0].ID, cs[1].ID})
	if plan.To == nil || plan.To.ID != cs[1].ID {
		t.Fatalf("to = %+v", plan.To)
	}
}

func TestUserSwitchUnknownAndAccountBound(t *testing.T) {
	p, db, _ := switchFixture(t)
	if _, err := p.RequestSwitch(context.Background(), "nope", provider.Anthropic, "", nil); !errors.Is(err, ErrNotBound) {
		t.Fatalf("unknown conversation: err = %v", err)
	}
	if _, err := p.RequestSwitch(context.Background(), "conv", provider.GLM, "", nil); !errors.Is(err, ErrNotBound) {
		t.Fatalf("other provider's key: err = %v", err)
	}
	execRebalance(t, db, `UPDATE conversations SET account_bound=1 WHERE id='conv'`)
	if _, err := p.RequestSwitch(context.Background(), "conv", provider.Anthropic, "", nil); !errors.Is(err, ErrAccountBound) {
		t.Fatalf("account-bound: err = %v", err)
	}
}

func TestUserSwitchIsIdempotentWhilePending(t *testing.T) {
	p, _, cs := switchFixture(t)
	first := requestSwitch(t, p, nil)
	pending := p.sessions["conv"].pending
	for range 5 {
		again := requestSwitch(t, p, nil)
		if again.From.ID != cs[0].ID || again.To == nil || again.To.ID != first.To.ID {
			t.Fatalf("second request changed the plan: %v -> %v", first.To.ID, again.To)
		}
	}
	if p.sessions["conv"].pending != pending {
		t.Fatal("second request replaced the pending plan")
	}
	l := acquireSwitch(t, p, RequestOptions{})
	if l.Credential.ID != first.To.ID || l.Rebalance != "switched" {
		t.Fatalf("lease = %+v", l)
	}
}

func TestUserSwitchRepicksWhenTargetBecomesIneligible(t *testing.T) {
	p, db, cs := switchFixture(t)
	plan := requestSwitch(t, p, nil)
	other := cs[1]
	if plan.To.ID == cs[1].ID {
		other = cs[2]
	}
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, plan.To.ID)
	l := acquireSwitch(t, p, RequestOptions{})
	if l.Credential.ID != other.ID || l.Rebalance != "switched" {
		t.Fatalf("lease = %+v, want switch to %s", l, other.ID)
	}
}

func TestUserSwitchCancelledWhenNoAlternativeRemains(t *testing.T) {
	p, db, cs := switchFixture(t)
	requestSwitch(t, p, nil)
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE label<>'A'`)
	l := acquireSwitch(t, p, RequestOptions{})
	if l.Credential.ID != cs[0].ID || l.Rebalance != "cancelled" || l.RebalanceReason != "no-alternative" {
		t.Fatalf("lease = %+v", l)
	}
	if p.sessions["conv"].pending != nil {
		t.Fatal("plan survived cancellation")
	}
}

func TestUserSwitchObserveOnlyNeverExecutes(t *testing.T) {
	p, _, cs := switchFixture(t)
	plan := requestSwitch(t, p, nil)
	l := acquireSwitch(t, p, RequestOptions{ObserveOnly: true})
	if l.Credential.ID != cs[0].ID || l.Rebalance != "" {
		t.Fatalf("count-only request executed the switch: %+v", l)
	}
	l.Release(false)
	next := acquireSwitch(t, p, RequestOptions{})
	if next.Credential.ID != plan.To.ID || next.Rebalance != "switched" {
		t.Fatalf("lease = %+v", next)
	}
}

func TestUserSwitchDrainsInFlightRequests(t *testing.T) {
	p, _, cs := switchFixture(t)
	old := acquireSwitch(t, p, RequestOptions{})
	plan := requestSwitch(t, p, nil)

	got := make(chan *Lease, 1)
	go func() {
		l, err := p.AcquireScoped(context.Background(), "conv", provider.Anthropic, "", nil, RequestOptions{})
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	select {
	case l := <-got:
		t.Fatalf("switched while a request was in flight on the old credential: %+v", l)
	case <-time.After(100 * time.Millisecond):
	}
	if old.Credential.ID != cs[0].ID {
		t.Fatalf("in-flight lease moved: %+v", old)
	}
	old.Release(false)
	select {
	case l := <-got:
		defer l.Release(false)
		if l.Credential.ID != plan.To.ID || l.Rebalance != "switched" {
			t.Fatalf("lease after drain = %+v", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken by drain")
	}
}

func TestUserSwitchDefersOnDrainTimeout(t *testing.T) {
	p, _, cs := switchFixture(t)
	old := acquireSwitch(t, p, RequestOptions{})
	plan := requestSwitch(t, p, nil)
	p.mu.Lock()
	p.sessions["conv"].drainWait = time.Millisecond
	p.mu.Unlock()

	deferred := acquireSwitch(t, p, RequestOptions{})
	if deferred.Credential.ID != cs[0].ID || deferred.Rebalance != "deferred" {
		t.Fatalf("lease = %+v", deferred)
	}
	old.Release(false)
	deferred.Release(false)
	next := acquireSwitch(t, p, RequestOptions{})
	if next.Credential.ID != plan.To.ID || next.Rebalance != "switched" {
		t.Fatalf("lease = %+v", next)
	}
}

func TestUserSwitchSupersedesElectivePlan(t *testing.T) {
	p, _, cs, _ := rebalanceFixture(t)
	opts := RequestOptions{Rebalance: true}
	l := acquireRebalance(t, p, opts)
	if l.Rebalance != "pending" {
		t.Fatalf("elective notice missing: %+v", l)
	}
	l.Release(false)
	plan, err := p.RequestSwitch(context.Background(), "long", provider.Anthropic, "", nil)
	if err != nil || plan.To == nil || plan.To.ID != cs[1].ID {
		t.Fatalf("plan = %+v, err = %v", plan, err)
	}
	// The elective plan needed a completed notice first; the user plan does not.
	next := acquireRebalance(t, p, opts)
	if next.Credential.ID != cs[1].ID || next.Rebalance != "switched" || next.RebalanceReason != "user" {
		t.Fatalf("lease = %+v", next)
	}
}

func TestUserSwitchDefaultProvider(t *testing.T) {
	p, _, cs := switchFixture(t)
	execRebalance(t, p.db, `UPDATE credentials SET status='disabled' WHERE id=?`, cs[2].ID)
	if _, err := p.RequestSwitch(context.Background(), "conv", "", "", nil); err != nil {
		t.Fatal(err)
	}
	l, err := p.AcquireScoped(context.Background(), "conv", "", "", nil, RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release(false)
	if l.Credential.ID != cs[1].ID || l.Rebalance != "switched" {
		t.Fatalf("lease = %+v", l)
	}
}
