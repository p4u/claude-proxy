package webui

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
)

type usageWindow struct {
	Pct      float64 `json:"pct"`
	ResetsAt *string `json:"resets_at"`
}

// selectionView mirrors the pool's usage-aware selection scoring for one
// credential so the UI can explain why traffic is (or isn't) routed to it. The
// score/room math is shared with internal/pool so the two can never drift.
type selectionView struct {
	Room5h    float64 `json:"room_5h"`
	Room7d    float64 `json:"room_7d"`
	Score     float64 `json:"score"`
	SharePct  float64 `json:"share_pct"`
	Saturated bool    `json:"saturated"`
}

// meteredWindow is what this proxy itself observed for a credential over a
// rolling window, summed from request_log.
//
// It is not a quota reading. Providers without a usage API publish no
// allowance, so there is no denominator and deliberately no percentage here —
// and it counts only traffic that went through this proxy, so a key also used
// elsewhere has consumed more than this shows.
type meteredWindow struct {
	Requests            int64 `json:"requests"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
}

type meteredUsage struct {
	FiveHour meteredWindow `json:"five_hour"`
	SevenDay meteredWindow `json:"seven_day"`
}

type usageCurrent struct {
	CredentialID     string        `json:"credential_id"`
	Label            string        `json:"label"`
	SubscriptionType string        `json:"subscription_type"`
	Provider         string        `json:"provider"`
	HasUsageAPI      bool          `json:"has_usage_api"`
	Status           string        `json:"status"`
	Weight           int           `json:"weight"`
	FiveHour         usageWindow   `json:"five_hour"`
	SevenDay         usageWindow   `json:"seven_day"`
	SevenDaySonnet   usageWindow   `json:"seven_day_sonnet"`
	CapturedAt       *string       `json:"captured_at"`
	Selection        selectionView `json:"selection"`
	// Metered is populated only for providers with no usage API, where it is
	// the only usage figure available.
	Metered *meteredUsage `json:"metered,omitempty"`
}

// meteredByCredential sums request_log over a rolling window, per credential.
// One query for all credentials rather than one per credential; the
// (ts) index carries it.
func (s *Server) meteredByCredential(ctx context.Context, window time.Duration) (map[string]meteredWindow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(credential_id,''), COUNT(*),
		       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0)
		FROM request_log
		WHERE ts >= ? AND credential_id != ''
		GROUP BY credential_id`, time.Now().Add(-window).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]meteredWindow{}
	for rows.Next() {
		var id string
		var m meteredWindow
		if err := rows.Scan(&id, &m.Requests, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheCreationTokens); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

func rfc3339Ptr(sec sql.NullInt64) *string {
	if !sec.Valid {
		return nil
	}
	s := time.Unix(sec.Int64, 0).UTC().Format(time.RFC3339)
	return &s
}

func (s *Server) handleUsageCurrent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := creds.List(ctx, s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only queried when some credential actually needs it.
	var metered5h, metered7d map[string]meteredWindow
	for _, c := range list {
		if !provider.Get(c.Provider).PollsUsage {
			metered5h, _ = s.meteredByCredential(ctx, 5*time.Hour)
			metered7d, _ = s.meteredByCredential(ctx, 7*24*time.Hour)
			break
		}
	}

	out := make([]usageCurrent, 0, len(list))
	for _, c := range list {
		uc := usageCurrent{
			CredentialID:     c.ID,
			Label:            c.Label,
			SubscriptionType: c.SubscriptionType,
			Provider:         string(provider.Get(c.Provider).ID),
			HasUsageAPI:      provider.Get(c.Provider).PollsUsage,
			Status:           string(c.Status),
			Weight:           c.Weight,
		}
		var capturedAt int64
		var fhReset, sdReset, sdsReset sql.NullInt64
		var capValid bool
		row := s.db.QueryRowContext(ctx, `
			SELECT captured_at,
			       five_hour_pct, five_hour_resets_at,
			       seven_day_pct, seven_day_resets_at,
			       seven_day_sonnet_pct, seven_day_sonnet_resets_at
			FROM usage_history WHERE credential_id = ?
			ORDER BY captured_at DESC LIMIT 1`, c.ID)
		if err := row.Scan(&capturedAt,
			&uc.FiveHour.Pct, &fhReset,
			&uc.SevenDay.Pct, &sdReset,
			&uc.SevenDaySonnet.Pct, &sdsReset); err == nil {
			capValid = true
		}
		if capValid {
			uc.FiveHour.ResetsAt = rfc3339Ptr(fhReset)
			uc.SevenDay.ResetsAt = rfc3339Ptr(sdReset)
			uc.SevenDaySonnet.ResetsAt = rfc3339Ptr(sdsReset)
			ca := time.Unix(capturedAt, 0).UTC().Format(time.RFC3339)
			uc.CapturedAt = &ca
		}
		if !provider.Get(c.Provider).PollsUsage {
			uc.Metered = &meteredUsage{
				FiveHour: metered5h[c.ID],
				SevenDay: metered7d[c.ID],
			}
		}
		// Selection scoring mirrors the pool exactly. With no snapshot the pcts
		// are 0 → rooms 1 → score = weight (the pool's bootstrap headroom).
		uc.Selection = selectionView{
			Room5h:    pool.Room(uc.FiveHour.Pct),
			Room7d:    pool.Room(uc.SevenDay.Pct),
			Score:     pool.Score(c.Weight, uc.FiveHour.Pct, uc.SevenDay.Pct),
			Saturated: pool.Saturated(uc.FiveHour.Pct, uc.SevenDay.Pct),
		}
		out = append(out, uc)
	}

	// share_pct = score / Σscore × 100 across ACTIVE credentials (the pool's
	// candidate set); non-active credentials are never selected → share 0.
	//
	// Totalled PER PROVIDER, because the pool filters by provider before it
	// scores: a GLM key only ever competes with other GLM keys. Summing across
	// providers would understate every credential of the smaller pool — a lone
	// GLM key that takes 100% of GLM traffic would read as a few percent, or as
	// 0% whenever the Anthropic scores dominate.
	totals := map[string]float64{}
	for i := range out {
		if out[i].Status == string(creds.StatusActive) {
			totals[out[i].Provider] += out[i].Selection.Score
		}
	}
	for i := range out {
		total := totals[out[i].Provider]
		if total > 0 && out[i].Status == string(creds.StatusActive) {
			out[i].Selection.SharePct = out[i].Selection.Score / total * 100
		}
	}
	writeJSON(w, out)
}

// usagePoint is one raw snapshot row for a single credential.
type usagePoint struct {
	TS                int64
	FiveHourPct       float64
	SevenDayPct       float64
	SevenDaySonnetPct float64
}

// usageGridSeries is one credential's values aligned to the shared bucket grid.
// A nil element means the credential had no snapshot in that bucket.
type usageGridSeries struct {
	CredentialID      string     `json:"credential_id"`
	Label             string     `json:"label"`
	FiveHourPct       []*float64 `json:"five_hour_pct"`
	SevenDayPct       []*float64 `json:"seven_day_pct"`
	SevenDaySonnetPct []*float64 `json:"seven_day_sonnet_pct"`
}

// handleUsageHistory returns an aligned grid: a single `buckets` axis (the union
// of snapshot timestamps across all credentials, downsampled to ≤200) plus one
// series per credential whose value arrays are null-filled where that credential
// has no snapshot at a bucket. This lets the frontend chart every series against
// one shared x-axis instead of misaligned per-credential timelines.
func (s *Server) handleUsageHistory(w http.ResponseWriter, r *http.Request) {
	from, to, _, err := parseWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	since := time.Unix(from, 0)
	until := time.Unix(to, 0)

	list, err := creds.List(ctx, s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	filter := r.URL.Query().Get("credential_id")

	// Gather each credential's raw snapshots and the union of timestamps.
	tsSet := map[int64]struct{}{}
	perCred := map[string][]usagePoint{}
	order := []string{}
	labels := map[string]string{}
	for _, c := range list {
		if filter != "" && c.ID != filter {
			continue
		}
		points, err := s.usageHistoryPoints(ctx, c.ID, since, until)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		perCred[c.ID] = points
		order = append(order, c.ID)
		labels[c.ID] = c.Label
		for _, p := range points {
			tsSet[p.TS] = struct{}{}
		}
	}

	buckets := make([]int64, 0, len(tsSet))
	for ts := range tsSet {
		buckets = append(buckets, ts)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
	buckets = downsample(buckets, maxBuckets)

	// Index each bucket timestamp for O(1) alignment.
	idx := make(map[int64]int, len(buckets))
	for i, ts := range buckets {
		idx[ts] = i
	}

	out := make([]usageGridSeries, 0, len(order))
	for _, cid := range order {
		g := usageGridSeries{
			CredentialID:      cid,
			Label:             labels[cid],
			FiveHourPct:       make([]*float64, len(buckets)),
			SevenDayPct:       make([]*float64, len(buckets)),
			SevenDaySonnetPct: make([]*float64, len(buckets)),
		}
		for _, p := range perCred[cid] {
			i, ok := idx[p.TS]
			if !ok {
				continue // dropped by downsampling
			}
			fh, sd, ss := p.FiveHourPct, p.SevenDayPct, p.SevenDaySonnetPct
			g.FiveHourPct[i] = &fh
			g.SevenDayPct[i] = &sd
			g.SevenDaySonnetPct[i] = &ss
		}
		out = append(out, g)
	}
	writeJSON(w, map[string]any{"buckets": buckets, "series": out})
}

// downsample reduces ts to at most maxN evenly-spaced entries, always keeping
// the first and last. Input must be sorted ascending.
func downsample(ts []int64, maxN int) []int64 {
	if len(ts) <= maxN || maxN <= 0 {
		return ts
	}
	out := make([]int64, 0, maxN)
	for i := 0; i < maxN; i++ {
		j := i * (len(ts) - 1) / (maxN - 1)
		out = append(out, ts[j])
	}
	return out
}

func (s *Server) usageHistoryPoints(ctx context.Context, credID string, since, until time.Time) ([]usagePoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT captured_at, five_hour_pct, seven_day_pct, seven_day_sonnet_pct
		FROM usage_history
		WHERE credential_id = ? AND captured_at >= ? AND captured_at < ?
		ORDER BY captured_at ASC`, credID, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usagePoint{}
	for rows.Next() {
		var p usagePoint
		if err := rows.Scan(&p.TS, &p.FiveHourPct, &p.SevenDayPct, &p.SevenDaySonnetPct); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
