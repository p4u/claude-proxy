package subscriptionstats

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

const (
	maxRows        = 100_000
	maxQuotaRows   = 100_000
	intervalBatch  = 64 // Four parameters each, below even SQLite's old 999 limit.
	minInterval    = int64(30 * time.Minute / time.Second)
	maxSnapshotAge = int64(30 * time.Minute / time.Second)
	minDelta       = 10.0
	resetTolerance = int64(2)
)

type builder struct {
	report      *Report
	conn        *sql.Conn
	groups      map[string]*Group
	accounts    map[string]*Account
	credentials map[string]credential
	duration    int64
}

// Build reads a consistent, read-only database snapshot. It measures completed
// requests in [from,to) and uses only explicit quota observations to calibrate
// workload-conditioned 100%-quota equivalents. It never polls a provider.
//
// window must be "five_hour" or "seven_day". Ranges are second-resolution and
// at most 90 days, with one extra second allowed to include the current second.
// Query results are bounded; Truncated and Notes disclose omitted detail.
// Callers should provide a deadline appropriate for their interactive budget.
func Build(ctx context.Context, db *sql.DB, from, to, timeNow time.Time, window string) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("subscription stats: nil database")
	}
	lo, hi, now := from.Unix(), to.Unix(), timeNow.Unix()
	if from.IsZero() || to.IsZero() || timeNow.IsZero() || lo < 0 || now < 0 || hi <= lo || hi-lo > int64(90*24*time.Hour/time.Second)+1 || hi > now && hi-now > 1 {
		return nil, errors.New("subscription stats: invalid time range")
	}
	var duration time.Duration
	switch window {
	case "five_hour":
		duration = 5 * time.Hour
	case "seven_day":
		duration = 7 * 24 * time.Hour
	default:
		return nil, errors.New("subscription stats: window must be five_hour or seven_day")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("subscription stats: connection: %w", err)
	}
	defer conn.Close()
	// store.Open uses _txlock=immediate for write safety. A regular BeginTx
	// would therefore take the writer lock, even for this read-only report.
	// An explicit deferred transaction pins only a WAL read snapshot instead.
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return nil, fmt.Errorf("subscription stats: begin snapshot: %w", err)
	}
	defer func() {
		// A canceled query must not return a connection with an open transaction
		// to the pool. ROLLBACK is local and does not wait for the writer lock.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	}()
	b := &builder{
		conn: conn, duration: int64(duration / time.Second),
		groups: make(map[string]*Group), accounts: make(map[string]*Account), credentials: make(map[string]credential),
		report: &Report{
			From: lo, To: hi, AsOf: now, Window: window,
			Groups: []Group{}, Accounts: []Account{}, Daily: []Daily{}, CapacityHistory: []CapacitySample{}, Models: []Model{},
			Notes: []string{
				"Tokens are recorded proxy observations, not a complete usage ledger: response counters may be missing or partial, and traffic outside this proxy is absent.",
				"Request timestamps record response logging completion, not quota consumption time; long requests can straddle a quota snapshot. Period totals use [from,to); calibration uses (start,end].",
				"Provider, plan and rate-limit tier are current credential metadata applied retrospectively, not historical subscription records. Routing weights never determine a tier. Deleted credentials remain unknown.",
				"Gateway traffic is attributed only to its provider across all plans. The proxy cannot assign these tokens to sidecar accounts, and never divides them by account counts, quota or weights.",
				"Quota equivalents scale recorded interval tokens by 100 / observed percentage-point increase for the observed model and cache mix. They are not published allowances, billing estimates, or evidence that every request or other quota would fit.",
				"Calibration requires at least two distinct explicit observations, at least 30 minutes and at least 10 percentage points in the selected quota window. It never bridges invalid, unobserved, decreasing or changed-reset observations, and never invents a zero baseline.",
				"Current windows are independent of the selected report period. Their tokens run from nominal reset-minus-duration through the displayed snapshot; a partial_coverage reason can coexist with a valid narrower, matched-delta estimate.",
				"Group estimates and ranges are per-field median and interquartile range (Q1–Q3), with equal weight per usable account/quota cycle, not pooled tokens or confidence bounds. Marginal category quantiles need not sum to the total quantile.",
				"Custom OpenAI input excludes its recorded cache reads to prevent double counting. Deleted credentials have no recoverable provider, so that normalization cannot be inferred for their historical rows.",
			},
		},
	}
	if err := b.readActuals(ctx); err != nil {
		return nil, fmt.Errorf("subscription stats: traffic: %w", err)
	}
	if err := b.readCapacity(ctx); err != nil {
		return nil, fmt.Errorf("subscription stats: quota: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.finish()
	return b.report, nil
}

func (b *builder) truncate() {
	if !b.report.Truncated {
		b.report.Truncated = true
		b.report.Notes = append(b.report.Notes, "The detail or quota observation limit was reached. Overall period totals remain complete for recorded rows, but lists, cohort detail and calibration samples may be incomplete.")
	}
}

func (b *builder) finish() {
	samples := make(map[string][]Tokens)
	for _, s := range b.report.CapacityHistory {
		samples[s.GroupKey] = append(samples[s.GroupKey], s.Estimate)
	}
	for _, g := range b.groups {
		if estimates := samples[g.Key]; len(estimates) > 0 {
			g.EstimateSamples = len(estimates)
			g.Estimate = tokenQuantile(estimates, 0.5)
			g.EstimateLow = tokenQuantile(estimates, 0.25)
			g.EstimateHigh = tokenQuantile(estimates, 0.75)
		}
		b.report.Groups = append(b.report.Groups, *g)
	}
	for _, a := range b.accounts {
		b.report.Accounts = append(b.report.Accounts, *a)
	}
	slices.SortFunc(b.report.Groups, func(a, c Group) int { return cmp.Compare(a.Key, c.Key) })
	slices.SortFunc(b.report.Accounts, func(a, c Account) int { return cmp.Compare(a.ID, c.ID) })
	slices.SortFunc(b.report.Daily, func(a, c Daily) int {
		if a.TS != c.TS {
			return cmp.Compare(a.TS, c.TS)
		}
		return cmp.Compare(a.GroupKey, c.GroupKey)
	})
	slices.SortFunc(b.report.Models, func(a, c Model) int {
		if a.GroupKey != c.GroupKey {
			return cmp.Compare(a.GroupKey, c.GroupKey)
		}
		return cmp.Compare(a.Model, c.Model)
	})
	slices.SortFunc(b.report.CapacityHistory, func(a, c CapacitySample) int {
		if a.End != c.End {
			return cmp.Compare(a.End, c.End)
		}
		return cmp.Compare(a.CredentialID, c.CredentialID)
	})
}

func addInt(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, errors.New("token or request count overflow")
	}
	return a + b, nil
}

func (t *Tokens) total() error {
	n, err := addInt(t.Input, t.Output)
	if err == nil {
		n, err = addInt(n, t.CacheCreation)
	}
	if err == nil {
		n, err = addInt(n, t.CacheRead)
	}
	t.Total = n
	return err
}

func addTokens(dst *Tokens, src Tokens) error {
	for _, pair := range []struct {
		dst *int64
		src int64
	}{{&dst.Input, src.Input}, {&dst.Output, src.Output}, {&dst.CacheCreation, src.CacheCreation}, {&dst.CacheRead, src.CacheRead}} {
		n, err := addInt(*pair.dst, pair.src)
		if err != nil {
			return err
		}
		*pair.dst = n
	}
	return dst.total()
}

func tokenQuantile(values []Tokens, q float64) *Tokens {
	result := &Tokens{}
	for _, field := range []struct {
		get func(Tokens) int64
		dst *int64
	}{
		{func(t Tokens) int64 { return t.Input }, &result.Input},
		{func(t Tokens) int64 { return t.Output }, &result.Output},
		{func(t Tokens) int64 { return t.CacheCreation }, &result.CacheCreation},
		{func(t Tokens) int64 { return t.CacheRead }, &result.CacheRead},
		{func(t Tokens) int64 { return t.Total }, &result.Total},
	} {
		ns := make([]int64, len(values))
		for i, value := range values {
			ns[i] = field.get(value)
		}
		slices.Sort(ns)
		position := float64(len(ns)-1) * q
		low := int(position)
		fraction := position - float64(low)
		n := ns[low]
		if fraction != 0 {
			// q is 1/4, 1/2 or 3/4, so use exact integer interpolation;
			// float64 would round large int64 counts outside their range.
			diff := ns[low+1] - n
			quarters := int64(math.Round(fraction * 4))
			n += diff/4*quarters + (diff%4*quarters+2)/4
		}
		*field.dst = n
	}
	return result
}
