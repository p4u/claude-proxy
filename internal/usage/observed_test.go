package usage

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

func TestSaveObservedUsage(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "observed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := creds.Insert(ctx, db, "observed", "max", "access", "refresh", time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body       string
		fh, sd           float64
		fhKnown, sdKnown bool
	}{
		{"absent", `{}`, 0, 0, false, false},
		{"null buckets", `{"five_hour":null,"seven_day":null}`, 0, 0, false, false},
		{"null utilization", `{"five_hour":{"utilization":null},"seven_day":{}}`, 0, 0, false, false},
		{"real zero", `{"five_hour":{"utilization":0},"seven_day":{"utilization":0}}`, 0, 0, true, true},
		{"independent presence", `{"five_hour":{"utilization":0}}`, 0, 0, true, false},
		{"upper endpoint", `{"five_hour":{"utilization":100},"seven_day":{"utilization":100}}`, 100, 100, true, true},
		{"valid fraction", `{"five_hour":{"utilization":1.25},"seven_day":{"utilization":67.5}}`, 1.25, 67.5, true, true},
		{"invalid finite unchanged", `{"five_hour":{"utilization":-1},"seven_day":{"utilization":101}}`, -1, 101, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response Response
			if err := json.Unmarshal([]byte(tc.body), &response); err != nil {
				t.Fatal(err)
			}
			if err := Save(ctx, db, c.ID, &response); err != nil {
				t.Fatal(err)
			}
			var fh, sd float64
			var fhKnown, sdKnown bool
			if err := db.QueryRowContext(ctx, `SELECT five_hour_pct,seven_day_pct,five_hour_observed,seven_day_observed
				FROM usage_history WHERE credential_id=? ORDER BY id DESC LIMIT 1`, c.ID).Scan(&fh, &sd, &fhKnown, &sdKnown); err != nil {
				t.Fatal(err)
			}
			if fh != tc.fh || sd != tc.sd || fhKnown != tc.fhKnown || sdKnown != tc.sdKnown {
				t.Fatalf("stored (%v,%v,%v,%v), want (%v,%v,%v,%v)", fh, sd, fhKnown, sdKnown, tc.fh, tc.sd, tc.fhKnown, tc.sdKnown)
			}
		})
	}
	// Rows written without presence flags (including old database rows) must
	// remain unknown even though their historical percentage defaults are zero.
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_history (credential_id,captured_at) VALUES (?,?)`, c.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var fhKnown, sdKnown bool
	if err := db.QueryRowContext(ctx, `SELECT five_hour_observed,seven_day_observed FROM usage_history ORDER BY id DESC LIMIT 1`).Scan(&fhKnown, &sdKnown); err != nil {
		t.Fatal(err)
	}
	if fhKnown || sdKnown {
		t.Fatal("historical zero fabricated an observation")
	}
}

func TestSaveRejectsNonfiniteUsage(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "nonfinite.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := creds.Insert(ctx, db, "nonfinite", "max", "access", "refresh", time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, r := range []*Response{{FiveHour: Bucket{Utilization: &v}}, {SevenDay: Bucket{Utilization: &v}}} {
			if err := Save(ctx, db, c.ID, r); err == nil {
				t.Fatalf("accepted nonfinite utilization %v", v)
			}
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_history`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected snapshots changed history: n=%d err=%v", n, err)
	}
}
