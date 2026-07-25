package usertoken

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

func TestUnitsWeighting(t *testing.T) {
	// output*5 + input*1 + cache_creation*1.25 + cache_read*0.1
	got := Units(100, 10, 40, 1000)
	want := 100*1.0 + 10*5.0 + 40*1.25 + 1000*0.1
	if got != want {
		t.Fatalf("Units = %v, want %v", got, want)
	}
	if Units(0, 0, 0, 0) != 0 {
		t.Fatal("zero tokens must be zero units")
	}
	// Weights must match the documented ratios exactly.
	if UnitWeightOutput != 5.0 || UnitWeightInput != 1.0 ||
		UnitWeightCacheCreation != 1.25 || UnitWeightCacheRead != 0.1 {
		t.Fatal("unit weights drifted from the contract")
	}
}

// TestUnitsSQLMatchesGo proves the SQL expression and the Go function agree,
// which is the whole point of deriving UnitsSQL from the constants.
func TestUnitsSQLMatchesGo(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, err := Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cases := [][4]int64{{100, 10, 40, 1000}, {0, 0, 0, 0}, {7, 3, 1, 9}, {1e6, 5e5, 2e5, 4e6}}
	var want float64
	for _, c := range cases {
		addUsage(t, db, ut.ID, time.Now(), c[0], c[1], c[2], c[3])
		want += Units(c[0], c[1], c[2], c[3])
	}
	var got float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(`+UnitsSQL+`),0) FROM request_log WHERE user_token_id=?`,
		ut.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("SQL units = %v, Go units = %v", got, want)
	}
}

// addUsage inserts one request_log row with the given tokens at ts.
func addUsage(t *testing.T, db *store.DB, userID string, ts time.Time, in, out, cc, cr int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO request_log
		  (user_token_id, credential_id, conv_id, ts, path, status_code,
		   input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
		VALUES (?, 'cred_x', 'conv', ?, '/v1/messages', 200, ?, ?, ?, ?)`,
		userID, ts.Unix(), in, out, cc, cr)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLimitStatusUnlimitedSkipsEverything(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, err := Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	addUsage(t, db, ut.ID, time.Now(), 1e6, 1e6, 0, 0)

	for _, c := range [][2]int64{{0, 0}, {5000, 0}, {0, 3600}} {
		st, err := LimitStatus(ctx, db, ut.ID, c[0], c[1], time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if st.Active || st.Blocked || st.UsageUnits != 0 || st.UsagePct() != 0 {
			t.Fatalf("limit %v: expected inactive empty state, got %+v", c, st)
		}
	}
}

func TestLimitStatusUnderAndOverCap(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, err := Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 1000 input = 1000 units.
	addUsage(t, db, ut.ID, now.Add(-time.Minute), 1000, 0, 0, 0)

	st, err := LimitStatus(ctx, db, ut.ID, 5000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active || st.Blocked {
		t.Fatalf("under cap should not block: %+v", st)
	}
	if st.UsageUnits != 1000 || st.UsagePct() != 20 {
		t.Fatalf("usage = %v (%.1f%%), want 1000 (20%%)", st.UsageUnits, st.UsagePct())
	}
	if !st.BlockedUntil.IsZero() {
		t.Fatal("must not report a reset time while under the cap")
	}

	// Push over: 1000 output = 5000 units, total 6000 >= 5000.
	addUsage(t, db, ut.ID, now.Add(-30*time.Second), 0, 1000, 0, 0)
	st, err = LimitStatus(ctx, db, ut.ID, 5000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Blocked || st.UsageUnits != 6000 {
		t.Fatalf("expected blocked at 6000 units, got %+v", st)
	}
	if st.BlockedUntil.IsZero() {
		t.Fatal("blocked state must carry an unblock time")
	}
}

// Rows older than the window must not count at all.
func TestLimitStatusWindowExcludesOldRows(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, _ := Create(ctx, db, "alice")
	now := time.Now()
	addUsage(t, db, ut.ID, now.Add(-2*time.Hour), 100000, 0, 0, 0) // outside 1h window
	addUsage(t, db, ut.ID, now.Add(-10*time.Minute), 50, 0, 0, 0)

	st, err := LimitStatus(ctx, db, ut.ID, 1000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocked || st.UsageUnits != 50 {
		t.Fatalf("old rows leaked into the window: %+v", st)
	}
}

// The unblock instant is exact: it is the ts of the oldest row whose expiry
// brings the running total back under the cap, plus the window, plus one second.
func TestUnblockTimeExactMultiRowAging(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, _ := Create(ctx, db, "alice")
	now := time.Now().Truncate(time.Second)
	window := int64(3600)

	// Four rows of 1000 units each (input tokens), total 4000, limit 2500.
	// Dropping row1 -> 3000 (still >= 2500). Dropping row1+row2 -> 2000 (< 2500).
	// So the answer is row2's ts + window + 1s.
	tss := []time.Time{
		now.Add(-50 * time.Minute),
		now.Add(-40 * time.Minute),
		now.Add(-30 * time.Minute),
		now.Add(-20 * time.Minute),
	}
	for _, ts := range tss {
		addUsage(t, db, ut.ID, ts, 1000, 0, 0, 0)
	}

	st, err := LimitStatus(ctx, db, ut.ID, 2500, window, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Blocked || st.UsageUnits != 4000 {
		t.Fatalf("expected blocked at 4000, got %+v", st)
	}
	want := tss[1].Add(time.Duration(window) * time.Second).Add(time.Second)
	if !st.BlockedUntil.Equal(want) {
		t.Fatalf("BlockedUntil = %v, want %v", st.BlockedUntil.UTC(), want.UTC())
	}

	// Sanity-check the answer against the definition: at BlockedUntil the
	// remaining window usage must be under the limit, and one second earlier
	// it must still be at or over it.
	assertUsageAt := func(at time.Time, wantUnder bool) {
		t.Helper()
		var sum float64
		if err := db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(`+UnitsSQL+`),0) FROM request_log
			 WHERE user_token_id=? AND ts >= ?`,
			ut.ID, at.Add(-time.Duration(window)*time.Second).Unix()).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		if under := sum < 2500; under != wantUnder {
			t.Fatalf("at %v usage=%v under=%v want under=%v", at.UTC(), sum, under, wantUnder)
		}
	}
	assertUsageAt(st.BlockedUntil, true)
	assertUsageAt(st.BlockedUntil.Add(-time.Second), false)
}

// A single dominant row: unblock is that row's ts + window + 1s.
func TestUnblockTimeSingleRow(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, _ := Create(ctx, db, "alice")
	now := time.Now().Truncate(time.Second)
	ts := now.Add(-10 * time.Minute)
	addUsage(t, db, ut.ID, ts, 0, 2000, 0, 0) // 10000 units

	st, err := LimitStatus(ctx, db, ut.ID, 5000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	want := ts.Add(time.Hour).Add(time.Second)
	if !st.BlockedUntil.Equal(want) {
		t.Fatalf("BlockedUntil = %v, want %v", st.BlockedUntil.UTC(), want.UTC())
	}
	if got := st.RetryAfterSeconds(now); got != int(time.Until(want).Seconds())+1 && got < 1 {
		t.Fatalf("RetryAfterSeconds = %d, want >= 1", got)
	}
}

func TestRetryAfterNeverBelowOne(t *testing.T) {
	st := LimitState{Blocked: true, BlockedUntil: time.Now().Add(-time.Hour)}
	if got := st.RetryAfterSeconds(time.Now()); got != 1 {
		t.Fatalf("RetryAfterSeconds = %d, want 1", got)
	}
	if got := (LimitState{}).RetryAfterSeconds(time.Now()); got != 0 {
		t.Fatalf("unblocked RetryAfterSeconds = %d, want 0", got)
	}
}

func TestSetLimitRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, _ := Create(ctx, db, "alice")
	if ut.LimitUnits != 0 || ut.LimitWindowSeconds != 0 {
		t.Fatal("new users must default to unlimited")
	}
	if err := SetLimit(ctx, db, ut.ID, 5_000_000, 86400); err != nil {
		t.Fatal(err)
	}
	got, err := Get(ctx, db, ut.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LimitUnits != 5_000_000 || got.LimitWindowSeconds != 86400 {
		t.Fatalf("Get limit = %d/%d", got.LimitUnits, got.LimitWindowSeconds)
	}
	byTok, err := LookupByToken(ctx, db, ut.Token)
	if err != nil {
		t.Fatal(err)
	}
	if byTok.LimitUnits != 5_000_000 || byTok.LimitWindowSeconds != 86400 {
		t.Fatal("LookupByToken must carry the limit (hot path, no extra query)")
	}
	list, err := List(ctx, db)
	if err != nil || len(list) != 1 || list[0].LimitUnits != 5_000_000 {
		t.Fatalf("List must carry the limit: %+v %v", list, err)
	}
	if err := SetLimit(ctx, db, ut.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = Get(ctx, db, ut.ID)
	if HasLimit(got.LimitUnits, got.LimitWindowSeconds) {
		t.Fatal("both zero must clear the limit")
	}
	if err := SetLimit(ctx, db, "utok_nope", 1, 1); err != ErrNotFound {
		t.Fatalf("SetLimit on missing id = %v, want ErrNotFound", err)
	}
}

func TestParseAndFormatUnits(t *testing.T) {
	ok := map[string]int64{
		"100": 100, "5M": 5_000_000, "500k": 500_000, "1.5M": 1_500_000,
		"2G": 2_000_000_000, " 42 ": 42, "0": 0,
	}
	for in, want := range ok {
		got, err := ParseUnits(in)
		if err != nil || got != want {
			t.Fatalf("ParseUnits(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5", "-1M", "5X2"} {
		if _, err := ParseUnits(bad); err == nil {
			t.Fatalf("ParseUnits(%q) should fail", bad)
		}
	}
	fmts := map[float64]string{
		5_000_000: "5M", 3_700_000: "3.7M", 812_000: "812K", 940: "940", 0: "0",
		2_000_000_000: "2G",
	}
	for in, want := range fmts {
		if got := FormatUnits(in); got != want {
			t.Fatalf("FormatUnits(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatWindowAndCountdown(t *testing.T) {
	w := map[time.Duration]string{
		time.Hour: "1h", 24 * time.Hour: "24h", 7 * 24 * time.Hour: "7d",
		6 * time.Hour: "6h", 90 * time.Minute: "90m", 45 * time.Second: "45s",
	}
	for in, want := range w {
		if got := FormatWindow(in); got != want {
			t.Fatalf("FormatWindow(%v) = %q, want %q", in, got, want)
		}
	}
	c := map[time.Duration]string{
		3*time.Hour + 12*time.Minute: "3h 12m",
		12 * time.Minute:             "12m",
		45 * time.Second:             "45s",
		0:                            "1s",
	}
	for in, want := range c {
		if got := FormatCountdown(in); got != want {
			t.Fatalf("FormatCountdown(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatGroupedAndQuotaMessage(t *testing.T) {
	g := map[int64]string{0: "0", 999: "999", 1000: "1,000", 5204118: "5,204,118",
		5000000: "5,000,000", -1234: "-1,234"}
	for in, want := range g {
		if got := FormatGrouped(in); got != want {
			t.Fatalf("FormatGrouped(%d) = %q, want %q", in, got, want)
		}
	}
	now := time.Date(2026, 7, 25, 11, 20, 0, 0, time.UTC)
	st := LimitState{
		Active: true, LimitUnits: 5_000_000, Window: 24 * time.Hour,
		UsageUnits: 5_204_118, Blocked: true,
		BlockedUntil: time.Date(2026, 7, 25, 14, 32, 0, 0, time.UTC),
	}
	want := "proxy: usage limit reached - 5,000,000 units per 24h (used 5,204,118). " +
		"Resets at 2026-07-25 14:32 UTC (in 3h 12m)."
	if got := st.QuotaMessage(now); got != want {
		t.Fatalf("QuotaMessage:\n got %q\nwant %q", got, want)
	}
}
