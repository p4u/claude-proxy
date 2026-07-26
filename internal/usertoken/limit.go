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

// Per-user usage limits meter OUTPUT TOKENS ONLY.
//
// An earlier version counted weighted "billable units" across all four token
// kinds. In practice cache reads dwarf everything else (hundreds of millions a
// day against a million output tokens), so the number an operator had to cap
// was tens of times larger than anything shown on the dashboard and impossible
// to set sensibly. Output tokens are the figure people already recognise, so
// that is the metric.

// LimitState is the evaluation of a user's rolling-window usage against their
// configured cap.
type LimitState struct {
	// Active reports whether the user has a limit configured at all.
	Active bool
	// LimitOutputTokens and Window echo the configured limit.
	LimitOutputTokens int64
	Window            time.Duration
	// UsageOutputTokens is the output tokens produced inside the rolling window.
	UsageOutputTokens int64
	// Blocked is true when UsageOutputTokens >= LimitOutputTokens.
	Blocked bool
	// BlockedUntil is the exact instant the user drops back under the cap as
	// old rows age out of the window. Zero when not blocked — a rolling window
	// has no reset instant, so this must not be reported when under the cap.
	BlockedUntil time.Time
}

// UsagePct is usage as a percentage of the limit (0 when unlimited, may exceed 100).
func (s LimitState) UsagePct() float64 {
	if !s.Active || s.LimitOutputTokens <= 0 {
		return 0
	}
	return float64(s.UsageOutputTokens) / float64(s.LimitOutputTokens) * 100
}

// HasLimit reports whether both halves of a limit are set (0 => unlimited).
func HasLimit(limitOutputTokens, windowSeconds int64) bool {
	return limitOutputTokens > 0 && windowSeconds > 0
}

// OutputTokensInWindow sums a user's output tokens over the rolling window
// ending at now. The UI uses it to show current usage while an operator picks a
// cap, independently of whether a limit is configured.
func OutputTokensInWindow(ctx context.Context, db *store.DB, userID string, window time.Duration, now time.Time) (int64, error) {
	if userID == "" || window <= 0 {
		return 0, nil
	}
	var total int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(output_tokens), 0) FROM request_log
		 WHERE user_token_id = ? AND ts >= ?`, userID, now.Add(-window).Unix()).Scan(&total)
	return total, err
}

// LimitStatus evaluates userID's usage over the rolling window ending at now.
//
// When no limit is configured it returns an inactive state and performs ZERO
// queries — unlimited users must not pay for this feature. When a limit exists
// it sums the window's output tokens; only if the user is over the cap does it
// do the second, exact pass computing the unblock instant.
func LimitStatus(ctx context.Context, db *store.DB, userID string, limitOutputTokens, windowSeconds int64, now time.Time) (LimitState, error) {
	if userID == "" || !HasLimit(limitOutputTokens, windowSeconds) {
		return LimitState{}, nil
	}
	window := time.Duration(windowSeconds) * time.Second
	st := LimitState{Active: true, LimitOutputTokens: limitOutputTokens, Window: window}
	since := now.Add(-window).Unix()

	var total int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(output_tokens), 0) FROM request_log
		 WHERE user_token_id = ? AND ts >= ?`, userID, since).Scan(&total)
	if err != nil {
		return st, err
	}
	st.UsageOutputTokens = total
	if total < limitOutputTokens {
		return st, nil
	}
	st.Blocked = true

	until, err := unblockAt(ctx, db, userID, since, total, limitOutputTokens)
	if err != nil {
		return st, err
	}
	st.BlockedUntil = until.Add(window).Add(time.Second)
	return st, nil
}

// unblockAt walks the window's rows ascending by ts, accumulating output
// tokens, and returns the ts of the first row whose expiry drops the running
// total below the limit. The caller adds the window (+1s) to turn it into an
// instant.
//
// This is exact, not estimated: usage only falls when a specific row leaves the
// window, so the answer is always one of the rows' timestamps.
func unblockAt(ctx context.Context, db *store.DB, userID string, since, total, limit int64) (time.Time, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ts, output_tokens FROM request_log
		 WHERE user_token_id = ? AND ts >= ? ORDER BY ts ASC, id ASC`, userID, since)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	var acc, last int64
	for rows.Next() {
		var ts, out int64
		if err := rows.Scan(&ts, &out); err != nil {
			return time.Time{}, err
		}
		last = ts
		acc += out
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
func SetLimit(ctx context.Context, db *store.DB, id string, outputTokens, windowSeconds int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE user_tokens SET limit_output_tokens=?, limit_window_seconds=? WHERE id=?`,
		outputTokens, windowSeconds, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ParseTokens accepts a plain integer or a K/M/G-suffixed shorthand ("1M",
// "500k", "1.5M"). Returns an error for negative or unparseable input.
func ParseTokens(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty token count")
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
		return 0, fmt.Errorf("invalid token count %q — use a number, optionally with K/M/G", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("token count must not be negative")
	}
	return int64(f * mult), nil
}

// FormatTokens renders a token count compactly ("5M", "3.7M", "812K", "940").
func FormatTokens(v int64) string {
	f := float64(v)
	abs := math.Abs(f)
	switch {
	case abs >= 1e9:
		return trimZero(f/1e9) + "G"
	case abs >= 1e6:
		return trimZero(f/1e6) + "M"
	case abs >= 1e3:
		return trimZero(f/1e3) + "K"
	default:
		return strconv.FormatInt(v, 10)
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

// FormatGrouped renders an integer with thousands separators (1043882 =>
// "1,043,882") for the user-facing quota message.
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
		"proxy: usage limit reached - %s output tokens per %s (used %s). Resets at %s (in %s).",
		FormatGrouped(s.LimitOutputTokens), FormatWindow(s.Window),
		FormatGrouped(s.UsageOutputTokens),
		s.BlockedUntil.UTC().Format("2006-01-02 15:04 UTC"),
		FormatCountdown(s.BlockedUntil.Sub(now)))
}
