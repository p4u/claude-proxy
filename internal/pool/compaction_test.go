package pool

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

func compactionDigest(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func summaryOptions(request string) RequestOptions {
	return RequestOptions{Rebalance: true, Compaction: CompactionRequest{
		Phase: CompactionSummary, Request: compactionDigest(request), Prefix: compactionDigest("original prefix"),
	}}
}

func summaryProof(s string) SummaryProof {
	return SummaryProof{Digest: compactionDigest(s), Bytes: len(s)}
}

func boundaryOptions(request string, proof SummaryProof) RequestOptions {
	phase := CompactionOrdinary
	if proof == (SummaryProof{}) {
		phase = CompactionBoundary
	}
	return RequestOptions{Rebalance: true, Compaction: CompactionRequest{
		Phase: phase, Request: compactionDigest(request), Prefix: compactionDigest("compacted prefix"), Adoption: proof,
	}}
}

func assertCompactionPin(t *testing.T, l *Lease, id string, switched bool) {
	t.Helper()
	if l.Credential.ID != id || (l.Rebalance == "switched") != switched || l.IsNew != switched {
		t.Fatalf("unexpected compaction lease: credential=%s new=%v rebalance=%q reason=%q", l.Credential.ID, l.IsNew, l.Rebalance, l.RebalanceReason)
	}
}

func TestCompactionSummaryAfterNoticeNeverWaitsOrSwitches(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprintf("stable=%v", stable), func(t *testing.T) {
			p, db, cs, _ := rebalanceFixture(t)
			old := announcedDrainSource(t, p)
			opts := summaryOptions("summary")
			if !stable {
				opts.Compaction.Request, opts.Compaction.Prefix = [32]byte{}, [32]byte{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			result := awaitAcquire(t, goAcquire(p, ctx, opts))
			if result.err != nil {
				t.Fatalf("summary waited behind acknowledged notice: %v", result.err)
			}
			assertCompactionPin(t, result.lease, cs[0].ID, false)
			if pin, _ := longPin(t, db); pin != cs[0].ID {
				t.Fatal("prepare-only revalidation changed the durable pin")
			}
			result.lease.ReleaseSummary(true, summaryProof("completed summary"))
			old.Release(false)
		})
	}
}

func TestCompactionSummaryProofBoundarySwitchesAtomically(t *testing.T) {
	for _, header := range []bool{false, true} {
		t.Run(fmt.Sprintf("header=%v", header), func(t *testing.T) {
			p, db, cs, _ := rebalanceFixture(t)
			proof := summaryProof("completed summary")
			first := acquireRebalance(t, p, summaryOptions("summary"))
			assertCompactionPin(t, first, cs[0].ID, false)
			if first.Rebalance != "pending" {
				t.Fatalf("summary did not prepare and announce: %+v", first)
			}
			first.ReleaseSummary(true, proof)
			if header {
				proof = SummaryProof{}
			}
			next := acquireRebalance(t, p, boundaryOptions("boundary", proof))
			assertCompactionPin(t, next, cs[1].ID, true)
			next.Release(false)
			var reason string
			if err := db.QueryRow(`SELECT reason FROM routing_event WHERE kind='switched'`).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			want := "compaction-inferred"
			if header {
				want = "compaction-header"
			}
			if reason != want {
				t.Fatalf("switch reason=%q, want %q", reason, want)
			}
			if pin, count := longPin(t, db); pin != cs[1].ID || count != 2 {
				t.Fatalf("pin=%s count=%d", pin, count)
			}
		})
	}
}

func TestCompactionGraceFixedAndOrdinaryFallback(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	first := acquireRebalance(t, p, summaryOptions("summary"))
	first.ReleaseSummary(true, summaryProof("completed summary"))
	for _, advance := range []time.Duration{30 * time.Second, 59 * time.Second} {
		p.now = func() time.Time { return now.Add(advance) }
		retry := acquireRebalance(t, p, summaryOptions("summary"))
		assertCompactionPin(t, retry, cs[0].ID, false)
		retry.ReleaseSummary(true, summaryProof("retry summary"))
		ordinary := acquireRebalance(t, p, RequestOptions{Rebalance: true})
		assertCompactionPin(t, ordinary, cs[0].ID, false)
		ordinary.Release(false)
	}
	p.now = func() time.Time { return now.Add(time.Minute) }
	ordinary := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	assertCompactionPin(t, ordinary, cs[1].ID, true)
}

func TestCompactionInflightSummaryFreezesPastGrace(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	notice.Release(true)
	summary := acquireRebalance(t, p, summaryOptions("summary"))
	p.now = func() time.Time { return now.Add(2 * time.Minute) }
	for _, opts := range []RequestOptions{{Rebalance: true}, boundaryOptions("boundary", SummaryProof{})} {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		result := awaitAcquire(t, goAcquire(p, ctx, opts))
		cancel()
		if result.err != nil {
			t.Fatalf("request waited on recognized summary: %v", result.err)
		}
		assertCompactionPin(t, result.lease, cs[0].ID, false)
		result.lease.Release(false)
	}
	summary.ReleaseSummary(true, summaryProof("completed summary"))
	next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	assertCompactionPin(t, next, cs[1].ID, true)
}

func TestCompactionInferenceRequiresSuccessfulMatchingCurrentProof(t *testing.T) {
	for _, kind := range []string{"failed", "zero", "digest", "length", "same-prefix", "zero-prefix", "unstable", "new-generation", "stale"} {
		t.Run(kind, func(t *testing.T) {
			p, _, cs, now := rebalanceFixture(t)
			proof := summaryProof("completed summary")
			summary := acquireRebalance(t, p, summaryOptions("summary"))
			notified, completed := true, proof
			if kind == "failed" {
				notified, completed = false, SummaryProof{}
			}
			if kind == "zero" {
				completed = SummaryProof{}
			}
			summary.ReleaseSummary(notified, completed)
			opts := boundaryOptions("boundary", proof)
			switch kind {
			case "digest":
				opts.Compaction.Adoption.Digest = compactionDigest("other")
			case "length":
				opts.Compaction.Adoption.Bytes++
			case "same-prefix":
				opts.Compaction.Prefix = summaryOptions("summary").Compaction.Prefix
			case "zero-prefix":
				opts.Compaction.Prefix = [32]byte{}
			case "unstable":
				opts.Compaction.Request = [32]byte{}
			case "new-generation":
				later := acquireRebalance(t, p, summaryOptions("summary-v2"))
				later.ReleaseSummary(true, summaryProof("new summary"))
			case "stale":
				p.now = func() time.Time { return now.Add(15*time.Minute + time.Second) }
			}
			next := acquireRebalance(t, p, opts)
			assertCompactionPin(t, next, cs[0].ID, false)
		})
	}
}

func TestCompactionBoundaryWithoutPriorNoticeCannotPrepareThenRetrySwitch(t *testing.T) {
	for _, unacknowledged := range []bool{false, true} {
		t.Run(fmt.Sprintf("unacknowledged=%v", unacknowledged), func(t *testing.T) {
			p, _, cs, now := rebalanceFixture(t)
			if unacknowledged {
				notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
				notice.Release(false)
			}
			opts := boundaryOptions("boundary", SummaryProof{})
			first := acquireRebalance(t, p, opts)
			assertCompactionPin(t, first, cs[0].ID, false)
			if !unacknowledged && first.Rebalance != "" {
				t.Fatalf("unprepared boundary manufactured a plan: %+v", first)
			}
			first.Release(true)
			// A normal request may prepare, but retrying the boundary must not
			// execute that newly acknowledged plan, even after fallback opens.
			if !unacknowledged {
				notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
				notice.Release(true)
			}
			p.now = func() time.Time { return now.Add(2 * time.Minute) }
			for range 2 {
				retry := acquireRebalance(t, p, opts)
				assertCompactionPin(t, retry, cs[0].ID, false)
				retry.Release(true)
			}
			// Header removal on a retry does not create a fresh opportunity.
			opts.Compaction.Phase = CompactionOrdinary
			retry := acquireRebalance(t, p, opts)
			assertCompactionPin(t, retry, cs[0].ID, false)
			retry.Release(false)
			next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
			assertCompactionPin(t, next, cs[1].ID, true)
		})
	}
}

func TestCompactionBoundaryCanceledBeforeDispatchKeepsProof(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	proof := summaryProof("completed summary")
	summary := acquireRebalance(t, p, summaryOptions("summary"))
	summary.ReleaseSummary(true, proof)
	old := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true})
	ctx, cancel := context.WithCancel(context.Background())
	waiter := goAcquire(p, ctx, boundaryOptions("boundary", proof))
	awaitRequestCount(t, db, 3)
	assertStillWaiting(t, waiter)
	cancel()
	result := awaitAcquire(t, waiter)
	if result.err != context.Canceled || result.lease != nil {
		t.Fatalf("canceled boundary dispatched: %+v", result)
	}
	old.Release(false)
	retry := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, retry, cs[1].ID, true)
}

func TestCompactionLateSummaryCannotOverwriteNewGeneration(t *testing.T) {
	p, _, cs, _ := rebalanceFixture(t)
	old := acquireRebalance(t, p, summaryOptions("summary-v1"))
	latest := acquireRebalance(t, p, summaryOptions("summary-v2"))
	latestProof := summaryProof("latest summary")
	latest.ReleaseSummary(true, latestProof)
	old.ReleaseSummary(true, summaryProof("old summary"))
	next := acquireRebalance(t, p, boundaryOptions("boundary", latestProof))
	assertCompactionPin(t, next, cs[1].ID, true)
}

func TestCompactionHelpersNeverConsumeOrEstablishEvidence(t *testing.T) {
	p, _, cs, _ := rebalanceFixture(t)
	proof := summaryProof("completed summary")
	summary := acquireRebalance(t, p, summaryOptions("summary"))
	summary.ReleaseSummary(true, proof)
	for _, opts := range []RequestOptions{summaryOptions("helper-summary"), boundaryOptions("boundary", proof)} {
		opts.ObserveOnly = true
		helper := acquireRebalance(t, p, opts)
		assertCompactionPin(t, helper, cs[0].ID, false)
		if helper.Rebalance != "" {
			t.Fatalf("helper announced compaction: %+v", helper)
		}
		helper.ReleaseSummary(true, summaryProof("helper summary"))
	}
	next := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, next, cs[1].ID, true)
}

func TestCompactionBoundaryRetainsSafetyGates(t *testing.T) {
	for _, query := range []string{
		`UPDATE conversations SET bound_at=strftime('%s','now')`,
		`UPDATE credentials SET status='disabled' WHERE label='B'`,
		`UPDATE credentials SET base_url='https://different.example' WHERE label='B'`,
		`UPDATE usage_history SET five_hour_pct=80 WHERE credential_id=(SELECT id FROM credentials WHERE label='B')`,
		`UPDATE usage_history SET captured_at=captured_at-1800`,
	} {
		t.Run(query, func(t *testing.T) {
			p, db, cs, _ := rebalanceFixture(t)
			notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
			notice.Release(true)
			execRebalance(t, db, query)
			next := acquireRebalance(t, p, boundaryOptions("boundary", SummaryProof{}))
			assertCompactionPin(t, next, cs[0].ID, false)
		})
	}
}

func TestCompactionEmergencyAndAccountAffinityRemainIndependent(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound=%v", bound), func(t *testing.T) {
			p, db, cs, _ := rebalanceFixture(t)
			opts := summaryOptions("summary")
			opts.AccountBound = bound
			summary := acquireRebalance(t, p, opts)
			summary.ReleaseSummary(true, summaryProof("completed summary"))
			execRebalance(t, db, `UPDATE credentials SET status='revoked' WHERE id=?`, cs[0].ID)
			l, err := p.AcquireScoped(context.Background(), "long", provider.Anthropic, "", nil, boundaryOptions("boundary", SummaryProof{}))
			if bound {
				if err != ErrCredentialOrphaned || l != nil {
					t.Fatalf("account-bound pin migrated: lease=%+v err=%v", l, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer l.Release(false)
			if l.Credential.ID != cs[1].ID || !l.IsNew || l.Rebalance != "" {
				t.Fatalf("compaction interfered with emergency: %+v", l)
			}
		})
	}
}

func TestCompactionFirstSummaryBypassesCadenceButRepeatsCannotExtendGrace(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	p.sessions["long"] = &sessionState{lastCheck: now}
	first := acquireRebalance(t, p, summaryOptions("summary-v1"))
	if first.Rebalance != "pending" {
		t.Fatalf("first distinct summary missed preparation inside debounce: %+v", first)
	}
	first.ReleaseSummary(true, summaryProof("first summary"))
	p.now = func() time.Time { return now.Add(59 * time.Second) }
	repeated := acquireRebalance(t, p, summaryOptions("summary-v2"))
	repeated.ReleaseSummary(true, summaryProof("replacement summary"))
	p.now = func() time.Time { return now.Add(time.Minute) }
	next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	assertCompactionPin(t, next, cs[1].ID, true)
}

func TestCompactionProofDoesNotAcknowledgeUnemittedNotice(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	proof := summaryProof("successful summary without notice")
	summary := acquireRebalance(t, p, summaryOptions("summary"))
	summary.ReleaseSummary(false, proof)
	if _, notified := longSession(t, p); notified {
		t.Fatal("summary proof falsely acknowledged an unemitted notice")
	}
	body := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, body, cs[0].ID, false)
	if body.Rebalance != "pending" {
		t.Fatalf("confirmed body boundary could not reannounce prior plan: %+v", body)
	}
	body.Release(true)
	p.now = func() time.Time { return now.Add(2 * time.Minute) }
	retry := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, retry, cs[0].ID, false)
	retry.Release(false)
	next := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	assertCompactionPin(t, next, cs[1].ID, true)
}

func TestCompactionInferredBoundaryWithoutPlanCannotPrepare(t *testing.T) {
	p, _, cs, _ := rebalanceFixture(t)
	proof := summaryProof("completed summary")
	summary := acquireRebalance(t, p, summaryOptions("summary"))
	summary.ReleaseSummary(true, proof)
	p.mu.Lock()
	p.finishPlan(p.sessions["long"], "disabled")
	p.mu.Unlock()
	boundary := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, boundary, cs[0].ID, false)
	if boundary.Rebalance != "" {
		t.Fatalf("inferred boundary manufactured new preparation: %+v", boundary)
	}
	boundary.Release(false)
	p.mu.Lock()
	consumed := p.sessions["long"].compaction.consumed
	p.mu.Unlock()
	if !consumed {
		t.Fatal("dispatch without an eligible plan failed to consume boundary proof")
	}
}

func TestCompactionDeferredBoundaryRetryCannotUseOrdinaryFallback(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	proof := summaryProof("completed summary")
	summary := acquireRebalance(t, p, summaryOptions("summary"))
	summary.ReleaseSummary(true, proof)
	old := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true})
	p.mu.Lock()
	p.sessions["long"].drainWait = time.Millisecond
	p.mu.Unlock()
	boundary := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, boundary, cs[0].ID, false)
	if boundary.Rebalance != "deferred" {
		t.Fatalf("boundary did not honor drain: %+v", boundary)
	}
	boundary.Release(false)
	old.Release(false)
	p.now = func() time.Time { return now.Add(2 * time.Minute) }
	retry := acquireRebalance(t, p, boundaryOptions("boundary", proof))
	assertCompactionPin(t, retry, cs[0].ID, false)
	retry.Release(false)
	// A genuinely different next turn may use the ordinary fallback, but the
	// summary text persisted in its wrapper must not rearm inferred execution.
	next := acquireRebalance(t, p, boundaryOptions("different next turn", proof))
	assertCompactionPin(t, next, cs[1].ID, true)
	var reason string
	if err := db.QueryRow(`SELECT reason FROM routing_event WHERE kind='switched'`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "usage-advantage" {
		t.Fatalf("persisted consumed wrapper rearmed compaction: reason=%q", reason)
	}
}

func TestCompactionRepeatedHeaderPrefixCannotRearmAfterSystemChange(t *testing.T) {
	p, _, cs, _ := rebalanceFixture(t)
	first := acquireRebalance(t, p, boundaryOptions("first boundary", SummaryProof{}))
	first.Release(false)
	notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	notice.Release(true)
	retry := acquireRebalance(t, p, boundaryOptions("same boundary changed system", SummaryProof{}))
	assertCompactionPin(t, retry, cs[0].ID, false)
	retry.Release(false)
	nextOpts := boundaryOptions("new compacted prefix", SummaryProof{})
	nextOpts.Compaction.Prefix = compactionDigest("genuinely new compaction")
	next := acquireRebalance(t, p, nextOpts)
	assertCompactionPin(t, next, cs[1].ID, true)
}

func TestCompactionProofExpiresAndRetriesDoNotRenewIt(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	proof := summaryProof("completed summary")
	first := acquireRebalance(t, p, summaryOptions("summary"))
	first.ReleaseSummary(true, proof)
	p.now = func() time.Time { return now.Add(14 * time.Minute) }
	retry := acquireRebalance(t, p, summaryOptions("summary"))
	retry.ReleaseSummary(true, proof)
	p.mu.Lock()
	s := p.sessions["long"]
	expires := s.compaction.expires
	req := boundaryOptions("boundary", proof).Compaction
	gate := p.compactionFor(s, compactionRequestKey("long", req), cs[0].ID, req, now.Add(compactionTTL))
	p.mu.Unlock()
	if !expires.Equal(now.Add(compactionTTL)) {
		t.Fatalf("retry renewed proof: %v", expires)
	}
	if gate.boundary {
		t.Fatal("expired proof accepted at TTL boundary")
	}
}

func TestCompactionEvidenceCapEvictionKeepsSafetyState(t *testing.T) {
	p, _, cs, now := rebalanceFixture(t)
	old := acquireRebalance(t, p, summaryOptions("summary-v1"))
	latest := acquireRebalance(t, p, summaryOptions("summary-v2"))
	latest.ReleaseSummary(true, summaryProof("latest summary"))
	p.mu.Lock()
	s := p.sessions["long"]
	plan, drained := s.pending, s.drained
	// Guarantee eviction chooses this generation rather than one of the new
	// synthetic records. Every record still contains bounded digests only.
	s.compaction.expires = now.Add(time.Second)
	for i := range compactionCap + 1 {
		req := CompactionRequest{Request: compactionDigest(fmt.Sprintf("request-%d", i))}
		p.addCompaction(s, compactionRequestKey("bounded-test", req), cs[0].ID, now)
	}
	records, current := len(p.compactions), s.compaction
	safetyIntact := p.sessions["long"] == s && s.pending == plan && s.drained == drained && s.inflight == 1 && s.summaryInflight == 1
	p.mu.Unlock()
	if records != compactionCap || current != nil {
		t.Fatalf("evidence not capped/evicted: records=%d current=%p", records, current)
	}
	if !safetyIntact {
		t.Fatal("optional evidence eviction changed lease or pending-plan safety state")
	}
	p.now = func() time.Time { return now.Add(2 * time.Minute) }
	next := acquireRebalance(t, p, boundaryOptions("boundary", SummaryProof{}))
	assertCompactionPin(t, next, cs[0].ID, false)
	next.Release(false)
	old.ReleaseSummary(true, summaryProof("late evicted summary"))
	p.mu.Lock()
	lateIgnored := s.compaction == nil && s.summaryInflight == 0
	// Expiry prunes evidence, not affinity/session/pending state.
	s.accountBound = true
	p.pruneCompactions(now.Add(20 * time.Minute))
	expirySafe := len(p.compactions) == 0 && p.sessions["long"] == s && s.accountBound && s.pending == plan
	p.mu.Unlock()
	if !lateIgnored {
		t.Fatal("late evicted proof revived evidence or leaked a summary lease")
	}
	if !expirySafe {
		t.Fatal("evidence expiry touched safety state")
	}
}

func TestCompactionLateSummaryAfterEmergencyCannotProveNewPin(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	old := acquireRebalance(t, p, summaryOptions("summary"))
	execRebalance(t, db, `UPDATE credentials SET status='revoked' WHERE id=?`, cs[0].ID)
	_, changed, err := p.Bind(context.Background(), "long", provider.Anthropic)
	if err != nil || !changed {
		t.Fatalf("emergency fixture failed: changed=%v err=%v", changed, err)
	}
	old.ReleaseSummary(true, summaryProof("late old-account summary"))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions["long"].compaction != nil || p.sessions["long"].summaryInflight != 0 {
		t.Fatal("late summary attached proof to an emergency replacement pin")
	}
}

func TestCompactionDistinctSummaryAfterEmergencyReplacesOldGeneration(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	old := acquireRebalance(t, p, summaryOptions("old summary request"))
	execRebalance(t, db, `UPDATE credentials SET status='revoked' WHERE id=?`, cs[0].ID)
	emergency := acquireRebalance(t, p, RequestOptions{Rebalance: true, ObserveOnly: true})
	if emergency.Credential.ID != cs[1].ID || !emergency.IsNew {
		t.Fatalf("emergency fixture failed: %+v", emergency)
	}
	emergency.Release(false)
	proof := summaryProof("summary on replacement account")
	fresh := acquireRebalance(t, p, summaryOptions("distinct summary request"))
	fresh.ReleaseSummary(false, proof)
	old.ReleaseSummary(true, summaryProof("late original-account summary"))
	// The prior fingerprint stays cached deliberately: retrying it must not
	// replace a newer generation, even though its original pin is now gone.
	retry := acquireRebalance(t, p, summaryOptions("old summary request"))
	retry.ReleaseSummary(false, summaryProof("retried old summary"))
	p.mu.Lock()
	r := p.sessions["long"].compaction
	valid := r != nil && r.source == cs[1].ID && r.proof == proof
	p.mu.Unlock()
	if !valid {
		t.Fatal("emergency prevented distinct summary tracking or revived an old generation")
	}
}

func TestCompactionAuditFailureRollsBackSwitch(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	notice := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	notice.Release(true)
	execRebalance(t, db, `CREATE TRIGGER reject_compaction_switch BEFORE INSERT ON routing_event WHEN NEW.kind='switched' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
	next := acquireRebalance(t, p, boundaryOptions("boundary", SummaryProof{}))
	assertCompactionPin(t, next, cs[0].ID, false)
	events, err := store.ListRoutingEvents(context.Background(), db, 0, 100, now)
	if err != nil || len(events) != 0 {
		t.Fatalf("failed switch left audit: events=%+v err=%v", events, err)
	}
}
