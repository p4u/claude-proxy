package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
)

func TestAccountAffinitySurvivesRestartAndSaturation(t *testing.T) {
	p, db, cs, now := rebalanceFixture(t)
	l := acquireRebalance(t, p, RequestOptions{AccountBound: true})
	l.Release(false)
	execRebalance(t, db, `UPDATE usage_history SET five_hour_pct=100 WHERE credential_id=?`, cs[0].ID)
	p = New(db)
	p.now = func() time.Time { return now }
	l = acquireRebalance(t, p, RequestOptions{Rebalance: true})
	if l.Credential.ID != cs[0].ID || l.IsNew || l.Rebalance == "pending" {
		t.Fatalf("account-bound conversation migrated: %+v", l)
	}
}

func TestAccountAffinityUnusableNeverMigrates(t *testing.T) {
	for _, status := range []string{"disabled", "expired", "revoked"} {
		t.Run(status, func(t *testing.T) {
			p, db, cs, _ := rebalanceFixture(t)
			execRebalance(t, db, `UPDATE credentials SET status=? WHERE id=?`, status, cs[0].ID)
			_, err := p.AcquireScoped(context.Background(), "long", provider.Anthropic, "", nil, RequestOptions{AccountBound: true})
			if !errors.Is(err, ErrCredentialOrphaned) {
				t.Fatalf("want orphaned, got %v", err)
			}
			// Even a failed request must persist the affinity before a restart.
			p = New(db)
			_, _, err = p.Bind(context.Background(), "long", provider.Anthropic)
			if !errors.Is(err, ErrCredentialOrphaned) {
				t.Fatalf("restart lost affinity: %v", err)
			}
			var id string
			if err := db.QueryRow(`SELECT credential_id FROM conversations WHERE id='long'`).Scan(&id); err != nil || id != cs[0].ID {
				t.Fatalf("pin changed: %s %v", id, err)
			}
		})
	}
}
