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
	"github.com/p4u/claude-proxy/internal/store"
)

// Bucket is one utilization window returned by the Anthropic usage API.
type Bucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// Response is the full payload from GET /api/oauth/usage.
type Response struct {
	FiveHour       Bucket  `json:"five_hour"`
	SevenDay       Bucket  `json:"seven_day"`
	SevenDayOpus   *Bucket `json:"seven_day_opus"`
	SevenDaySonnet *Bucket `json:"seven_day_sonnet"`
}

// Snapshot is one stored measurement for a credential.
type Snapshot struct {
	CredentialID       string
	CapturedAt         time.Time
	FiveHourPct        float64
	SevenDayPct        float64
	SevenDaySonnetPct  float64
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

// Save stores a usage snapshot in the database.
func Save(ctx context.Context, db *store.DB, credID string, r *Response) error {
	fhPct := bucketPct(&r.FiveHour)
	sdPct := bucketPct(&r.SevenDay)
	sdsPct := 0.0
	if r.SevenDaySonnet != nil {
		sdsPct = bucketPct(r.SevenDaySonnet)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO usage_history
		  (credential_id, captured_at,
		   five_hour_pct,    five_hour_resets_at,
		   seven_day_pct,    seven_day_resets_at,
		   seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		credID, time.Now().Unix(),
		fhPct, parseResetsAt(r.FiveHour.ResetsAt),
		sdPct, parseResetsAt(r.SevenDay.ResetsAt),
		sdsPct, func() *int64 {
			if r.SevenDaySonnet != nil {
				return parseResetsAt(r.SevenDaySonnet.ResetsAt)
			}
			return nil
		}())
	return err
}

// History returns snapshots for a credential captured on or after `since`,
// ordered oldest-first.
func History(ctx context.Context, db *store.DB, credID string, since time.Time) ([]Snapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT credential_id, captured_at,
		       five_hour_pct, seven_day_pct, seven_day_sonnet_pct
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
			&s.FiveHourPct, &s.SevenDayPct, &s.SevenDaySonnetPct); err != nil {
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
		       five_hour_pct, seven_day_pct, seven_day_sonnet_pct
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
			&s.FiveHourPct, &s.SevenDayPct, &s.SevenDaySonnetPct); err != nil {
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
		       five_hour_pct, seven_day_pct, seven_day_sonnet_pct
		FROM usage_history
		WHERE credential_id = ?
		ORDER BY captured_at DESC LIMIT 1`, credID)
	var s Snapshot
	var ts int64
	if err := row.Scan(&s.CredentialID, &ts,
		&s.FiveHourPct, &s.SevenDayPct, &s.SevenDaySonnetPct); err != nil {
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
	fh := extractSeries(snapshots, func(s Snapshot) float64 { return s.FiveHourPct })
	sd := extractSeries(snapshots, func(s Snapshot) float64 { return s.SevenDayPct })
	sds := extractSeries(snapshots, func(s Snapshot) float64 { return s.SevenDaySonnetPct })

	fh = downsample(fh, maxPts)
	sd = downsample(sd, maxPts)
	sds = downsample(sds, maxPts)

	from := snapshots[0].CapturedAt.UTC().Format("2006-01-02 15:04 UTC")
	to := snapshots[len(snapshots)-1].CapturedAt.UTC().Format("2006-01-02 15:04 UTC")

	graph := asciigraph.PlotMany(
		[][]float64{fh, sd, sds},
		asciigraph.Height(10),
		asciigraph.LowerBound(0),
		asciigraph.UpperBound(100),
		asciigraph.SeriesColors(asciigraph.Red, asciigraph.Blue, asciigraph.Green),
		asciigraph.SeriesLegends("5h", "7d", "7d-sonnet"),
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
