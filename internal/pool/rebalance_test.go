package pool

import (
	"context"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

func rebalanceFixture(t *testing.T) (*Pool, *store.DB, []*creds.Credential, time.Time) {
	t.Helper()
	db, cs := setup(t)
	now := time.Now().Truncate(time.Second)
	p := New(db)
	p.now = func() time.Time { return now }
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, cs[2].ID)
	execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES ('long',?,?,?,?)`, cs[0].ID, now.Add(-2*time.Hour).Unix(), now.Unix(), now.Add(-2*time.Hour).Unix())
	for i, c := range cs[:2] {
		fh, sd := 80., 70.
		if i == 1 {
			fh, sd = 20, 20
		}
		for _, age := range []time.Duration{10 * time.Minute, 0} {
			execRebalance(t, db, `INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct,five_hour_resets_at,seven_day_resets_at) VALUES (?,?,?,?,?,?)`, c.ID, now.Add(-age).Unix(), fh, sd, now.Add(2*time.Hour).Unix(), now.Add(3*24*time.Hour).Unix())
		}
	}
	return p, db, cs, now
}

func execRebalance(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func acquireRebalance(t *testing.T, p *Pool, opts RequestOptions) *Lease {
	t.Helper()
	l, err := p.AcquireScoped(context.Background(), "long", provider.Anthropic, "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Release(false) })
	return l
}

func TestRebalanceAnnounceThenSwitch(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	opts := RequestOptions{Rebalance: true}
	first := acquireRebalance(t, p, opts)
	if first.Credential.ID != cs[0].ID || first.Rebalance != "pending" || first.IsNew {
		t.Fatalf("first lease: %+v", first)
	}
	first.Release(false)
	second := acquireRebalance(t, p, opts)
	if second.Credential.ID != cs[0].ID || second.Rebalance != "pending" {
		t.Fatalf("unannounced switch: %+v", second)
	}
	second.Release(true)
	third := acquireRebalance(t, p, opts)
	if third.Credential.ID != cs[1].ID || third.Rebalance != "switched" || !third.IsNew {
		t.Fatalf("no real switch: %+v", third)
	}
	third.Release(false)
	var bound, count int64
	if err := db.QueryRow(`SELECT bound_at,request_count FROM conversations WHERE id='long'`).Scan(&bound, &count); err != nil {
		t.Fatal(err)
	}
	if bound != now.Unix() || count != 3 {
		t.Fatalf("bound=%d count=%d", bound, count)
	}
	fourth := acquireRebalance(t, p, opts)
	if fourth.Credential.ID != cs[1].ID || fourth.Rebalance != "" {
		t.Fatalf("unstable new pin: %+v", fourth)
	}
}

func TestRebalanceSafetyGates(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"young pin", `UPDATE conversations SET bound_at=strftime('%s','now')`},
		{"stale latest", `UPDATE usage_history SET captured_at=captured_at-1800`},
		{"future observation", `UPDATE usage_history SET captured_at=captured_at+3600`},
		{"single observation", `DELETE FROM usage_history WHERE id IN (SELECT MIN(id) FROM usage_history GROUP BY credential_id)`},
		{"duplicate observations", `UPDATE usage_history SET captured_at=strftime('%s','now')`},
		{"elapsed reset", `UPDATE usage_history SET five_hour_resets_at=1`},
		{"changed weekly window", `UPDATE usage_history SET seven_day_resets_at=seven_day_resets_at+3600 WHERE id IN (SELECT MIN(id) FROM usage_history GROUP BY credential_id)`},
		{"negative usage", `UPDATE usage_history SET five_hour_pct=-1`},
		{"target limited", `UPDATE credentials SET status='limited' WHERE label='B'`},
		{"target cooling down", `UPDATE credentials SET retry_after=strftime('%s','now')+3600 WHERE label='B'`},
		{"target expiring", `UPDATE credentials SET expires_at=strftime('%s','now')+60 WHERE label='B'`},
		{"different endpoint", `UPDATE credentials SET base_url='https://other.example' WHERE label='B'`},
		{"different provider", `UPDATE credentials SET provider='glm' WHERE label='B'`},
		{"weight alone", `UPDATE usage_history SET five_hour_pct=20,seven_day_pct=20; UPDATE credentials SET weight=100 WHERE label='B'`},
		{"one sample improved", `UPDATE usage_history SET five_hour_pct=80,seven_day_pct=70 WHERE id=(SELECT MIN(id) FROM usage_history WHERE credential_id=(SELECT id FROM credentials WHERE label='B'))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, db, cs, _ := rebalanceFixture(t)
			execRebalance(t, db, tc.query)
			l := acquireRebalance(t, p, RequestOptions{Rebalance: true})
			if l.Credential.ID != cs[0].ID || l.Rebalance != "" {
				t.Fatalf("unsafe rebalance: %+v", l)
			}
		})
	}
}

func TestRebalanceRevalidatesAndReannouncesAfterRestart(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	opts := RequestOptions{Rebalance: true}
	l := acquireRebalance(t, p, opts)
	l.Release(true)
	p = New(db)
	l = acquireRebalance(t, p, opts)
	if l.Credential.ID != cs[0].ID || l.Rebalance != "pending" {
		t.Fatalf("restart bypassed notice: %+v", l)
	}
	l.Release(true)
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, cs[1].ID)
	l = acquireRebalance(t, p, opts)
	if l.Credential.ID != cs[0].ID || l.Rebalance != "" {
		t.Fatalf("stale destination used: %+v", l)
	}
}

func TestRebalanceCooldownSurvivesRestart(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	opts := RequestOptions{Rebalance: true}
	l := acquireRebalance(t, p, opts)
	l.Release(true)
	l = acquireRebalance(t, p, opts)
	if l.Rebalance != "switched" {
		t.Fatal("fixture did not switch")
	}
	l.Release(false)
	// Reverse the advantage, then recreate the pool. Durable bound_at, not
	// transient notice state or the original creation time, prevents ping-pong.
	execRebalance(t, db, `UPDATE usage_history SET five_hour_pct=20,seven_day_pct=20 WHERE credential_id=?`, cs[0].ID)
	execRebalance(t, db, `UPDATE usage_history SET five_hour_pct=80,seven_day_pct=70 WHERE credential_id=?`, cs[1].ID)
	p = New(db)
	l = acquireRebalance(t, p, opts)
	if l.Credential.ID != cs[1].ID || l.Rebalance != "" {
		t.Fatalf("restart lost cooldown: %+v", l)
	}
}

func TestRebalanceUsesExpiringWeeklyBudget(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	// Neither account has more physical room: only the target's expiring
	// weekly allowance makes moving useful. A pressure-only gate misses this.
	execRebalance(t, db, `UPDATE usage_history SET five_hour_pct=40,seven_day_pct=40`)
	execRebalance(t, db, `UPDATE usage_history SET seven_day_resets_at=? WHERE credential_id=?`, now.Add(90*time.Minute).Unix(), cs[1].ID)
	l := acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if l.Rebalance != "pending" {
		t.Fatalf("expiring budget ignored: %+v", l)
	}
	l.Release(true)
	l = acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if l.Rebalance != "switched" || l.Credential.ID != cs[1].ID {
		t.Fatalf("expiring budget not used: %+v", l)
	}
}

func TestRebalanceObserveOnlyAndEmergency(t *testing.T) {
	p, db, cs, _ := rebalanceFixture(t)
	count := RequestOptions{Rebalance: true, ObserveOnly: true}
	l := acquireRebalance(t, p, count)
	if l.Rebalance != "" {
		t.Fatal("count_tokens initiated a switch")
	}
	l.Release(false)
	l = acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if l.Rebalance != "pending" {
		t.Fatal("message did not announce")
	}
	l.Release(true)
	l = acquireRebalance(t, p, count)
	if l.Credential.ID != cs[0].ID || l.Rebalance != "" {
		t.Fatal("count_tokens executed a switch")
	}
	l.Release(false)
	execRebalance(t, db, `UPDATE conversations SET bound_at=strftime('%s','now')`)
	execRebalance(t, db, `UPDATE credentials SET status='revoked' WHERE id=?`, cs[0].ID)
	l = acquireRebalance(t, p, RequestOptions{})
	if l.Credential.ID != cs[1].ID || !l.IsNew || l.Rebalance != "" {
		t.Fatalf("emergency failed: %+v", l)
	}
}
