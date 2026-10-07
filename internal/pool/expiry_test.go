package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func expiryTestSamples(now int64) []expirySample {
	return []expirySample{
		{at: now - 5*60, fh: 10, sd: 20, fhReset: now + 3*3600, sdReset: now + 3600, fhObserved: true, sdObserved: true},
		{at: now - 15*60, fh: 9, sd: 18, fhReset: now + 3*3600, sdReset: now + 3600, fhObserved: true, sdObserved: true},
		{at: now - 25*60, fh: 8, sd: 16, fhReset: now + 3*3600, sdReset: now + 3600, fhObserved: true, sdObserved: true},
	}
}

func expiryTestFixture(t *testing.T) (*Pool, *store.DB, string, int64) {
	t.Helper()
	db, cs := setup(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Unix()
	p := New(db)
	p.now = func() time.Time { return time.Unix(now, 0) }
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id<>?`, cs[0].ID)
	for _, s := range expiryTestSamples(now) {
		execRebalance(t, db, `INSERT INTO usage_history
			(credential_id,captured_at,five_hour_pct,seven_day_pct,five_hour_resets_at,seven_day_resets_at,five_hour_observed,seven_day_observed)
			VALUES (?,?,?,?,?,?,?,?)`, cs[0].ID, s.at, s.fh, s.sd, s.fhReset, s.sdReset, s.fhObserved, s.sdObserved)
	}
	return p, db, cs[0].ID, now
}

func expiryTestEvidence(t *testing.T, event store.RoutingEvent) expiryEvidence {
	t.Helper()
	if event.Policy != "expiry" || event.Mode != "shadow" || event.Kind != "shadow_decision" || event.TargetID == "" || event.SourceID != "" || event.Conversation != "" {
		t.Fatalf("not a destination-only shadow decision: %+v", event)
	}
	if len(event.Evidence) > 2048 || !json.Valid(event.Evidence) {
		t.Fatalf("unbounded/invalid evidence (%d bytes): %s", len(event.Evidence), event.Evidence)
	}
	var e expiryEvidence
	if err := json.Unmarshal(event.Evidence, &e); err != nil {
		t.Fatal(err)
	}
	if e.Executable || e.ScopeKnowledge != "max_only" || e.Measurement != "destination_opportunity" ||
		(e.Confidence != "unknown" && e.Confidence != "heuristic") {
		t.Fatalf("unsafe evidence: %+v", e)
	}
	for _, blocker := range []string{"scope_eligibility_unknown", "incremental_demand_unknown", "cache_cost_unknown", "stream_success_unknown"} {
		if !slices.Contains(e.Blockers, blocker) {
			t.Fatalf("missing readiness blocker %s: %+v", blocker, e)
		}
	}
	return e
}

func expiryTestObserve(t *testing.T, p *Pool) (store.RoutingEvent, expiryEvidence) {
	t.Helper()
	events, err := p.ObserveExpiry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want one", len(events))
	}
	return events[0], expiryTestEvidence(t, events[0])
}

// A large fixture under -race may deliberately exhaust the observer's fixed
// production budget. Its safe contract is then partial uncertainty plus error,
// not a longer budget or a flaky requirement that the scan always completes.
func expiryTestBoundedObserve(t *testing.T, p *Pool) (store.RoutingEvent, expiryEvidence) {
	t.Helper()
	events, err := p.ObserveExpiry(context.Background())
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want one; err=%v", len(events), err)
	}
	e := expiryTestEvidence(t, events[0])
	if err != nil && (events[0].Reason != "workload-unknown" || e.Workload.Complete || !slices.Contains(e.Blockers, "inputs_unavailable")) {
		t.Fatalf("deadline lost partial uncertainty: %s %+v", events[0].Reason, e)
	}
	return events[0], e
}

func TestExpiryForecastIncludesSampleAge(t *testing.T) {
	p, _, _, now := expiryTestFixture(t)
	event, e := expiryTestObserve(t, p)
	if event.Reason != "expiry-opportunity" || e.Confidence != "heuristic" || e.Baseline != "spare_weekly_capacity" {
		t.Fatalf("opportunity = %s %+v", event.Reason, e)
	}
	f := e.Forecast
	if f == nil || f.WeeklyRate != (expiryRange{12, 12}) || f.FiveHourRate != (expiryRange{6, 6}) ||
		f.UnusedWeekly != (expiryRange{67, 67}) || f.SampleAgeSeconds != 300 || f.HorizonSeconds != 3900 || f.ResetAt != now+3600 {
		t.Fatalf("forecast did not project from the sample: %+v", f)
	}
}

func TestExpirySampleValidation(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Unix()
	for _, tc := range []struct {
		name string
		edit func([]expirySample) []expirySample
		want string
	}{
		{"missing", func(s []expirySample) []expirySample { return nil }, "quota_missing"},
		{"one interval", func(s []expirySample) []expirySample { return s[:2] }, "samples_insufficient_or_bounded"},
		{"unknown five hour", func(s []expirySample) []expirySample { s[0].fhObserved = false; return s }, "quota_unobserved"},
		{"unknown weekly", func(s []expirySample) []expirySample { s[1].sdObserved = false; return s }, "quota_unobserved"},
		{"NaN", func(s []expirySample) []expirySample { s[0].sd = math.NaN(); return s }, "quota_unobserved"},
		{"Inf", func(s []expirySample) []expirySample { s[0].fh = math.Inf(1); return s }, "quota_unobserved"},
		{"negative", func(s []expirySample) []expirySample { s[0].sd = -1; return s }, "quota_unobserved"},
		{"over 100", func(s []expirySample) []expirySample { s[0].fh = 101; return s }, "quota_unobserved"},
		{"unknown reset", func(s []expirySample) []expirySample { s[1].sdReset = 0; return s }, "reset_unknown_or_elapsed"},
		{"elapsed reset", func(s []expirySample) []expirySample { s[0].fhReset = now; return s }, "reset_unknown_or_elapsed"},
		{"real weekly reset", func(s []expirySample) []expirySample { s[1].sdReset += 3; return s }, "reset_changed"},
		{"real five hour reset", func(s []expirySample) []expirySample { s[2].fhReset += 3600; return s }, "reset_changed"},
		{"decreasing weekly", func(s []expirySample) []expirySample { s[0].sd = 0; return s }, "usage_decreased_or_conflicting"},
		{"decreasing five hour", func(s []expirySample) []expirySample { s[0].fh = 0; return s }, "usage_decreased_or_conflicting"},
		{"future sample", func(s []expirySample) []expirySample { s[0].at = now + 1; return s }, "sample_time_invalid"},
		{"stale latest", func(s []expirySample) []expirySample { s[0].at = now - 16*60; return s }, "samples_stale"},
		{"stale previous", func(s []expirySample) []expirySample { s[1].at = now - 31*60; return s }, "samples_stale"},
		{"stale oldest", func(s []expirySample) []expirySample { s[2].at = now - 61*60; return s }, "samples_stale"},
		{"interval too short", func(s []expirySample) []expirySample { s[1].at = s[0].at - 299; return s }, "samples_insufficient_or_bounded"},
		{"duplicate is not distinct", func(s []expirySample) []expirySample { s[1] = s[0]; return s }, "samples_insufficient_or_bounded"},
		{"conflicting duplicate", func(s []expirySample) []expirySample { s[1].at = s[0].at; return s }, "usage_decreased_or_conflicting"},
		{"outside horizon", func(s []expirySample) []expirySample {
			for i := range s {
				s[i].sdReset = now + 7201
			}
			return s
		}, "outside_expiry_horizon"},
		{"five hour resets first", func(s []expirySample) []expirySample {
			for i := range s {
				s[i].fhReset = s[i].sdReset
			}
			return s
		}, "five_hour_reset_before_weekly"},
		{"jitter spans over two seconds", func(s []expirySample) []expirySample { s[1].sdReset++; s[2].sdReset += 3; return s }, "reset_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, blocker := expiryForecastFor(tc.edit(expiryTestSamples(now)), now)
			if f != nil || blocker != tc.want {
				t.Fatalf("forecast=%+v blocker=%q, want %q", f, blocker, tc.want)
			}
		})
	}
}

func TestExpiryJitterAndDistinctSamples(t *testing.T) {
	now := time.Now().Unix()
	s := expiryTestSamples(now)
	s[1].sdReset++
	s[2].sdReset += 2
	s[2].fhReset -= 2
	f, blocker := expiryForecastFor(s, now)
	if blocker != "" || f == nil {
		t.Fatalf("two-second jitter rejected: %q", blocker)
	}
	if f.UnusedWeekly.Min >= 67 || f.UnusedWeekly.Max != 67 || f.HorizonSeconds != 3902 {
		t.Fatalf("jitter failed to widen range conservatively: %+v", f)
	}
	// Exact duplicate polls are harmless, but do not form an extra interval.
	s = expiryTestSamples(now)
	s = []expirySample{s[0], s[0], s[1], s[1], s[2], s[2]}
	if f, blocker = expiryForecastFor(s, now); f == nil || blocker != "" {
		t.Fatalf("valid distinct samples lost among duplicates: %q", blocker)
	}
	// A close poll cannot conceal an intermediate decrease or unknown value.
	s = expiryTestSamples(now)
	closePoll := s[0]
	closePoll.at -= 60
	closePoll.sd = 21
	s = slices.Insert(s, 1, closePoll)
	if f, blocker = expiryForecastFor(s, now); f != nil || blocker != "usage_decreased_or_conflicting" {
		t.Fatalf("skipped close poll concealed a decrease: %+v %q", f, blocker)
	}
}

func TestExpiryMissingVersusRealZero(t *testing.T) {
	p, db, _, _ := expiryTestFixture(t)
	execRebalance(t, db, `UPDATE usage_history SET five_hour_pct=0,seven_day_pct=0,five_hour_observed=0,seven_day_observed=0`)
	event, e := expiryTestObserve(t, p)
	if event.Reason != "quota-unknown" || e.Forecast != nil {
		t.Fatalf("historical unknown treated as spare quota: %s %+v", event.Reason, e)
	}
	execRebalance(t, db, `UPDATE usage_history SET five_hour_observed=1,seven_day_observed=1`)
	event, e = expiryTestObserve(t, p)
	if event.Reason != "expiry-opportunity" || e.Forecast == nil || e.Forecast.UnusedWeekly != (expiryRange{100, 100}) {
		t.Fatalf("real zero was not recognized: %s %+v", event.Reason, e)
	}
}

func TestExpiryFullyUtilizedBaselineNoNeed(t *testing.T) {
	p, db, id, now := expiryTestFixture(t)
	for i, sd := range []float64{70, 60, 50} {
		execRebalance(t, db, `UPDATE usage_history SET seven_day_pct=?,five_hour_pct=5 WHERE credential_id=? AND captured_at=?`, sd, id, now-int64(5+10*i)*60)
	}
	event, e := expiryTestObserve(t, p)
	if event.Reason != "no-expiry-opportunity" || e.Baseline != "no_need" || e.Forecast == nil || e.Forecast.UnusedWeekly != (expiryRange{}) {
		t.Fatalf("already-fully-utilized forecast is not no_need: %s %+v", event.Reason, e)
	}
	// A measured 100% is valid known exhaustion, not a missing quota sample.
	execRebalance(t, db, `UPDATE usage_history SET seven_day_pct=100`)
	event, e = expiryTestObserve(t, p)
	if event.Reason != "no-expiry-opportunity" || e.Baseline != "no_need" {
		t.Fatalf("observed exhaustion was not no_need: %s %+v", event.Reason, e)
	}
}

func TestExpiryWorkloadUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name, blocker string
		prepare       func(*testing.T, *Pool, *store.DB, string, int64)
	}{
		{"rate trend", "request_rate_changed", func(t *testing.T, p *Pool, db *store.DB, id string, now int64) {
			execRebalance(t, db, `INSERT INTO request_log (credential_id,ts,output_tokens,cache_creation_tokens,cache_read_tokens) VALUES (?,?,3,4,5)`, id, now-60)
		}},
		{"new binding not new conversation", "new_arrivals", func(t *testing.T, p *Pool, db *store.DB, id string, now int64) {
			execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES ('arrived',?,?,?,?)`, id, now-24*3600, now, now-60)
		}},
		{"pending target", "pending_targets", func(t *testing.T, p *Pool, db *store.DB, id string, now int64) {
			p.sessions["pending"] = &sessionState{pending: &rebalancePlan{source: "other", target: id, created: time.Unix(now, 0), done: make(chan struct{})}}
		}},
		{"inflight", "inflight_work_unknown", func(t *testing.T, p *Pool, db *store.DB, id string, now int64) {
			execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES ('inflight',?,?,?,?)`, id, now-7200, now, now-7200)
			p.sessions["inflight"] = &sessionState{inflight: 2}
		}},
		{"unknown negative counters", "workload_metadata_invalid", func(t *testing.T, p *Pool, db *store.DB, id string, now int64) {
			execRebalance(t, db, `INSERT INTO request_log (credential_id,ts,output_tokens) VALUES (?,?,-1)`, id, now-1800)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, db, id, now := expiryTestFixture(t)
			tc.prepare(t, p, db, id, now)
			event, e := expiryTestObserve(t, p)
			if event.Reason != "workload-unknown" || e.Confidence != "unknown" || !slices.Contains(e.Blockers, tc.blocker) {
				t.Fatalf("workload uncertainty lost: %s %+v", event.Reason, e)
			}
			if e.Forecast == nil || e.Forecast.UnusedWeekly != (expiryRange{67, 67}) {
				t.Fatalf("workload evidence double-counted as burn: %+v", e.Forecast)
			}
		})
	}
}

func TestExpiryCountsAreEvidenceNotDenominators(t *testing.T) {
	p, db, id, now := expiryTestFixture(t)
	// Equal arrival rates in both recent five-minute windows, and before/after
	// the newest sample. Arbitrarily huge token counts cannot alter quota math.
	for _, age := range []int64{60, 360, 660} {
		execRebalance(t, db, `INSERT INTO request_log (credential_id,ts,output_tokens,cache_creation_tokens,cache_read_tokens)
			VALUES (?,?,1000000000,2000000000,3000000000)`, id, now-age)
	}
	event, e := expiryTestObserve(t, p)
	if event.Reason != "expiry-opportunity" || e.Forecast == nil || e.Forecast.UnusedWeekly != (expiryRange{67, 67}) {
		t.Fatalf("counts leaked into forecasting: %s %+v", event.Reason, e)
	}
	if e.Workload.Requests != 3 || e.Workload.Output != 3e9 || e.Workload.CacheCreation != 6e9 || e.Workload.CacheRead != 9e9 ||
		e.Workload.Trend != "steady" || e.Workload.RecentRate != 12 || e.Workload.PreviousRate != 12 {
		t.Fatalf("observed counts/trend lost: %+v", e.Workload)
	}
}

func TestExpiryReadsAtMostSixSamples(t *testing.T) {
	p, db, id, now := expiryTestFixture(t)
	// Six newer duplicate polls hide older good intervals. The observer must
	// report uncertainty rather than search unlimited history for a forecast.
	for range expiryMaxSamples {
		execRebalance(t, db, `INSERT INTO usage_history
			(credential_id,captured_at,five_hour_pct,seven_day_pct,five_hour_resets_at,seven_day_resets_at,five_hour_observed,seven_day_observed)
			VALUES (?,?,10,20,?,?,1,1)`, id, now-60, now+10800, now+3600)
	}
	samples, err := p.expirySamples(context.Background(), id)
	if err != nil || len(samples) != expiryMaxSamples {
		t.Fatalf("sample bound: %d %v", len(samples), err)
	}
	event, e := expiryTestObserve(t, p)
	if event.Reason != "quota-unknown" || !slices.Contains(e.Blockers, "samples_insufficient_or_bounded") {
		t.Fatalf("sample truncation concealed: %s %+v", event.Reason, e)
	}
}

func TestExpiryBoundedGlobalRequestScan(t *testing.T) {
	p, db, id, now := expiryTestFixture(t)
	// These are other-provider requests, still counted against the GLOBAL
	// limit. Filtering first by destination would quietly exceed that budget.
	execRebalance(t, db, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
		INSERT INTO request_log (credential_id,ts) SELECT 'other-provider',? FROM n`, expiryMaxRequests+1, now-100)
	a := &expiryAccount{id: id, samples: expiryTestSamples(now)}
	truncated, err := p.expiryRequests(context.Background(), map[string]*expiryAccount{id: a}, now)
	if err != nil || !truncated || a.work.Requests != 0 {
		t.Fatalf("global request row bound failed: truncated=%v observed=%d err=%v", truncated, a.work.Requests, err)
	}
	event, e := expiryTestBoundedObserve(t, p)
	if event.Reason != "workload-unknown" || e.Workload.Complete ||
		!slices.Contains(e.Blockers, "requests_truncated") && !slices.Contains(e.Blockers, "inputs_unavailable") {
		t.Fatalf("request truncation treated as zero cost: %s %+v", event.Reason, e)
	}
	// Old traffic outside the one-hour window cannot exhaust today's budget.
	execRebalance(t, db, `UPDATE request_log SET ts=?`, now-3601)
	execRebalance(t, db, `INSERT INTO request_log (credential_id,ts) VALUES (?,?)`, id, now-1800)
	event, e = expiryTestObserve(t, p)
	if event.Reason != "expiry-opportunity" || !e.Workload.Complete || e.Workload.Requests != 1 {
		t.Fatalf("old rows consumed the scan budget: %s %+v", event.Reason, e)
	}
}

func TestExpiryBoundedArrivalsAndSessions(t *testing.T) {
	p, db, id, now := expiryTestFixture(t)
	execRebalance(t, db, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
		INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at)
		SELECT 'bulk-'||x,?,?,?,? FROM n`, expiryMaxArrivals+1, id, now-7200, now, now-10)
	a := &expiryAccount{id: id, samples: expiryTestSamples(now)}
	truncated, err := p.expiryArrivals(context.Background(), map[string]*expiryAccount{id: a}, now)
	if err != nil || !truncated || a.work.Arrivals != expiryMaxArrivals {
		t.Fatalf("arrival row bound failed: truncated=%v observed=%d err=%v", truncated, a.work.Arrivals, err)
	}
	event, e := expiryTestBoundedObserve(t, p)
	if event.Reason != "workload-unknown" || e.Workload.Arrivals > expiryMaxArrivals ||
		!slices.Contains(e.Blockers, "arrivals_truncated") && !slices.Contains(e.Blockers, "inputs_unavailable") {
		t.Fatalf("unbounded or concealed arrivals: %s %+v", event.Reason, e)
	}
	execRebalance(t, db, `DELETE FROM conversations`)
	for i := range expiryMaxSessions + 1 {
		p.sessions[fmt.Sprint(i)] = &sessionState{}
	}
	event, e = expiryTestObserve(t, p)
	if event.Reason != "workload-unknown" || e.Workload.Complete || !slices.Contains(e.Blockers, "sessions_unavailable_or_truncated") {
		t.Fatalf("unbounded session snapshot: %s %+v", event.Reason, e)
	}
}

func TestExpiryCredentialAndEventBounds(t *testing.T) {
	p, db, _, now := expiryTestFixture(t)
	execRebalance(t, db, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<130)
		INSERT INTO credentials (id,access_token,refresh_token,expires_at,status,created_at)
		SELECT printf('bounded-%03d',x),'access','refresh',?,'active',? FROM n`, now+3600, now)
	accounts, overflow, err := p.expiryAccounts(context.Background())
	if err != nil || len(accounts) != expiryMaxCredentials || !overflow {
		t.Fatalf("credential bound: %d %v %v", len(accounts), overflow, err)
	}
	events, err := p.ObserveExpiry(context.Background())
	if err != nil || len(events) != expiryMaxEvents {
		t.Fatalf("event bound: %d %v", len(events), err)
	}
	for _, event := range events {
		e := expiryTestEvidence(t, event)
		if !slices.Contains(e.Blockers, "credentials_truncated") || e.Workload.Complete {
			t.Fatalf("credential truncation hidden: %+v", e)
		}
	}
	// Partial failures share the same output cap, even if every account needs
	// an uncertainty event and no workload metadata could be loaded.
	execRebalance(t, db, `DROP TABLE request_log`)
	events, err = p.ObserveExpiry(context.Background())
	if err == nil || len(events) != expiryMaxEvents {
		t.Fatalf("partial failure event bound: %d %v", len(events), err)
	}
	for _, event := range events {
		if e := expiryTestEvidence(t, event); !slices.Contains(e.Blockers, "inputs_unavailable") {
			t.Fatalf("partial failure not reported: %+v", e)
		}
	}
}

func TestExpiryShadowNoninterference(t *testing.T) {
	p, db, id, now := expiryTestFixture(t)
	execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at,request_count)
		VALUES ('pin',?,?,?,?,9)`, id, now-7200, now-60, now-7200)
	plan := &rebalancePlan{source: id, target: id, created: time.Unix(now-10, 0), notified: true, done: make(chan struct{})}
	state := &sessionState{inflight: 1, waiting: 1, pending: plan, lastUsed: time.Unix(now-5, 0), lastCheck: time.Unix(now-10, 0)}
	before := *state
	p.sessions["pin"] = state
	p.destinations[id] = time.Unix(now-40, 0)
	// A single read-only connection turns any accidental DB write into a test
	// failure, including a reservation, routing-event insert, or counter bump.
	db.SetMaxOpenConns(1)
	execRebalance(t, db, `PRAGMA query_only=ON`)
	event, e := expiryTestObserve(t, p)
	if event.Reason != "workload-unknown" || e.Workload.PendingTargets != 1 || e.Workload.Inflight != 1 {
		t.Fatalf("snapshot state lost: %s %+v", event.Reason, e)
	}
	if !reflect.DeepEqual(before, *state) || p.destinations[id] != time.Unix(now-40, 0) || len(p.sessions) != 1 {
		t.Fatal("observer changed live in-memory routing state")
	}
	select {
	case <-plan.done:
		t.Fatal("observer completed a real plan")
	default:
	}
	var pin string
	var bound, seen, count int64
	if err := db.QueryRow(`SELECT credential_id,bound_at,last_seen_at,request_count FROM conversations WHERE id='pin'`).Scan(&pin, &bound, &seen, &count); err != nil {
		t.Fatal(err)
	}
	if pin != id || bound != now-7200 || seen != now-60 || count != 9 {
		t.Fatalf("observer changed pin/counters: %s %d %d %d", pin, bound, seen, count)
	}
	again, _ := expiryTestObserve(t, p)
	if !reflect.DeepEqual(event, again) {
		t.Fatal("unchanged shadow inputs produced different decisions")
	}
}

func TestExpiryDoesNotWaitForPoolMutex(t *testing.T) {
	p, _, _, _ := expiryTestFixture(t)
	p.mu.Lock()
	defer p.mu.Unlock()
	event, e := expiryTestObserve(t, p)
	if event.Reason != "workload-unknown" || !slices.Contains(e.Blockers, "sessions_unavailable_or_truncated") {
		t.Fatalf("busy mutex was not reported as uncertainty: %s %+v", event.Reason, e)
	}
}

func TestExpiryChildReadDeadline(t *testing.T) {
	p, db, _, _ := expiryTestFixture(t)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	events, err := p.ObserveExpiry(parent)
	if !errors.Is(err, context.DeadlineExceeded) || len(events) != 0 || parent.Err() != nil {
		t.Fatalf("read budget did not isolate cancellation: events=%v err=%v parent=%v", events, err, parent.Err())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("500ms budget was not bounded: %v", elapsed)
	}
}

func TestExpiryPartialReadFailureReturnsUncertainty(t *testing.T) {
	p, db, _, _ := expiryTestFixture(t)
	execRebalance(t, db, `DROP TABLE request_log`)
	events, err := p.ObserveExpiry(context.Background())
	if err == nil || len(events) != 1 {
		t.Fatalf("missing partial failure evidence: events=%d err=%v", len(events), err)
	}
	e := expiryTestEvidence(t, events[0])
	if events[0].Reason != "workload-unknown" || e.Workload.Complete || e.Forecast == nil || !slices.Contains(e.Blockers, "inputs_unavailable") {
		t.Fatalf("failed metadata silently became zero demand: %s %+v", events[0].Reason, e)
	}
}

func TestExpiryEvidenceSizeFallback(t *testing.T) {
	now := time.Now().Unix()
	var blockers []string
	for i := range 100 {
		blockers = append(blockers, fmt.Sprintf("extra_input_blocker_%d", i))
	}
	event := expiryDecision(expiryAccount{id: "target", samples: expiryTestSamples(now)}, now, blockers)
	e := expiryTestEvidence(t, event)
	if event.Reason != "workload-unknown" || e.Confidence != "unknown" || !slices.Contains(e.Blockers, "evidence_bounded") {
		t.Fatalf("evidence fallback lost uncertainty: %s %+v", event.Reason, e)
	}
}
