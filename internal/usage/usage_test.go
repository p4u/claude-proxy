package usage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

// Trimmed from a live GET /api/oauth/usage response (2026-09-29, Max plan):
// the per-model seven_day_sonnet/opus buckets are null, and the Fable cap is
// published only as a "weekly_scoped" entry in limits[].
const livePayload = `{
 "five_hour": {"utilization": 23.0, "resets_at": "2026-09-29T15:00:00.252549+00:00"},
 "seven_day": {"utilization": 77.0, "resets_at": "2026-09-30T18:00:00.252569+00:00"},
 "seven_day_opus": null,
 "seven_day_sonnet": null,
 "limits": [
  {"kind": "session", "group": "session", "percent": 23, "severity": "normal",
   "resets_at": "2026-09-29T15:00:00.252549+00:00", "scope": null, "is_active": false},
  {"kind": "weekly_all", "group": "weekly", "percent": 77, "severity": "warning",
   "resets_at": "2026-09-30T18:00:00.252569+00:00", "scope": null, "is_active": true},
  {"kind": "weekly_scoped", "group": "weekly", "percent": 30, "severity": "normal",
   "resets_at": "2026-09-30T18:00:00.252737+00:00",
   "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
 ]
}`

func TestScopedWeeklyFromLimits(t *testing.T) {
	var r Response
	if err := json.Unmarshal([]byte(livePayload), &r); err != nil {
		t.Fatal(err)
	}
	sc := r.ScopedWeekly()
	if sc == nil {
		t.Fatal("weekly_scoped limit not found")
	}
	if sc.Label != "Fable" || sc.Pct != 30 || sc.ResetsAt == nil || *sc.ResetsAt != "2026-09-30T18:00:00.252737+00:00" {
		t.Fatalf("scoped = %+v", sc)
	}

	var none Response
	if err := json.Unmarshal([]byte(`{"five_hour":{"utilization":1},"seven_day":{"utilization":2},"limits":[]}`), &none); err != nil {
		t.Fatal(err)
	}
	if none.ScopedWeekly() != nil {
		t.Fatal("no weekly_scoped entry must yield nil")
	}
}

// Several scoped caps: the fullest one is the one about to block requests.
func TestScopedWeeklyPicksFullest(t *testing.T) {
	var r Response
	if err := json.Unmarshal([]byte(`{"limits":[
	  {"kind":"weekly_scoped","percent":12,"scope":{"model":{"display_name":"Fable"}}},
	  {"kind":"weekly_scoped","percent":64,"scope":{"model":{"id":"claude-opus-5-5","display_name":""}}},
	  {"kind":"weekly_all","percent":90}
	]}`), &r); err != nil {
		t.Fatal(err)
	}
	if sc := r.ScopedWeekly(); sc == nil || sc.Pct != 64 || sc.Label != "claude-opus-5-5" {
		t.Fatalf("scoped = %+v, want the 64%% limit labelled by model id", sc)
	}
}

func TestSaveStoresScopedLimit(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}

	var r Response
	if err := json.Unmarshal([]byte(livePayload), &r); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, db, c.ID, &r); err != nil {
		t.Fatal(err)
	}
	s, err := LastSnapshot(ctx, db, c.ID)
	if err != nil || s == nil {
		t.Fatalf("snapshot = %v, %v", s, err)
	}
	if s.FiveHourPct != 23 || s.SevenDayPct != 77 {
		t.Fatalf("windows = %v / %v", s.FiveHourPct, s.SevenDayPct)
	}
	if s.SevenDayScopedPct == nil || *s.SevenDayScopedPct != 30 || s.SevenDayScopedLabel != "Fable" {
		t.Fatalf("scoped = %v %q", s.SevenDayScopedPct, s.SevenDayScopedLabel)
	}

	// A plan without a scoped cap stores NULL, not a misleading 0%.
	if err := Save(ctx, db, c.ID, &Response{}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_history WHERE seven_day_scoped_pct IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("NULL scoped rows = %d, %v", n, err)
	}
}
