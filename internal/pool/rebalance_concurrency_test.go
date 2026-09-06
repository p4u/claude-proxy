package pool

import (
	"context"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// Concurrency behaviour of AcquireScoped's drain wait. Once a pending notice
// has been acknowledged, a new generation request must wait for earlier leases
// to drain — without holding p.mu or a database transaction — re-run its
// binding with touch=false after the drain (no double count), and honour
// context cancellation without leaking a lease.
//
// Everything is deterministic: a waiter parks only after its first, counted
// bind has committed, so polling request_count plus a non-blocking channel
// check proves it is parked; every sleep below is a bounded timeout or poll
// guard, never a synchronization mechanism. p.now is only reassigned in tests
// that have no live goroutines, so the clock setter can never race a lease.

type acquireResult struct {
	lease *Lease
	err   error
}

// goAcquire runs AcquireScoped for the fixture's "long" conversation in the
// background. The buffered channel lets the goroutine exit even if the test
// stops caring about the result.
func goAcquire(p *Pool, ctx context.Context, opts RequestOptions) <-chan acquireResult {
	ch := make(chan acquireResult, 1)
	go func() {
		l, err := p.AcquireScoped(ctx, "long", provider.Anthropic, "", nil, opts)
		ch <- acquireResult{lease: l, err: err}
	}()
	return ch
}

// awaitRequestCount polls the committed request_count of the "long"
// conversation until it reaches want. Reaching it proves the background
// acquire has completed its first (counted) bind; the deadline keeps a broken
// implementation from hanging the test.
func awaitRequestCount(t *testing.T, db *store.DB, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got int64
		if err := db.QueryRow(`SELECT request_count FROM conversations WHERE id='long'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("request_count never reached %d", want)
}

func awaitAcquire(t *testing.T, acq <-chan acquireResult) acquireResult {
	t.Helper()
	select {
	case r := <-acq:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not complete within 5s")
		return acquireResult{}
	}
}

func assertStillWaiting(t *testing.T, acq <-chan acquireResult) {
	t.Helper()
	select {
	case r := <-acq:
		t.Fatalf("acquire returned while earlier leases were still draining: lease=%v err=%v", r.lease, r.err)
	default:
	}
}

// longSession samples the in-memory session under p.mu, so the race detector
// never sees unsynchronised access from the test side.
func longSession(t *testing.T, p *Pool) (inflight int, pendingNotified bool) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions["long"]
	if s == nil {
		return 0, false
	}
	return s.inflight, s.pending != nil && s.pending.notified
}

func destinationCount(t *testing.T, p *Pool) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.destinations)
}

func longPin(t *testing.T, db *store.DB) (pin string, count int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT credential_id, request_count FROM conversations WHERE id='long'`).Scan(&pin, &count); err != nil {
		t.Fatal(err)
	}
	return pin, count
}

// parkedDrainWaiter drives the fixture's conversation to the state the drain
// tests start from: an observe-only lease still open on the source, a normal
// lease that announced pending and acknowledged it (Release(true)), and one
// normal request parked in the drain wait. request_count sits at 3 — observe,
// notice, and the parked request's first, counted bind — and the returned
// waiter has been verified to be waiting, not finished.
func parkedDrainWaiter(t *testing.T, p *Pool, db *store.DB) (*Lease, <-chan acquireResult, context.CancelFunc) {
	t.Helper()
	observe := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true})
	if observe.Rebalance != "" {
		t.Fatalf("observe-only lease announced: %+v", observe)
	}
	if inflight, pendingNotified := longSession(t, p); inflight != 1 || pendingNotified {
		t.Fatalf("observe-only lease not tracked without a plan: inflight=%d pendingNotified=%v", inflight, pendingNotified)
	}
	notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if notice.Rebalance != "pending" {
		t.Fatalf("notice lease: %+v", notice)
	}
	notice.Release(true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	waiter := goAcquire(p, ctx, RequestOptions{Rebalance: true})
	awaitRequestCount(t, db, 3)
	assertStillWaiting(t, waiter)
	return observe, waiter, cancel
}

// Cancellation: a request parked in the drain wait must return
// context.Canceled with no lease, leak nothing, and leave the acknowledged
// plan intact. While it waits it must hold neither p.mu nor a write
// transaction, so an unrelated conversation can still bind.
func TestRebalanceDrainWaitCancellation(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, cancel := parkedDrainWaiter(t, p, db)

	bindDone := make(chan error, 1)
	go func() {
		_, _, err := p.Bind(context.Background(), "unrelated-conv", provider.Anthropic)
		bindDone <- err
	}()
	select {
	case err := <-bindDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bind blocked while a drain wait was parked; the wait must hold no mutex or transaction")
	}

	cancel()
	res := awaitAcquire(t, waiter)
	if res.err != context.Canceled {
		t.Fatalf("cancelled drain wait: err=%v lease=%v, want context.Canceled and no lease", res.err, res.lease)
	}
	if res.lease != nil {
		t.Fatalf("cancelled drain wait delivered a lease: %+v", res.lease)
	}
	if inflight, pendingNotified := longSession(t, p); inflight != 1 || !pendingNotified {
		t.Fatalf("after cancellation: inflight=%d pendingNotified=%v, want 1 and an intact acknowledged plan", inflight, pendingNotified)
	}
	if _, count := longPin(t, db); count != 3 {
		t.Fatalf("request_count=%d after cancellation, want 3 (nothing re-ran)", count)
	}

	// Releasing the last earlier lease afterwards still lets a fresh request
	// execute the switch.
	observe.Release(false)
	l := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if l.Credential.ID != cs[1].ID || l.Rebalance != "switched" || !l.IsNew {
		t.Fatalf("post-cancellation switch: %+v", l)
	}
}

// Drain then switch: releasing the last earlier lease wakes the parked
// request, which switches to the other credential; its post-drain binding
// re-run must not count the request twice.
func TestRebalanceDrainThenSwitchNoExtraCount(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, _ := parkedDrainWaiter(t, p, db)

	observe.Release(false)
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatalf("waiter after drain: %v", res.err)
	}
	l := res.lease
	if l.Credential.ID != cs[1].ID || !l.IsNew || l.Rebalance != "switched" {
		t.Fatalf("waiter did not switch after the drain: %+v", l)
	}
	if pin, count := longPin(t, db); pin != cs[1].ID || count != 3 {
		t.Fatalf("pin=%s request_count=%d, want the new credential and 3 (no double count)", pin, count)
	}
	l.Release(false)
}

// If the source credential dies while a request waits, the post-drain re-run
// must fail over through the emergency path: a plain rebind, no "switched"
// notice, and no elective destination recorded.
func TestRebalanceEmergencyDuringDrainWait(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, _ := parkedDrainWaiter(t, p, db)

	execRebalance(t, db, `UPDATE credentials SET status='revoked' WHERE id=?`, cs[0].ID)
	observe.Release(false)

	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatalf("waiter after emergency: %v", res.err)
	}
	l := res.lease
	if l.Credential.ID != cs[1].ID || !l.IsNew || l.Rebalance != "" {
		t.Fatalf("emergency during the drain did not fail over plainly: %+v", l)
	}
	if got := destinationCount(t, p); got != 0 {
		t.Fatalf("elective migration ran during an emergency: %d destinations recorded", got)
	}
	if _, count := longPin(t, db); count != 3 {
		t.Fatalf("request_count=%d, want 3 (no double count)", count)
	}
	l.Release(false)
}

// Several requests waiting at once: after the drain exactly one of them
// executes and reports the switch, all of them land on the new credential,
// each is counted once, and every lease releases cleanly.
func TestRebalanceParallelWaitersSingleSwitch(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true})
	notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if notice.Rebalance != "pending" {
		t.Fatalf("notice lease: %+v", notice)
	}
	notice.Release(true)

	const extra = 3
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	waiters := make([]<-chan acquireResult, extra)
	for i := range waiters {
		waiters[i] = goAcquire(p, ctx, RequestOptions{Rebalance: true})
	}
	awaitRequestCount(t, db, 2+extra)
	for _, w := range waiters {
		assertStillWaiting(t, w)
	}

	observe.Release(false)
	switched := 0
	for _, w := range waiters {
		res := awaitAcquire(t, w)
		if res.err != nil {
			t.Fatalf("parallel waiter: %v", res.err)
		}
		l := res.lease
		if l.Credential.ID != cs[1].ID {
			t.Fatalf("waiter landed on %s, want the new credential %s", l.Credential.ID, cs[1].ID)
		}
		switch l.Rebalance {
		case "switched":
			switched++
		case "":
		default:
			t.Fatalf("unexpected rebalance notice %q", l.Rebalance)
		}
		l.Release(false)
	}
	if switched != 1 {
		t.Fatalf("%d waiters reported switched, want exactly 1", switched)
	}
	if _, count := longPin(t, db); count != 2+extra {
		t.Fatalf("request_count=%d, want %d (each waited request counted once)", count, 2+extra)
	}
	if inflight, _ := longSession(t, p); inflight != 0 {
		t.Fatalf("inflight=%d after every lease was released, want 0", inflight)
	}
}

// A count-only request neither waits nor executes: while a message request is
// parked, an observe-only acquire is served on the spot by the still-pinned
// source and is tracked, and the parked waiter only proceeds once both
// earlier leases drain.
func TestRebalanceObserveOnlyBypassesWaitWithoutExecuting(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	observe, waiter, _ := parkedDrainWaiter(t, p, db)

	ob2 := goAcquire(p, context.Background(), RequestOptions{Rebalance: true, ObserveOnly: true})
	res2 := awaitAcquire(t, ob2)
	if res2.err != nil {
		t.Fatal(res2.err)
	}
	l := res2.lease
	if l.Credential.ID != cs[0].ID || l.Rebalance != "" {
		t.Fatalf("observe-only request waited or executed: %+v", l)
	}
	if inflight, _ := longSession(t, p); inflight != 2 {
		t.Fatalf("observe-only request not tracked: inflight=%d, want 2", inflight)
	}
	assertStillWaiting(t, waiter)

	l.Release(false)
	observe.Release(false)
	res := awaitAcquire(t, waiter)
	if res.err != nil {
		t.Fatalf("waiter: %v", res.err)
	}
	if res.lease.Credential.ID != cs[1].ID || res.lease.Rebalance != "switched" {
		t.Fatalf("waiter did not switch after the observe-only leases drained: %+v", res.lease)
	}
	res.lease.Release(false)
}

// A pending notice expires after its TTL: the next request must re-announce
// (not switch unannounced), and the re-announced notice still switches. The
// clock is advanced by exactly the TTL, which also leaves the latest usage
// sample exactly at its freshness limit, so the fixture stays valid.
func TestRebalancePendingExpiryReannounces(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	opts := RequestOptions{Rebalance: true}
	notice := acquireRebalance(t, p, opts)
	if notice.Rebalance != "pending" {
		t.Fatalf("notice lease: %+v", notice)
	}
	notice.Release(true)

	p.now = func() time.Time { return now.Add(rebalanceNoticeTTL) }
	l := acquireRebalance(t, p, opts)
	if l.Credential.ID != cs[0].ID || l.Rebalance != "pending" || l.IsNew {
		t.Fatalf("expired notice not re-announced: %+v", l)
	}
	l.Release(true)
	next := acquireRebalance(t, p, opts)
	if next.Credential.ID != cs[1].ID || next.Rebalance != "switched" || !next.IsNew {
		t.Fatalf("re-announced notice did not switch: %+v", next)
	}
}

// A credential that just received a conversation is throttled as a
// destination: a second long-pinned conversation must not migrate to it inside
// the cooldown, and must announce normally once the cooldown passes.
func TestRebalanceDestinationThrottle(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	opts := RequestOptions{Rebalance: true}
	notice := acquireRebalance(t, p, opts)
	notice.Release(true)
	sw := acquireRebalance(t, p, opts)
	if sw.Credential.ID != cs[1].ID || sw.Rebalance != "switched" {
		t.Fatalf("initial switch: %+v", sw)
	}
	sw.Release(false)

	execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES ('long2',?,?,?,?)`,
		cs[0].ID, now.Add(-2*time.Hour).Unix(), now.Unix(), now.Add(-2*time.Hour).Unix())

	l2, err := p.AcquireScoped(context.Background(), "long2", provider.Anthropic, "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Credential.ID != cs[0].ID || l2.Rebalance != "" {
		t.Fatalf("destination cooldown did not throttle a second migration: %+v", l2)
	}
	l2.Release(false)

	p.now = func() time.Time { return now.Add(rebalanceDestination + time.Minute) }
	l2b, err := p.AcquireScoped(context.Background(), "long2", provider.Anthropic, "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l2b.Release(false) })
	if l2b.Credential.ID != cs[0].ID || l2b.Rebalance != "pending" {
		t.Fatalf("throttle never lifted: %+v", l2b)
	}
}
