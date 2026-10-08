package webui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/subscriptionstats"
	"github.com/p4u/claude-proxy/internal/usage"
)

func (s *Server) handleSubscriptionValue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	now := time.Now()
	from, to, err := subscriptionValueWindow(r, now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	window := r.URL.Query().Get("quota_window")
	if window == "" {
		window = "seven_day"
	}
	if window != "seven_day" && window != "five_hour" {
		writeErr(w, http.StatusBadRequest, "quota_window must be seven_day or five_hour")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	report, err := subscriptionstats.Build(ctx, s.db.DB, time.Unix(from, 0), time.Unix(to, 0), now, window)
	if err != nil {
		if ctx.Err() != nil {
			writeErr(w, http.StatusServiceUnavailable, "statistics query interrupted; try a shorter period")
			return
		}
		slog.Warn("subscription statistics query failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "could not load subscription statistics")
		return
	}
	writeJSON(w, report)
}

// Keep the shared stats presets unchanged; this page defaults to 30d and also
// offers 90d. Custom ranges retain the common half-open interval convention.
func subscriptionValueWindow(r *http.Request, now time.Time) (int64, int64, error) {
	q := r.URL.Query()
	var from, to int64
	var err error
	if !q.Has("from") && !q.Has("to") {
		span := 30 * 24 * time.Hour
		if period := q.Get("period"); period == "90d" {
			span = 90 * 24 * time.Hour
		} else if period != "" {
			span, err = usage.ParsePeriod(period)
			if err != nil {
				return 0, 0, err
			}
		}
		from, to = now.Add(-span).Unix(), now.Unix()+1
	} else {
		if (q.Has("from") || q.Has("to")) && (q.Get("from") == "" || q.Get("to") == "") {
			return 0, 0, errors.New("both from and to are required")
		}
		from, to, _, err = parseWindow(r)
		if err != nil {
			return 0, 0, err
		}
	}
	// Check absolute bounds before subtraction to reject integer overflow in
	// hostile query strings as well as future windows with misleading empty data.
	if from < 0 || to <= from || to > now.Unix()+1 {
		return 0, 0, errors.New("window must be nonnegative, ordered, and not in the future")
	}
	if to-from > maxWindowSeconds+1 {
		return 0, 0, errors.New("window too large: max span is 90 days")
	}
	return from, to, nil
}

func (s *Server) setCredentialTier(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Tier *string `json:"tier"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Tier == nil {
		writeErr(w, http.StatusBadRequest, "tier must be a string; use an empty string to clear it")
		return
	}
	if err := creds.SetRateLimitTier(r.Context(), s.db, id, *body.Tier); err != nil {
		switch {
		case errors.Is(err, creds.ErrNotFound):
			writeErr(w, http.StatusNotFound, "credential not found")
		case errors.Is(err, creds.ErrInvalidRateLimitTier):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			slog.Warn("subscription tier update failed", "error", err)
			writeErr(w, http.StatusInternalServerError, "could not update subscription tier")
		}
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
