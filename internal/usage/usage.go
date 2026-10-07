package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/guptarohit/asciigraph"
	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// Bucket is one utilization window returned by the Anthropic usage API.
type Bucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// Response is the part of GET /api/oauth/usage this proxy reads.
//
// five_hour and seven_day are the two plan-wide ceilings. Model-specific
// weekly caps are no longer published as per-model buckets —
// seven_day_sonnet and seven_day_opus are always null as of 2026-09 — but as
// "weekly_scoped" entries in Limits, whose scope names the model (today
// "Fable"). See ScopedWeekly.
type Response struct {
	FiveHour Bucket  `json:"five_hour"`
	SevenDay Bucket  `json:"seven_day"`
	Limits   []Limit `json:"limits"`
}

// Limit is one entry of the usage API's limits[] array. Observed kinds:
// "session" (mirrors five_hour), "weekly_all" (mirrors seven_day) and
// "weekly_scoped" (a weekly cap on one model).
type Limit struct {
	Kind     string      `json:"kind"`
	Group    string      `json:"group"`
	Percent  *float64    `json:"percent"`
	Severity string      `json:"severity"`
	ResetsAt *string     `json:"resets_at"`
	Scope    *LimitScope `json:"scope"`
	IsActive bool        `json:"is_active"`
}

// LimitScope names what a scoped limit applies to. Only a model scope has
// been observed; surface is kept raw because its shape is not yet known.
type LimitScope struct {
	Model *struct {
		ID          *string `json:"id"`
		DisplayName string  `json:"display_name"`
	} `json:"model"`
	Surface json.RawMessage `json:"surface"`
}

// Label is the human name of the scope ("Fable"), or "" when it names none.
func (s *LimitScope) Label() string {
	if s == nil {
		return ""
	}
	if s.Model != nil {
		if s.Model.DisplayName != "" {
			return s.Model.DisplayName
		}
		if s.Model.ID != nil {
			return *s.Model.ID
		}
	}
	var surface struct {
		DisplayName string `json:"display_name"`
	}
	if json.Unmarshal(s.Surface, &surface) == nil && surface.DisplayName != "" {
		return surface.DisplayName
	}
	var name string
	if json.Unmarshal(s.Surface, &name) == nil {
		return name
	}
	return ""
}

// ScopedLimit is the model-scoped weekly cap stored per snapshot.
type ScopedLimit struct {
	Label    string
	Pct      float64
	ResetsAt *string
}

// ScopedWeekly returns the binding model-scoped weekly limit, or nil when the
// plan publishes none. With several scoped limits the fullest one is kept:
// it is the one closest to blocking requests, and one column per snapshot is
// all the dashboard needs.
func (r *Response) ScopedWeekly() *ScopedLimit {
	var best *ScopedLimit
	for _, l := range r.Limits {
		if l.Kind != "weekly_scoped" || l.Percent == nil {
			continue
		}
		label := l.Scope.Label()
		if label == "" {
			label = "scoped"
		}
		if best == nil || *l.Percent > best.Pct {
			best = &ScopedLimit{Label: label, Pct: *l.Percent, ResetsAt: l.ResetsAt}
		}
	}
	return best
}

// Snapshot is one stored measurement for a credential. SevenDayScopedPct is
// nil when the snapshot carries no model-scoped weekly limit.
type Snapshot struct {
	CredentialID        string
	CapturedAt          time.Time
	FiveHourPct         float64
	SevenDayPct         float64
	SevenDayScopedPct   *float64
	SevenDayScopedLabel string
}

// Fetch calls GET https://api.anthropic.com/api/oauth/usage for one access token.
func Fetch(ctx context.Context, client *http.Client, accessToken string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &r, nil
}

// Save stores a usage snapshot in the database. Observation flags distinguish
// actual zero utilization from a missing or invalid reading; historical rows
// retain their unknown flags. The existing percentage columns stay unchanged
// for finite values, so the live selection policy does not consume these flags.
func Save(ctx context.Context, db *store.DB, credID string, r *Response) error {
	fhPct, fhObserved := bucketObservation(&r.FiveHour)
	sdPct, sdObserved := bucketObservation(&r.SevenDay)
	if math.IsNaN(fhPct) || math.IsInf(fhPct, 0) || math.IsNaN(sdPct) || math.IsInf(sdPct, 0) {
		return fmt.Errorf("usage snapshot: nonfinite utilization")
	}
	var (
		scPct   *float64
		scReset *int64
		scLabel *string
	)
	if sc := r.ScopedWeekly(); sc != nil {
		scPct, scReset, scLabel = &sc.Pct, parseResetsAt(sc.ResetsAt), &sc.Label
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO usage_history
		  (credential_id, captured_at,
		   five_hour_pct,    five_hour_resets_at, five_hour_observed,
		   seven_day_pct,    seven_day_resets_at, seven_day_observed,
		   seven_day_scoped_pct, seven_day_scoped_resets_at, seven_day_scoped_label)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		credID, time.Now().Unix(),
		fhPct, parseResetsAt(r.FiveHour.ResetsAt), fhObserved,
		sdPct, parseResetsAt(r.SevenDay.ResetsAt), sdObserved,
		scPct, scReset, scLabel)
	return err
}

// History returns snapshots for a credential captured on or after `since`,
// ordered oldest-first.
func History(ctx context.Context, db *store.DB, credID string, since time.Time) ([]Snapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT credential_id, captured_at,
		       five_hour_pct, seven_day_pct,
		       seven_day_scoped_pct, COALESCE(seven_day_scoped_label, '')
		FROM usage_history
		WHERE credential_id = ? AND captured_at >= ?
		ORDER BY captured_at ASC`,
		credID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		var ts int64
		if err := rows.Scan(&s.CredentialID, &ts,
			&s.FiveHourPct, &s.SevenDayPct, &s.SevenDayScopedPct, &s.SevenDayScopedLabel); err != nil {
			return nil, err
		}
		s.CapturedAt = time.Unix(ts, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// HistoryAll returns snapshots for every credential since `since`.
func HistoryAll(ctx context.Context, db *store.DB, since time.Time) (map[string][]Snapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT credential_id, captured_at,
		       five_hour_pct, seven_day_pct,
		       seven_day_scoped_pct, COALESCE(seven_day_scoped_label, '')
		FROM usage_history
		WHERE captured_at >= ?
		ORDER BY credential_id, captured_at ASC`,
		since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Snapshot{}
	for rows.Next() {
		var s Snapshot
		var ts int64
		if err := rows.Scan(&s.CredentialID, &ts,
			&s.FiveHourPct, &s.SevenDayPct, &s.SevenDayScopedPct, &s.SevenDayScopedLabel); err != nil {
			return nil, err
		}
		s.CapturedAt = time.Unix(ts, 0)
		out[s.CredentialID] = append(out[s.CredentialID], s)
	}
	return out, rows.Err()
}

// LastSnapshot returns the most recent snapshot for a credential, if any.
func LastSnapshot(ctx context.Context, db *store.DB, credID string) (*Snapshot, error) {
	row := db.QueryRowContext(ctx, `
		SELECT credential_id, captured_at,
		       five_hour_pct, seven_day_pct,
		       seven_day_scoped_pct, COALESCE(seven_day_scoped_label, '')
		FROM usage_history
		WHERE credential_id = ?
		ORDER BY captured_at DESC LIMIT 1`, credID)
	var s Snapshot
	var ts int64
	if err := row.Scan(&s.CredentialID, &ts,
		&s.FiveHourPct, &s.SevenDayPct, &s.SevenDayScopedPct, &s.SevenDayScopedLabel); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	s.CapturedAt = time.Unix(ts, 0)
	return &s, nil
}

// ParsePeriod converts "1h", "6h", "24h", "7d", "30d" to a duration.
func ParsePeriod(s string) (time.Duration, error) {
	switch s {
	case "1h":
		return time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "30d":
		return 30 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown period %q — use 1h, 6h, 24h, 7d or 30d", s)
}

// Chart renders a terminal line chart for one credential's snapshots.
// Returns the chart string, or a message when no data is available.
func Chart(snapshots []Snapshot, label, period string) string {
	if len(snapshots) == 0 {
		return "  (no data yet — poller records every 10 minutes)\n"
	}

	const maxPts = 120
	fh := downsample(extractSeries(snapshots, func(s Snapshot) float64 { return s.FiveHourPct }), maxPts)
	sd := downsample(extractSeries(snapshots, func(s Snapshot) float64 { return s.SevenDayPct }), maxPts)
	series := [][]float64{fh, sd}
	colors := []asciigraph.AnsiColor{asciigraph.Red, asciigraph.Blue}
	legends := []string{"5h", "7d"}

	// The scoped series is drawn only when the window has one; snapshots
	// taken before it was recorded count as 0%, like any unknown reading.
	scopedLabel := ""
	for _, s := range snapshots {
		if s.SevenDayScopedPct != nil {
			scopedLabel = s.SevenDayScopedLabel
		}
	}
	if scopedLabel != "" {
		sc := downsample(extractSeries(snapshots, func(s Snapshot) float64 {
			if s.SevenDayScopedPct == nil {
				return 0
			}
			return *s.SevenDayScopedPct
		}), maxPts)
		series = append(series, sc)
		colors = append(colors, asciigraph.Green)
		legends = append(legends, "7d-"+strings.ToLower(scopedLabel))
	}

	from := snapshots[0].CapturedAt.UTC().Format("2006-01-02 15:04 UTC")
	to := snapshots[len(snapshots)-1].CapturedAt.UTC().Format("2006-01-02 15:04 UTC")

	graph := asciigraph.PlotMany(
		series,
		asciigraph.Height(10),
		asciigraph.LowerBound(0),
		asciigraph.UpperBound(100),
		asciigraph.SeriesColors(colors...),
		asciigraph.SeriesLegends(legends...),
		asciigraph.Caption(fmt.Sprintf("%s — %s → %s  (%d samples)", label, from, to, len(snapshots))),
	)

	return graph + "\n"
}

func extractSeries(s []Snapshot, fn func(Snapshot) float64) []float64 {
	out := make([]float64, len(s))
	for i, snap := range s {
		out[i] = fn(snap)
	}
	return out
}

func downsample(data []float64, target int) []float64 {
	if len(data) <= target {
		return data
	}
	out := make([]float64, target)
	bucketSize := float64(len(data)) / float64(target)
	for i := range target {
		start := int(math.Round(float64(i) * bucketSize))
		end := int(math.Round(float64(i+1) * bucketSize))
		if end > len(data) {
			end = len(data)
		}
		sum := 0.0
		for _, v := range data[start:end] {
			sum += v
		}
		out[i] = sum / float64(end-start)
	}
	return out
}

func bucketPct(b *Bucket) float64 {
	if b == nil || b.Utilization == nil {
		return 0
	}
	return *b.Utilization
}

func bucketObservation(b *Bucket) (pct float64, observed bool) {
	pct = bucketPct(b)
	return pct, b != nil && b.Utilization != nil && pct >= 0 && pct <= 100
}

func parseResetsAt(s *string) *int64 {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	v := t.Unix()
	return &v
}

// Poller fetches usage for all credentials every interval and stores snapshots.
type Poller struct {
	db       *store.DB
	client   *http.Client
	log      *slog.Logger
	interval time.Duration
}

func NewPoller(db *store.DB, log *slog.Logger) *Poller {
	return &Poller{
		db:       db,
		client:   &http.Client{Timeout: 15 * time.Second},
		log:      log,
		interval: 10 * time.Minute,
	}
}

func (p *Poller) Loop(ctx context.Context) {
	p.poll(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	list, err := creds.List(ctx, p.db)
	if err != nil {
		p.log.Error("usage poller: list credentials", "err", err)
		return
	}
	for _, c := range list {
		// Only Anthropic publishes a utilization API. Polling a provider that
		// has none would fail every cycle; worse, recording a zeroed snapshot
		// instead would be read by the pool as "wide open", making an
		// exhausted key look like the most attractive one in the pool.
		// Providers without a usage API are left with no snapshots at all, so
		// selection falls through to its headroom=1.0 weight-only path.
		if !provider.Get(c.Provider).PollsUsage {
			continue
		}
		if c.Status == creds.StatusDisabled || c.Status == creds.StatusRevoked {
			continue
		}
		r, err := Fetch(ctx, p.client, c.AccessToken)
		if err != nil {
			p.log.Warn("usage poller: fetch", "cred", c.ID, "label", c.Label, "err", err)
			continue
		}
		if err := Save(ctx, p.db, c.ID, r); err != nil {
			p.log.Error("usage poller: save", "cred", c.ID, "err", err)
			continue
		}
		p.log.Debug("usage polled",
			"cred", c.ID, "label", c.Label,
			"5h", fmt.Sprintf("%.1f%%", bucketPct(&r.FiveHour)),
			"7d", fmt.Sprintf("%.1f%%", bucketPct(&r.SevenDay)))
	}
}
