package usertoken

import (
	"context"
	"time"

	"github.com/p4u/claude-proxy/internal/store"
)

// UserStat is per-user request aggregation over a time window, joining
// user_tokens with request_log. Users with no requests appear with zeroes.
type UserStat struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Requests      int64  `json:"requests"`
	OK            int64  `json:"ok"`
	Errors        int64  `json:"errors"`
	TokensIn      int64  `json:"tokens_in"`
	TokensOut     int64  `json:"tokens_out"`
	CacheRead     int64  `json:"cache_read"`
	CacheCreation int64  `json:"cache_creation"`
	BytesSent     int64  `json:"bytes_sent"`
	BytesReceived int64  `json:"bytes_received"`
	AvgLatencyMs  int64  `json:"avg_latency_ms"`
	Conversations int64  `json:"conversations"`
}

// Stats returns per-user request aggregation for rows since `since`. When
// filterID is non-empty only that user is returned. Every listed user is
// included even with zero activity in the window.
func Stats(ctx context.Context, db *store.DB, since time.Time, filterID string) ([]UserStat, error) {
	list, err := List(ctx, db)
	if err != nil {
		return nil, err
	}
	sinceUnix := since.Unix()
	out := make([]UserStat, 0, len(list))
	for _, ut := range list {
		if filterID != "" && ut.ID != filterID {
			continue
		}
		s := UserStat{ID: ut.ID, Name: ut.Name}
		var latencySum int64
		err := db.QueryRowContext(ctx, `
			SELECT
				COUNT(*),
				COALESCE(SUM(CASE WHEN status_code=200 THEN 1 ELSE 0 END),0),
				COALESCE(SUM(CASE WHEN status_code>=400 OR status_code=-1 THEN 1 ELSE 0 END),0),
				COALESCE(SUM(input_tokens),0),
				COALESCE(SUM(output_tokens),0),
				COALESCE(SUM(cache_read_tokens),0),
				COALESCE(SUM(cache_creation_tokens),0),
				COALESCE(SUM(bytes_sent),0),
				COALESCE(SUM(bytes_received),0),
				COALESCE(SUM(latency_ms),0),
				COUNT(DISTINCT CASE WHEN conv_id!='' THEN conv_id END)
			FROM request_log
			WHERE user_token_id=? AND ts>=?`, ut.ID, sinceUnix).
			Scan(&s.Requests, &s.OK, &s.Errors,
				&s.TokensIn, &s.TokensOut, &s.CacheRead, &s.CacheCreation,
				&s.BytesSent, &s.BytesReceived, &latencySum, &s.Conversations)
		if err != nil {
			return nil, err
		}
		if s.Requests > 0 {
			s.AvgLatencyMs = latencySum / s.Requests
		}
		out = append(out, s)
	}
	return out, nil
}
