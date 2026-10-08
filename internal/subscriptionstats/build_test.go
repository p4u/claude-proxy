package subscriptionstats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t  *testing.T
	db *store.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &fixture{t: t, db: db}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(f.t.Context(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) credential(id, provider, plan, tier string, weight int) {
	f.t.Helper()
	f.exec(`INSERT INTO credentials(id,label,provider,subscription_type,rate_limit_tier,access_token,refresh_token,expires_at,status,weight,created_at)
		VALUES(?,?,?,?,?,'access','refresh',?,'active',?,?)`, id, "Account "+id, provider, plan, tier, testNow.Add(24*time.Hour).Unix(), weight, testNow.Add(-100*24*time.Hour).Unix())
}

func (f *fixture) request(id, model string, ts time.Time, tokens Tokens) {
	f.t.Helper()
	f.exec(`INSERT INTO request_log(credential_id,ts,model,path,status_code,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens)
		VALUES(?,?,?,'/v1/messages',200,?,?,?,?)`, id, ts.Unix(), model, tokens.Input, tokens.Output, tokens.CacheCreation, tokens.CacheRead)
}

func (f *fixture) quota(id, window string, ts time.Time, pct, reset, observed any) {
	f.t.Helper()
	if window != "five_hour" && window != "seven_day" {
		f.t.Fatal("invalid test window")
	}
	f.exec(`INSERT INTO usage_history(credential_id,captured_at,`+window+`_pct,`+window+`_resets_at,`+window+`_observed) VALUES(?,?,?,?,?)`, id, ts.Unix(), pct, reset, observed)
}

func (f *fixture) build(from, to time.Time, window string) *Report {
	f.t.Helper()
	r, err := Build(f.t.Context(), f.db.DB, from, to, testNow, window)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func accountByID(t *testing.T, r *Report, id string) Account {
	t.Helper()
	for _, a := range r.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %q absent", id)
	return Account{}
}

func TestMatchedDeltaNotWholePeriodOrObservedPercentage(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "default_claude_max_20x", 1)
	windowStart := testNow.Add(-48 * time.Hour)
	reset := windowStart.Add(7 * 24 * time.Hour)
	first := testNow.Add(-2 * time.Hour)
	for i, pct := range []int{34, 44, 54, 64, 74} {
		f.quota("a", "seven_day", first.Add(time.Duration(i)*30*time.Minute), pct, reset.Unix(), 1)
	}
	f.request("a", "claude-a", windowStart, Tokens{Input: 9000})
	f.request("a", "claude-a", first, Tokens{Input: 100}) // Excluded from (first,last].
	f.request("a", "claude-a", first.Add(time.Second), Tokens{Input: 100, Output: 20, CacheCreation: 30, CacheRead: 50})
	f.request("a", "claude-b", testNow, Tokens{Input: 300, Output: 60, CacheCreation: 90, CacheRead: 150})
	r := f.build(testNow.Add(-3*time.Hour), testNow.Add(time.Second), "seven_day")
	if r.Requests != 3 || r.Tokens != (Tokens{Input: 500, Output: 80, CacheCreation: 120, CacheRead: 200, Total: 900}) {
		t.Fatalf("period actuals = requests %d tokens %+v", r.Requests, r.Tokens)
	}
	if len(r.CapacityHistory) != 1 {
		t.Fatalf("history = %+v", r.CapacityHistory)
	}
	s := r.CapacityHistory[0]
	want := Tokens{Input: 1000, Output: 200, CacheCreation: 300, CacheRead: 500, Total: 2000}
	if s.Start != first.Unix() || s.End != testNow.Unix() || s.DeltaPct != 40 || s.Samples != 5 || s.Tokens.Total != 800 || s.Estimate != want {
		t.Fatalf("matched interval = %+v, want estimate %+v", s, want)
	}
	current := accountByID(t, r, "a").Current
	if current == nil || current.UsedPct != 74 || current.Tokens.Total != 9900 || current.Requests != 4 || current.Estimate == nil || *current.Estimate != want || current.Reason != "partial_coverage" {
		t.Fatalf("current = %+v", current)
	}
	// Narrowing the report cannot reuse unmatched period tokens against 74% or
	// against an observation outside the selected interval. Current stays fixed.
	narrow := f.build(testNow.Add(-time.Hour), testNow.Add(time.Second), "seven_day")
	if narrow.Tokens.Total != 600 || len(narrow.CapacityHistory) != 1 || narrow.CapacityHistory[0].DeltaPct != 20 || narrow.CapacityHistory[0].Estimate.Total != 3000 {
		t.Fatalf("narrow report = %+v", narrow)
	}
	if got := accountByID(t, narrow, "a").Current; got.Tokens.Total != current.Tokens.Total || *got.Estimate != *current.Estimate {
		t.Fatalf("current changed with report range: %+v", got)
	}
	tooNarrow := f.build(testNow.Add(-10*time.Minute), testNow.Add(time.Second), "seven_day")
	if len(tooNarrow.CapacityHistory) != 0 || tooNarrow.Groups[0].Estimate != nil {
		t.Fatal("unmatched historical observations produced a period estimate")
	}
}

func TestCurrentMetadataAttributionAndCounterNormalization(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		id, provider, plan, tier string
		weight                   int
	}{
		{"max-unknown", "anthropic", "max", "", 20},
		{"max-five", "anthropic", "max", "default_claude_max_5x", 1},
		{"max-five-alias", "anthropic", "max", "5x", 999},
		{"max-twenty", "anthropic", "max", "default_claude_max_20x", 1},
		{"team-unknown", "anthropic", "team", "", 20},
		{"openai", "custom_openai", "", "", 1},
		{"deleted", "custom_openai", "pro", "", 1},
		{"gateway_codex", "codex", "pro", "20x", 5},
		{"gateway_gemini", "gemini", "ultra", "5x", 100},
		{"glm", "glm", "pro", "", 1},
		{"mimo", "mimo", "max", "", 1},
		{"custom", "custom", "", "", 1},
	}
	for _, c := range cases {
		f.credential(c.id, c.provider, c.plan, c.tier, c.weight)
		f.request(c.id, "claude-do-not-infer-provider", testNow.Add(-time.Hour), Tokens{Input: 100, Output: 10, CacheCreation: 20, CacheRead: 30})
	}
	f.request("openai", "other", testNow.Add(-time.Minute), Tokens{Input: 5, CacheRead: 10})
	f.exec(`DELETE FROM credentials WHERE id='deleted'`)
	f.request("", "gpt-do-not-infer-provider", testNow.Add(-time.Hour), Tokens{Output: 2})
	// Even a stray gateway quota row cannot manufacture per-sidecar-account
	// attribution or a capacity denominator from its provider-wide traffic.
	for _, id := range []string{"gateway_codex", "gateway_gemini", "glm", "mimo", "custom"} {
		f.quota(id, "seven_day", testNow.Add(-time.Hour), 20, testNow.Add(time.Hour).Unix(), 1)
		f.quota(id, "seven_day", testNow, 74, testNow.Add(time.Hour).Unix(), 1)
	}
	r := f.build(testNow.Add(-24*time.Hour), testNow.Add(time.Second), "seven_day")
	if a := accountByID(t, r, "openai"); a.Tokens != (Tokens{Input: 70, Output: 10, CacheCreation: 20, CacheRead: 40, Total: 140}) {
		t.Fatalf("OpenAI normalization = %+v", a.Tokens)
	}
	if a := accountByID(t, r, "max-five"); a.Tokens.Total != 160 || a.Tokens.Input != 100 {
		t.Fatalf("ordinary counters were double-subtracted: %+v", a.Tokens)
	}
	five, alias := accountByID(t, r, "max-five"), accountByID(t, r, "max-five-alias")
	unknown, twenty := accountByID(t, r, "max-unknown"), accountByID(t, r, "max-twenty")
	if five.GroupKey != alias.GroupKey || five.Tier != "5x" || unknown.Tier != "unknown" || twenty.Tier != "20x" || unknown.GroupKey == twenty.GroupKey {
		t.Fatalf("tiers inferred from weights or not normalized: %+v %+v %+v %+v", five, alias, unknown, twenty)
	}
	for _, id := range []string{"deleted", ""} {
		a := accountByID(t, r, id)
		if a.Attribution != "unknown" || a.Provider != "unknown" || a.Plan != "unknown" || a.Current != nil {
			t.Fatalf("unknown attribution = %+v", a)
		}
	}
	if a := accountByID(t, r, "deleted"); a.Tokens.Total != 160 {
		t.Fatalf("deleted row guessed an unrecoverable normalization: %+v", a)
	}
	for _, id := range []string{"gateway_codex", "gateway_gemini"} {
		a := accountByID(t, r, id)
		if a.Attribution != "gateway" || a.Plan != "all" || a.Tier != "unknown" || a.Current != nil || a.Tokens.Total != 160 {
			t.Fatalf("gateway attribution = %+v", a)
		}
	}
	if len(r.CapacityHistory) != 0 {
		t.Fatalf("unattributable estimates: %+v", r.CapacityHistory)
	}
	for _, g := range r.Groups {
		if g.Key == five.GroupKey && (g.Credentials != 2 || g.Requests != 2 || g.Tokens.Total != 320) {
			t.Fatalf("five-x cohort = %+v", g)
		}
		if g.Tier == "unknown" && g.Provider == "anthropic" && !strings.Contains(g.Label, "tier unknown") {
			t.Fatalf("unknown-tier label = %q", g.Label)
		}
	}
	// The same old requests move cohorts when current metadata changes. The
	// report states this retrospective limitation instead of inventing history.
	f.exec(`UPDATE credentials SET rate_limit_tier='default_claude_max_5x',weight=777 WHERE id='max-unknown'`)
	after := f.build(testNow.Add(-24*time.Hour), testNow.Add(time.Second), "seven_day")
	if a := accountByID(t, after, "max-unknown"); a.GroupKey != five.GroupKey || a.Tokens != unknown.Tokens {
		t.Fatalf("current metadata did not regroup old actuals: %+v", a)
	}
}

func TestRangeUTCDaysAndRecordedModels(t *testing.T) {
	f := newFixture(t)
	f.credential("a", "anthropic", "max", "20x", 1)
	from, to := testNow.Add(-48*time.Hour), testNow
	f.request("a", "outside", from.Add(-time.Second), Tokens{Output: 1000})
	f.request("a", "m1", from, Tokens{Input: 1, Output: 2, CacheCreation: 3, CacheRead: 4})
	midnight := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	f.request("a", "m1", midnight.Add(-time.Second), Tokens{Output: 10})
	f.request("a", "m2", midnight, Tokens{Output: 20})
	f.request("a", "", to.Add(-time.Second), Tokens{Output: 30})
	f.request("a", "outside", to, Tokens{Output: 1000})
	r := f.build(from.In(time.FixedZone("west", -7*60*60)), to, "five_hour")
	if r.Requests != 4 || r.Tokens.Total != 70 || len(r.Daily) != 3 || len(r.Models) != 3 {
		t.Fatalf("range actuals = %+v", r)
	}
	for i, want := range []int64{20, 20, 30} {
		d := r.Daily[i]
		if d.TS%86400 != 0 || d.Tokens.Total != want {
			t.Fatalf("UTC day %d = %+v", i, d)
		}
	}
	for _, model := range r.Models {
		if model.Model == "outside" || model.Tokens.Total != map[string]int64{"m1": 20, "m2": 20, "": 30}[model.Model] {
			t.Fatalf("model = %+v", model)
		}
	}
	var groupTotal, dayTotal, modelTotal Tokens
	for _, g := range r.Groups {
		if err := addTokens(&groupTotal, g.Tokens); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range r.Daily {
		if err := addTokens(&dayTotal, d.Tokens); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range r.Models {
		if err := addTokens(&modelTotal, m.Tokens); err != nil {
			t.Fatal(err)
		}
	}
	if groupTotal != r.Tokens || dayTotal != r.Tokens || modelTotal != r.Tokens {
		t.Fatalf("breakdowns do not reconcile: report=%+v groups=%+v days=%+v models=%+v", r.Tokens, groupTotal, dayTotal, modelTotal)
	}
}

func TestGroupMedianAndIQRAreAccountCycleSamples(t *testing.T) {
	f := newFixture(t)
	for i, total := range []int64{30, 60, 90, 300} {
		id := fmt.Sprintf("a%d", i)
		f.credential(id, "anthropic", "max", "20x", i+1)
		f.quota(id, "seven_day", testNow.Add(-time.Hour), 44, testNow.Add(time.Hour).Unix(), 1)
		f.quota(id, "seven_day", testNow, 74, testNow.Add(time.Hour).Unix(), 1)
		f.request(id, "model", testNow.Add(-time.Minute), Tokens{Output: total})
	}
	r := f.build(testNow.Add(-2*time.Hour), testNow.Add(time.Second), "seven_day")
	if len(r.Groups) != 1 || len(r.CapacityHistory) != 4 {
		t.Fatalf("groups/history = %+v / %+v", r.Groups, r.CapacityHistory)
	}
	g := r.Groups[0]
	if g.EstimateSamples != 4 || g.Estimate == nil || g.Estimate.Total != 250 || g.EstimateLow.Total != 175 || g.EstimateHigh.Total != 475 {
		t.Fatalf("median/IQR = %+v", g)
	}
	// Different categories can have different median samples. The reported
	// total quantile remains a quantile of totals, not a sum of medians.
	values := []Tokens{{Input: 100, Total: 100}, {Output: 100, Total: 100}, {CacheRead: 100, Total: 100}}
	q := tokenQuantile(values, .5)
	if q.Total != 100 || q.Input+q.Output+q.CacheRead != 0 {
		t.Fatalf("marginal medians = %+v", q)
	}
}

func TestEmptyReportAndJSONContract(t *testing.T) {
	f := newFixture(t)
	r := f.build(testNow.Add(-time.Hour), testNow, "seven_day")
	if r.Requests != 0 || r.Tokens.Total != 0 || r.Truncated {
		t.Fatalf("empty = %+v", r)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"groups", "accounts", "daily", "capacity_history", "models"} {
		if string(fields[name]) != "[]" {
			t.Fatalf("%s is not []: %s", name, fields[name])
		}
	}
	for _, name := range []string{"from", "to", "as_of", "window", "requests", "tokens", "notes", "truncated"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing field %s", name)
		}
	}
	f.credential("a", "anthropic", "team", "", 20)
	r = f.build(testNow.Add(-time.Hour), testNow, "seven_day")
	if r.Accounts[0].Current == nil || r.Accounts[0].Current.Reason != "missing_quota" || r.Groups[0].Estimate != nil {
		t.Fatalf("missing quota = %+v", r)
	}
	data, err = json.Marshal(r)
	if err != nil || !strings.Contains(string(data), `"estimate":null`) || !strings.Contains(string(data), `"estimate_low":null`) {
		t.Fatalf("nullable estimates: %s err=%v", data, err)
	}
}

func TestBoundsCancellationAndErrors(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name          string
		from, to, now time.Time
		window        string
	}{
		{"equal", testNow, testNow, testNow, "seven_day"},
		{"reversed", testNow, testNow.Add(-time.Hour), testNow, "seven_day"},
		{"too long", testNow.Add(-91 * 24 * time.Hour), testNow, testNow, "seven_day"},
		{"future", testNow.Add(-time.Hour), testNow.Add(2 * time.Second), testNow, "seven_day"},
		{"negative", time.Unix(-1, 0), testNow, testNow, "seven_day"},
		{"zero", time.Time{}, testNow, testNow, "seven_day"},
		{"zero now", testNow.Add(-time.Hour), testNow, time.Time{}, "seven_day"},
		{"window", testNow.Add(-time.Hour), testNow, testNow, "seven_day);DROP TABLE credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(t.Context(), f.db.DB, tc.from, tc.to, tc.now, tc.window); err == nil {
				t.Fatal("invalid bounds accepted")
			}
		})
	}
	if _, err := Build(t.Context(), nil, testNow.Add(-time.Hour), testNow, testNow, "seven_day"); err == nil {
		t.Fatal("nil database accepted")
	}
	f.build(testNow.Add(-90*24*time.Hour), testNow.Add(time.Second), "seven_day")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Build(ctx, f.db.DB, testNow.Add(-time.Hour), testNow, testNow, "five_hour"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	// Exercise cancellation during SQL execution, not just the preflight check.
	conn, err := f.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	b := builder{conn: conn}
	queryCtx, stop := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer stop()
	err = b.readIntervalBatch(queryCtx, `WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<100000000)
		SELECT 0,COUNT(*),0,0,0,0,0 FROM n`, nil, []*tokenInterval{{}}, 0, 1)
	if err == nil || queryCtx.Err() == nil {
		t.Fatalf("running SQL ignored cancellation: %v / %v", err, queryCtx.Err())
	}
	f.exec(`DROP TABLE request_log`)
	if _, err := Build(t.Context(), f.db.DB, testNow.Add(-time.Hour), testNow, testNow, "five_hour"); err == nil {
		t.Fatal("SQL failure hidden")
	}
}

func TestMalformedCountersAndOverflow(t *testing.T) {
	t.Run("malformed counters", func(t *testing.T) {
		f := newFixture(t)
		f.credential("a", "anthropic", "max", "5x", 1)
		f.quota("a", "seven_day", testNow.Add(-time.Hour), 20, testNow.Add(time.Hour).Unix(), 1)
		f.quota("a", "seven_day", testNow, 74, testNow.Add(time.Hour).Unix(), 1)
		f.request("a", "m", testNow.Add(-time.Minute), Tokens{Input: -100, Output: 20})
		f.exec(`UPDATE request_log SET cache_read_tokens='bad'`)
		r := f.build(testNow.Add(-time.Hour), testNow.Add(time.Second), "seven_day")
		if r.Tokens.Total != 20 || len(r.CapacityHistory) != 0 || r.Accounts[0].Current.Reason != "invalid_token_counts" {
			t.Fatalf("malformed counters were calibrated: %+v", r)
		}
	})
	t.Run("aggregate sum overflow", func(t *testing.T) {
		f := newFixture(t)
		f.request("missing", "m", testNow.Add(-time.Minute), Tokens{Input: math.MaxInt64, Output: 1})
		if _, err := Build(t.Context(), f.db.DB, testNow.Add(-time.Hour), testNow, testNow, "seven_day"); err == nil {
			t.Fatal("token total overflow accepted")
		}
	})
	for _, delta := range []float64{0, -1, 9.9, 101, math.NaN(), math.Inf(1)} {
		if _, ok := scaleTokens(Tokens{Output: 10, Total: 10}, delta); ok {
			t.Fatalf("invalid denominator %v accepted", delta)
		}
	}
	if _, ok := scaleTokens(Tokens{Output: math.MaxInt64 / 2, Total: math.MaxInt64 / 2}, 10); ok {
		t.Fatal("scaled overflow accepted")
	}
	q := tokenQuantile([]Tokens{{Total: math.MaxInt64 - 4}, {Total: math.MaxInt64}}, .75)
	if q.Total != math.MaxInt64-1 {
		t.Fatalf("large integer quantile overflowed: %+v", q)
	}
}

func TestIntervalAggregationBatches(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < intervalBatch+7; i++ {
		id := fmt.Sprintf("account-%03d", i)
		f.credential(id, "anthropic", "max", "20x", 1)
		f.quota(id, "five_hour", testNow.Add(-time.Hour), 20, testNow.Add(time.Hour).Unix(), 1)
		f.quota(id, "five_hour", testNow, 40, testNow.Add(time.Hour).Unix(), 1)
		f.request(id, "m", testNow.Add(-time.Minute), Tokens{Output: 10})
	}
	r := f.build(testNow.Add(-2*time.Hour), testNow.Add(time.Second), "five_hour")
	if len(r.CapacityHistory) != intervalBatch+7 || r.Groups[0].EstimateSamples != intervalBatch+7 || r.Groups[0].Estimate.Total != 50 {
		t.Fatalf("batch aggregation = %+v", r.Groups)
	}
	if slices.ContainsFunc(r.Accounts, func(a Account) bool {
		return a.Current == nil || a.Current.Estimate == nil || a.Current.Estimate.Total != 50
	}) {
		t.Fatal("a current window was lost across an interval batch boundary")
	}
}

// Compile-time contract used by the API owner: the package accepts *sql.DB,
// not the store wrapper, and keeps all report timestamps at integer seconds.
var _ func(context.Context, *sql.DB, time.Time, time.Time, time.Time, string) (*Report, error) = Build
