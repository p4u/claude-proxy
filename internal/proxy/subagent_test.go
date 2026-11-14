package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/router"
	"github.com/p4u/claude-proxy/internal/sessionbind"
	"github.com/p4u/claude-proxy/internal/store"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// A metadata.user_id shaped like Claude Code's: longer than 32 bytes, so its
// key is hashed. Each test session uses its own suffix.
const spreadUID = "user_0123456789abcdef_account_acct-1_session_"

// spreadUpstream answers 200 and records the Authorization of every request.
type spreadUpstream struct {
	mu   sync.Mutex
	auth []string
}

func (u *spreadUpstream) handler(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.auth = append(u.auth, r.Header.Get("Authorization"))
	u.mu.Unlock()
	okJSON(w, r)
}

func (u *spreadUpstream) last() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auth[len(u.auth)-1]
}

// spreadSetup is setupProxy (Anthropic A and B) plus extra Anthropic
// credentials C, D…; tokenOf maps a credential to the Authorization it sends.
func spreadSetup(t *testing.T, extra int) (*Handler, []*creds.Credential, *store.DB, *spreadUpstream) {
	t.Helper()
	up := &spreadUpstream{}
	h, cs, db, _ := setupProxy(t, up.handler)
	for i := range extra {
		lbl := string(rune('C' + i))
		c, err := creds.Insert(context.Background(), db, lbl, "max", "sk-ant-oat-fake-"+lbl, "rt-"+lbl, time.Now().Add(time.Hour), 0)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	return h, cs, db, up
}

func tokenOf(c *creds.Credential) string { return "Bearer " + c.AccessToken }

// spreadBody is a /v1/messages body for session suffix sess. history adds an
// assistant turn; extra is spliced in as top-level fields.
func spreadBody(model, sess string, history bool, extra string) string {
	msgs := `[{"role":"user","content":"do the task"}]`
	if history {
		msgs = `[{"role":"user","content":"do the task"},{"role":"assistant","content":"working"},{"role":"user","content":"go on"}]`
	}
	return fmt.Sprintf(`{"model":%q,"max_tokens":8,"metadata":{"user_id":%q},%s"messages":%s}`, model, spreadUID+sess, extra, msgs)
}

// spreadSend posts body through srv with the given agent id ("" = main
// thread), identity (nil = none) and extra headers, and requires a 200.
func spreadSend(t *testing.T, srv http.Handler, id *usertoken.Identity, agent, body string, hdr map[string]string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if agent != "" {
		req.Header.Set(router.AgentIDHeader, agent)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if id != nil {
		req = req.WithContext(usertoken.WithIdentity(req.Context(), id))
	}
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
}

// sessionKey is the u_ key router.Derive gives session sess.
func sessionKey(t *testing.T, sess string) string {
	t.Helper()
	body := spreadBody("claude-sonnet-5", sess, false, "")
	dr := router.Derive(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), []byte(body))
	if dr.Source != router.SourceMetadataUID {
		t.Fatalf("source %s", dr.Source)
	}
	return dr.ConvID
}

// boundCred returns the credential bound to key, or "" when there is no row.
func boundCred(t *testing.T, db *store.DB, key string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`SELECT credential_id FROM conversations WHERE id=?`, key).Scan(&id)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		t.Fatal(err)
	}
	return id
}

func childRows(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id LIKE '%/a\_%' ESCAPE '\'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rowsLike(t *testing.T, db *store.DB, pattern string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id LIKE ? ESCAPE '\'`, pattern).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lastLoggedConv(t *testing.T, db *store.DB) string {
	t.Helper()
	var conv string
	if err := db.QueryRow(`SELECT conv_id FROM request_log ORDER BY id DESC LIMIT 1`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	return conv
}

// AC2: with three subscriptions, the main thread and two subagents of one
// session bind three distinct credentials and each subagent sticks to its own.
// Through AuthMiddleware with a new user, so the default-on switch is tested.
func TestSubagentsSpreadAcrossSubscriptions(t *testing.T) {
	h, cs, db, up := spreadSetup(t, 1)
	ut, err := usertoken.Create(context.Background(), db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	srv := AuthMiddleware("", db, false, h)
	auth := map[string]string{"Authorization": "Bearer " + ut.Token}
	model := "claude-sonnet-5"
	byID := map[string]*creds.Credential{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	// Five sessions, so a pick that ignores the family cannot pass by chance.
	for n := range 5 {
		sess := fmt.Sprintf("s%d", n)
		parent := sessionKey(t, sess)
		spreadSend(t, srv, nil, "", spreadBody(model, sess, false, ""), auth)
		main := boundCred(t, db, parent)
		spreadSend(t, srv, nil, "a1", spreadBody(model, sess, false, ""), auth)
		first := boundCred(t, db, router.SubagentKey(parent, "a1"))
		spreadSend(t, srv, nil, "a2", spreadBody(model, sess, false, ""), auth)
		second := boundCred(t, db, router.SubagentKey(parent, "a2"))
		if main == "" || first == "" || second == "" || main == first || main == second || first == second {
			t.Fatalf("main %q, subagent a1 %q, subagent a2 %q: want three distinct credentials", main, first, second)
		}
		for agent, want := range map[string]string{"a1": first, "a2": second} {
			spreadSend(t, srv, nil, agent, spreadBody(model, sess, true, ""), auth)
			child := router.SubagentKey(parent, agent)
			if got := boundCred(t, db, child); got != want {
				t.Fatalf("%s moved from %s to %s", agent, want, got)
			}
			if up.last() != tokenOf(byID[want]) {
				t.Fatalf("%s's second request served by another credential", agent)
			}
			if got := lastLoggedConv(t, db); got != child {
				t.Fatalf("request_log conv_id %q, want %q", got, child)
			}
		}
	}
}

// AC3: two subscriptions and three subagents. The first subagent binds the
// credential the main thread is not using while it has headroom ≥ 0.25; past
// that, and once both are in use, every request still succeeds.
func TestSubagentSpreadWithTwoSubscriptions(t *testing.T) {
	h, cs, db, _ := spreadSetup(t, 0)
	now := time.Now().Unix()
	for _, c := range cs {
		if _, err := db.Exec(`INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct) VALUES (?,?,50,0)`, c.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	model := "claude-sonnet-5"
	// Ten sessions, so a pick that ignores the family cannot pass by chance.
	for n := range 10 {
		sess := fmt.Sprintf("two%d", n)
		parent := sessionKey(t, sess)
		spreadSend(t, h, nil, "", spreadBody(model, sess, false, ""), nil)
		main := boundCred(t, db, parent)
		for i, agent := range []string{"a1", "a2", "a3"} {
			spreadSend(t, h, nil, agent, spreadBody(model, sess, false, ""), nil)
			got := boundCred(t, db, router.SubagentKey(parent, agent))
			if got == "" || (i == 0 && got == main) {
				t.Fatalf("%s: subagent %s on %q (main on %s)", sess, agent, got, main)
			}
		}
	}

	// The other subscription below the floor: the first subagent may bind either.
	parent2 := sessionKey(t, "low")
	spreadSend(t, h, nil, "", spreadBody(model, "low", false, ""), nil)
	main2 := boundCred(t, db, parent2)
	for _, c := range cs {
		if c.ID != main2 {
			if _, err := db.Exec(`INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct) VALUES (?,?,90,90)`, c.ID, now+1); err != nil {
				t.Fatal(err)
			}
		}
	}
	spreadSend(t, h, nil, "b1", spreadBody(model, "low", false, ""), nil)
	if boundCred(t, db, router.SubagentKey(parent2, "b1")) == "" {
		t.Fatal("subagent b1 has no binding")
	}
}

// AC4: a user with spreading off binds the main conversation's key; a
// first-turn subagent request that is not portable uses its child key but on
// the main thread's credential (R9).
func TestSubagentNotSpreadWhenOffOrNotPortable(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 1)
	ctx := context.Background()
	off, _ := usertoken.Create(ctx, db, "off")
	if err := usertoken.SetSpreadSubagents(ctx, db, off.ID, false); err != nil {
		t.Fatal(err)
	}
	on, _ := usertoken.Create(ctx, db, "on")
	srv := AuthMiddleware("", db, false, h)
	model := "claude-sonnet-5"

	offAuth := map[string]string{"Authorization": "Bearer " + off.Token}
	parent := sessionKey(t, "off")
	spreadSend(t, srv, nil, "", spreadBody(model, "off", false, ""), offAuth)
	spreadSend(t, srv, nil, "a1", spreadBody(model, "off", false, ""), offAuth)
	if got := lastLoggedConv(t, db); got != parent || childRows(t, db) != 0 {
		t.Fatalf("switch off: logged %q, %d child rows", got, childRows(t, db))
	}

	onAuth := map[string]string{"Authorization": "Bearer " + on.Token}
	parent2 := sessionKey(t, "np")
	spreadSend(t, srv, nil, "", spreadBody(model, "np", false, ""), onAuth)
	spreadSend(t, srv, nil, "a1", spreadBody(model, "np", false, `"container":"cntr_1",`), onAuth)
	child2 := router.SubagentKey(parent2, "a1")
	if got := lastLoggedConv(t, db); got != child2 || boundCred(t, db, child2) != boundCred(t, db, parent2) || childRows(t, db) != 1 {
		t.Fatalf("not portable: logged %q, child on %q, main on %q, %d child rows",
			got, boundCred(t, db, child2), boundCred(t, db, parent2), childRows(t, db))
	}
}

// AC5: a GLM request and a custom-host request bind the same key with the
// agent header as without it.
func TestSubagentHeaderIgnoredForOtherProviders(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 0)
	ctx := context.Background()
	if _, err := creds.InsertKey(ctx, db, provider.GLM, "zai", "pro", "zai-key", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := creds.InsertCustomKey(ctx, db, "house", "house-key", "https://house.example", []creds.Model{{ID: "house-model"}}, 1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ model, sess, key string }{
		{"glm-4.7", "glm", pool.KeyScoped(sessionKey(t, "glm"), provider.GLM, "")},
		{"house-model", "house", pool.KeyScoped(sessionKey(t, "house"), provider.Custom, "house-model")},
	} {
		spreadSend(t, h, nil, "a1", spreadBody(tc.model, tc.sess, false, ""), nil)
		spreadSend(t, h, nil, "", spreadBody(tc.model, tc.sess, false, ""), nil)
		var n int
		if err := db.QueryRow(`SELECT request_count FROM conversations WHERE id=?`, tc.key).Scan(&n); err != nil || n != 2 {
			t.Fatalf("%s: key %q request_count %d (err %v), want 2", tc.model, tc.key, n, err)
		}
	}
	if n := childRows(t, db); n != 0 {
		t.Fatalf("%d child rows", n)
	}
}

// AC6: the session registry keeps describing the main conversation after a
// subagent of the same Claude Code session is served elsewhere.
func TestSubagentDoesNotMoveSessionRecord(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 0)
	model := "claude-sonnet-5"
	// Ten sessions, so a subagent sharing the main credential cannot hide a
	// moved record by chance.
	for n := range 10 {
		sid := fmt.Sprintf("5e55a0a1-0000-4000-8000-0000000000%02d", n)
		sess := fmt.Sprintf("rec%d", n)
		hdr := map[string]string{sessionbind.Header: sid}
		parent := sessionKey(t, sess)
		spreadSend(t, h, nil, "", spreadBody(model, sess, false, ""), hdr)
		main := boundCred(t, db, parent)
		spreadSend(t, h, nil, "a1", spreadBody(model, sess, false, ""), hdr)
		if child := boundCred(t, db, router.SubagentKey(parent, "a1")); child == "" || child == main {
			t.Fatalf("%s: subagent on %q, main on %s", sess, child, main)
		}
		b, ok := h.Sessions.Lookup(sessionbind.OwnerKey(nil), sid)
		if !ok || b.CredentialID != main || b.Route.ConvID != parent {
			t.Fatalf("%s: session record %+v (ok %v), want credential %s and route %s", sess, b, ok, main, parent)
		}
	}
}

// AC12: once a subagent has its child binding, an account-scoped request stays
// on it and latches account_bound on the child row only.
func TestSubagentAccountBoundStaysOnChild(t *testing.T) {
	h, _, db, up := spreadSetup(t, 0)
	model := "claude-sonnet-5"
	parent := sessionKey(t, "acct")
	child := router.SubagentKey(parent, "a1")
	spreadSend(t, h, nil, "", spreadBody(model, "acct", false, ""), nil)
	spreadSend(t, h, nil, "a1", spreadBody(model, "acct", false, ""), nil)
	served := up.last()
	bound := boundCred(t, db, child)
	spreadSend(t, h, nil, "a1", spreadBody(model, "acct", true, `"container":"cntr_1",`), nil)
	if up.last() != served || boundCred(t, db, child) != bound || lastLoggedConv(t, db) != child {
		t.Fatalf("account-scoped request left the child binding")
	}
	var childBound, parentBound bool
	if err := db.QueryRow(`SELECT account_bound FROM conversations WHERE id=?`, child).Scan(&childBound); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT account_bound FROM conversations WHERE id=?`, parent).Scan(&parentBound); err != nil {
		t.Fatal(err)
	}
	if !childBound || parentBound {
		t.Fatalf("account_bound: child %v, parent %v", childBound, parentBound)
	}
}

// AC14 (R9): a subagent whose history has an assistant turn and no child row (a
// fork) uses its child key on the main thread's credential; a fresh one is
// spread to a child key too.
func TestSubagentWithHistoryUsesChildOnMainCredential(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 1)
	model := "claude-sonnet-5"
	parent := sessionKey(t, "hist")
	spreadSend(t, h, nil, "", spreadBody(model, "hist", false, ""), nil)
	spreadSend(t, h, nil, "late", spreadBody(model, "hist", true, ""), nil)
	late := router.SubagentKey(parent, "late")
	if got := lastLoggedConv(t, db); got != late || boundCred(t, db, late) == "" || boundCred(t, db, late) != boundCred(t, db, parent) {
		t.Fatalf("subagent with history logged %q, child on %q, main on %q", got, boundCred(t, db, late), boundCred(t, db, parent))
	}
	spreadSend(t, h, nil, "fresh", spreadBody(model, "hist", false, ""), nil)
	child := router.SubagentKey(parent, "fresh")
	if got := lastLoggedConv(t, db); got != child || boundCred(t, db, child) == "" {
		t.Fatalf("fresh subagent logged %q", got)
	}
}

// R9: a fork's side requests (haiku, count_tokens, a single user message after a
// compaction) carry its agent id and stay on its child key, served by the
// main credential; a fresh haiku subagent still spreads.
func TestSubagentForkSideRequestsStayOnSession(t *testing.T) {
	h, _, db, up := spreadSetup(t, 1)
	byID := map[string]*creds.Credential{}
	cs, err := creds.List(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		byID[c.ID] = c
	}
	model, haiku := "claude-sonnet-5", "claude-haiku-4-5"
	for n := range 10 {
		sess := fmt.Sprintf("side%d", n)
		parent := sessionKey(t, sess)
		spreadSend(t, h, nil, "", spreadBody(model, sess, false, ""), nil)
		main := boundCred(t, db, parent)
		f1 := router.SubagentKey(parent, "f1")
		check := func(step string) {
			t.Helper()
			if got := lastLoggedConv(t, db); got != f1 || boundCred(t, db, f1) != main || up.last() != tokenOf(byID[main]) {
				t.Fatalf("%s %s: logged %q, child on %q, main on %q", sess, step, got, boundCred(t, db, f1), main)
			}
		}
		spreadSend(t, h, nil, "f1", spreadBody(model, sess, true, ""), nil) // a fork: history, no child row
		check("fork")
		spreadSend(t, h, nil, "f1", spreadBody(haiku, sess, false, ""), nil)
		check("haiku")
		req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(spreadBody(model, sess, false, "")))
		req.Header.Set(router.AgentIDHeader, "f1")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("count_tokens: status %d", rw.Code)
		}
		check("count_tokens")
		spreadSend(t, h, nil, "f1", spreadBody(model, sess, false, ""), nil) // after a compaction
		check("compaction")
		if n := rowsLike(t, db, parent+"/a\\_f1"); n != 1 {
			t.Fatalf("%s: %d child rows for f1", sess, n)
		}

		h1 := router.SubagentKey(parent, "h1")
		spreadSend(t, h, nil, "h1", spreadBody(haiku, sess, false, ""), nil)
		if got := boundCred(t, db, h1); got == "" || got == main {
			t.Fatalf("%s: fresh haiku subagent on %q, main on %s", sess, got, main)
		}
	}
}

// AC15: the admin token and an unauthenticated deployment spread subagents.
func TestSubagentSpreadForAdminAndAnonymous(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 0)
	model := "claude-sonnet-5"
	for _, tc := range []struct {
		sess string
		id   *usertoken.Identity
	}{{"admin", &usertoken.Identity{IsAdmin: true}}, {"anon", nil}} {
		parent := sessionKey(t, tc.sess)
		spreadSend(t, h, tc.id, "", spreadBody(model, tc.sess, false, ""), nil)
		spreadSend(t, h, tc.id, "a1", spreadBody(model, tc.sess, false, ""), nil)
		child := router.SubagentKey(parent, "a1")
		if boundCred(t, db, child) == "" || lastLoggedConv(t, db) != child {
			t.Fatalf("%s: no child binding", tc.sess)
		}
	}
}

// AC16: with the agent header, a request keyed by X-Router-Conversation-ID,
// one with no metadata.user_id, and one with an invalid agent id bind the same
// key and credential as without the header.
func TestSubagentHeaderIgnoredWithoutUserIDKey(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 1)
	model := "claude-sonnet-5"
	noUID := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"no metadata here"}]}`, model)
	for _, tc := range []struct {
		name, agent, body string
		hdr               map[string]string
	}{
		{"explicit key", "a1", spreadBody(model, "x", false, ""), map[string]string{"X-Router-Conversation-ID": "explicit-1"}},
		{"content hash", "a1", noUID, nil},
		{"agent id too long", strings.Repeat("a", 129), spreadBody(model, "long", false, ""), nil},
		{"agent id with slash", "a/b", spreadBody(model, "slash", false, ""), nil},
	} {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(tc.body))
		for k, v := range tc.hdr {
			req.Header.Set(k, v)
		}
		key := router.Derive(req, []byte(tc.body)).ConvID
		spreadSend(t, h, nil, tc.agent, tc.body, tc.hdr)
		with := boundCred(t, db, key)
		spreadSend(t, h, nil, "", tc.body, tc.hdr)
		var n int
		if err := db.QueryRow(`SELECT request_count FROM conversations WHERE id=?`, key).Scan(&n); err != nil || n != 2 || boundCred(t, db, key) != with {
			t.Fatalf("%s: key %q request_count %d (err %v)", tc.name, key, n, err)
		}
	}
	if n := childRows(t, db); n != 0 {
		t.Fatalf("%d child rows", n)
	}
}

// AC18: a child binding's bind line names the agent and the family, and no
// log line carries the raw metadata.user_id.
func TestSubagentBindLogFields(t *testing.T) {
	h, _, _, _ := spreadSetup(t, 0)
	var logs bytes.Buffer
	h.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	model := "claude-sonnet-5"
	parent := sessionKey(t, "log")
	spreadSend(t, h, nil, "", spreadBody(model, "log", false, ""), nil)
	spreadSend(t, h, nil, "a1", spreadBody(model, "log", false, ""), nil)

	var binds []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == "bind" {
			binds = append(binds, rec)
		}
	}
	if len(binds) != 2 {
		t.Fatalf("%d bind lines", len(binds))
	}
	if _, ok := binds[0]["agent"]; ok || binds[0]["family"] != nil || binds[0]["conv"] != parent {
		t.Fatalf("main bind line: %v", binds[0])
	}
	if binds[1]["agent"] != "a1" || binds[1]["family"] != parent || binds[1]["conv"] != router.SubagentKey(parent, "a1") {
		t.Fatalf("subagent bind line: %v", binds[1])
	}
	if strings.Contains(logs.String(), spreadUID) {
		t.Fatal("a log line carries the raw metadata.user_id")
	}
}
