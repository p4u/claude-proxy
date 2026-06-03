package pool

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

func setup(t *testing.T) (*store.DB, []*creds.Credential) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	var cs []*creds.Credential
	for _, lbl := range []string{"A", "B", "C"} {
		c, err := creds.Insert(ctx, db, lbl, "pro", "sk-ant-oat-fake-"+lbl, "rt-"+lbl, time.Now().Add(time.Hour), 1)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	return db, cs
}

func TestRoundRobinNewConversations(t *testing.T) {
	db, cs := setup(t)
	p := New(db)
	ctx := context.Background()

	// Over enough new conversations all three equal-weight credentials must be
	// selected. With weighted-random and no usage data the probability of any
	// one credential being skipped in 30 draws is (2/3)^30 < 0.0001.
	got := map[string]bool{}
	for i := 0; i < 30; i++ {
		c, isNew, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		if !isNew {
			t.Fatalf("bind %d: expected new conversation", i)
		}
		got[c.ID] = true
	}
	for _, c := range cs {
		if !got[c.ID] {
			t.Fatalf("credential %s never selected across 30 new conversations", c.ID)
		}
	}
}

func TestStickyBinding(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	ctx := context.Background()

	c1, isNew, err := p.Bind(ctx, "convX")
	if err != nil || !isNew {
		t.Fatalf("first bind: isNew=%v err=%v", isNew, err)
	}
	for i := 0; i < 5; i++ {
		c, isNew, _ := p.Bind(ctx, "convX")
		if isNew {
			t.Fatalf("repeat bind reported new")
		}
		if c.ID != c1.ID {
			t.Fatalf("sticky broken: was %s now %s", c1.ID, c.ID)
		}
	}
}

func TestSkipsLimitedOnNewConversation(t *testing.T) {
	db, cs := setup(t)
	p := New(db)
	ctx := context.Background()

	// Limit one specific credential.
	limitedID := cs[1].ID
	if err := creds.MarkLimited(ctx, db, limitedID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, conv := range []string{"n1", "n2", "n3", "n4"} {
		c, _, err := p.Bind(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		if c.ID == limitedID {
			t.Fatalf("limited credential was selected for %s", conv)
		}
	}
}

func TestExistingConvKeptOnLimited(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	ctx := context.Background()

	c1, _, _ := p.Bind(ctx, "convY")
	if err := creds.MarkLimited(ctx, db, c1.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c2, isNew, err := p.Bind(ctx, "convY")
	if err != nil {
		t.Fatalf("expected sticky-passthrough, got %v", err)
	}
	if isNew {
		t.Fatalf("repeat bind reported new")
	}
	if c2.ID != c1.ID {
		t.Fatalf("strict sticky broken under limited: %s vs %s", c1.ID, c2.ID)
	}
}

func TestAllLimitedFallback(t *testing.T) {
	db, cs := setup(t)
	p := New(db)
	ctx := context.Background()

	// Mark all credentials limited.
	for _, c := range cs {
		if err := creds.MarkLimited(ctx, db, c.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// A new conversation should still bind (fall back to a limited credential)
	// rather than return ErrNoCredentials.
	c, isNew, err := p.Bind(ctx, "new-conv-all-limited")
	if err != nil {
		t.Fatalf("expected fallback to limited credential, got err: %v", err)
	}
	if !isNew {
		t.Fatal("expected new conversation")
	}
	if c.Status != creds.StatusLimited {
		t.Fatalf("expected limited credential, got status %s", c.Status)
	}
}

func TestSpreadAcrossTwoCreds(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	a, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-A", "rt-A", time.Now().Add(time.Hour), 5)
	b, _ := creds.Insert(ctx, db, "B", "max", "sk-ant-oat-B", "rt-B", time.Now().Add(time.Hour), 5)

	p := New(db)
	// Over 20 conversations both equal-weight creds must be hit.
	// P(one cred never picked) = (1/2)^20 < 0.000001.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		c, _, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		seen[c.ID] = true
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("one credential never selected across 20 picks (a=%s b=%s)", a.ID, b.ID)
	}
}

func TestWeightedSelection(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	heavy, _ := creds.Insert(ctx, db, "heavy", "max", "sk-ant-oat-h", "rt-h", time.Now().Add(time.Hour), 5)
	light, _ := creds.Insert(ctx, db, "light", "pro", "sk-ant-oat-l", "rt-l", time.Now().Add(time.Hour), 1)

	p := New(db)
	const N = 1200
	count := map[string]int{}
	for i := 0; i < N; i++ {
		c, _, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		count[c.ID]++
	}

	// heavy has weight 5, light weight 1 → expected ratio 5:1 (heavy≈83%, light≈17%).
	// Allow ±5 percentage points to keep the test robust against randomness.
	heavyPct := float64(count[heavy.ID]) / N * 100
	lightPct := float64(count[light.ID]) / N * 100
	if heavyPct < 78 || heavyPct > 88 {
		t.Fatalf("heavy selection rate %.1f%% outside [78,88]%% (light=%.1f%%)", heavyPct, lightPct)
	}
}

func TestUsageAwareScoring(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	fresh, _ := creds.Insert(ctx, db, "fresh", "pro", "sk-ant-oat-f", "rt-f", time.Now().Add(time.Hour), 1)
	busy, _ := creds.Insert(ctx, db, "busy", "pro", "sk-ant-oat-b", "rt-b", time.Now().Add(time.Hour), 1)

	// Inject a fresh usage snapshot: "busy" is at 90% on both windows.
	now := time.Now().Unix()
	_, _ = db.ExecContext(ctx, `
		INSERT INTO usage_history
		  (credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		   seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 90.0, NULL, 90.0, NULL, 0.0, NULL)`, busy.ID, now)
	_, _ = db.ExecContext(ctx, `
		INSERT INTO usage_history
		  (credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		   seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 5.0, NULL, 5.0, NULL, 0.0, NULL)`, fresh.ID, now)

	// "fresh" score = 1 × 0.95 × 0.95^1.5 ≈ 0.880
	// "busy"  score = 1 × 0.10 × 0.10^1.5 ≈ 0.00316
	// Expected selection ratio ≈ 278:1 in favour of "fresh".

	p := New(db)
	const N = 200
	count := map[string]int{}
	for i := range N {
		c, _, err := p.Bind(ctx, fmt.Sprintf("u-%d", i))
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		count[c.ID]++
	}

	// fresh should dominate — at least 80% of picks despite equal configured weight.
	freshPct := float64(count[fresh.ID]) / N * 100
	if freshPct < 80 {
		t.Fatalf("usage-aware scoring failed: fresh=%.1f%% (want ≥80%%) busy=%.1f%%",
			freshPct, float64(count[busy.ID])/N*100)
	}
}

// TestSevenDayExhaustedAvoided guards the bottleneck fix: a credential whose
// weekly quota is spent (7d=100%) must not be picked just because its 5h
// window looks free. The old additive blend scored it 0.6×room_5h and kept
// routing to it; the multiplicative model drives its headroom to zero.
func TestSevenDayExhaustedAvoided(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	exhausted, _ := creds.Insert(ctx, db, "exhausted", "max", "sk-ant-oat-e", "rt-e", time.Now().Add(time.Hour), 5)
	healthy, _ := creds.Insert(ctx, db, "healthy", "pro", "sk-ant-oat-h", "rt-h", time.Now().Add(time.Hour), 1)

	now := time.Now().Unix()
	// "exhausted": 5h wide open but 7-day quota fully spent. Higher weight too,
	// so the old additive model would have strongly preferred it.
	_, _ = db.ExecContext(ctx, `
		INSERT INTO usage_history
		  (credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		   seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 0.0, NULL, 100.0, NULL, 0.0, NULL)`, exhausted.ID, now)
	// "healthy": balanced, modest usage, lower weight.
	_, _ = db.ExecContext(ctx, `
		INSERT INTO usage_history
		  (credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		   seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 40.0, NULL, 40.0, NULL, 0.0, NULL)`, healthy.ID, now)

	p := New(db)
	const N = 200
	count := map[string]int{}
	for i := range N {
		c, _, err := p.Bind(ctx, fmt.Sprintf("e-%d", i))
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		count[c.ID]++
	}

	// The 7d-exhausted cred has headroom 0 → score 0 → must never win, despite
	// its open 5h window and higher weight.
	if count[exhausted.ID] > N/20 {
		t.Fatalf("7d-exhausted credential picked %d/%d times (want ~0); healthy=%d",
			count[exhausted.ID], N, count[healthy.ID])
	}
}

// TestSaturatedCredsHardExcluded verifies the ≥100% cutoff: a credential maxed
// on EITHER window is never selected for a new conversation, even with the
// highest weight, as long as a non-saturated credential exists.
func TestSaturatedCredsHardExcluded(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	full5h, _ := creds.Insert(ctx, db, "full5h", "max", "sk-ant-oat-5", "rt-5", time.Now().Add(time.Hour), 5)
	full7d, _ := creds.Insert(ctx, db, "full7d", "max", "sk-ant-oat-7", "rt-7", time.Now().Add(time.Hour), 5)
	ok, _ := creds.Insert(ctx, db, "ok", "pro", "sk-ant-oat-o", "rt-o", time.Now().Add(time.Hour), 1)

	now := time.Now().Unix()
	ins := func(id string, fh, sd float64) {
		_, _ = db.ExecContext(ctx, `INSERT INTO usage_history
			(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
			 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
			VALUES (?, ?, ?, NULL, ?, NULL, 0.0, NULL)`, id, now, fh, sd)
	}
	ins(full5h.ID, 100.0, 5.0) // 5h window maxed
	ins(full7d.ID, 5.0, 100.0) // 7d window maxed
	ins(ok.ID, 30.0, 30.0)     // healthy, lowest weight

	p := New(db)
	const N = 100
	count := map[string]int{}
	for i := range N {
		c, _, err := p.Bind(ctx, fmt.Sprintf("s-%d", i))
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		count[c.ID]++
	}
	if count[full5h.ID] != 0 || count[full7d.ID] != 0 {
		t.Fatalf("saturated creds were selected: 5h-maxed=%d 7d-maxed=%d (want 0/0); ok=%d",
			count[full5h.ID], count[full7d.ID], count[ok.ID])
	}
	if count[ok.ID] != N {
		t.Fatalf("healthy cred should take all %d binds, got %d", N, count[ok.ID])
	}
}

// TestAllSaturatedNoActive verifies that when every active credential is maxed
// out (and none are status='limited'), binding reports ErrNoCredentials rather
// than routing to a saturated subscription.
func TestAllSaturatedNoActive(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	a, _ := creds.Insert(ctx, db, "a", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	b, _ := creds.Insert(ctx, db, "b", "max", "sk-ant-oat-b", "rt-b", time.Now().Add(time.Hour), 5)

	now := time.Now().Unix()
	for _, id := range []string{a.ID, b.ID} {
		_, _ = db.ExecContext(ctx, `INSERT INTO usage_history
			(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
			 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
			VALUES (?, ?, 100.0, NULL, 100.0, NULL, 0.0, NULL)`, id, now)
	}

	p := New(db)
	if _, _, err := p.Bind(ctx, "x"); err != ErrNoCredentials {
		t.Fatalf("expected ErrNoCredentials when all active creds are saturated, got %v", err)
	}
}

func TestNoCredentials(t *testing.T) {
	dir := t.TempDir()
	db, _ := store.Open(filepath.Join(dir, "t.db"))
	defer db.Close()
	p := New(db)
	_, _, err := p.Bind(context.Background(), "x")
	if err != ErrNoCredentials {
		t.Fatalf("expected ErrNoCredentials, got %v", err)
	}
}

func TestOrphanedConversationRebinds(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	ctx := context.Background()

	c1, _, _ := p.Bind(ctx, "convZ")
	if err := creds.SetStatus(ctx, db, c1.ID, creds.StatusRevoked); err != nil {
		t.Fatal(err)
	}
	c2, isNew, err := p.Bind(ctx, "convZ")
	if err != nil {
		t.Fatalf("expected auto-rebind to a healthy cred, got %v", err)
	}
	if !isNew {
		t.Fatalf("rebind should report isNew=true so callers log it")
	}
	if c2.ID == c1.ID {
		t.Fatalf("rebind picked the same dead cred: %s", c2.ID)
	}
	// Confirm the row was actually moved.
	var stored string
	_ = db.QueryRow(`SELECT credential_id FROM conversations WHERE id='convZ'`).Scan(&stored)
	if stored != c2.ID {
		t.Fatalf("conversations row not updated: have %s want %s", stored, c2.ID)
	}
}

func TestOrphanedConversationFailsIfNoAlternative(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	only, _ := creds.Insert(ctx, db, "only", "pro", "sk-ant-oat-x", "rt-x", time.Now().Add(time.Hour), 1)
	p := New(db)

	if _, _, err := p.Bind(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := creds.SetStatus(ctx, db, only.ID, creds.StatusRevoked); err != nil {
		t.Fatal(err)
	}
	_, _, err = p.Bind(ctx, "c1")
	if err != ErrCredentialOrphaned {
		t.Fatalf("expected ErrCredentialOrphaned when no alternative exists, got %v", err)
	}
}
