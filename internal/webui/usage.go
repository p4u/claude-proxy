package webui

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

type usageWindow struct {
	Pct      float64 `json:"pct"`
	ResetsAt *string `json:"resets_at"`
}

type usageCurrent struct {
	CredentialID     string      `json:"credential_id"`
	Label            string      `json:"label"`
	SubscriptionType string      `json:"subscription_type"`
	Status           string      `json:"status"`
	Weight           int         `json:"weight"`
	FiveHour         usageWindow `json:"five_hour"`
	SevenDay         usageWindow `json:"seven_day"`
	SevenDaySonnet   usageWindow `json:"seven_day_sonnet"`
	CapturedAt       *string     `json:"captured_at"`
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
	out := make([]usageCurrent, 0, len(list))
	for _, c := range list {
		uc := usageCurrent{
			CredentialID:     c.ID,
			Label:            c.Label,
			SubscriptionType: c.SubscriptionType,
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
		out = append(out, uc)
	}
	writeJSON(w, out)
}

type usagePoint struct {
	TS                int64   `json:"ts"`
	FiveHourPct       float64 `json:"five_hour_pct"`
	SevenDayPct       float64 `json:"seven_day_pct"`
	SevenDaySonnetPct float64 `json:"seven_day_sonnet_pct"`
}

type usageSeries struct {
	CredentialID string       `json:"credential_id"`
	Label        string       `json:"label"`
	Points       []usagePoint `json:"points"`
}

func (s *Server) handleUsageHistory(w http.ResponseWriter, r *http.Request) {
	from, _, _, err := periodBounds(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	since := time.Unix(from, 0)

	list, err := creds.List(ctx, s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	filter := r.URL.Query().Get("credential_id")
	labels := map[string]string{}
	for _, c := range list {
		labels[c.ID] = c.Label
	}

	out := make([]usageSeries, 0, len(list))
	for _, c := range list {
		if filter != "" && c.ID != filter {
			continue
		}
		points, err := s.usageHistoryPoints(ctx, c.ID, since)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, usageSeries{CredentialID: c.ID, Label: c.Label, Points: points})
	}
	writeJSON(w, map[string]any{"series": out})
}

func (s *Server) usageHistoryPoints(ctx context.Context, credID string, since time.Time) ([]usagePoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT captured_at, five_hour_pct, seven_day_pct, seven_day_sonnet_pct
		FROM usage_history
		WHERE credential_id = ? AND captured_at >= ?
		ORDER BY captured_at ASC`, credID, since.Unix())
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
