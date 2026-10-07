package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

const (
	expiryMaxCredentials = 128
	expiryMaxSamples     = 6
	expiryMaxEvents      = 64
	expiryMaxRequests    = 20000
	expiryMaxArrivals    = 20000
	expiryMaxSessions    = 4096
	expiryReadBudget     = 500 * time.Millisecond
	expirySampleGap      = 5 * time.Minute
	expiryTrendWindow    = 5 * time.Minute
)

type expirySample struct {
	at, fhReset, sdReset   int64
	fh, sd                 float64
	fhObserved, sdObserved bool
}

type expiryAccount struct {
	id      string
	samples []expirySample // newest first, at most expiryMaxSamples
	work    expiryWorkload
}

type expiryRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

type expiryForecast struct {
	SampleTimes      [3]int64    `json:"sample_times"`
	SampleAgeSeconds int64       `json:"sample_age_seconds"`
	ResetAt          int64       `json:"reset_at"`
	HorizonSeconds   int64       `json:"horizon_seconds"` // includes sample age
	FiveHourRate     expiryRange `json:"five_hour_pct_per_hour"`
	WeeklyRate       expiryRange `json:"weekly_pct_per_hour"`
	UnusedWeekly     expiryRange `json:"unused_weekly_pct"`
}

// Counts describe observed traffic, not allowance, demand, or billing. In
// particular, an HTTP success does not prove an SSE stream completed normally.
// They never enter the percentage-rate or unused-capacity arithmetic.
type expiryWorkload struct {
	Requests       int64  `json:"requests"`
	Output         int64  `json:"output_tokens"`
	CacheCreation  int64  `json:"cache_creation_tokens"`
	CacheRead      int64  `json:"cache_read_tokens"`
	PreviousRate   int64  `json:"previous_requests_per_hour"`
	RecentRate     int64  `json:"recent_requests_per_hour"`
	Trend          string `json:"request_rate_trend"`
	AfterSample    int64  `json:"requests_after_sample"`
	Arrivals       int64  `json:"arrivals_after_sample"`
	PendingTargets int64  `json:"pending_targets"`
	Inflight       int64  `json:"inflight"`
	Complete       bool   `json:"complete"`

	intervalRequests int64
	invalid          bool
}

type expiryEvidence struct {
	Executable     bool            `json:"executable"`
	Confidence     string          `json:"confidence"`
	ScopeKnowledge string          `json:"scope_knowledge"`
	Measurement    string          `json:"measurement"`
	Baseline       string          `json:"baseline"`
	Blockers       []string        `json:"blockers"`
	Forecast       *expiryForecast `json:"forecast,omitempty"`
	Workload       expiryWorkload  `json:"workload_last_hour"`
}

type expirySession struct {
	key, target string
	inflight    int
}

// ObserveExpiry measures destination-local weekly-expiry opportunities only.
// It must be called outside p.mu. It never changes bindings, plans, notices,
// cooldowns, RNG state, or counters, and never credits hypothetical source relief.
//
// This first stage is NOT an allocation simulation: no conversation is proposed
// for a destination, so there are no reservations. "Opportunity" means forecast
// spare weekly capacity, not an actionable recommendation. Scope eligibility,
// incremental demand, and cold-cache costs remain explicit readiness blockers.
//
// All database reads share a 500ms child deadline; fixed row limits bound memory
// and work. On partial read failure it returns uncertainty events together with
// the error; callers should retain those events as well as report the error.
func (p *Pool) ObserveExpiry(ctx context.Context) ([]store.RoutingEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, expiryReadBudget)
	defer cancel()
	now := p.now().Unix()
	accounts, overflow, err := p.expiryAccounts(ctx)
	if err != nil || len(accounts) == 0 {
		return nil, err
	}
	var inputBlockers []string
	if overflow {
		inputBlockers = append(inputBlockers, "credentials_truncated")
	}
	for i := range accounts {
		accounts[i].samples, err = p.expirySamples(ctx, accounts[i].id)
		if err != nil {
			break
		}
	}
	if err == nil {
		var blockers []string
		blockers, err = p.expiryMetadata(ctx, accounts, now)
		inputBlockers = append(inputBlockers, blockers...)
	}
	if err != nil {
		inputBlockers = append(inputBlockers, "inputs_unavailable")
	}
	events := make([]store.RoutingEvent, 0, len(accounts))
	for _, a := range accounts {
		events = append(events, expiryDecision(a, now, inputBlockers))
	}
	// Stable ordering puts opportunities and uncertain near-expiry observations
	// ahead of no-need baselines without using the live selector or its RNG.
	slices.SortStableFunc(events, func(a, b store.RoutingEvent) int {
		rank := func(reason string) int {
			switch reason {
			case "expiry-opportunity":
				return 0
			case "workload-unknown":
				return 1
			case "quota-unknown":
				return 2
			default:
				return 3
			}
		}
		return rank(a.Reason) - rank(b.Reason)
	})
	return events[:min(len(events), expiryMaxEvents)], err
}

func (p *Pool) expiryAccounts(ctx context.Context) ([]expiryAccount, bool, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id FROM credentials
		WHERE provider='anthropic' AND status='active' ORDER BY id LIMIT ?`, expiryMaxCredentials)
	if err != nil {
		return nil, false, fmt.Errorf("expiry credentials: %w", err)
	}
	var accounts []expiryAccount
	for rows.Next() {
		var a expiryAccount
		if err := rows.Scan(&a.id); err != nil {
			rows.Close()
			return nil, false, err
		}
		accounts = append(accounts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(accounts) < expiryMaxCredentials {
		return accounts, false, err
	}
	// An existence probe detects overflow without loading a 129th credential.
	var overflow bool
	err = p.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM credentials
		WHERE provider='anthropic' AND status='active' AND id>? LIMIT 1)`, accounts[len(accounts)-1].id).Scan(&overflow)
	return accounts, overflow, err
}

func (p *Pool) expirySamples(ctx context.Context, id string) ([]expirySample, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT captured_at, five_hour_pct, seven_day_pct,
		COALESCE(five_hour_resets_at,0), COALESCE(seven_day_resets_at,0),
		five_hour_observed, seven_day_observed FROM usage_history
		WHERE credential_id=? ORDER BY captured_at DESC, id DESC LIMIT ?`, id, expiryMaxSamples)
	if err != nil {
		return nil, fmt.Errorf("expiry samples: %w", err)
	}
	defer rows.Close()
	var samples []expirySample
	for rows.Next() {
		var s expirySample
		if err := rows.Scan(&s.at, &s.fh, &s.sd, &s.fhReset, &s.sdReset, &s.fhObserved, &s.sdObserved); err != nil {
			return samples, err
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

// expiryForecastFor uses three distinct points with two >=5min intervals. Every
// intervening reading examined to find those points must agree with the same
// monotone windows: skipping a duplicate/close poll cannot conceal a rollover.
func expiryForecastFor(samples []expirySample, now int64) (*expiryForecast, string) {
	if len(samples) == 0 {
		return nil, "quota_missing"
	}
	var picked []expirySample
	fhMin, fhMax := samples[0].fhReset, samples[0].fhReset
	sdMin, sdMax := samples[0].sdReset, samples[0].sdReset
	for i, s := range samples {
		if !s.fhObserved || !s.sdObserved || !(s.fh >= 0 && s.fh <= 100 && s.sd >= 0 && s.sd <= 100) {
			return nil, "quota_unobserved"
		}
		if s.at > now || s.at <= 0 {
			return nil, "sample_time_invalid"
		}
		if s.fhReset <= now || s.sdReset <= now {
			return nil, "reset_unknown_or_elapsed"
		}
		fhMin, fhMax = min(fhMin, s.fhReset), max(fhMax, s.fhReset)
		sdMin, sdMax = min(sdMin, s.sdReset), max(sdMax, s.sdReset)
		jitter := int64(rebalanceResetJitter / time.Second)
		if fhMax-fhMin > jitter || sdMax-sdMin > jitter {
			return nil, "reset_changed"
		}
		if i > 0 {
			newer := samples[i-1]
			if s.at > newer.at || s.fh > newer.fh || s.sd > newer.sd ||
				s.at == newer.at && (s.fh != newer.fh || s.sd != newer.sd) {
				return nil, "usage_decreased_or_conflicting"
			}
		}
		if len(picked) == 0 || picked[len(picked)-1].at-s.at >= int64(expirySampleGap/time.Second) {
			picked = append(picked, s)
			ageLimit := []int64{15 * 60, 30 * 60, 60 * 60}[len(picked)-1]
			if now-s.at > ageLimit {
				return nil, "samples_stale"
			}
			if len(picked) == 3 {
				break
			}
		}
	}
	if len(picked) != 3 {
		return nil, "samples_insufficient_or_bounded"
	}
	if sdMax-now > 2*60*60 {
		return nil, "outside_expiry_horizon"
	}
	if sdMax >= fhMin {
		return nil, "five_hour_reset_before_weekly"
	}
	a, b, c := picked[0], picked[1], picked[2]
	rate := func(newer, older expirySample, weekly bool) float64 {
		delta := newer.fh - older.fh
		if weekly {
			delta = newer.sd - older.sd
		}
		return delta * 3600 / float64(newer.at-older.at)
	}
	fh1, fh2 := rate(a, b, false), rate(b, c, false)
	sd1, sd2 := rate(a, b, true), rate(b, c, true)
	weekly := expiryRange{Min: min(sd1, sd2), Max: max(sd1, sd2)}
	// Project from the measurement, not from now: otherwise sample age would
	// invent unused quota. Reset jitter widens rather than tightens the range.
	unused := expiryRange{
		Min: max(0, 100-a.sd-weekly.Max*float64(sdMax-a.at)/3600),
		Max: max(0, 100-a.sd-weekly.Min*float64(sdMin-a.at)/3600),
	}
	return &expiryForecast{
		SampleTimes: [3]int64{a.at, b.at, c.at}, SampleAgeSeconds: now - a.at,
		ResetAt: sdMin, HorizonSeconds: sdMax - a.at,
		FiveHourRate: expiryRange{Min: min(fh1, fh2), Max: max(fh1, fh2)},
		WeeklyRate:   weekly, UnusedWeekly: unused,
	}, ""
}

// TryLock avoids queuing a background observer behind a foreground operation
// doing DB work under p.mu. Bounded copies only; no SQL or live state mutation.
func (p *Pool) expirySessions() ([]expirySession, bool) {
	if !p.mu.TryLock() {
		return nil, false
	}
	defer p.mu.Unlock()
	if len(p.sessions) > expiryMaxSessions {
		return nil, false
	}
	sessions := make([]expirySession, 0, len(p.sessions))
	for key, s := range p.sessions {
		if s.inflight == 0 && s.pending == nil {
			continue
		}
		copy := expirySession{key: key, inflight: s.inflight}
		if s.pending != nil {
			copy.target = s.pending.target
		}
		sessions = append(sessions, copy)
	}
	return sessions, true
}

func (p *Pool) expiryMetadata(ctx context.Context, accounts []expiryAccount, now int64) ([]string, error) {
	byID := make(map[string]*expiryAccount, len(accounts))
	for i := range accounts {
		byID[accounts[i].id] = &accounts[i]
	}
	var blockers []string
	requestsTruncated, err := p.expiryRequests(ctx, byID, now)
	if err != nil {
		return blockers, err
	}
	if requestsTruncated {
		blockers = append(blockers, "requests_truncated")
	}
	arrivalsTruncated, err := p.expiryArrivals(ctx, byID, now)
	if err != nil {
		return blockers, err
	}
	if arrivalsTruncated {
		blockers = append(blockers, "arrivals_truncated")
	}
	sessions, complete := p.expirySessions()
	if !complete {
		blockers = append(blockers, "sessions_unavailable_or_truncated")
	}
	if err := p.expiryInflight(ctx, byID, sessions); err != nil {
		return blockers, err
	}
	for i := range accounts {
		accounts[i].work.Complete = len(blockers) == 0
	}
	return blockers, nil
}

func (p *Pool) expiryRequests(ctx context.Context, accounts map[string]*expiryAccount, now int64) (bool, error) {
	// Compute each quota interval once, not once per observed request. The
	// metadata scan may contain twenty thousand rows inside its short budget.
	intervals := make(map[string][2]int64, len(accounts))
	for id, a := range accounts {
		if f, _ := expiryForecastFor(a.samples, now); f != nil {
			intervals[id] = [2]int64{f.SampleTimes[1], f.SampleTimes[0]}
		}
	}
	rows, err := p.db.QueryContext(ctx, `SELECT COALESCE(credential_id,''), ts,
		output_tokens, cache_creation_tokens, cache_read_tokens FROM request_log
		WHERE ts>=? ORDER BY ts DESC, id DESC LIMIT ?`, now-3600, expiryMaxRequests+1)
	if err != nil {
		return false, fmt.Errorf("expiry request metadata: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > expiryMaxRequests {
			return true, nil
		}
		var id string
		var ts, output, creation, read int64
		if err := rows.Scan(&id, &ts, &output, &creation, &read); err != nil {
			return false, err
		}
		a := accounts[id]
		if a == nil {
			continue
		}
		w := &a.work
		w.Requests++
		for _, pair := range []struct {
			total *int64
			n     int64
		}{{&w.Output, output}, {&w.CacheCreation, creation}, {&w.CacheRead, read}} {
			if pair.n < 0 || *pair.total > math.MaxInt64-pair.n {
				w.invalid = true
			} else {
				*pair.total += pair.n
			}
		}
		if ts > now {
			w.invalid = true
			continue
		}
		window := int64(expiryTrendWindow / time.Second)
		switch {
		case ts > now-window:
			w.RecentRate += 3600 / window
		case ts > now-2*window:
			w.PreviousRate += 3600 / window
		}
		if len(a.samples) > 0 && ts > a.samples[0].at {
			w.AfterSample++
		}
		// Compare post-sample arrivals against the exact last quota interval,
		// not token totals. This is a workload change detector, never burn math.
		if interval, ok := intervals[id]; ok && ts > interval[0] && ts <= interval[1] {
			w.intervalRequests++
		}
	}
	return false, rows.Err()
}

func (p *Pool) expiryArrivals(ctx context.Context, accounts map[string]*expiryAccount, now int64) (bool, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT credential_id, bound_at FROM conversations
		WHERE bound_at>=? ORDER BY bound_at DESC LIMIT ?`, now-3600, expiryMaxArrivals+1)
	if err != nil {
		return false, fmt.Errorf("expiry arrivals: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > expiryMaxArrivals {
			return true, nil
		}
		var id string
		var bound int64
		if err := rows.Scan(&id, &bound); err != nil {
			return false, err
		}
		if a := accounts[id]; a != nil {
			if bound > now {
				a.work.invalid = true
			}
			if len(a.samples) > 0 && bound > a.samples[0].at {
				a.work.Arrivals++
			}
		}
	}
	return false, rows.Err()
}

func (p *Pool) expiryInflight(ctx context.Context, accounts map[string]*expiryAccount, sessions []expirySession) error {
	inflight := make(map[string]int, len(sessions))
	var args []any
	for _, s := range sessions {
		if a := accounts[s.target]; a != nil {
			a.work.PendingTargets++
		}
		if s.inflight > 0 {
			inflight[s.key] = s.inflight
			args = append(args, s.key)
		}
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id, credential_id FROM conversations WHERE id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return fmt.Errorf("expiry inflight: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return err
		}
		if a := accounts[id]; a != nil {
			a.work.Inflight += int64(inflight[key])
		}
		delete(inflight, key)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(inflight) > 0 {
		return fmt.Errorf("expiry inflight: snapshot bindings unavailable")
	}
	return nil
}

func expiryDecision(a expiryAccount, now int64, inputBlockers []string) store.RoutingEvent {
	e := expiryEvidence{
		Confidence: "unknown", ScopeKnowledge: "max_only", Measurement: "destination_opportunity",
		Baseline: "unknown", Workload: a.work,
		Blockers: []string{"scope_eligibility_unknown", "incremental_demand_unknown", "cache_cost_unknown", "stream_success_unknown", "workload_snapshot_nonatomic"},
	}
	e.Blockers = append(e.Blockers, inputBlockers...)
	if len(inputBlockers) > 0 {
		e.Workload.Complete = false
	}
	switch {
	case a.work.RecentRate > a.work.PreviousRate:
		e.Workload.Trend = "rising"
	case a.work.RecentRate < a.work.PreviousRate:
		e.Workload.Trend = "falling"
	default:
		e.Workload.Trend = "steady"
	}
	if !e.Workload.Complete {
		e.Workload.Trend = "unknown"
	}
	reason := "quota-unknown"
	f, blocker := expiryForecastFor(a.samples, now)
	if blocker != "" {
		e.Blockers = append(e.Blockers, blocker)
		if blocker == "outside_expiry_horizon" || blocker == "five_hour_reset_before_weekly" {
			reason = "no-expiry-opportunity"
			e.Baseline = "outside_horizon"
		}
	} else {
		e.Forecast, e.Confidence = f, "heuristic"
		e.Baseline, reason = "spare_weekly_capacity", "expiry-opportunity"
		if f.UnusedWeekly.Max == 0 {
			e.Baseline, reason = "no_need", "no-expiry-opportunity"
		} else if f.UnusedWeekly.Min == 0 {
			e.Blockers = append(e.Blockers, "forecast_range_includes_no_need")
			e.Baseline = "uncertain_spare_capacity"
		}
		workChanged := len(inputBlockers) > 0 || !a.work.Complete || a.work.invalid
		addWorkBlocker := func(yes bool, name string) {
			if yes {
				e.Blockers = append(e.Blockers, name)
				workChanged = true
			}
		}
		addWorkBlocker(a.work.invalid, "workload_metadata_invalid")
		addWorkBlocker(a.work.RecentRate != a.work.PreviousRate, "request_rate_changed")
		if f.SampleAgeSeconds > 0 {
			// Cross multiplication avoids treating an unobserved denominator as
			// zero, or assigning a quota cost to one observed request.
			interval := f.SampleTimes[0] - f.SampleTimes[1]
			addWorkBlocker(a.work.AfterSample*interval != a.work.intervalRequests*f.SampleAgeSeconds, "post_sample_rate_changed")
		}
		addWorkBlocker(a.work.Arrivals > 0, "new_arrivals")
		addWorkBlocker(a.work.PendingTargets > 0, "pending_targets")
		addWorkBlocker(a.work.Inflight > 0, "inflight_work_unknown")
		addWorkBlocker(a.samples[0].fh+f.FiveHourRate.Max*float64(f.HorizonSeconds)/3600 >= 100 && a.samples[0].sd < 100,
			"five_hour_capacity_uncertain")
		if workChanged {
			e.Confidence, reason = "unknown", "workload-unknown"
		}
	}
	data, err := json.Marshal(e)
	if err != nil || len(data) > 2048 {
		// A bounded, still explicitly non-executable fallback is safer than
		// truncating JSON or accidentally dropping readiness blockers.
		e.Forecast = nil
		e.Confidence, e.Baseline = "unknown", "unknown"
		e.Workload = expiryWorkload{Trend: "unknown"}
		e.Blockers = []string{"scope_eligibility_unknown", "incremental_demand_unknown", "cache_cost_unknown", "stream_success_unknown", "evidence_bounded"}
		data, _ = json.Marshal(e)
		reason = "workload-unknown"
	}
	return store.RoutingEvent{TS: now, Policy: "expiry", Mode: "shadow", Kind: "shadow_decision", Reason: reason, TargetID: a.id, Evidence: data}
}
