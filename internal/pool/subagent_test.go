package pool

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// childOpts is what the proxy sends for a subagent candidate of family.
func childOpts(family, agent string, start bool) RequestOptions {
	return RequestOptions{Family: family, Child: family + "/a_" + agent, StartChild: start}
}

// acquireAs runs one request lease for family with opts and releases it.
func acquireAs(t *testing.T, p *Pool, family string, opts RequestOptions) *Lease {
	t.Helper()
	l, err := p.AcquireScoped(context.Background(), family, provider.Anthropic, "", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	l.Release(false)
	return l
}

// bindRow writes a binding directly, last seen at seen.
func bindRow(t *testing.T, db *store.DB, key, credID string, seen time.Time) {
	t.Helper()
	execRebalance(t, db, `INSERT INTO conversations (id,credential_id,created_at,last_seen_at,bound_at) VALUES (?,?,?,?,?)`,
		key, credID, seen.Unix(), seen.Unix(), seen.Unix())
}

func boundTo(t *testing.T, db *store.DB, key string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT credential_id FROM conversations WHERE id=?`, key).Scan(&id); err != nil {
		t.Fatalf("binding %q: %v", key, err)
	}
	return id
}

func rowCount(t *testing.T, db *store.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id=?`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The key rule (R9): every valid subagent candidate uses its child key, with
// or without StartChild; every malformed option set keeps the key passed in.
func TestSubagentKeyResolution(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	const fam = "u_fam"

	main := acquireAs(t, p, fam, RequestOptions{})
	if main.ConvID != fam {
		t.Fatalf("plain request: ConvID %q", main.ConvID)
	}
	if l := acquireAs(t, p, fam, childOpts(fam, "late", false)); l.ConvID != fam+"/a_late" || rowCount(t, db, fam+"/a_late") != 1 ||
		l.Credential.ID != main.Credential.ID {
		t.Fatalf("no child row, StartChild false: ConvID %q on %s, session on %s", l.ConvID, l.Credential.ID, main.Credential.ID)
	}
	first := acquireAs(t, p, fam, childOpts(fam, "fresh", true))
	if first.ConvID != fam+"/a_fresh" || !first.IsNew {
		t.Fatalf("StartChild: %+v", first)
	}
	next := acquireAs(t, p, fam, childOpts(fam, "fresh", false))
	if next.ConvID != fam+"/a_fresh" || next.IsNew || next.Credential.ID != first.Credential.ID {
		t.Fatalf("existing child row: %+v, first on %s", next, first.Credential.ID)
	}
	for _, bad := range []RequestOptions{
		{Family: "u_other", Child: "u_other/a_x", StartChild: true}, // not this request's family
		{Family: fam, Child: "u_other/a_x", StartChild: true},       // child outside the family
		{Child: fam + "/a_x", StartChild: true},                     // no family
	} {
		if l := acquireAs(t, p, fam, bad); l.ConvID != fam {
			t.Fatalf("%+v: ConvID %q, want %q", bad, l.ConvID, fam)
		}
	}
	// A scoped (custom host) binding is never spread.
	l, err := p.AcquireScoped(context.Background(), fam, provider.Anthropic, "house-model", nil, childOpts(fam, "s", true))
	if err != nil {
		t.Fatal(err)
	}
	l.Release(false)
	if l.ConvID != fam {
		t.Fatalf("scoped: ConvID %q", l.ConvID)
	}
	if n := rowCount(t, db, fam+"/a_x") + rowCount(t, db, "u_other/a_x") + rowCount(t, db, fam+"/a_s"); n != 0 {
		t.Fatalf("%d child rows created by malformed options", n)
	}
}

// R9: a child that may not start its own binding (a fork, its side requests,
// the first request after a compaction) is created on the session's
// credential while that one is active or limited, else by the ordinary pick.
func TestChildWithoutStartFollowsSession(t *testing.T) {
	t.Run("follows the session", func(t *testing.T) {
		db, _ := setup(t)
		p := New(db)
		// Ten families, so a random pick cannot pass by chance.
		for i := range 10 {
			fam := fmt.Sprintf("u_follow%d", i)
			main := acquireAs(t, p, fam, RequestOptions{}).Credential.ID
			l := acquireAs(t, p, fam, childOpts(fam, "fork", false))
			if l.ConvID != fam+"/a_fork" || !l.IsNew || l.Credential.ID != main || boundTo(t, db, fam+"/a_fork") != main {
				t.Fatalf("%s: child %q new=%v on %s, session on %s", fam, l.ConvID, l.IsNew, l.Credential.ID, main)
			}
		}
	})
	t.Run("session credential disabled", func(t *testing.T) {
		db, cs := setup(t)
		c := cs[2].ID
		p := New(db)
		bindRow(t, db, "u_dis", c, time.Now())
		execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, c)
		l := acquireAs(t, p, "u_dis", childOpts("u_dis", "fork", false))
		if l.ConvID != "u_dis/a_fork" || l.Credential.ID == c || l.Credential.Status != creds.StatusActive ||
			boundTo(t, db, "u_dis/a_fork") != l.Credential.ID {
			t.Fatalf("child %q on %s (%s), session on disabled %s", l.ConvID, l.Credential.ID, l.Credential.Status, c)
		}
	})
	t.Run("no session row", func(t *testing.T) {
		db, _ := setup(t)
		p := New(db)
		l := acquireAs(t, p, "u_none", childOpts("u_none", "fork", false))
		if l.ConvID != "u_none/a_fork" || !l.IsNew || l.Credential.Status != creds.StatusActive ||
			boundTo(t, db, "u_none/a_fork") != l.Credential.ID || rowCount(t, db, "u_none") != 0 {
			t.Fatalf("child %q new=%v on %s (%s)", l.ConvID, l.IsNew, l.Credential.ID, l.Credential.Status)
		}
	})
	t.Run("session credential limited", func(t *testing.T) {
		db, cs := setup(t)
		b := cs[1].ID
		p := New(db)
		now := time.Now()
		bindRow(t, db, "u_lim", b, now)
		if err := creds.MarkLimited(context.Background(), db, b, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		first := acquireAs(t, p, "u_lim", childOpts("u_lim", "fork", false))
		if first.ConvID != "u_lim/a_fork" || !first.IsNew || first.Credential.ID != b || boundTo(t, db, "u_lim/a_fork") != b {
			t.Fatalf("child %q new=%v on %s, session on limited %s", first.ConvID, first.IsNew, first.Credential.ID, b)
		}
		next := acquireAs(t, p, "u_lim", childOpts("u_lim", "fork", false))
		if next.ConvID != "u_lim/a_fork" || !next.IsNew || next.Credential.ID == b || next.Credential.Status != creds.StatusActive ||
			boundTo(t, db, "u_lim/a_fork") != next.Credential.ID {
			t.Fatalf("next request: child %q new=%v on %s (%s)", next.ConvID, next.IsNew, next.Credential.ID, next.Credential.Status)
		}
	})
}

// AC17: after a restart (a new Pool on the same database) a new child of the
// family avoids the main thread's and a live sibling's credentials. Ten
// families, so a missed exclusion cannot pass by chance.
func TestChildAvoidsFamilyAfterRestart(t *testing.T) {
	db, _ := setup(t)
	p1 := New(db)
	type family struct{ key, main, sibling string }
	var fams []family
	for i := range 10 {
		f := family{key: fmt.Sprintf("u_fam%d", i)}
		f.main = acquireAs(t, p1, f.key, RequestOptions{}).Credential.ID
		f.sibling = acquireAs(t, p1, f.key, childOpts(f.key, "s1", true)).Credential.ID
		if f.sibling == f.main {
			t.Fatalf("%s: first child shares the main thread's credential %s", f.key, f.main)
		}
		fams = append(fams, f)
	}
	p2 := New(db)
	for _, f := range fams {
		l := acquireAs(t, p2, f.key, childOpts(f.key, "s2", true))
		if l.ConvID != f.key+"/a_s2" || l.Credential.ID == f.main || l.Credential.ID == f.sibling {
			t.Fatalf("%s: second child on %s (main %s, sibling %s)", f.key, l.Credential.ID, f.main, f.sibling)
		}
		if got := boundTo(t, db, f.key+"/a_s2"); got != l.Credential.ID {
			t.Fatalf("%s: stored %s, leased %s", f.key, got, l.Credential.ID)
		}
	}
}

// The family is the session's own binding at any age plus its children seen
// in the last 15 minutes; keys outside the range never match.
func TestFamilyCredentialsRange(t *testing.T) {
	db, cs := setup(t)
	a, b, c := cs[0].ID, cs[1].ID, cs[2].ID
	now := time.Now()
	bindRow(t, db, "u_fam", a, now.Add(-24*time.Hour))
	bindRow(t, db, "u_fam/a_live", b, now.Add(-14*time.Minute))
	bindRow(t, db, "u_fam/a_stale", c, now.Add(-16*time.Minute))
	for _, outside := range []string{"u_fam0", "u_fam.x", "u_famz/a_1", "u_fa/a_1", "glm:u_fam/a_1"} {
		bindRow(t, db, outside, c, now)
	}
	got, err := familyCredentials(context.Background(), db, "u_fam", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Fatalf("family credentials = %v, want [%s %s]", got, a, b)
	}
}

// AC7: a sibling last seen more than 15 minutes ago is not excluded, and an
// exclusion that leaves no candidate with headroom ≥ 0.25 falls back to the
// ordinary pick.
func TestFamilyExclusionLivenessAndFallback(t *testing.T) {
	db, cs := setup(t)
	a, b, c := cs[0].ID, cs[1].ID, cs[2].ID
	execRebalance(t, db, `UPDATE credentials SET status='disabled' WHERE id=?`, c)
	p := New(db)
	now := time.Now()

	// Main thread on A, last seen an hour ago, and a stale sibling on B: only A
	// is excluded, so B wins every time. Were A kept or B excluded, A would win
	// about half the time.
	for i := range 20 {
		fam := fmt.Sprintf("u_stale%d", i)
		bindRow(t, db, fam, a, now.Add(-time.Hour))
		bindRow(t, db, fam+"/a_old", b, now.Add(-20*time.Minute))
		if l := acquireAs(t, p, fam, childOpts(fam, "new", true)); l.Credential.ID != b {
			t.Fatalf("family %d: child on %s, want the stale sibling's %s", i, l.Credential.ID, b)
		}
	}

	// Live sibling on B: the exclusion empties the set, so the ordinary pick
	// still binds the child.
	bindRow(t, db, "u_live", a, now)
	bindRow(t, db, "u_live/a_s", b, now.Add(-time.Minute))
	if l := acquireAs(t, p, "u_live", childOpts("u_live", "new", true)); l.Credential.ID != a && l.Credential.ID != b {
		t.Fatalf("fallback child on %s", l.Credential.ID)
	}

	// B below the headroom floor (90%/90%): dropped, so the fallback runs and,
	// weighted by score, nearly always picks A.
	execRebalance(t, db, `INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct) VALUES (?,?,90,90)`, b, now.Unix())
	pickedA := false
	for i := range 5 {
		fam := fmt.Sprintf("u_low%d", i)
		bindRow(t, db, fam, a, now)
		if acquireAs(t, p, fam, childOpts(fam, "new", true)).Credential.ID == a {
			pickedA = true
		}
	}
	if !pickedA {
		t.Fatal("a candidate below the headroom floor kept the exclusion")
	}

	// B with room (40%/10%, headroom ≈ 0.51): the exclusion holds.
	execRebalance(t, db, `INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct) VALUES (?,?,40,10)`, b, now.Unix()+1)
	for i := range 7 {
		fam := fmt.Sprintf("u_room%d", i)
		bindRow(t, db, fam, a, now)
		if l := acquireAs(t, p, fam, childOpts(fam, "new", true)); l.Credential.ID != b {
			t.Fatalf("child on %s, want %s (headroom above the floor)", l.Credential.ID, b)
		}
	}
}

// AC13: a child binding on a limited credential moves to an active one on its
// next request; a main binding, and an account-bound child, keep the pin; with
// no active candidate the child stays for the real 429.
func TestLimitedChildMoves(t *testing.T) {
	db, cs := setup(t)
	a, b, c := cs[0].ID, cs[1].ID, cs[2].ID
	p := New(db)
	now := time.Now()
	bindRow(t, db, "u_m", b, now)
	bindRow(t, db, "u_m/a_1", b, now)
	bindRow(t, db, "u_m/a_bound", b, now)
	execRebalance(t, db, `UPDATE conversations SET account_bound=1 WHERE id='u_m/a_bound'`)
	if err := creds.MarkLimited(context.Background(), db, b, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	moved := acquireAs(t, p, "u_m", childOpts("u_m", "1", false))
	if moved.ConvID != "u_m/a_1" || !moved.IsNew || (moved.Credential.ID != a && moved.Credential.ID != c) {
		t.Fatalf("limited child: %+v", moved)
	}
	if moved.Credential.Status != creds.StatusActive || boundTo(t, db, "u_m/a_1") != moved.Credential.ID {
		t.Fatalf("child moved to %s (%s), stored %s", moved.Credential.ID, moved.Credential.Status, boundTo(t, db, "u_m/a_1"))
	}
	if l := acquireAs(t, p, "u_m", RequestOptions{}); l.Credential.ID != b || l.IsNew {
		t.Fatalf("main binding left its limited pin: %+v", l)
	}
	if l := acquireAs(t, p, "u_m", childOpts("u_m", "bound", false)); l.Credential.ID != b || l.IsNew {
		t.Fatalf("account-bound child left its pin: %+v", l)
	}

	for _, id := range []string{a, c} {
		if err := creds.MarkLimited(context.Background(), db, id, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// Ten children, so a move to another limited credential cannot pass by chance.
	for i := range 10 {
		agent := fmt.Sprintf("n%d", i)
		bindRow(t, db, "u_m/a_"+agent, b, now)
		if l := acquireAs(t, p, "u_m", childOpts("u_m", agent, false)); l.Credential.ID != b || l.IsNew {
			t.Fatalf("no active candidate: child moved to %+v", l)
		}
	}
}

// The sibling lookup is a primary-key range scan, never LIKE or a table scan.
func TestFamilyQueryUsesPrimaryKeyRange(t *testing.T) {
	db, _ := setup(t)
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+familyQuery, "u_f", "u_f/", "u_f0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	got := strings.Join(plan, "\n")
	if !strings.Contains(got, "(id>? AND id<?)") || strings.Contains(got, "SCAN conversations") {
		t.Fatalf("query plan:\n%s", got)
	}
}
