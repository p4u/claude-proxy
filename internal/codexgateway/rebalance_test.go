package codexgateway

import (
	"testing"
	"time"
)

func TestParseCodexQuotaMapsByWindowMinutes(t *testing.T) {
	// prolite plan: primary is the 7-day window, secondary is zero.
	prolite := rawQuotaBlob{
		ObservedAt: "2026-09-05T04:31:21.399327709+08:00",
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":     "7",
			"X-Codex-Primary-Window-Minutes":   "10080",
			"X-Codex-Primary-Reset-At":         "1789082556",
			"X-Codex-Secondary-Used-Percent":   "0",
			"X-Codex-Secondary-Window-Minutes": "0",
			"X-Codex-Plan-Type":                "prolite",
		},
	}
	q := parseCodexQuota(prolite)
	if q.PlanType != "prolite" {
		t.Fatalf("plan type: got %q", q.PlanType)
	}
	if q.SevenDayPct != 7 || q.SevenDayResets != 1789082556 {
		t.Fatalf("primary 10080min should map to seven-day; got pct=%v resets=%v", q.SevenDayPct, q.SevenDayResets)
	}
	if q.FiveHourPct != 0 || q.FiveHourResets != 0 {
		t.Fatalf("no 5h data → zero; got pct=%v resets=%v", q.FiveHourPct, q.FiveHourResets)
	}

	// team plan: primary is 5h, secondary is 7d.
	team := rawQuotaBlob{
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":     "12",
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         "1788569818",
			"X-Codex-Secondary-Used-Percent":   "3",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       "1789156618",
			"X-Codex-Plan-Type":                "team",
		},
	}
	q2 := parseCodexQuota(team)
	if q2.FiveHourPct != 12 || q2.FiveHourResets != 1788569818 {
		t.Fatalf("primary 300min should map to 5h; got pct=%v resets=%v", q2.FiveHourPct, q2.FiveHourResets)
	}
	if q2.SevenDayPct != 3 || q2.SevenDayResets != 1789156618 {
		t.Fatalf("secondary 10080min should map to 7d; got pct=%v resets=%v", q2.SevenDayPct, q2.SevenDayResets)
	}

	if empty := parseCodexQuota(rawQuotaBlob{}); empty.HasSignals {
		t.Fatal("empty signals must be marked HasSignals=false")
	}
}

func TestEffectiveWeightsScalesByScore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	accounts := []Account{
		// Fresh 5h+7d, weight 1 → score ~1
		{Name: "A", Quota: CodexQuota{HasSignals: true, FiveHourPct: 0, SevenDayPct: 0}},
		// Half-saturated → smaller score
		{Name: "B", Quota: CodexQuota{HasSignals: true, FiveHourPct: 50, SevenDayPct: 50}},
		// Fully saturated → floor 1
		{Name: "C", Quota: CodexQuota{HasSignals: true, FiveHourPct: 100, SevenDayPct: 100}},
		// Disabled → skipped
		{Name: "D", Disabled: true},
	}
	base := map[string]int64{"A": 1, "B": 1, "C": 1}
	got := EffectiveWeights(accounts, base, now)

	if _, ok := got["D"]; ok {
		t.Fatal("disabled accounts must be omitted")
	}
	if got["A"] != effectiveWeightMax {
		t.Fatalf("A should get max weight; got %d", got["A"])
	}
	if got["B"] <= 1 || got["B"] >= got["A"] {
		t.Fatalf("B should be between floor and A; got A=%d B=%d", got["A"], got["B"])
	}
	if got["C"] != 1 {
		t.Fatalf("saturated C should floor to 1; got %d", got["C"])
	}
}

func TestEffectiveWeightsUsesBaseWeightInScore(t *testing.T) {
	// Identical quota; base 1 vs 5. Base enters pool.Score linearly, so the
	// integer ratio must be 1:5 (± one from rounding).
	now := time.Unix(1_700_000_000, 0)
	q := CodexQuota{HasSignals: true, FiveHourPct: 20, SevenDayPct: 20}
	accounts := []Account{{Name: "small", Quota: q}, {Name: "big", Quota: q}}
	base := map[string]int64{"small": 1, "big": 5}
	got := EffectiveWeights(accounts, base, now)
	if got["big"] != effectiveWeightMax {
		t.Fatalf("higher base should win max; got big=%d", got["big"])
	}
	want := int64(effectiveWeightMax / 5)
	if got["small"] != want && got["small"] != want+1 {
		t.Fatalf("small should be ~1/5 of big; got small=%d big=%d", got["small"], got["big"])
	}
}
