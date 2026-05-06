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
	db, _ := setup(t)
	p := New(db)
	ctx := context.Background()

	// First three new conversations must each get a distinct credential.
	got := map[string]bool{}
	var firstID string
	for i, conv := range []string{"conv1", "conv2", "conv3"} {
		c, isNew, err := p.Bind(ctx, conv)
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		if !isNew {
			t.Fatalf("expected new conversation for %s", conv)
		}
		got[c.ID] = true
		if i == 0 {
			firstID = c.ID
		}
	}
	if len(got) != 3 {
		t.Fatalf("RR did not spread across 3 credentials: %v", got)
	}
	// Fourth new conversation wraps around to the first credential.
	c4, _, _ := p.Bind(ctx, "conv4")
	if c4.ID != firstID {
		t.Fatalf("RR did not wrap: c4=%s expected=%s", c4.ID, firstID)
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

func TestExpandInterleaved(t *testing.T) {
	cases := []struct {
		name string
		in   []weightedEntry
		want []string
	}{
		{
			name: "equal weights interleave",
			in:   []weightedEntry{{"A", 5}, {"B", 5}},
			want: []string{"A", "B", "A", "B", "A", "B", "A", "B", "A", "B"},
		},
		{
			name: "unequal weights — heavier early then drained",
			in:   []weightedEntry{{"A", 5}, {"B", 1}},
			want: []string{"A", "B", "A", "A", "A", "A"},
		},
		{
			name: "three creds mixed",
			in:   []weightedEntry{{"A", 5}, {"B", 1}, {"C", 2}},
			want: []string{"A", "B", "C", "A", "C", "A", "A", "A"},
		},
	}
	for _, tc := range cases {
		got := expandInterleaved(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s: len got=%d want=%d (%v)", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: slot %d got=%s want=%s (full=%v)", tc.name, i, got[i], tc.want[i], got)
				break
			}
		}
	}
}

func TestInterleavedRoundRobinAcrossClients(t *testing.T) {
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
	got := []string{}
	for i := 0; i < 4; i++ {
		c, _, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c.ID)
	}
	// First four conversations across two equal-weight max creds must hit
	// both creds within the first two picks (no long runs of the same one).
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("first two new conversations did not interleave across creds: %v (a=%s b=%s)",
			got, a.ID, b.ID)
	}
}

func TestWeightedRoundRobin(t *testing.T) {
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
	const N = 600
	count := map[string]int{}
	for i := 0; i < N; i++ {
		c, _, err := p.Bind(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
		count[c.ID]++
	}

	// heavy has weight 5, light weight 1 → heavy should get 5/6 of the new convs.
	wantHeavy := N * 5 / 6
	wantLight := N / 6
	if count[heavy.ID] != wantHeavy || count[light.ID] != wantLight {
		t.Fatalf("weighted RR off: heavy=%d (want %d), light=%d (want %d)",
			count[heavy.ID], wantHeavy, count[light.ID], wantLight)
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
