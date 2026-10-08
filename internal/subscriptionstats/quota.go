package subscriptionstats

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/p4u/claude-proxy/internal/provider"
)

type observation struct {
	ts, reset int64
	pct       float64
	valid     bool
	reason    string
}

type quotaCycle struct {
	reset    int64
	segments [][]observation
}

type calibration struct {
	sample   CapacitySample
	interval tokenInterval
}

type currentCalibration struct {
	window  *Window
	full    tokenInterval
	matched *calibration
}

func (b *builder) readCapacity(ctx context.Context) error {
	observations, err := b.readObservations(ctx)
	if err != nil {
		return err
	}
	var intervals []*tokenInterval
	var history []*calibration
	var currents []*currentCalibration
	for id, c := range b.credentials {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.provider != "" && c.provider != string(provider.Anthropic) {
			continue
		}
		a := b.accounts[id]
		obs := distinctObservations(observations[id])
		cycles := splitCycles(obs)
		current := b.currentWindow(c, obs, cycles)
		a.Current = current.window
		if current.window.ObservedAt > 0 {
			intervals = append(intervals, &current.full)
			currents = append(currents, current)
		}
		if current.matched != nil {
			intervals = append(intervals, &current.matched.interval)
		}
		for _, cycle := range cycles {
			for _, segment := range cycle.segments {
				start := slices.IndexFunc(segment, func(o observation) bool { return o.ts >= b.report.From })
				if start < 0 {
					continue
				}
				end := start
				for end < len(segment) && segment[end].ts < b.report.To {
					end++
				}
				selected := segment[start:end]
				if intervalReason(selected) != "" {
					continue
				}
				candidate := newCalibration(a, cycle.reset, selected)
				history = append(history, candidate)
				intervals = append(intervals, &candidate.interval)
			}
		}
	}
	if err := b.aggregateIntervals(ctx, intervals); err != nil {
		return err
	}
	for _, current := range currents {
		w := current.window
		w.Requests, w.Tokens = current.full.value.requests, current.full.value.tokens
		if current.matched == nil {
			continue
		}
		estimate, reason := estimateInterval(current.matched)
		w.Estimate = estimate
		if reason != "" {
			w.Reason = reason
		}
	}
	// Several disjoint, valid segments may survive a discontinuity in one
	// cycle. Retain only the longest usable segment, never duplicate the cycle
	// in group quantiles or pool tokens across the gap.
	type cycleKey struct {
		id    string
		reset int64
	}
	best := make(map[cycleKey]*calibration)
	for _, candidate := range history {
		estimate, _ := estimateInterval(candidate)
		if estimate == nil {
			continue
		}
		candidate.sample.Tokens, candidate.sample.Estimate = candidate.interval.value.tokens, *estimate
		key := cycleKey{candidate.sample.CredentialID, candidate.sample.ResetAt}
		previous := best[key]
		if previous == nil || preferCalibration(candidate, previous) {
			best[key] = candidate
		}
	}
	for _, candidate := range best {
		b.report.CapacityHistory = append(b.report.CapacityHistory, candidate.sample)
	}
	return nil
}

func preferCalibration(a, b *calibration) bool {
	spanA, spanB := a.sample.End-a.sample.Start, b.sample.End-b.sample.Start
	if spanA != spanB {
		return spanA > spanB
	}
	if a.sample.DeltaPct != b.sample.DeltaPct {
		return a.sample.DeltaPct > b.sample.DeltaPct
	}
	return a.sample.End > b.sample.End
}

func newCalibration(a *Account, reset int64, obs []observation) *calibration {
	first, last := obs[0], obs[len(obs)-1]
	return &calibration{
		sample: CapacitySample{
			GroupKey: a.GroupKey, CredentialID: a.ID, Start: first.ts, End: last.ts,
			ResetAt: reset, ObservedAt: last.ts, DeltaPct: last.pct - first.pct, Samples: len(obs),
		},
		interval: tokenInterval{credentialID: a.ID, lo: first.ts + 1, hi: last.ts},
	}
}

func intervalReason(obs []observation) string {
	if len(obs) < 2 {
		return "insufficient_observations"
	}
	first, last := obs[0], obs[len(obs)-1]
	if last.ts-first.ts < minInterval {
		return "insufficient_interval"
	}
	if last.pct-first.pct < minDelta {
		return "insufficient_delta"
	}
	return ""
}

func (b *builder) currentWindow(c credential, obs []observation, cycles []quotaCycle) *currentCalibration {
	result := &currentCalibration{window: &Window{Reason: "missing_quota"}}
	if len(obs) == 0 {
		return result
	}
	latest := -1
	for i := len(obs) - 1; i >= 0; i-- {
		if obs[i].valid {
			latest = i
			break
		}
	}
	if latest < 0 {
		result.window.Reason = obs[len(obs)-1].reason
		return result
	}
	q := obs[latest]
	w := result.window
	w.Start, w.ResetAt, w.ObservedAt, w.UsedPct = q.reset-b.duration, q.reset, q.ts, q.pct
	result.full = tokenInterval{credentialID: c.id, lo: w.Start, hi: q.ts}
	var matched []observation
	for _, cycle := range cycles {
		if !sameReset(cycle.reset, q.reset) {
			continue
		}
		w.ResetAt, w.Start = cycle.reset, cycle.reset-b.duration
		result.full.lo = w.Start
		for _, segment := range cycle.segments {
			if segment[len(segment)-1].ts == q.ts {
				matched = segment
			}
		}
	}
	if len(matched) > 0 {
		w.Samples = len(matched)
		w.DeltaPct = matched[len(matched)-1].pct - matched[0].pct
	}
	switch {
	case latest != len(obs)-1:
		w.Reason = obs[len(obs)-1].reason
	case b.report.AsOf-q.ts > maxSnapshotAge:
		w.Reason = "stale_snapshot"
	case w.ResetAt <= b.report.AsOf:
		w.Reason = "expired_window"
	case len(matched) == 0:
		w.Reason = "reset_changed"
	default:
		w.Reason = intervalReason(matched)
		if w.Reason == "" {
			result.matched = newCalibration(b.accounts[c.id], w.ResetAt, matched)
			if matched[0].ts > w.Start || c.createdAt > w.Start {
				w.Reason = "partial_coverage"
			}
		}
	}
	return result
}

func sameReset(a, b int64) bool {
	// Resets are validated as nonnegative first, so subtraction cannot wrap.
	return a >= b && a-b <= resetTolerance || b > a && b-a <= resetTolerance
}

func splitCycles(obs []observation) []quotaCycle {
	cycles := []quotaCycle{}
	var current *quotaCycle
	var segment []observation
	flush := func() {
		if len(segment) > 0 && current != nil {
			current.segments = append(current.segments, segment)
		}
		segment = nil
	}
	for _, q := range obs {
		if !q.valid {
			flush()
			continue
		}
		if current == nil || q.ts >= current.reset-resetTolerance && q.reset > current.reset && !sameReset(q.reset, current.reset) {
			flush()
			cycles = append(cycles, quotaCycle{reset: q.reset})
			current = &cycles[len(cycles)-1]
		}
		if !sameReset(q.reset, current.reset) || q.ts >= current.reset {
			// A changed reset before the old window ended is not a rollover.
			// Keep the original cluster anchor; chained +/-2s jitter must not
			// slowly drift into a different reset or produce overlapping cycles.
			flush()
			continue
		}
		if len(segment) > 0 && q.pct < segment[len(segment)-1].pct {
			flush()
		}
		segment = append(segment, q)
	}
	flush()
	return cycles
}

// Rows at the same second do not add observations. Conflicting readings at that
// second invalidate the boundary rather than associating a token with whichever
// duplicate happens to be chosen by SQL ordering.
func distinctObservations(raw []observation) []observation {
	result := make([]observation, 0, len(raw))
	for i := 0; i < len(raw); {
		q := raw[i]
		j := i + 1
		for j < len(raw) && raw[j].ts == q.ts {
			other := raw[j]
			if !q.valid || !other.valid || q.pct != other.pct || !sameReset(q.reset, other.reset) {
				q.valid, q.reason = false, "ambiguous_timestamp"
			}
			j++
		}
		result = append(result, q)
		i = j
	}
	return result
}

func estimateInterval(c *calibration) (*Tokens, string) {
	if c.interval.value.invalid > 0 {
		return nil, "invalid_token_counts"
	}
	if c.interval.value.tokens.Total == 0 {
		return nil, "empty_workload"
	}
	tokens, ok := scaleTokens(c.interval.value.tokens, c.sample.DeltaPct)
	if !ok {
		return nil, "estimate_overflow"
	}
	return tokens, ""
}

func scaleTokens(tokens Tokens, delta float64) (*Tokens, bool) {
	if math.IsNaN(delta) || math.IsInf(delta, 0) || delta < minDelta || delta > 100 {
		return nil, false
	}
	result := &Tokens{}
	for _, field := range []struct {
		in  int64
		out *int64
	}{{tokens.Input, &result.Input}, {tokens.Output, &result.Output}, {tokens.CacheCreation, &result.CacheCreation}, {tokens.CacheRead, &result.CacheRead}} {
		value := math.Round(float64(field.in) * (100 / delta))
		if field.in < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value >= float64(math.MaxInt64) {
			return nil, false
		}
		*field.out = int64(value)
	}
	if err := result.total(); err != nil {
		return nil, false
	}
	return result, true
}

func (b *builder) readObservations(ctx context.Context) (map[string][]observation, error) {
	result := make(map[string][]observation)
	// Read the historical period plus a full cycle boundary, and separately
	// the current cycle. An old 30-day report must not scan years of unrelated
	// quota history just to supply its independent current-window display.
	historyLower := max(int64(0), b.report.From-b.duration-resetTolerance)
	historyUpper := min(b.report.To-1, b.report.AsOf)
	currentLower := max(int64(0), b.report.AsOf-b.duration-resetTolerance)
	columns := "u.credential_id,u.captured_at,u." + b.report.Window + "_pct,u." + b.report.Window + "_resets_at,u." + b.report.Window + "_observed,u.id AS observation_id"
	selectRange := `SELECT ` + columns + ` FROM credentials c JOIN usage_history u ON u.credential_id=c.id
		WHERE c.provider IN ('anthropic','') AND u.captured_at>=? AND u.captured_at<=?`
	query := selectRange
	var args []any
	if historyUpper >= currentLower-1 {
		// Merge touching/overlapping ranges so no row consumes the bounded
		// observation budget twice, and each range remains indexable.
		args = []any{min(historyLower, currentLower), b.report.AsOf}
	} else {
		query += ` UNION ALL ` + selectRange
		args = []any{historyLower, historyUpper, currentLower, b.report.AsOf}
	}
	query += ` ORDER BY captured_at DESC,observation_id DESC LIMIT ?`
	args = append(args, maxQuotaRows+1)
	used, err := b.readObservationRows(ctx, result, query, args, maxQuotaRows)
	if err != nil {
		return nil, err
	}
	if remaining := maxQuotaRows - used; remaining > 0 {
		// One predecessor before the current range preserves a genuinely stale
		// latest snapshot. Exclude any predecessor already in the historical
		// fetch. A historical predecessor is unnecessary: the extra full duration
		// above already contains every cycle able to overlap the report period.
		// The correlated lookup uses (credential_id,captured_at), not a scan of
		// all historical requests or quota cycles.
		query = `SELECT ` + columns + ` FROM credentials c JOIN usage_history u ON u.id=(
			SELECT p.id FROM usage_history p WHERE p.credential_id=c.id AND p.captured_at<?
			ORDER BY p.captured_at DESC,p.id DESC LIMIT 1)
			WHERE c.provider IN ('anthropic','') AND (u.captured_at<? OR u.captured_at>?)
			ORDER BY c.id LIMIT ?`
		if _, err := b.readObservationRows(ctx, result, query, []any{currentLower, historyLower, historyUpper, remaining + 1}, remaining); err != nil {
			return nil, err
		}
	} else {
		b.truncate()
	}
	for id, obs := range result {
		// The main bounded fetch runs newest first. Stable sorting keeps an
		// arbitrary duplicate from becoming a hidden extra measurement.
		slices.SortStableFunc(obs, func(a, c observation) int { return cmp.Compare(a.ts, c.ts) })
		result[id] = obs
	}
	return result, nil
}

func (b *builder) readObservationRows(ctx context.Context, into map[string][]observation, query string, args []any, limit int) (int, error) {
	rows, err := b.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if n == limit {
			b.truncate()
			break
		}
		var id string
		var rowID int64
		var ts, pct, reset, observed any
		if err := rows.Scan(&id, &ts, &pct, &reset, &observed, &rowID); err != nil {
			return n, err
		}
		n++
		q, err := decodeObservation(ts, pct, reset, observed, b.duration, b.report.AsOf)
		if err != nil {
			// An unordered malformed timestamp cannot safely be placed between
			// valid samples. Fail explicitly instead of bridging an unknown gap.
			return n, err
		}
		into[id] = append(into[id], q)
	}
	return n, rows.Err()
}

func decodeObservation(tsValue, pctValue, resetValue, observedValue any, duration, now int64) (observation, error) {
	ts, ok := tsValue.(int64)
	if !ok || ts < 0 || ts > now {
		return observation{}, fmt.Errorf("invalid quota observation timestamp")
	}
	q := observation{ts: ts, reason: "unobserved_quota"}
	observed, ok := observedValue.(int64)
	if !ok || observed != 1 {
		return q, nil
	}
	q.reason = "invalid_quota"
	switch pct := pctValue.(type) {
	case int64:
		q.pct = float64(pct)
	case float64:
		q.pct = pct
	default:
		return q, nil
	}
	if math.IsNaN(q.pct) || math.IsInf(q.pct, 0) || q.pct < 0 || q.pct > 100 {
		return q, nil
	}
	q.reason = "invalid_reset"
	reset, ok := resetValue.(int64)
	if !ok || reset <= ts || reset-ts > duration+resetTolerance {
		return q, nil
	}
	q.reset, q.valid, q.reason = reset, true, ""
	return q, nil
}
