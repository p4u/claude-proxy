package subscriptionstats

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestQuotaContinuityAndEvidenceThresholds(t *testing.T) {
	type point struct {
		ago      time.Duration
		pct      any
		reset    any
		observed int
	}
	reset := testNow.Add(24 * time.Hour).Unix()
	base := func() []point {
		return []point{{time.Hour, 20, reset, 1}, {30 * time.Minute, 22, reset, 1}, {0, 40, reset, 1}}
	}
	cases := []struct {
		name    string
		mutate  func([]point) []point
		want    bool
		reason  string
		samples int
	}{
		{"valid nonzero baseline", func(p []point) []point { return p }, true, "partial_coverage", 3},
		{"explicit zero baseline", func(p []point) []point { p[0].pct = 0; return p }, true, "partial_coverage", 3},
		{"reset jitter", func(p []point) []point { p[1].reset = reset + 1; p[2].reset = reset + 2; return p }, true, "partial_coverage", 3},
		{"jitter cannot drift", func(p []point) []point { p[1].reset = reset + 2; p[2].reset = reset + 4; return p }, false, "reset_changed", 0},
		{"changed reset gap", func(p []point) []point { p[1].reset = reset + 3; return p }, false, "insufficient_observations", 1},
		{"unobserved gap", func(p []point) []point { p[1].observed = 0; return p }, false, "insufficient_observations", 1},
		{"unobserved latest", func(p []point) []point { p[2].observed = 0; return p }, false, "unobserved_quota", 2},
		{"legacy flags", func(p []point) []point {
			for i := range p {
				p[i].observed = 0
			}
			return p
		}, false, "unobserved_quota", 0},
		{"invalid percent gap", func(p []point) []point { p[1].pct = 101; return p }, false, "insufficient_observations", 1},
		{"negative percent gap", func(p []point) []point { p[1].pct = -1; return p }, false, "insufficient_observations", 1},
		{"infinite percent gap", func(p []point) []point { p[1].pct = math.Inf(1); return p }, false, "insufficient_observations", 1},
		{"text percent gap", func(p []point) []point { p[1].pct = "broken"; return p }, false, "insufficient_observations", 1},
		{"missing reset gap", func(p []point) []point { p[1].reset = nil; return p }, false, "insufficient_observations", 1},
		{"past reset gap", func(p []point) []point { p[1].reset = testNow.Add(-31 * time.Minute).Unix(); return p }, false, "insufficient_observations", 1},
		{"reset too far future", func(p []point) []point { p[1].reset = testNow.Add(8 * 24 * time.Hour).Unix(); return p }, false, "insufficient_observations", 1},
		{"decreasing quota", func(p []point) []point { p[0].pct = 50; p[1].pct = 30; p[2].pct = 35; return p }, false, "insufficient_delta", 2},
		{"zero delta", func(p []point) []point {
			for i := range p {
				p[i].pct = 74
			}
			return p
		}, false, "insufficient_delta", 3},
		{"small delta", func(p []point) []point { p[2].pct = 29.9; return p }, false, "insufficient_delta", 3},
		{"short interval", func(p []point) []point { return []point{{29 * time.Minute, 20, reset, 1}, {0, 74, reset, 1}} }, false, "insufficient_interval", 2},
		{"single observation", func(p []point) []point { return p[2:] }, false, "insufficient_observations", 1},
		{"same second duplicates", func(p []point) []point { return append(p, p[2]) }, true, "partial_coverage", 3},
		{"conflicting second", func(p []point) []point { return append(p, point{0, 74, reset, 1}) }, false, "ambiguous_timestamp", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.credential("a", "anthropic", "max", "20x", 1)
			for _, p := range tc.mutate(base()) {
				f.quota("a", "seven_day", testNow.Add(-p.ago), p.pct, p.reset, p.observed)
			}
			f.request("a", "m", testNow.Add(-time.Minute), Tokens{Input: 100, Output: 20})
			r := f.build(testNow.Add(-2*time.Hour), testNow.Add(time.Second), "seven_day")
			w := r.Accounts[0].Current
			if w == nil || (w.Estimate != nil) != tc.want || w.Reason != tc.reason || w.Samples != tc.samples {
				t.Fatalf("current=%+v wantEstimate=%v reason=%s samples=%d", w, tc.want, tc.reason, tc.samples)
			}
			if (len(r.CapacityHistory) != 0) != tc.want {
				t.Fatalf("history=%+v want usable=%v", r.CapacityHistory, tc.want)
			}
			if tc.want && r.CapacityHistory[0].ResetAt != reset {
				t.Fatal("canonical reset drifted from the first observation")
			}
		})
	}
}

func TestRolloverAndSingleEstimatePerCycle(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	oldReset, newReset := testNow.Add(-3*time.Hour), testNow.Add(2*time.Hour)
	// Two complete segments inside the first quota cycle are separated by an
	// explicit unknown reading. Only one segment may weight the group estimate.
	for _, p := range []struct {
		hours float64
		pct   int
		known int
	}{
		{-6, 10, 1}, {-5.5, 30, 1}, {-5.25, 0, 0}, {-5, 40, 1}, {-4, 60, 1},
	} {
		f.quota("a", "five_hour", testNow.Add(time.Duration(p.hours*float64(time.Hour))), p.pct, oldReset.Unix(), p.known)
	}
	f.request("a", "old-short", testNow.Add(-5*time.Hour-45*time.Minute), Tokens{Output: 4})
	f.request("a", "old-long", testNow.Add(-4*time.Hour-30*time.Minute), Tokens{Output: 10})
	// After a real rollover, 5% is a new observed baseline, never a fabricated 0.
	for _, p := range []struct {
		ago time.Duration
		pct int
	}{{2*time.Hour + 50*time.Minute, 5}, {2*time.Hour + 20*time.Minute, 25}, {time.Hour + 40*time.Minute, 45}, {20 * time.Minute, 65}} {
		f.quota("a", "five_hour", testNow.Add(-p.ago), p.pct, newReset.Unix(), 1)
	}
	for _, ago := range []time.Duration{2*time.Hour + 40*time.Minute, 2*time.Hour + 10*time.Minute, time.Hour} {
		f.request("a", "new", testNow.Add(-ago), Tokens{Output: 20})
	}
	r := f.build(testNow.Add(-7*time.Hour), testNow.Add(time.Second), "five_hour")
	if len(r.CapacityHistory) != 2 {
		t.Fatalf("cycle count = %+v", r.CapacityHistory)
	}
	old, next := r.CapacityHistory[0], r.CapacityHistory[1]
	if old.Start != testNow.Add(-5*time.Hour).Unix() || old.Tokens.Output != 10 || old.Estimate.Total != 50 || next.Estimate.Total != 100 || next.DeltaPct != 60 {
		t.Fatalf("rollover intervals = %+v", r.CapacityHistory)
	}
	g := r.Groups[0]
	if g.EstimateSamples != 2 || g.Estimate.Total != 75 || g.EstimateLow.Total != 63 || g.EstimateHigh.Total != 88 {
		t.Fatalf("cycle quantiles = %+v", g)
	}
	w := r.Accounts[0].Current
	if w == nil || w.ResetAt != newReset.Unix() || w.UsedPct != 65 || w.Estimate == nil || w.Estimate.Total != 100 {
		t.Fatalf("post-rollover current = %+v", w)
	}
}

func TestCurrentFreshnessAndEmptyWorkload(t *testing.T) {
	for _, tc := range []struct {
		name, wantReason string
		lastAge          time.Duration
		resetAfter       time.Duration
		workload         bool
	}{
		{"stale", "stale_snapshot", 31 * time.Minute, time.Hour, true},
		{"fresh boundary", "partial_coverage", 30 * time.Minute, time.Hour, true},
		{"expired", "expired_window", 10 * time.Minute, -time.Minute, true},
		{"empty workload", "empty_workload", 0, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.credential("a", "anthropic", "pro", "", 1)
			last := testNow.Add(-tc.lastAge)
			reset := testNow.Add(tc.resetAfter).Unix()
			f.quota("a", "five_hour", last.Add(-time.Hour), 34, reset, 1)
			f.quota("a", "five_hour", last, 74, reset, 1)
			if tc.workload {
				f.request("a", "m", last, Tokens{Output: 40})
			}
			r := f.build(testNow.Add(-3*time.Hour), testNow.Add(time.Second), "five_hour")
			w := r.Accounts[0].Current
			if w == nil || w.Reason != tc.wantReason || w.ObservedAt != last.Unix() || w.UsedPct != 74 {
				t.Fatalf("current = %+v", w)
			}
			if (w.Estimate != nil) != (tc.wantReason == "partial_coverage") {
				t.Fatalf("current estimate freshness = %+v", w)
			}
			if (len(r.CapacityHistory) == 1) != tc.workload {
				t.Fatalf("historical valid intervals lost due to wall-clock age: %+v", r.CapacityHistory)
			}
		})
	}
}

func TestStalePredecessorAndQuotaSelection(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	old := testNow.Add(-20 * 24 * time.Hour)
	f.quota("a", "seven_day", old, 74, old.Add(time.Hour).Unix(), 1)
	// A fresh five-hour sample must not be mistaken for a seven-day reading.
	f.quota("a", "five_hour", testNow, 20, testNow.Add(time.Hour).Unix(), 1)
	r := f.build(testNow.Add(-time.Hour), testNow.Add(time.Second), "seven_day")
	w := r.Accounts[0].Current
	if w == nil || w.ObservedAt != old.Unix() || w.UsedPct != 74 || w.Estimate != nil || w.Reason != "unobserved_quota" {
		t.Fatalf("old predecessor/latest unobserved = %+v", w)
	}
	f.exec(`DELETE FROM usage_history WHERE captured_at=?`, testNow.Unix())
	r = f.build(testNow.Add(-time.Hour), testNow.Add(time.Second), "seven_day")
	if w = r.Accounts[0].Current; w.Reason != "stale_snapshot" || w.UsedPct != 74 {
		t.Fatalf("missing stale predecessor = %+v", w)
	}
}

func TestHistoricalAndCurrentObservationRangesAreDisjointAndBounded(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	old := testNow.Add(-365 * 24 * time.Hour)
	for _, baseline := range []time.Time{old, testNow.Add(-time.Hour)} {
		f.quota("a", "seven_day", baseline, 20, baseline.Add(24*time.Hour).Unix(), 1)
		f.quota("a", "seven_day", baseline.Add(time.Hour), 40, baseline.Add(24*time.Hour).Unix(), 1)
	}
	for i := 0; i < 10; i++ {
		ts := testNow.Add(time.Duration(-100+i) * 24 * time.Hour)
		f.quota("a", "seven_day", ts, 74, ts.Add(time.Hour).Unix(), 1)
	}
	f.request("a", "historical", old.Add(time.Minute), Tokens{Output: 20})
	f.request("a", "current", testNow.Add(-time.Minute), Tokens{Output: 40})
	r := f.build(old, old.Add(2*time.Hour), "seven_day")
	if r.Tokens.Total != 20 || len(r.CapacityHistory) != 1 || r.CapacityHistory[0].Estimate.Total != 100 || r.Accounts[0].Current.Estimate == nil || r.Accounts[0].Current.Estimate.Total != 200 {
		t.Fatalf("historical/current alignment = %+v", r)
	}
	conn, err := f.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	b := builder{conn: conn, report: r, duration: int64(7 * 24 * time.Hour / time.Second)}
	obs, err := b.readObservations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Two historical, two current, and a single stale predecessor. The other
	// nine readings in the intervening year are not fetched or budgeted.
	if len(obs["a"]) != 5 {
		t.Fatalf("fetched unrelated intervening quota history: %d rows", len(obs["a"]))
	}
}

func TestCleanCalibrationSurvivesMalformedCountersOutsideItsInterval(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	reset := testNow.Add(24 * time.Hour).Unix()
	f.quota("a", "seven_day", testNow.Add(-time.Hour), 20, reset, 1)
	f.quota("a", "seven_day", testNow, 40, reset, 1)
	f.request("a", "old-malformed", testNow.Add(-48*time.Hour), Tokens{Input: -10})
	f.request("a", "clean", testNow.Add(-time.Minute), Tokens{Output: 20})
	r := f.build(testNow.Add(-2*time.Hour), testNow.Add(time.Second), "seven_day")
	w := r.Accounts[0].Current
	if w.Estimate == nil || w.Estimate.Total != 100 || w.Reason != "partial_coverage" || w.Tokens.Total != 20 || w.Requests != 2 {
		t.Fatalf("unmatched old malformed counter suppressed clean calibration: %+v", w)
	}
}

func TestObservationDecoderRejectsMalformedValues(t *testing.T) {
	ts, duration := testNow.Unix(), int64(5*time.Hour/time.Second)
	for _, pct := range []any{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 100.1, nil, "74"} {
		q, err := decodeObservation(ts, pct, ts+3600, int64(1), duration, ts)
		if err != nil || q.valid || q.reason != "invalid_quota" {
			t.Fatalf("malformed pct %v decoded as %+v err=%v", pct, q, err)
		}
	}
	for _, reset := range []any{nil, "later", ts - 1, ts, ts + duration + resetTolerance + 1, math.MaxInt64} {
		q, err := decodeObservation(ts, 74.0, reset, int64(1), duration, ts)
		if err != nil || q.valid || q.reason != "invalid_reset" {
			t.Fatalf("malformed reset %v decoded as %+v err=%v", reset, q, err)
		}
	}
	for _, timestamp := range []any{"yesterday", int64(-1), ts + 1} {
		if _, err := decodeObservation(timestamp, 74.0, ts+3600, int64(1), duration, ts); err == nil {
			t.Fatalf("invalid timestamp %v accepted", timestamp)
		}
	}
}

func TestQuotaObservationLimitIsExplicit(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded 100k-row fixture")
	}
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	f.exec(`WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<?)
		INSERT INTO usage_history(credential_id,captured_at,seven_day_pct,seven_day_resets_at,seven_day_observed)
		SELECT 'a',?-x,20,?,1 FROM n`, maxQuotaRows+1, testNow.Unix(), testNow.Add(24*time.Hour).Unix())
	f.request("a", "m", testNow.Add(-time.Minute), Tokens{Output: 7})
	r := f.build(testNow.Add(-48*time.Hour), testNow.Add(time.Second), "seven_day")
	if !r.Truncated || r.Requests != 1 || r.Tokens.Total != 7 || len(r.CapacityHistory) != 0 || !strings.Contains(strings.Join(r.Notes, " "), "limit was reached") {
		t.Fatalf("truncated report = requests %d tokens %+v history %d notes %+v", r.Requests, r.Tokens, len(r.CapacityHistory), r.Notes)
	}
}
