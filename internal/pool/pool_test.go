package pool

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
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
		c, isNew, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i), provider.Anthropic)
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

	c1, isNew, err := p.Bind(ctx, "convX", provider.Anthropic)
	if err != nil || !isNew {
		t.Fatalf("first bind: isNew=%v err=%v", isNew, err)
	}
	for i := 0; i < 5; i++ {
		c, isNew, _ := p.Bind(ctx, "convX", provider.Anthropic)
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
		c, _, err := p.Bind(ctx, conv, provider.Anthropic)
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

	c1, _, _ := p.Bind(ctx, "convY", provider.Anthropic)
	if err := creds.MarkLimited(ctx, db, c1.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c2, isNew, err := p.Bind(ctx, "convY", provider.Anthropic)
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
	c, isNew, err := p.Bind(ctx, "new-conv-all-limited", provider.Anthropic)
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
		c, _, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i), provider.Anthropic)
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
		c, _, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i), provider.Anthropic)
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
		c, _, err := p.Bind(ctx, fmt.Sprintf("u-%d", i), provider.Anthropic)
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
		c, _, err := p.Bind(ctx, fmt.Sprintf("e-%d", i), provider.Anthropic)
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
		c, _, err := p.Bind(ctx, fmt.Sprintf("s-%d", i), provider.Anthropic)
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
	if _, _, err := p.Bind(ctx, "x", provider.Anthropic); err != ErrNoCredentials {
		t.Fatalf("expected ErrNoCredentials when all active creds are saturated, got %v", err)
	}
}

// TestStickyRebindsOffSaturated verifies that an existing conversation pinned to
// a credential whose latest snapshot is ≥100% on either window is migrated onto
// a fresh, non-saturated credential on the next Bind (extending the ≥100% cutoff
// from new bindings to sticky ones).
func TestStickyRebindsOffSaturated(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	ctx := context.Background()

	c1, isNew, err := p.Bind(ctx, "convSat", provider.Anthropic)
	if err != nil || !isNew {
		t.Fatalf("first bind: isNew=%v err=%v", isNew, err)
	}

	// Saturate the bound credential's latest snapshot (7d maxed); the other two
	// setup() creds stay snapshot-free (healthy).
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_history
		(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 10.0, NULL, 100.0, NULL, 0.0, NULL)`, c1.ID, now); err != nil {
		t.Fatal(err)
	}

	c2, isNew, err := p.Bind(ctx, "convSat", provider.Anthropic)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if !isNew {
		t.Fatal("rebind off a saturated cred should report isNew=true")
	}
	if c2.ID == c1.ID {
		t.Fatalf("conversation stayed on saturated cred %s", c1.ID)
	}

	// Confirm the conversation row actually moved.
	var stored string
	_ = db.QueryRow(`SELECT credential_id FROM conversations WHERE id='convSat'`).Scan(&stored)
	if stored != c2.ID {
		t.Fatalf("conversations row not updated: have %s want %s", stored, c2.ID)
	}
}

// TestStickySaturatedKeptWhenNoAlternative verifies that a saturated sticky
// binding is retained (not failed) when there is no healthy alternative, so the
// request still reaches Anthropic for a real 429.
func TestStickySaturatedKeptWhenNoAlternative(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	only, _ := creds.Insert(ctx, db, "only", "pro", "sk-ant-oat-x", "rt-x", time.Now().Add(time.Hour), 1)
	p := New(db)

	if _, _, err := p.Bind(ctx, "c1", provider.Anthropic); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_history
		(credential_id, captured_at, five_hour_pct, five_hour_resets_at,
		 seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
		VALUES (?, ?, 100.0, NULL, 100.0, NULL, 0.0, NULL)`, only.ID, now); err != nil {
		t.Fatal(err)
	}
	c, isNew, err := p.Bind(ctx, "c1", provider.Anthropic)
	if err != nil {
		t.Fatalf("expected saturated sticky to be kept, got %v", err)
	}
	if isNew {
		t.Fatal("keeping a saturated pin must not report isNew")
	}
	if c.ID != only.ID {
		t.Fatalf("expected to keep %s, got %s", only.ID, c.ID)
	}
}

func TestScoreSharedFormula(t *testing.T) {
	// Score must equal weight × room_5h × room_7d^SevenDayExp.
	got := Score(2, 20, 40)
	want := 2.0 * Room(20) * math.Pow(Room(40), SevenDayExp)
	if got != want {
		t.Fatalf("Score=%v want %v", got, want)
	}
	if !Saturated(100, 0) || !Saturated(0, 100) || Saturated(99, 99) {
		t.Fatal("Saturated cutoff wrong")
	}
}

func TestBurnBoostRules(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fhWindow := int64(FiveHourWindow / time.Second)
	sdWindow := int64(SevenDayWindow / time.Second)

	cases := []struct {
		name               string
		fhPct, sdPct       float64
		fhResets, sdResets int64 // seconds from now, negative = past, 0 = unknown
		wantMin, wantMax   float64
	}{
		{"no resets known", 50, 50, 0, 0, 0, 0},
		{"resets in past", 10, 10, -10, -10, 0, 0},
		{"start of 5h window, 0% used", 0, 0, fhWindow - 1, sdWindow - 1, 0, 0.01},
		{"halfway 5h at 50%", 50, 0, fhWindow / 2, sdWindow - 1, 0, 0.01},
		{"halfway 5h at 10% → 0.4 boost", 10, 0, fhWindow / 2, sdWindow - 1, 0.39, 0.41},
		{"about to reset 5h at 0% → boost near 1", 0, 0, 60, sdWindow - 1, 0.99, 1.0},
		{"about to reset 7d at 20% → 0.8 boost", 0, 20, fhWindow - 1, 60, 0.79, 0.81},
		{"take max across windows", 20, 90, fhWindow / 2, 60, 0.29, 0.31},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fh := int64(0)
			if c.fhResets != 0 {
				fh = now.Unix() + c.fhResets
			}
			sd := int64(0)
			if c.sdResets != 0 {
				sd = now.Unix() + c.sdResets
			}
			got := BurnBoost(c.fhPct, c.sdPct, fh, sd, now)
			if got < c.wantMin || got > c.wantMax {
				t.Fatalf("BurnBoost=%.4f, want in [%.4f,%.4f]", got, c.wantMin, c.wantMax)
			}
		})
	}
}

func TestNoCredentials(t *testing.T) {
	dir := t.TempDir()
	db, _ := store.Open(filepath.Join(dir, "t.db"))
	defer db.Close()
	p := New(db)
	_, _, err := p.Bind(context.Background(), "x", provider.Anthropic)
	if err != ErrNoCredentials {
		t.Fatalf("expected ErrNoCredentials, got %v", err)
	}
}

func TestOrphanedConversationRebinds(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	ctx := context.Background()

	c1, _, _ := p.Bind(ctx, "convZ", provider.Anthropic)
	if err := creds.SetStatus(ctx, db, c1.ID, creds.StatusRevoked); err != nil {
		t.Fatal(err)
	}
	c2, isNew, err := p.Bind(ctx, "convZ", provider.Anthropic)
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

	if _, _, err := p.Bind(ctx, "c1", provider.Anthropic); err != nil {
		t.Fatal(err)
	}
	if err := creds.SetStatus(ctx, db, only.ID, creds.StatusRevoked); err != nil {
		t.Fatal(err)
	}
	_, _, err = p.Bind(ctx, "c1", provider.Anthropic)
	if err != ErrCredentialOrphaned {
		t.Fatalf("expected ErrCredentialOrphaned when no alternative exists, got %v", err)
	}
}

// Provider is a hard filter: a GLM model can only be served by a GLM key, so
// selection must never cross providers even when the other side is idle and
// heavily weighted.
func TestProviderFilteringIsHard(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// Anthropic is deliberately the more attractive candidate on every axis.
	anth, _ := creds.Insert(ctx, db, "anth", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 50)
	glm, _ := creds.InsertKey(ctx, db, provider.GLM, "zai", "pro", "zai-key", "", 1)

	p := New(db)
	for i := range 20 {
		c, _, err := p.Bind(ctx, fmt.Sprintf("g-%d", i), provider.GLM)
		if err != nil {
			t.Fatalf("bind glm %d: %v", i, err)
		}
		if c.ID != glm.ID {
			t.Fatalf("GLM request bound to %s (want %s) — provider filter leaked", c.ID, glm.ID)
		}
	}
	for i := range 20 {
		c, _, err := p.Bind(ctx, fmt.Sprintf("a-%d", i), provider.Anthropic)
		if err != nil {
			t.Fatalf("bind anthropic %d: %v", i, err)
		}
		if c.ID != anth.ID {
			t.Fatalf("Anthropic request bound to %s (want %s)", c.ID, anth.ID)
		}
	}
}

// With no credential for the requested provider the pool must fail rather than
// fall back to another provider's credential, which could not serve the model.
func TestBindFailsWhenProviderHasNoCredential(t *testing.T) {
	db, _ := setup(t) // Anthropic credentials only
	p := New(db)
	if _, _, err := p.Bind(context.Background(), "x", provider.GLM); err != ErrNoCredentials {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

// A limited GLM key is still preferable to failing: the request reaches Z.AI
// and earns a real 429 + Retry-After. But the fallback must stay in-provider.
func TestLimitedFallbackStaysWithinProvider(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	_, _ = creds.Insert(ctx, db, "anth", "max", "sk-ant-oat-a", "rt-a", time.Now().Add(time.Hour), 5)
	glm, _ := creds.InsertKey(ctx, db, provider.GLM, "zai", "pro", "zai-key", "", 1)
	if err := creds.MarkLimited(ctx, db, glm.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	c, _, err := New(db).Bind(ctx, "conv", provider.GLM)
	if err != nil {
		t.Fatalf("expected fallback to the limited GLM key, got %v", err)
	}
	if c.ID != glm.ID {
		t.Fatalf("fell back to %s, crossing providers; want the limited GLM key %s", c.ID, glm.ID)
	}
}

func TestKeyIsProviderQualified(t *testing.T) {
	// Anthropic keeps the bare ID so pre-existing rows and bindings still match.
	if got := Key("abc", provider.Anthropic); got != "abc" {
		t.Errorf("Key(anthropic) = %q, want %q", got, "abc")
	}
	if got := Key("abc", ""); got != "abc" {
		t.Errorf("Key(empty) = %q, want %q", got, "abc")
	}
	if got := Key("abc", provider.GLM); got != "glm:abc" {
		t.Errorf("Key(glm) = %q, want %q", got, "glm:abc")
	}
}
