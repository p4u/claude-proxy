package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
)

// AC6: after a transient refresh failure the credential keeps its status but
// sits out new conversations for a minute (retry_after).
func TestTransientRefreshFailureLeavesActiveCandidates(t *testing.T) {
	db, cs := setup(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	prev := creds.TokenURL
	creds.SetTokenURL(srv.URL)
	t.Cleanup(func() { creds.SetTokenURL(prev) })
	if _, err := db.Exec(`UPDATE credentials SET expires_at=? WHERE id=?`, time.Now().Add(time.Minute).Unix(), cs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := creds.NewRefresher(db).Refresh(ctx, cs[0].ID); err == nil {
		t.Fatal("want a transient refresh error")
	}
	entries, err := activeCandidates(ctx, db, provider.Anthropic, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.id == cs[0].ID {
			t.Fatal("a credential whose refresh just failed is still a candidate for new conversations")
		}
	}
	if len(entries) != len(cs)-1 {
		t.Fatalf("candidates = %d, want %d", len(entries), len(cs)-1)
	}
	if got, _ := creds.Get(ctx, db, cs[0].ID); got.Status != creds.StatusActive {
		t.Fatalf("status = %q, want active", got.Status)
	}
}
