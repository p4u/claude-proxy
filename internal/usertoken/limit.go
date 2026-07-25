package usertoken

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

// Weighted billable units.
//
// Raw token counts understate cost asymmetry between token kinds, so per-user
// limits count "units" approximating Anthropic's price ratios. These constants
// are the single source of truth: the enforcement query, the web UI display and
// the CLI all derive from them so they cannot drift.
const (
	UnitWeightOutput        = 5.0
	UnitWeightInput         = 1.0
	UnitWeightCacheCreation = 1.25
	UnitWeightCacheRead     = 0.1
)

// Units returns the weighted billable units for one request's token counts.
func Units(input, output, cacheCreation, cacheRead int64) float64 {
	return float64(output)*UnitWeightOutput +
		float64(input)*UnitWeightInput +
		float64(cacheCreation)*UnitWeightCacheCreation +
		float64(cacheRead)*UnitWeightCacheRead
}

// UnitsSQL is the SQL expression computing Units() for a request_log row. Built
// from the same constants so SQL and Go can never disagree.
var UnitsSQL = fmt.Sprintf(
	"(output_tokens*%v + input_tokens*%v + cache_creation_tokens*%v + cache_read_tokens*%v)",
	UnitWeightOutput, UnitWeightInput, UnitWeightCacheCreation, UnitWeightCacheRead)

// WeightsDescription is the human-readable weighting, for UI/CLI captions.
const WeightsDescription = "output x5, input x1, cache write x1.25, cache read x0.1"

// LimitState is the evaluation of a user's rolling-window usage against their
// configured cap.
type LimitState struct {
	// Active reports whether the user has a limit configured at all.
	Active bool
	// LimitUnits and Window echo the configured limit.
	LimitUnits int64
	Window     time.Duration
	// UsageUnits is the weighted usage inside the rolling window.
	UsageUnits float64
	// Blocked is true when UsageUnits >= LimitUnits.
	Blocked bool
	// BlockedUntil is the exact instant the user drops back under the cap as
	// old rows age out of the window. Zero when not blocked — a rolling window
	// has no reset instant, so this must not be reported when under the cap.
	BlockedUntil time.Time
}

// UsagePct is usage as a percentage of the limit (0 when unlimited, may exceed 100).
func (s LimitState) UsagePct() float64 {
	if !s.Active || s.LimitUnits <= 0 {
		return 0
	}
	return s.UsageUnits / float64(s.LimitUnits) * 100
}

// HasLimit reports whether both halves of a limit are set (0 => unlimited).
func HasLimit(limitUnits, windowSeconds int64) bool {
	return limitUnits > 0 && windowSeconds > 0
}

// LimitStatus evaluates userID's usage over the rolling window ending at now.
//
// When no limit is configured it returns an inactive state and performs ZERO
// queries — unlimited users must not pay for this feature. When a limit exists
// it sums the window's units; only if the user is over the cap does it do the
// second, exact pass computing the unblock instant.
func LimitStatus(ctx context.Context, db *store.DB, userID string, limitUnits, windowSeconds int64, now time.Time) (LimitState, error) {
	if userID == "" || !HasLimit(limitUnits, windowSeconds) {
		return LimitState{}, nil
	}
	window := time.Duration(windowSeconds) * time.Second
	st := LimitState{Active: true, LimitUnits: limitUnits, Window: window}
	since := now.Add(-window).Unix()

	var total float64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(`+UnitsSQL+`), 0) FROM request_log
		 WHERE user_token_id = ? AND ts >= ?`, userID, since).Scan(&total)
	if err != nil {
		return st, err
	}
	st.UsageUnits = total
	if total < float64(limitUnits) {
		return st, nil
	}
	st.Blocked = true

	until, err := unblockAt(ctx, db, userID, since, total, float64(limitUnits))
	if err != nil {
		return st, err
	}
	st.BlockedUntil = until.Add(window).Add(time.Second)
	return st, nil
}

// unblockAt walks the window's rows ascending by ts, accumulating units, and
// returns the ts of the first row whose expiry drops the running total below
// the limit. The caller adds the window (+1s) to turn it into an instant.
//
// This is exact, not estimated: usage only falls when a specific row leaves the
// window, so the answer is always one of the rows' timestamps.
func unblockAt(ctx context.Context, db *store.DB, userID string, since int64, total, limit float64) (time.Time, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ts, `+UnitsSQL+` FROM request_log
		 WHERE user_token_id = ? AND ts >= ? ORDER BY ts ASC, id ASC`, userID, since)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	var acc float64
	var last int64
	for rows.Next() {
		var ts int64
		var units float64
		if err := rows.Scan(&ts, &units); err != nil {
			return time.Time{}, err
		}
		last = ts
		acc += units
		if total-acc < limit {
			return time.Unix(ts, 0), nil
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	// Unreachable in practice (draining every row leaves 0 < limit), but if the
	// rows vanished under us, fall back to the newest ts we saw.
	return time.Unix(last, 0), nil
}

// RetryAfterSeconds is the Retry-After value for a blocked state: whole seconds
// until the user drops back under the cap, never below 1.
func (s LimitState) RetryAfterSeconds(now time.Time) int {
	if !s.Blocked {
		return 0
	}
	secs := int(math.Ceil(s.BlockedUntil.Sub(now).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// SetLimit configures (or clears, with both zero) a user's usage limit.
func SetLimit(ctx context.Context, db *store.DB, id string, units, windowSeconds int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET limit_units=?, limit_window_seconds=? WHERE id=?`,
		units, windowSeconds, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ParseUnits accepts a plain integer or a K/M/G-suffixed shorthand ("5M",
// "500k", "1.5M"). Returns an error for negative or unparseable input.
func ParseUnits(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty units value")
	}
	mult := float64(1)
	switch t[len(t)-1] {
	case 'k', 'K':
		mult, t = 1e3, t[:len(t)-1]
	case 'm', 'M':
		mult, t = 1e6, t[:len(t)-1]
	case 'g', 'G':
		mult, t = 1e9, t[:len(t)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid units %q — use a number, optionally with K/M/G", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("units must not be negative")
	}
	return int64(f * mult), nil
}

// FormatUnits renders a unit count compactly ("5M", "3.7M", "812K", "940").
func FormatUnits(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e9:
		return trimZero(v/1e9) + "G"
	case abs >= 1e6:
		return trimZero(v/1e6) + "M"
	case abs >= 1e3:
		return trimZero(v/1e3) + "K"
	default:
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
}

func trimZero(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// FormatWindow renders a window duration in the CLI's period vocabulary
// ("24h", "7d", "90m").
func FormatWindow(d time.Duration) string {
	secs := int64(d / time.Second)
	switch {
	case secs <= 0:
		return "0s"
	case secs%86400 == 0 && secs >= 2*86400:
		return fmt.Sprintf("%dd", secs/86400)
	case secs%3600 == 0:
		return fmt.Sprintf("%dh", secs/3600)
	case secs%60 == 0:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// FormatCountdown renders a wait as "3h 12m", "12m" or "45s".
func FormatCountdown(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	secs := int64(d.Round(time.Second) / time.Second)
	h, m, s := secs/3600, (secs%3600)/60, secs%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// FormatGrouped renders an integer with thousands separators (5204118 =>
// "5,204,118") for the user-facing quota message.
func FormatGrouped(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// QuotaMessage is the client-facing 429 message for a blocked user.
func (s LimitState) QuotaMessage(now time.Time) string {
	return fmt.Sprintf(
		"proxy: usage limit reached - %s units per %s (used %s). Resets at %s (in %s).",
		FormatGrouped(s.LimitUnits), FormatWindow(s.Window),
		FormatGrouped(int64(s.UsageUnits)),
		s.BlockedUntil.UTC().Format("2006-01-02 15:04 UTC"),
		FormatCountdown(s.BlockedUntil.Sub(now)))
}
