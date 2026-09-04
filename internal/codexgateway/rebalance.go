package codexgateway

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/store"
)

// parseCodexQuota lifts the string-typed sidecar signals into the typed
// CodexQuota we score against. Windows are picked by their published length
// (300 min = 5h, 10080 min = 7d) rather than by primary/secondary name,
// because the mapping is plan-dependent: on "prolite" the primary window is
// weekly; on "team" it is 5-hour.
func parseCodexQuota(raw rawQuotaBlob) CodexQuota {
	q := CodexQuota{}
	if len(raw.Signals) == 0 {
		return q
	}
	q.HasSignals = true
	q.PlanType = strings.TrimSpace(raw.Signals["X-Codex-Plan-Type"])
	if t, err := time.Parse(time.RFC3339Nano, raw.ObservedAt); err == nil {
		q.ObservedAtUnix = t.Unix()
	}
	for _, slot := range []string{"Primary", "Secondary"} {
		win := parseIntSignal(raw.Signals, "X-Codex-"+slot+"-Window-Minutes")
		if win == 0 {
			continue
		}
		pct := parseFloatSignal(raw.Signals, "X-Codex-"+slot+"-Used-Percent")
		reset := parseIntSignal(raw.Signals, "X-Codex-"+slot+"-Reset-At")
		switch {
		case win <= 360:
			q.FiveHourPct, q.FiveHourResets = pct, reset
		case win >= 6*24*60:
			q.SevenDayPct, q.SevenDayResets = pct, reset
		}
	}
	return q
}

func parseIntSignal(m map[string]string, k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(m[k]), 10, 64)
	return n
}
func parseFloatSignal(m map[string]string, k string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(m[k]), 64)
	return f
}

// AccountScore is the effective selection score for one Codex account, using
// the same math the Anthropic pool uses per pick.
func AccountScore(baseWeight int, q CodexQuota, now time.Time) float64 {
	base := pool.Score(baseWeight, q.FiveHourPct, q.SevenDayPct)
	boost := pool.BurnBoost(q.FiveHourPct, q.SevenDayPct, q.FiveHourResets, q.SevenDayResets, now)
	return base * (1 + pool.BurnCoef*boost)
}

// BaseWeight returns the operator-configured weight for a Codex account.
// Missing rows default to 1.
func BaseWeight(ctx context.Context, db *store.DB, name string) (int64, error) {
	var w int64
	err := db.QueryRowContext(ctx, `SELECT weight FROM codex_account_weight WHERE name=?`, name).Scan(&w)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	if w < 1 {
		w = 1
	}
	return w, nil
}

func SetBaseWeight(ctx context.Context, db *store.DB, name string, weight int64) error {
	if weight < 1 || weight > 1_000_000 {
		return errors.New("weight must be between 1 and 1000000")
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO codex_account_weight(name, weight) VALUES(?, ?)
		ON CONFLICT(name) DO UPDATE SET weight=excluded.weight`, name, weight)
	return err
}

// BaseWeightsMap loads every configured base weight in one query. Exported
// for the web UI's decoration pass.
func BaseWeightsMap(ctx context.Context, db *store.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, weight FROM codex_account_weight`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var n string
		var w int64
		if err := rows.Scan(&n, &w); err != nil {
			return nil, err
		}
		out[n] = w
	}
	return out, rows.Err()
}

// Sidecar's integer weight domain; 1000 gives enough resolution without
// producing meaningless six-digit numbers in the account list.
const effectiveWeightMax = 1000

// EffectiveWeights normalizes per-account scores into integer sidecar weights.
// Shared with the /api/codex/accounts endpoint so the browser view matches
// what the loop will push next.
func EffectiveWeights(accounts []Account, base map[string]int64, now time.Time) map[string]int64 {
	scores := make(map[string]float64, len(accounts))
	var maxScore float64
	for _, a := range accounts {
		if a.Disabled || a.Unavailable {
			continue
		}
		w := base[a.Name]
		if w < 1 {
			w = 1
		}
		s := AccountScore(int(w), a.Quota, now)
		scores[a.Name] = s
		if s > maxScore {
			maxScore = s
		}
	}
	out := make(map[string]int64, len(scores))
	for name, s := range scores {
		if maxScore <= 0 || s <= 0 {
			// Saturated or fully spent: floor of 1 so the account can still
			// receive a probe request that would surface a real 429.
			out[name] = 1
			continue
		}
		w := int64(math.Round(s / maxScore * effectiveWeightMax))
		if w < 1 {
			w = 1
		}
		out[name] = w
	}
	return out
}

func RebalanceOnce(ctx context.Context, db *store.DB, c *Client, log *slog.Logger, now time.Time) error {
	if c == nil {
		return nil
	}
	accounts, err := c.Accounts(ctx)
	if err != nil {
		return err
	}
	base, err := BaseWeightsMap(ctx, db)
	if err != nil {
		return err
	}
	target := EffectiveWeights(accounts, base, now)
	for _, a := range accounts {
		want, ok := target[a.Name]
		if !ok {
			continue
		}
		var have int64
		if a.Weight != nil {
			have = *a.Weight
		}
		if have == want {
			continue
		}
		if err := c.SetWeight(ctx, a.Name, want); err != nil {
			if log != nil {
				log.Warn("codex rebalance set weight failed", "account", a.Name, "want", want, "err", err)
			}
			continue
		}
		if log != nil {
			log.Debug("codex rebalance", "account", a.Name, "from", have, "to", want,
				"fh_pct", a.Quota.FiveHourPct, "sd_pct", a.Quota.SevenDayPct,
				"base_weight", base[a.Name])
		}
	}
	return nil
}

func RebalanceLoop(ctx context.Context, db *store.DB, c *Client, log *slog.Logger, interval time.Duration) {
	if c == nil {
		return
	}
	if interval <= 0 {
		interval = 90 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	if err := RebalanceOnce(ctx, db, c, log, time.Now()); err != nil && log != nil {
		log.Warn("codex rebalance initial pass failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := RebalanceOnce(ctx, db, c, log, now); err != nil && log != nil {
				log.Warn("codex rebalance pass failed", "err", err)
			}
		}
	}
}
