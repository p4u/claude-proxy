package usertoken

import (
	"context"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

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

// Only output tokens are metered. Input, cache writes and (above all) cache
// reads are ignored: cache reads dwarf everything else, which is what made the
// earlier weighted-units metric unusable.
func TestOnlyOutputTokensCount(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, err := Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	addUsage(t, db, ut.ID, now.Add(-time.Minute), 5_000_000, 100, 2_000_000, 380_000_000)

	st, err := LimitStatus(ctx, db, ut.ID, 1000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.UsageOutputTokens != 100 || st.Blocked {
		t.Fatalf("usage = %d (blocked=%v), want 100 output tokens and no block",
			st.UsageOutputTokens, st.Blocked)
	}

	got, err := OutputTokensInWindow(ctx, db, ut.ID, time.Hour, now)
	if err != nil || got != 100 {
		t.Fatalf("OutputTokensInWindow = %d, %v; want 100", got, err)
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
		if st.Active || st.Blocked || st.UsageOutputTokens != 0 || st.UsagePct() != 0 {
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
	addUsage(t, db, ut.ID, now.Add(-time.Minute), 0, 1000, 0, 0)

	st, err := LimitStatus(ctx, db, ut.ID, 5000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active || st.Blocked {
		t.Fatalf("under cap should not block: %+v", st)
	}
	if st.UsageOutputTokens != 1000 || st.UsagePct() != 20 {
		t.Fatalf("usage = %d (%.1f%%), want 1000 (20%%)", st.UsageOutputTokens, st.UsagePct())
	}
	if !st.BlockedUntil.IsZero() {
		t.Fatal("must not report a reset time while under the cap")
	}

	// Push over the cap: 1000 + 5000 = 6000 >= 5000.
	addUsage(t, db, ut.ID, now.Add(-30*time.Second), 0, 5000, 0, 0)
	st, err = LimitStatus(ctx, db, ut.ID, 5000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Blocked || st.UsageOutputTokens != 6000 {
		t.Fatalf("expected blocked at 6000 output tokens, got %+v", st)
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
	addUsage(t, db, ut.ID, now.Add(-2*time.Hour), 0, 100000, 0, 0) // outside 1h window
	addUsage(t, db, ut.ID, now.Add(-10*time.Minute), 0, 50, 0, 0)

	st, err := LimitStatus(ctx, db, ut.ID, 1000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocked || st.UsageOutputTokens != 50 {
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

	// Four rows of 1000 output tokens each, total 4000, limit 2500.
	// Dropping row1 -> 3000 (still >= 2500). Dropping row1+row2 -> 2000 (< 2500).
	// So the answer is row2's ts + window + 1s.
	tss := []time.Time{
		now.Add(-50 * time.Minute),
		now.Add(-40 * time.Minute),
		now.Add(-30 * time.Minute),
		now.Add(-20 * time.Minute),
	}
	for _, ts := range tss {
		addUsage(t, db, ut.ID, ts, 0, 1000, 0, 0)
	}

	st, err := LimitStatus(ctx, db, ut.ID, 2500, window, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Blocked || st.UsageOutputTokens != 4000 {
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
		sum, err := OutputTokensInWindow(ctx, db, ut.ID, time.Duration(window)*time.Second, at)
		if err != nil {
			t.Fatal(err)
		}
		if under := sum < 2500; under != wantUnder {
			t.Fatalf("at %v usage=%d under=%v want under=%v", at.UTC(), sum, under, wantUnder)
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
	addUsage(t, db, ut.ID, ts, 0, 10000, 0, 0)

	st, err := LimitStatus(ctx, db, ut.ID, 5000, 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	want := ts.Add(time.Hour).Add(time.Second)
	if !st.BlockedUntil.Equal(want) {
		t.Fatalf("BlockedUntil = %v, want %v", st.BlockedUntil.UTC(), want.UTC())
	}
	if got := st.RetryAfterSeconds(now); got < 1 {
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
	if ut.LimitOutputTokens != 0 || ut.LimitWindowSeconds != 0 {
		t.Fatal("new users must default to unlimited")
	}
	if err := SetLimit(ctx, db, ut.ID, 1_000_000, 86400); err != nil {
		t.Fatal(err)
	}
	got, err := Get(ctx, db, ut.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LimitOutputTokens != 1_000_000 || got.LimitWindowSeconds != 86400 {
		t.Fatalf("Get limit = %d/%d", got.LimitOutputTokens, got.LimitWindowSeconds)
	}
	byTok, err := LookupByToken(ctx, db, ut.Token)
	if err != nil {
		t.Fatal(err)
	}
	if byTok.LimitOutputTokens != 1_000_000 || byTok.LimitWindowSeconds != 86400 {
		t.Fatal("LookupByToken must carry the limit (hot path, no extra query)")
	}
	list, err := List(ctx, db)
	if err != nil || len(list) != 1 || list[0].LimitOutputTokens != 1_000_000 {
		t.Fatalf("List must carry the limit: %+v %v", list, err)
	}
	if err := SetLimit(ctx, db, ut.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = Get(ctx, db, ut.ID)
	if HasLimit(got.LimitOutputTokens, got.LimitWindowSeconds) {
		t.Fatal("both zero must clear the limit")
	}
	if err := SetLimit(ctx, db, "utok_nope", 1, 1); err != ErrNotFound {
		t.Fatalf("SetLimit on missing id = %v, want ErrNotFound", err)
	}
}

func TestParseAndFormatTokens(t *testing.T) {
	ok := map[string]int64{
		"100": 100, "5M": 5_000_000, "500k": 500_000, "1.5M": 1_500_000,
		"2G": 2_000_000_000, " 42 ": 42, "0": 0,
	}
	for in, want := range ok {
		got, err := ParseTokens(in)
		if err != nil || got != want {
			t.Fatalf("ParseTokens(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5", "-1M", "5X2"} {
		if _, err := ParseTokens(bad); err == nil {
			t.Fatalf("ParseTokens(%q) should fail", bad)
		}
	}
	fmts := map[int64]string{
		5_000_000: "5M", 3_700_000: "3.7M", 812_000: "812K", 940: "940", 0: "0",
		2_000_000_000: "2G",
	}
	for in, want := range fmts {
		if got := FormatTokens(in); got != want {
			t.Fatalf("FormatTokens(%v) = %q, want %q", in, got, want)
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
	g := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1043882: "1,043,882",
		1000000: "1,000,000", -1234: "-1,234"}
	for in, want := range g {
		if got := FormatGrouped(in); got != want {
			t.Fatalf("FormatGrouped(%d) = %q, want %q", in, got, want)
		}
	}
	now := time.Date(2026, 7, 25, 11, 20, 0, 0, time.UTC)
	st := LimitState{
		Active: true, LimitOutputTokens: 1_000_000, Window: 24 * time.Hour,
		UsageOutputTokens: 1_043_882, Blocked: true,
		BlockedUntil: time.Date(2026, 7, 25, 14, 32, 0, 0, time.UTC),
	}
	want := "proxy: usage limit reached - 1,000,000 output tokens per 24h (used 1,043,882). " +
		"Resets at 2026-07-25 14:32 UTC (in 3h 12m)."
	if got := st.QuotaMessage(now); got != want {
		t.Fatalf("QuotaMessage:\n got %q\nwant %q", got, want)
	}
}
