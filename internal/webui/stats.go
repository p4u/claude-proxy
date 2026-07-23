package webui

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/p4u/claude-proxy/internal/usage"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

const (
	defaultBuckets = 60
	maxBuckets     = 200
)

// periodBounds parses ?period into an absolute [from, now] window in unix secs.
func periodBounds(r *http.Request) (from, now, dur int64, err error) {
	p := r.URL.Query().Get("period")
	if p == "" {
		p = "24h"
	}
	d, perr := usage.ParsePeriod(p)
	if perr != nil {
		return 0, 0, 0, perr
	}
	now = time.Now().Unix()
	dur = int64(d.Seconds())
	from = now - dur
	return from, now, dur, nil
}

func bucketCount(r *http.Request) int {
	n := defaultBuckets
	if v := r.URL.Query().Get("buckets"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > maxBuckets {
		n = maxBuckets
	}
	return n
}

// bucketTimestamps returns the start-of-bucket unix timestamps.
func bucketTimestamps(from, dur int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = from + (int64(i)*dur)/int64(n)
	}
	return out
}

// bucketIndex maps a ts within [from, from+dur] to a bucket in [0, n-1].
func bucketIndex(ts, from, dur int64, n int) int {
	if dur <= 0 {
		return 0
	}
	idx := int((ts - from) * int64(n) / dur)
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

type seriesRow struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Requests  []int64 `json:"requests"`
	Errors    []int64 `json:"errors"`
	TokensIn  []int64 `json:"tokens_in"`
	TokensOut []int64 `json:"tokens_out"`
}

func newSeriesRow(id, label string, n int) *seriesRow {
	return &seriesRow{
		ID: id, Label: label,
		Requests:  make([]int64, n),
		Errors:    make([]int64, n),
		TokensIn:  make([]int64, n),
		TokensOut: make([]int64, n),
	}
}

// handleStatsSeries powers both /stats/requests and /stats/tokens: they share
// the same {buckets, series} shape; the frontend reads the fields it needs.
func (s *Server) handleStatsSeries(w http.ResponseWriter, r *http.Request, _ string) {
	from, now, dur, err := periodBounds(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n := bucketCount(r)
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "user"
	}

	ctx := r.Context()
	labels, err := s.labelMap(ctx, groupBy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, COALESCE(user_token_id,''), COALESCE(credential_id,''),
		       status_code, input_tokens, output_tokens
		FROM request_log WHERE ts >= ? AND ts <= ?`, from, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	series := map[string]*seriesRow{}
	order := []string{}
	for rows.Next() {
		var ts, status, tin, tout int64
		var uid, cid string
		if err := rows.Scan(&ts, &uid, &cid, &status, &tin, &tout); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		id, label := seriesKey(groupBy, uid, cid, labels)
		sr := series[id]
		if sr == nil {
			sr = newSeriesRow(id, label, n)
			series[id] = sr
			order = append(order, id)
		}
		bi := bucketIndex(ts, from, dur, n)
		sr.Requests[bi]++
		if status >= 400 || status == -1 {
			sr.Errors[bi]++
		}
		sr.TokensIn[bi] += tin
		sr.TokensOut[bi] += tout
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]*seriesRow, 0, len(order))
	for _, id := range order {
		out = append(out, series[id])
	}
	writeJSON(w, map[string]any{
		"buckets": bucketTimestamps(from, dur, n),
		"series":  out,
	})
}

// seriesKey resolves the grouping key + display label for a request_log row.
func seriesKey(groupBy, uid, cid string, labels map[string]string) (string, string) {
	switch groupBy {
	case "credential":
		if cid == "" {
			return "none", "(none)"
		}
		lbl := labels[cid]
		if lbl == "" {
			lbl = cid
		}
		return cid, lbl
	case "none":
		return "all", "all"
	default: // user
		if uid == "" {
			return "anonymous", "anonymous"
		}
		lbl := labels[uid]
		if lbl == "" {
			lbl = uid
		}
		return uid, lbl
	}
}

func (s *Server) labelMap(ctx context.Context, groupBy string) (map[string]string, error) {
	out := map[string]string{}
	var q string
	switch groupBy {
	case "credential":
		q = `SELECT id, COALESCE(label,'') FROM credentials`
	case "user":
		q = `SELECT id, name FROM user_tokens`
	default:
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		out[id] = label
	}
	return out, rows.Err()
}

func (s *Server) handleStatsUsers(w http.ResponseWriter, r *http.Request) {
	from, _, _, err := periodBounds(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := usertoken.Stats(r.Context(), s.db, time.Unix(from, 0), "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if stats == nil {
		stats = []usertoken.UserStat{}
	}
	writeJSON(w, stats)
}

func (s *Server) handleStatsLatency(w http.ResponseWriter, r *http.Request) {
	from, now, dur, err := periodBounds(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n := bucketCount(r)

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT ts, latency_ms FROM request_log WHERE ts >= ? AND ts <= ?`, from, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	perBucket := make([][]int64, n)
	for rows.Next() {
		var ts, lat int64
		if err := rows.Scan(&ts, &lat); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		bi := bucketIndex(ts, from, dur, n)
		perBucket[bi] = append(perBucket[bi], lat)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	avg := make([]int64, n)
	p95 := make([]int64, n)
	for i, vals := range perBucket {
		if len(vals) == 0 {
			continue
		}
		var sum int64
		for _, v := range vals {
			sum += v
		}
		avg[i] = sum / int64(len(vals))
		p95[i] = percentile(vals, 95)
	}
	writeJSON(w, map[string]any{
		"buckets": bucketTimestamps(from, dur, n),
		"avg_ms":  avg,
		"p95_ms":  p95,
	})
}

// percentile returns the p-th percentile (nearest-rank) of vals.
func percentile(vals []int64, p int) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since := time.Now().Add(-24 * time.Hour).Unix()

	var (
		requests, errors                    int64
		tin, tout, cacheRead, cacheCreation int64
		latencySum                          int64
	)
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status_code>=400 OR status_code=-1 THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
		       COALESCE(SUM(latency_ms),0)
		FROM request_log WHERE ts >= ?`, since).
		Scan(&requests, &errors, &tin, &tout, &cacheRead, &cacheCreation, &latencySum)

	var activeConvs, usersTotal int64
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE status='active'`).Scan(&activeConvs)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_tokens`).Scan(&usersTotal)

	creds := map[string]int64{"total": 0, "active": 0, "limited": 0, "errored": 0}
	crows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM credentials GROUP BY status`)
	if err == nil {
		for crows.Next() {
			var status string
			var c int64
			_ = crows.Scan(&status, &c)
			creds["total"] += c
			switch status {
			case "active":
				creds["active"] += c
			case "limited":
				creds["limited"] += c
			case "expired", "revoked":
				creds["errored"] += c
			}
		}
		crows.Close()
	}

	var avgLat int64
	if requests > 0 {
		avgLat = latencySum / requests
	}
	var errRate float64
	if requests > 0 {
		errRate = float64(errors) / float64(requests)
	}

	writeJSON(w, map[string]any{
		"requests_24h": requests,
		"tokens_24h": map[string]int64{
			"input": tin, "output": tout,
			"cache_read": cacheRead, "cache_creation": cacheCreation,
		},
		"active_conversations": activeConvs,
		"credentials":          creds,
		"users_total":          usersTotal,
		"avg_latency_ms_24h":   avgLat,
		"error_rate_24h":       errRate,
	})
}
