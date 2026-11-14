# Subagents on their own subscriptions: implementation plan

Spec: docs/specs/2026-11-14-subagent-spread-design.md
Class: architectural
Repos and branches: /home/p4u/claude-proxy → feat/subagent-spread (base 916803b), implemented in the worktree ~/.cache/claude-proxy/wt/subagent-spread

## Global Constraints
- Commit convention: conventional commits `type(scope): lowercase summary`; no AI attribution, no Co-Authored-By or similar trailers.
- Never push; commit with explicit paths only (`git commit -m "…" -- <paths>`); never `git add -A`; never `git stash`; never switch branches.
- Work only in the worktree ~/.cache/claude-proxy/wt/subagent-spread on feat/subagent-spread. Never touch the main checkout /home/p4u/claude-proxy or its CLAUDE.md.
- Build output never in /tmp. Run every `go`, `make` and `node` command from the worktree root in a shell that first ran:
  ```sh
  mkdir -p ~/.cache/claude-proxy/tmp
  export GOTOOLCHAIN=go1.26.2 GOTMPDIR=$HOME/.cache/claude-proxy/tmp TMPDIR=$HOME/.cache/claude-proxy/tmp PATH=$HOME/.cache/claude-proxy/bin:$PATH
  ```
  `GOTOOLCHAIN=go1.26.2` is CI's Go (go.mod). The host default, go1.27.4, makes golangci-lint fail on the standard library's export data, and `~/go/bin/golangci-lint` (v2.5.0) panics on it. `~/.cache/claude-proxy/bin/golangci-lint` is CI's v2.12.2 built with go1.26.2; if it is missing, rebuild it with `GOTOOLCHAIN=go1.26.2 GOBIN=$HOME/.cache/claude-proxy/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`.
- No change to the scoring formula, weights, saturation cutoffs, rebalance thresholds, compaction logic, or Codex/Gemini balancing (spec Non-goals).
- Raw `metadata.user_id`, tokens, and header values other than the validated/shortened agent id are never logged.
- Behaviour for every non-candidate request must be byte-for-byte unchanged.
- `make lint` and `go test -race` for the touched packages pass before each commit.
- How to read the code blocks: "Create `path`" means a new file with exactly that content. "Append to `path`" means adding the block after the file's last line, separated by one blank line. A `diff` block applies from the worktree root with `git apply <file>` after saving the block to a file under ~/.cache/claude-proxy/ (not /tmp), or you can make the same edits by hand. Hunk line numbers are at the base commit.
- Tests that depend on the weighted random pick repeat enough times that a broken implementation fails with probability ≥ 1 − 2⁻¹⁰. Do not reduce the repetitions.
- Comments are 1-3 lines and say why. Add no dependencies.

## Review Focus
- Key resolution under `p.mu`: `subagentKey` (new internal/pool/subagent.go) runs first in `AcquireScoped` (internal/pool/rebalance.go:146 at base), before `p.sessions` is read. The resolved key then feeds `key`, `compactionKey`, `bindLocked`, `executeUserSwitch` and `Lease.ConvID`. The existence check reads outside the bind transaction. If the row is deleted between that check and the bind (credential deletion by another process, or during `waitForDrain`, which releases `p.mu`), the row is recreated with the family exclusion. This is accepted.
- The family lookup runs inside `bindOnce`'s transaction (`pickChildLocked`, new-row branch at internal/pool/pool.go:153 at base). Its bounds `family+"/"` and `family+"0"` are bound as Go strings (a `[]byte` bind matches nothing, and `TestFamilyCredentialsRange` catches that). The session row is excluded at any age; siblings only when `last_seen_at ≥ now−15m`. The headroom floor of 0.25 is `Score(1, fh, sd)`. The fallback is `pickActiveLocked` unchanged.
- The limited re-pin (internal/pool/pool.go:240 at base) applies to child keys only (`family != ""`) and only when the credential is not saturated (the saturation path is unchanged). It needs `limited` with `retry_after` in the future and is guarded by `!accountBound`. It picks active credentials only (`pickActiveOnly`), so it never moves to another limited credential.
- `activeCandidates` now takes `exclude []string`. The switch callers (internal/pool/switch.go:87, :165) pass one id, and `NOT IN (?)` behaves like `<> ?`.
- Every later use of `convID` in `ServeHTTP` after the key swap. `convID = pool.KeyScoped(lease.ConvID, …)` runs right after `cred = lease.Credential` (internal/proxy/proxy.go:215 at base). The `bind` line, `capturePrompt`, `captureConversation`, the `forwarded` line, `captureAssistantReply` and `logRequest` all follow it. `RecordRequest` keeps `dr.ConvID` as its route. The `bind failed` line logs the session key because no lease exists yet.
- `usertoken.SpreadSubagentsFor` is on for a nil identity and for `IsAdmin` (internal/proxy/auth.go:61 builds the admin identity without the field). `AuthMiddleware` copies the column for user tokens, and `insert` returns `SpreadSubagents: true`, which is the column default.
- The `RecordRequest` skip applies only when `lease.ConvID == opts.Child`. A candidate that resolves to the session key still records, as today (internal/proxy/proxy.go:220-222 at base).
- `StartChild = directSubscription && !accountBound && noAssistantTurn(body)` is `portableRebalanceRequest(body)` for Anthropic and fails closed for anything else. No extra JSON pass runs for non-candidates.

## Task order and parallelism
| Task | Depends on | Repo/files | May run in parallel with |
|---|---|---|---|
| 1 router keys | none | internal/router/subagent.go, subagent_test.go (new) | 2, 3 |
| 2 per-user flag | none | internal/store/{schema,store,store_test}.go, internal/usertoken/{usertoken,usertoken_test}.go, internal/proxy/{auth,auth_test}.go | 1, 3 (see note) |
| 3 pool | none | internal/pool/{pool,rebalance,switch,refresh_backoff_test}.go, internal/pool/subagent.go, subagent_test.go (new) | 1, 2 (see note) |
| 4 proxy wiring | 1, 2, 3 | internal/proxy/proxy.go, internal/proxy/subagent.go, subagent_test.go (new) | 5 (see note) |
| 5 switch surfaces | 2 | internal/webui/{users,webui_test}.go, internal/webui/static/js/{api,mock}.js, static/js/pages/users.js, cmd/claude-proxy/{main,users_test}.go, docs/WEBUI.md, README.md | 3, 4 (see note) |

Note: the tasks share one worktree, and a test command compiles every package it imports: pool imports store (Task 2), and webui and cmd import pool and proxy (Tasks 3, 4). Tasks that run in parallel rerun their Step 2 commands after the other task commits. Each task runs `make lint` (whole repo) when no other task is in the middle of an edit. Run them one after another (1 → 2 → 3 → 4 → 5) unless time matters.

## Tasks

### Task 1: router: subagent child keys and agent-id validation
Model: haiku   Repo: ~/.cache/claude-proxy/wt/subagent-spread   Files: internal/router/subagent.go (new), internal/router/subagent_test.go (new)
Goal: `router.ValidAgentID`, `router.SubagentKey` and `router.AgentIDHeader`, with the AC1 tests.

Step 1: write the failing test

Create `internal/router/subagent_test.go`:
```go
package router

import (
	"strings"
	"testing"
)

// AC1: the agent id is accepted only as 1-128 characters of [0-9A-Za-z_-].
func TestValidAgentID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"a0123456789abcdef", true},
		{"aexplore-label-0123456789abcdef", true},
		{"hook-agent-3f2a9c1e-0b7d-4e55-9a10-2c4b6d8e0f12", true},
		{"A_b-9", true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{"", false},
		{"a/b", false},
		{"a b", false},
		{"a.b", false},
		{"a%2Fb", false},
		{"é", false},
	} {
		if got := ValidAgentID(tc.id); got != tc.want {
			t.Errorf("ValidAgentID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// AC1: a child key is the session key plus "/a_" and the shortened agent id,
// and sorts inside the family's primary-key range [parent+"/", parent+"0").
func TestSubagentKey(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"user_0123456789abcdef_account_acct_session_0b7d4e55"}}`)
	parent := Derive(req(""), body).ConvID
	if !strings.HasPrefix(parent, "u_") || len(parent) != len("u_")+16 {
		t.Fatalf("parent = %q, want a hashed u_ key", parent)
	}

	short := SubagentKey(parent, "a0123456789abcdef")
	if short != parent+"/a_a0123456789abcdef" {
		t.Fatalf("short id: %q", short)
	}
	long := strings.Repeat("x", 40)
	hashed := SubagentKey(parent, long)
	if hashed != parent+"/a_"+shorten(long) || len(hashed) != len(parent)+len("/a_")+16 {
		t.Fatalf("long id: %q", hashed)
	}
	if SubagentKey(parent, "a1") == SubagentKey(parent, "a2") {
		t.Fatal("distinct agent ids share a key")
	}
	for _, k := range []string{short, hashed} {
		if k < parent+"/" || k >= parent+"0" {
			t.Fatalf("%q is outside the family range of %q", k, parent)
		}
	}

	for _, tc := range []struct{ parent, agent string }{
		{parent, strings.Repeat("a", 129)},
		{parent, "a/b"},
		{parent, ""},
		{"u_short/raw", "a1"}, // a short raw metadata.user_id containing "/"
		{"", "a1"},
	} {
		if got := SubagentKey(tc.parent, tc.agent); got != "" {
			t.Errorf("SubagentKey(%q, %q) = %q, want \"\"", tc.parent, tc.agent, got)
		}
	}
}
```

Run: `go test -race ./internal/router/`
Expected: `internal/router/subagent_test.go:27:13: undefined: ValidAgentID` and `FAIL	github.com/p4u/claude-proxy/internal/router [build failed]`

Step 2: implement

Create `internal/router/subagent.go`:
```go
package router

import "strings"

// AgentIDHeader carries the id of the Claude Code subagent that sent a
// request. The main thread never sends it.
const AgentIDHeader = "X-Claude-Code-Agent-Id"

// maxAgentIDLen bounds an accepted agent id. Claude Code's own ids are at most
// 81 characters.
const maxAgentIDLen = 128

// ValidAgentID reports whether s is 1 to 128 characters of [0-9A-Za-z_-].
// Anything else is treated as if the header were absent.
func ValidAgentID(s string) bool {
	if len(s) == 0 || len(s) > maxAgentIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// SubagentKey returns a subagent's sticky key under its session's key:
// parent + "/a_" + the shortened agent id, so every subagent of a session
// sorts in the range [parent+"/", parent+"0"). It returns "" when agentID is
// invalid, or parent is empty or already contains "/" (a short raw
// metadata.user_id); such a request keeps the parent key.
func SubagentKey(parent, agentID string) string {
	if parent == "" || strings.Contains(parent, "/") || !ValidAgentID(agentID) {
		return ""
	}
	return parent + "/a_" + shorten(agentID)
}
```

Run: `go test -race -v -run 'TestValidAgentID|TestSubagentKey' ./internal/router/ && go test -race ./internal/router/ && make lint`
Expected: `--- PASS: TestValidAgentID`, `--- PASS: TestSubagentKey`, `ok  	github.com/p4u/claude-proxy/internal/router` (twice), then `0 issues.`

Step 3: commit
`git add internal/router/subagent.go internal/router/subagent_test.go && git commit -m "feat(router): subagent child keys and agent-id validation" -- internal/router/subagent.go internal/router/subagent_test.go`

Interfaces this task provides to later tasks:
- `const router.AgentIDHeader = "X-Claude-Code-Agent-Id"`
- `func router.ValidAgentID(s string) bool`: 1-128 bytes of `[0-9A-Za-z_-]`.
- `func router.SubagentKey(parent, agentID string) string`: `parent + "/a_" + shorten(agentID)`, or `""` when the agent id is invalid or `parent` is empty or contains `/`. Plan decision: the spec's condition 4 and its "a parent with `/` is not spread" rule both live here, so a caller only checks for `""`.

### Task 2: per-user `spread_subagents` switch (store, usertoken, auth)
Model: sonnet   Repo: ~/.cache/claude-proxy/wt/subagent-spread   Files: internal/store/schema.go, internal/store/store.go, internal/store/store_test.go, internal/usertoken/usertoken.go, internal/usertoken/usertoken_test.go, internal/proxy/auth.go, internal/proxy/auth_test.go
Goal: the `user_tokens.spread_subagents` column (default 1) with its model, setter, single reader and auth copy (AC9, and the identity half of AC4/AC15).

Step 1: write the failing tests

Append to `internal/store/store_test.go`:
```go
// AC9: a database from before user_tokens.spread_subagents opens with every
// existing user at 1 (subagent spreading on).
func TestMigrateUserTokensSpreadSubagents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-users.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the previous user_tokens shape in this synthetic database.
	if _, err := db.Exec(`ALTER TABLE user_tokens DROP COLUMN spread_subagents;
		INSERT INTO user_tokens (id,name,token,created_at) VALUES ('utok_a','a','tok-a',1700000000),('utok_b','b','tok-b',1700000000)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var n, on int
		err = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(spread_subagents),0) FROM user_tokens`).Scan(&n, &on)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 || on != 2 {
			t.Fatalf("users=%d with spread_subagents=1: %d, want 2 of 2", n, on)
		}
	}
}
```

Append to `internal/usertoken/usertoken_test.go`:
```go
// The subagent-spread switch is on for new users, persists when changed, and
// SpreadSubagentsFor is on for anonymous and admin callers.
func TestSpreadSubagents(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	ut, err := Create(ctx, db, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if !ut.SpreadSubagents {
		t.Fatal("Create: SpreadSubagents should default to true")
	}
	if got, _ := Get(ctx, db, ut.ID); !got.SpreadSubagents {
		t.Fatal("Get: SpreadSubagents should default to true")
	}
	if err := SetSpreadSubagents(ctx, db, ut.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := LookupByToken(ctx, db, ut.Token); got.SpreadSubagents {
		t.Fatal("SetSpreadSubagents(false) not persisted")
	}
	if err := SetSpreadSubagents(ctx, db, ut.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, ut.ID); !got.SpreadSubagents {
		t.Fatal("SetSpreadSubagents(true) not persisted")
	}
	if err := SetSpreadSubagents(ctx, db, "utok_missing", true); err != ErrNotFound {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}

	for _, tc := range []struct {
		name string
		id   *Identity
		want bool
	}{
		{"anonymous", nil, true},
		{"admin", &Identity{IsAdmin: true}, true},
		{"user on", &Identity{UserTokenID: "u", SpreadSubagents: true}, true},
		{"user off", &Identity{UserTokenID: "u"}, false},
	} {
		if got := SpreadSubagentsFor(tc.id); got != tc.want {
			t.Errorf("SpreadSubagentsFor(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

Append to `internal/proxy/auth_test.go`:
```go
// AuthMiddleware carries the user's subagent-spread switch into the identity.
func TestAuthMiddlewareCopiesSpreadSubagents(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	on, _ := usertoken.Create(ctx, db, "on")
	off, _ := usertoken.Create(ctx, db, "off")
	if err := usertoken.SetSpreadSubagents(ctx, db, off.ID, false); err != nil {
		t.Fatal(err)
	}
	var got *usertoken.Identity
	h := AuthMiddleware("", db, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = usertoken.FromContext(r.Context())
		w.WriteHeader(200)
	}))
	for _, tc := range []struct {
		token string
		want  bool
	}{{on.Token, true}, {off.Token, false}} {
		got = nil
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got == nil || got.SpreadSubagents != tc.want {
			t.Fatalf("identity = %+v, want SpreadSubagents=%v", got, tc.want)
		}
	}
}
```

Run: `go test -race -run TestMigrateUserTokensSpreadSubagents ./internal/store/; go test -race -run TestSpreadSubagents ./internal/usertoken/; go test -race -run TestAuthMiddlewareCopiesSpreadSubagents ./internal/proxy/`
Expected:
- `store_test.go:129: SQL logic error: no such column: "spread_subagents" (1)` and `FAIL	github.com/p4u/claude-proxy/internal/store`
- `internal/usertoken/usertoken_test.go:198:9: ut.SpreadSubagents undefined (type *UserToken has no field or method SpreadSubagents)`, `undefined: SetSpreadSubagents`, `undefined: SpreadSubagentsFor` and `FAIL	github.com/p4u/claude-proxy/internal/usertoken [build failed]`
- `internal/proxy/auth_test.go:273:22: undefined: usertoken.SetSpreadSubagents` and `FAIL	github.com/p4u/claude-proxy/internal/proxy [build failed]`

Step 2: implement
```diff
diff --git a/internal/proxy/auth.go b/internal/proxy/auth.go
index 11a826f..b190d81 100644
--- a/internal/proxy/auth.go
+++ b/internal/proxy/auth.go
@@ -81,6 +81,7 @@ func AuthMiddleware(adminToken string, db *store.DB, uiEnabled bool, next http.H
 					LimitOutputTokens:  ut.LimitOutputTokens,
 					LimitWindowSeconds: ut.LimitWindowSeconds,
 					BlockSuggestions:   ut.BlockSuggestions,
+					SpreadSubagents:    ut.SpreadSubagents,
 				})
 				next.ServeHTTP(w, r.WithContext(ctx))
 				return
diff --git a/internal/store/schema.go b/internal/store/schema.go
index b761844..934cf31 100644
--- a/internal/store/schema.go
+++ b/internal/store/schema.go
@@ -66,7 +66,10 @@ CREATE TABLE IF NOT EXISTS user_tokens (
   limit_window_seconds INTEGER NOT NULL DEFAULT 0,
   -- 1 => answer Claude Code's prompt-suggestion requests locally with an empty
   -- completion instead of forwarding them upstream. Default 0 = forward.
-  block_suggestions INTEGER NOT NULL DEFAULT 0
+  block_suggestions INTEGER NOT NULL DEFAULT 0,
+  -- 1 => a Claude Code subagent's first request gets its own binding,
+  -- preferring a subscription its session is not using. Default 1 = on.
+  spread_subagents INTEGER NOT NULL DEFAULT 1
 );
 CREATE INDEX IF NOT EXISTS idx_user_tokens_token  ON user_tokens(token);
 CREATE INDEX IF NOT EXISTS idx_user_tokens_status ON user_tokens(status);
diff --git a/internal/store/store.go b/internal/store/store.go
index c57174b..633bde2 100644
--- a/internal/store/store.go
+++ b/internal/store/store.go
@@ -114,6 +114,9 @@ func Open(path string) (*DB, error) {
 		// Login refresh deadline in Unix seconds. The default 0 means unknown,
 		// which is what every row that predates the column reads as.
 		`ALTER TABLE credentials ADD COLUMN refresh_expires_at INTEGER NOT NULL DEFAULT 0`,
+		// Subagent spreading is on by default; the DEFAULT turns it on for
+		// every user that predates the column.
+		`ALTER TABLE user_tokens ADD COLUMN spread_subagents INTEGER NOT NULL DEFAULT 1`,
 	} {
 		if _, err := sdb.Exec(alter); err != nil && !isDuplicateColumn(err) {
 			_ = sdb.Close()
diff --git a/internal/usertoken/usertoken.go b/internal/usertoken/usertoken.go
index 74e1b07..c8cec55 100644
--- a/internal/usertoken/usertoken.go
+++ b/internal/usertoken/usertoken.go
@@ -20,7 +20,8 @@ var ErrEmailTaken = errors.New("e-mail already belongs to another user token")
 
 // columns is the SELECT list scan reads, in order.
 const columns = `id, name, token, status, created_at, last_used_at, full_capture,
-		       limit_output_tokens, limit_window_seconds, block_suggestions, email`
+		       limit_output_tokens, limit_window_seconds, block_suggestions, email,
+		       spread_subagents`
 
 type Status string
 
@@ -51,6 +52,9 @@ type UserToken struct {
 	// Email is the member this token belongs to (lower-cased); empty for
 	// admin-created tokens.
 	Email string
+	// SpreadSubagents gives each Claude Code subagent its own binding, on a
+	// subscription its session is not using when one has room. Default true.
+	SpreadSubagents bool
 }
 
 // contextKey is unexported to prevent collisions across packages.
@@ -71,6 +75,18 @@ type Identity struct {
 	// BlockSuggestions => answer Claude Code's prompt-suggestion requests
 	// locally instead of forwarding them upstream.
 	BlockSuggestions bool
+	// SpreadSubagents is the user's switch; read it through SpreadSubagentsFor.
+	SpreadSubagents bool
+}
+
+// SpreadSubagentsFor reports whether a caller's subagent requests get their
+// own bindings. It is on for a deployment without auth (nil) and for the
+// admin token; a user token carries its own switch.
+func SpreadSubagentsFor(id *Identity) bool {
+	if id == nil || id.IsAdmin {
+		return true
+	}
+	return id.SpreadSubagents
 }
 
 // FromContext returns the identity attached by the auth middleware, or nil.
@@ -140,12 +156,13 @@ func CreateForEmail(ctx context.Context, db *store.DB, email string) (*UserToken
 // insert stores a new active token; email "" is stored as NULL.
 func insert(ctx context.Context, db *store.DB, name, email string) (*UserToken, error) {
 	ut := &UserToken{
-		ID:        newID(),
-		Name:      name,
-		Token:     GenerateToken(),
-		Status:    StatusActive,
-		CreatedAt: time.Now(),
-		Email:     email,
+		ID:              newID(),
+		Name:            name,
+		Token:           GenerateToken(),
+		Status:          StatusActive,
+		CreatedAt:       time.Now(),
+		Email:           email,
+		SpreadSubagents: true, // the column's default
 	}
 	_, err := db.ExecContext(ctx, `
 		INSERT INTO user_tokens (id, name, token, status, created_at, email)
@@ -309,6 +326,19 @@ func SetBlockSuggestions(ctx context.Context, db *store.DB, id string, block boo
 	return nil
 }
 
+// SetSpreadSubagents turns subagent spreading on or off for a user.
+func SetSpreadSubagents(ctx context.Context, db *store.DB, id string, on bool) error {
+	res, err := db.ExecContext(ctx,
+		`UPDATE user_tokens SET spread_subagents=? WHERE id=?`, boolToInt(on), id)
+	if err != nil {
+		return err
+	}
+	if n, _ := res.RowsAffected(); n == 0 {
+		return ErrNotFound
+	}
+	return nil
+}
+
 func boolToInt(b bool) int {
 	if b {
 		return 1
@@ -362,7 +392,8 @@ func scan(rs interface{ Scan(...any) error }) (*UserToken, error) {
 	var email sql.NullString
 	var status string
 	if err := rs.Scan(&ut.ID, &ut.Name, &ut.Token, &status, &created, &lu, &ut.FullCapture,
-		&ut.LimitOutputTokens, &ut.LimitWindowSeconds, &ut.BlockSuggestions, &email); err != nil {
+		&ut.LimitOutputTokens, &ut.LimitWindowSeconds, &ut.BlockSuggestions, &email,
+		&ut.SpreadSubagents); err != nil {
 		return nil, err
 	}
 	ut.Status = Status(status)
```

Run: `go test -race -v -run TestMigrateUserTokensSpreadSubagents ./internal/store/ && go test -race -v -run TestSpreadSubagents ./internal/usertoken/ && go test -race -v -run TestAuthMiddlewareCopiesSpreadSubagents ./internal/proxy/ && go test -race ./internal/store/ ./internal/usertoken/ ./internal/proxy/ ./internal/webui/ ./cmd/... && make lint`
Expected: `--- PASS: TestMigrateUserTokensSpreadSubagents`, `--- PASS: TestSpreadSubagents`, `--- PASS: TestAuthMiddlewareCopiesSpreadSubagents`, then five `ok` lines (`internal/store`, `internal/usertoken`, `internal/proxy`, `internal/webui`, `cmd/claude-proxy`), then `0 issues.`

Step 3: commit
`git commit -m "feat(usertoken): per-user spread_subagents switch, on by default" -- internal/store/schema.go internal/store/store.go internal/store/store_test.go internal/usertoken/usertoken.go internal/usertoken/usertoken_test.go internal/proxy/auth.go internal/proxy/auth_test.go`

Interfaces this task provides to later tasks:
- Column `user_tokens.spread_subagents INTEGER NOT NULL DEFAULT 1` (CREATE TABLE and ALTER list).
- `UserToken.SpreadSubagents bool`, `Identity.SpreadSubagents bool` (filled by `AuthMiddleware`).
- `func usertoken.SetSpreadSubagents(ctx context.Context, db *store.DB, id string, on bool) error`, which returns `ErrNotFound` on 0 rows.
- `func usertoken.SpreadSubagentsFor(id *usertoken.Identity) bool`: true for nil and `IsAdmin`, else `id.SpreadSubagents`. It is the only reader of the flag on the request path.

### Task 3: pool: child keys, family anti-affinity, limited child re-pin
Model: opus   Repo: ~/.cache/claude-proxy/wt/subagent-spread   Files: internal/pool/pool.go, internal/pool/rebalance.go, internal/pool/switch.go, internal/pool/refresh_backoff_test.go, internal/pool/subagent.go (new), internal/pool/subagent_test.go (new)
Goal: the pool resolves a subagent candidate's key under `p.mu` and reports it in `Lease.ConvID`. A new child binding avoids its family's credentials (with the 0.25 headroom floor and the fallback), and a limited child binding moves (AC7, AC13, AC17, plus the spec's key rule). Every existing pool suite stays green.

Step 1: write the failing test

Create `internal/pool/subagent_test.go`:
```go
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

// The key rule: an existing child row or StartChild selects the child key;
// anything else, and every malformed option set, keeps the key passed in.
func TestSubagentKeyResolution(t *testing.T) {
	db, _ := setup(t)
	p := New(db)
	const fam = "u_fam"

	if l := acquireAs(t, p, fam, RequestOptions{}); l.ConvID != fam {
		t.Fatalf("plain request: ConvID %q", l.ConvID)
	}
	if l := acquireAs(t, p, fam, childOpts(fam, "late", false)); l.ConvID != fam || rowCount(t, db, fam+"/a_late") != 0 {
		t.Fatalf("no child row, StartChild false: ConvID %q", l.ConvID)
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
	for i := range 5 {
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
```

Run: `go test -race ./internal/pool/`
Expected: `internal/pool/subagent_test.go:18:24: unknown field Family in struct literal of type RequestOptions`, then more `unknown field`/`ConvID undefined` lines, and `FAIL	github.com/p4u/claude-proxy/internal/pool [build failed]`

Step 2: implement

Create `internal/pool/subagent.go`:
```go
package pool

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/provider"
)

const (
	// familyHeadroom is the least headroom, Room(5h) × Room(7d)^SevenDayExp,
	// a candidate needs for a subagent's first binding to avoid its family's
	// credentials. Below it on every other candidate, the ordinary pick runs.
	familyHeadroom = 0.25
	// familyLive is how recently a sibling subagent must have been seen for
	// its credential to count as in use by the family.
	familyLive = 15 * time.Minute
)

// familyQuery lists the credentials a session already uses: its own binding,
// whatever its age, and its subagents' bindings seen since the cut-off. Every
// child key sorts in [family+"/", family+"0") because '0' follows '/', so the
// second part is a primary-key range scan. The bounds must be bound as Go
// strings: a []byte bind compares as BLOB and matches nothing.
const familyQuery = `
	SELECT credential_id FROM conversations WHERE id = ?
	UNION
	SELECT credential_id FROM conversations
	WHERE id >= ? AND id < ? AND last_seen_at >= ?`

// subagentKey resolves the key of a subagent candidate: the child key when
// its row exists or the request may start it, else convID. family is the
// session key whenever the child key is used; bindOnce then applies the
// anti-affinity pick to a new row and the limited re-pin to an existing one.
// Options that do not describe a child of convID on an unscoped Anthropic
// binding are ignored. Callers hold p.mu.
func (p *Pool) subagentKey(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, opts RequestOptions) (string, string, error) {
	if opts.Child == "" || opts.Family == "" || opts.Family != convID || !strings.HasPrefix(opts.Child, opts.Family+"/") ||
		(prov != "" && prov != provider.Anthropic) || scope != "" || len(allowed) > 0 {
		return convID, "", nil
	}
	if opts.StartChild {
		return opts.Child, opts.Family, nil
	}
	var one int
	err := p.db.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id=?`, opts.Child).Scan(&one)
	switch {
	case err == nil:
		return opts.Child, opts.Family, nil
	case errors.Is(err, sql.ErrNoRows):
		return convID, "", nil
	default:
		return "", "", err
	}
}

// pickChildLocked picks the credential of a new child binding. The family's
// credentials are excluded while another candidate keeps familyHeadroom, and
// candidates below it are dropped; otherwise this is the ordinary pick. The
// weighted pick decides among what is left.
func (p *Pool) pickChildLocked(ctx context.Context, tx *sql.Tx, prov provider.ID, allowed []string, family string) (string, error) {
	now := time.Now()
	exclude, err := familyCredentials(ctx, tx, family, now)
	if err != nil {
		return "", err
	}
	if len(exclude) > 0 {
		candidates, err := activeCandidates(ctx, tx, prov, allowed, exclude, now)
		if err != nil {
			return "", err
		}
		if candidates, err = withHeadroom(ctx, tx, candidates, familyHeadroom); err != nil {
			return "", err
		}
		if len(candidates) > 0 {
			return p.weightedRandPick(ctx, tx, candidates)
		}
	}
	return p.pickActiveLocked(ctx, tx, prov, allowed)
}

// familyCredentials returns the credentials of family's own binding and of
// its children seen within familyLive of now.
func familyCredentials(ctx context.Context, q rebalanceReader, family string, now time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, familyQuery, family, family+"/", family+"0", now.Add(-familyLive).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// withHeadroom keeps the candidates whose latest usage snapshot leaves at
// least floor of headroom (Score at weight 1). A credential without a
// snapshot has full headroom, as in weightedRandPick.
func withHeadroom(ctx context.Context, q rebalanceReader, candidates []weightedEntry, floor float64) ([]weightedEntry, error) {
	var kept []weightedEntry
	for _, e := range candidates {
		var fh, sd float64
		err := q.QueryRowContext(ctx, `
			SELECT five_hour_pct, seven_day_pct FROM usage_history
			WHERE credential_id=? ORDER BY captured_at DESC LIMIT 1`, e.id).Scan(&fh, &sd)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if Score(1, fh, sd) >= floor {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// limitedNow reports a credential cooling down after a 429.
func limitedNow(c *creds.Credential, now time.Time) bool {
	return c.Status == creds.StatusLimited && c.RetryAfter != nil && c.RetryAfter.After(now)
}

// pickActiveOnly is the ordinary pick without the limited fallback:
// ErrNoCredentials when no active candidate is left.
func (p *Pool) pickActiveOnly(ctx context.Context, tx rebalanceReader, prov provider.ID, allowed []string) (string, error) {
	candidates, err := activeCandidates(ctx, tx, prov, allowed, nil, time.Now())
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", ErrNoCredentials
	}
	return p.weightedRandPick(ctx, tx, candidates)
}
```

Then apply:
```diff
diff --git a/internal/pool/pool.go b/internal/pool/pool.go
index b658f19..fa3a7fd 100644
--- a/internal/pool/pool.go
+++ b/internal/pool/pool.go
@@ -80,10 +80,12 @@ func (p *Pool) BindScoped(ctx context.Context, convID string, prov provider.ID,
 	p.mu.Lock()
 	defer p.mu.Unlock()
 
-	return p.bindLocked(ctx, convID, prov, scope, allowed, true)
+	return p.bindLocked(ctx, convID, prov, scope, allowed, "", true)
 }
 
-func (p *Pool) bindLocked(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, touch bool) (*creds.Credential, bool, error) {
+// bindLocked binds convID. A non-empty family marks convID as a subagent's
+// child key under that session key (see subagentKey).
+func (p *Pool) bindLocked(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, family string, touch bool) (*creds.Credential, bool, error) {
 	if prov == "" {
 		prov = provider.Default
 	}
@@ -93,7 +95,7 @@ func (p *Pool) bindLocked(ctx context.Context, convID string, prov provider.ID,
 	)
 	err := store.Retry(ctx, func() error {
 		var err error
-		c, isNew, err = p.bindOnce(ctx, convID, prov, scope, allowed, touch)
+		c, isNew, err = p.bindOnce(ctx, convID, prov, scope, allowed, family, touch)
 		return err
 	})
 	if err == nil {
@@ -134,7 +136,7 @@ func KeyScoped(convID string, prov provider.ID, scope string) string {
 	return key
 }
 
-func (p *Pool) bindOnce(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, touch bool) (*creds.Credential, bool, error) {
+func (p *Pool) bindOnce(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, family string, touch bool) (*creds.Credential, bool, error) {
 	tx, err := p.db.BeginTx(ctx, nil)
 	if err != nil {
 		return nil, false, err
@@ -152,7 +154,11 @@ func (p *Pool) bindOnce(ctx context.Context, convID string, prov provider.ID, sc
 	switch {
 	case err == sql.ErrNoRows:
 		newConv = true
-		credID, err = p.pickActiveLocked(ctx, tx, prov, allowed)
+		if family != "" {
+			credID, err = p.pickChildLocked(ctx, tx, prov, allowed, family)
+		} else {
+			credID, err = p.pickActiveLocked(ctx, tx, prov, allowed)
+		}
 		if err != nil {
 			return nil, false, err
 		}
@@ -237,8 +243,15 @@ func (p *Pool) bindOnce(ctx context.Context, convID string, prov provider.ID, sc
 			if serr != nil {
 				return nil, false, serr
 			}
-			if saturated && !accountBound {
-				newCredID, perr := p.pickActiveLocked(ctx, tx, prov, allowed)
+			// A child binding has no user switch, so it leaves a pin cooling
+			// down after a 429 for an active credential, if there is one.
+			limitedChild := family != "" && !saturated && limitedNow(c, time.Now())
+			if (saturated || limitedChild) && !accountBound {
+				pick := p.pickActiveLocked
+				if limitedChild {
+					pick = p.pickActiveOnly
+				}
+				newCredID, perr := pick(ctx, tx, prov, allowed)
 				switch {
 				case errors.Is(perr, ErrNoCredentials):
 					// No healthy alternative — keep the sticky (saturated) pin
@@ -308,7 +321,7 @@ func (p *Pool) pickActiveLocked(ctx context.Context, tx rebalanceReader, prov pr
 	if prov == "" {
 		prov = provider.Default
 	}
-	candidates, err := activeCandidates(ctx, tx, prov, allowed, "", time.Now())
+	candidates, err := activeCandidates(ctx, tx, prov, allowed, nil, time.Now())
 	if err != nil {
 		return "", err
 	}
@@ -352,14 +365,19 @@ func (p *Pool) pickActiveLocked(ctx context.Context, tx rebalanceReader, prov pr
 
 // activeCandidates is the picker's eligible set: active credentials of prov
 // (restricted to allowed when non-empty) that are not cooling down after a 429
-// and whose latest usage snapshot is not saturated. exclude, when set, removes
-// one credential — the current pin of a user-requested switch.
-func activeCandidates(ctx context.Context, q rebalanceReader, prov provider.ID, allowed []string, exclude string, now time.Time) ([]weightedEntry, error) {
+// and whose latest usage snapshot is not saturated. exclude removes
+// credentials: the current pin of a user-requested switch, or the family of a
+// subagent's first binding.
+func activeCandidates(ctx context.Context, q rebalanceReader, prov provider.ID, allowed []string, exclude []string, now time.Time) ([]weightedEntry, error) {
 	inClause, inArgs := idFilter(allowed)
 	args := append([]any{string(prov), now.Unix()}, inArgs...)
-	if exclude != "" {
-		inClause += " AND c.id <> ?"
-		args = append(args, exclude)
+	if len(exclude) > 0 {
+		ph := make([]string, len(exclude))
+		for i, id := range exclude {
+			ph[i] = "?"
+			args = append(args, id)
+		}
+		inClause += " AND c.id NOT IN (" + strings.Join(ph, ",") + ")"
 	}
 	rows, err := q.QueryContext(ctx, `
 		SELECT c.id, c.weight FROM credentials c
diff --git a/internal/pool/rebalance.go b/internal/pool/rebalance.go
index 8574920..c8097d5 100644
--- a/internal/pool/rebalance.go
+++ b/internal/pool/rebalance.go
@@ -34,6 +34,14 @@ type RequestOptions struct {
 	// including later requests that omit account-scoped objects.
 	AccountBound bool
 	Compaction   CompactionRequest
+	// A subagent candidate (spec "Which key a subagent request uses"): Family
+	// is the session's raw key, which the caller also passes as convID, and
+	// Child the subagent's key under it. StartChild allows creating the child
+	// binding: the request is portable and carries no assistant turn. All are
+	// empty for any other request.
+	Family     string
+	Child      string
+	StartChild bool
 }
 
 type rebalancePlan struct {
@@ -93,6 +101,7 @@ type Lease struct {
 	IsNew           bool
 	Rebalance       string // empty, "pending", "switched", "deferred", or "cancelled"
 	RebalanceReason string // bounded, account-neutral diagnostic for deferred/cancelled; "user" for a requested switch
+	ConvID          string // raw key the binding used: the convID passed in, or RequestOptions.Child
 
 	pool       *Pool
 	state      *sessionState
@@ -143,6 +152,10 @@ func (l *Lease) ReleaseSummary(notified bool, proof SummaryProof) {
 func (p *Pool) AcquireScoped(ctx context.Context, convID string, prov provider.ID, scope string, allowed []string, opts RequestOptions) (*Lease, error) {
 	p.mu.Lock()
 	defer p.mu.Unlock()
+	convID, family, err := p.subagentKey(ctx, convID, prov, scope, allowed, opts)
+	if err != nil {
+		return nil, err
+	}
 	key := KeyScoped(convID, prov, scope)
 	compactionKey := compactionRequestKey(key, opts.Compaction)
 	s := p.sessions[key]
@@ -166,7 +179,7 @@ func (p *Pool) AcquireScoped(ctx context.Context, convID string, prov provider.I
 		if err := ctx.Err(); err != nil {
 			return nil, err
 		}
-		c, isNew, err := p.bindLocked(ctx, convID, prov, scope, allowed, touch)
+		c, isNew, err := p.bindLocked(ctx, convID, prov, scope, allowed, family, touch)
 		if err != nil {
 			return nil, err
 		}
@@ -191,7 +204,7 @@ func (p *Pool) AcquireScoped(ctx context.Context, convID string, prov provider.I
 		case s.pending != nil && now.Sub(s.pending.created) >= rebalanceNoticeTTL:
 			p.finishPlan(s, "notice-expired")
 		}
-		l := &Lease{Credential: c, IsNew: isNew, pool: p, state: s}
+		l := &Lease{Credential: c, IsNew: isNew, ConvID: convID, pool: p, state: s}
 		gate := compactionGate{allowSwitch: true, allowCheck: true, reason: "usage-advantage"}
 		if trackCompaction {
 			gate = p.compactionFor(s, compactionKey, c.ID, opts.Compaction, now)
diff --git a/internal/pool/refresh_backoff_test.go b/internal/pool/refresh_backoff_test.go
index 59b2a64..fad0b43 100644
--- a/internal/pool/refresh_backoff_test.go
+++ b/internal/pool/refresh_backoff_test.go
@@ -29,7 +29,7 @@ func TestTransientRefreshFailureLeavesActiveCandidates(t *testing.T) {
 	if _, _, err := creds.NewRefresher(db).Refresh(ctx, cs[0].ID); err == nil {
 		t.Fatal("want a transient refresh error")
 	}
-	entries, err := activeCandidates(ctx, db, provider.Anthropic, nil, "", time.Now())
+	entries, err := activeCandidates(ctx, db, provider.Anthropic, nil, nil, time.Now())
 	if err != nil {
 		t.Fatal(err)
 	}
diff --git a/internal/pool/switch.go b/internal/pool/switch.go
index df2c53a..5e1cfc0 100644
--- a/internal/pool/switch.go
+++ b/internal/pool/switch.go
@@ -84,7 +84,7 @@ func (p *Pool) RequestSwitch(ctx context.Context, convID string, prov provider.I
 		return SwitchPlan{From: from, To: to}, nil
 	}
 
-	candidates, err := activeCandidates(ctx, p.db, prov, allowed, from.ID, time.Now())
+	candidates, err := activeCandidates(ctx, p.db, prov, allowed, []string{from.ID}, time.Now())
 	if err != nil {
 		return SwitchPlan{}, err
 	}
@@ -162,7 +162,7 @@ func (p *Pool) userSwitchOnce(ctx context.Context, key string, current *creds.Cr
 	if stored != current.ID || accountBound {
 		return nil, nil
 	}
-	candidates, err := activeCandidates(ctx, tx, prov, allowed, current.ID, time.Now())
+	candidates, err := activeCandidates(ctx, tx, prov, allowed, []string{current.ID}, time.Now())
 	if err != nil {
 		return nil, err
 	}
```

Run: `go test -race -v -run 'TestSubagentKeyResolution|TestChildAvoidsFamilyAfterRestart|TestFamilyCredentialsRange|TestFamilyExclusionLivenessAndFallback|TestLimitedChildMoves|TestFamilyQueryUsesPrimaryKeyRange' ./internal/pool/ && go test -race ./internal/pool/ ./internal/proxy/ ./internal/claudioapi/ && go test -race -count=20 -run 'TestSubagentKeyResolution|TestChildAvoidsFamilyAfterRestart|TestFamilyCredentialsRange|TestFamilyExclusionLivenessAndFallback|TestLimitedChildMoves|TestFamilyQueryUsesPrimaryKeyRange' ./internal/pool/ && make lint`
Expected: six `--- PASS:` lines (`TestSubagentKeyResolution`, `TestChildAvoidsFamilyAfterRestart`, `TestFamilyCredentialsRange`, `TestFamilyExclusionLivenessAndFallback`, `TestLimitedChildMoves`, `TestFamilyQueryUsesPrimaryKeyRange`), `ok  	github.com/p4u/claude-proxy/internal/pool` (the existing suites take about 30 s), `ok` for `internal/proxy` and `internal/claudioapi`, `ok  	github.com/p4u/claude-proxy/internal/pool` for the 20 repetitions, then `0 issues.`

Step 3: commit
`git add internal/pool/subagent.go internal/pool/subagent_test.go && git commit -m "feat(pool): subagent child bindings avoid their family's subscriptions" -- internal/pool/pool.go internal/pool/rebalance.go internal/pool/switch.go internal/pool/refresh_backoff_test.go internal/pool/subagent.go internal/pool/subagent_test.go`

Interfaces this task provides to later tasks:
- `pool.RequestOptions` gains `Family string`, `Child string`, `StartChild bool`. All are empty for non-candidates. The caller passes the session key as `convID` and `Family == convID`. Options that do not describe a child of `convID` on an unscoped Anthropic binding are ignored.
- `pool.Lease.ConvID string`: the raw key the binding used, which is `convID` or `opts.Child`.
- Unexported: `activeCandidates(..., exclude []string, ...)`; `bindLocked`/`bindOnce` take `family string` before `touch` (`BindScoped` passes `""`); `subagentKey`, `pickChildLocked`, `familyCredentials`, `familyQuery`, `withHeadroom`, `limitedNow`, `pickActiveOnly`; constants `familyHeadroom = 0.25` and `familyLive = 15 * time.Minute`.

### Task 4: proxy: detect subagent candidates and use the pool's key
Model: sonnet   Repo: ~/.cache/claude-proxy/wt/subagent-spread   Files: internal/proxy/proxy.go, internal/proxy/subagent.go (new), internal/proxy/subagent_test.go (new)
Why sonnet: Task 3 already holds the selection and concurrency logic. This task is one 20-line diff plus a helper file and tests, all written out below, and the controller checks the `convID` uses against Review Focus.
Goal: candidate detection (spec conditions 1-4 and 6), `StartChild`, the session key passed with the options, `Lease.ConvID` for every later record and log, the `RecordRequest` skip, and the `agent`/`family` bind fields (AC2-AC6, AC12, AC14-AC16, AC18).

Step 1: write the failing test

Create `internal/proxy/subagent_test.go`:
```go
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
	parent := sessionKey(t, "s1")
	model := "claude-sonnet-5"

	spreadSend(t, srv, nil, "", spreadBody(model, "s1", false, ""), auth)
	main := boundCred(t, db, parent)
	spreadSend(t, srv, nil, "a1", spreadBody(model, "s1", false, ""), auth)
	first := boundCred(t, db, router.SubagentKey(parent, "a1"))
	spreadSend(t, srv, nil, "a2", spreadBody(model, "s1", false, ""), auth)
	second := boundCred(t, db, router.SubagentKey(parent, "a2"))
	if main == "" || first == "" || second == "" || main == first || main == second || first == second {
		t.Fatalf("main %q, subagent a1 %q, subagent a2 %q: want three distinct credentials", main, first, second)
	}
	byID := map[string]*creds.Credential{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	for agent, want := range map[string]string{"a1": first, "a2": second} {
		spreadSend(t, srv, nil, agent, spreadBody(model, "s1", true, ""), auth)
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
	parent := sessionKey(t, "s1")
	spreadSend(t, h, nil, "", spreadBody(model, "s1", false, ""), nil)
	main := boundCred(t, db, parent)
	for i, agent := range []string{"a1", "a2", "a3"} {
		spreadSend(t, h, nil, agent, spreadBody(model, "s1", false, ""), nil)
		got := boundCred(t, db, router.SubagentKey(parent, agent))
		if got == "" || (i == 0 && got == main) {
			t.Fatalf("subagent %s on %q (main on %s)", agent, got, main)
		}
	}

	// The other subscription below the floor: the first subagent may bind either.
	parent2 := sessionKey(t, "s2")
	spreadSend(t, h, nil, "", spreadBody(model, "s2", false, ""), nil)
	main2 := boundCred(t, db, parent2)
	for _, c := range cs {
		if c.ID != main2 {
			if _, err := db.Exec(`INSERT INTO usage_history (credential_id,captured_at,five_hour_pct,seven_day_pct) VALUES (?,?,90,90)`, c.ID, now+1); err != nil {
				t.Fatal(err)
			}
		}
	}
	spreadSend(t, h, nil, "b1", spreadBody(model, "s2", false, ""), nil)
	if boundCred(t, db, router.SubagentKey(parent2, "b1")) == "" {
		t.Fatal("subagent b1 has no binding")
	}
}

// AC4: a user with spreading off, and a first-turn subagent request that is
// not portable, bind the main conversation's key and credential.
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
	if got := lastLoggedConv(t, db); got != parent2 || childRows(t, db) != 0 {
		t.Fatalf("not portable: logged %q, %d child rows", got, childRows(t, db))
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
	const sid = "5e55a0a1-0000-4000-8000-0000000000aa"
	hdr := map[string]string{sessionbind.Header: sid}
	model := "claude-sonnet-5"
	parent := sessionKey(t, "rec")
	spreadSend(t, h, nil, "", spreadBody(model, "rec", false, ""), hdr)
	main := boundCred(t, db, parent)
	spreadSend(t, h, nil, "a1", spreadBody(model, "rec", false, ""), hdr)
	if child := boundCred(t, db, router.SubagentKey(parent, "a1")); child == "" || child == main {
		t.Fatalf("subagent on %q, main on %s", child, main)
	}
	b, ok := h.Sessions.Lookup(sessionbind.OwnerKey(nil), sid)
	if !ok || b.CredentialID != main || b.Route.ConvID != parent {
		t.Fatalf("session record %+v (ok %v), want credential %s and route %s", b, ok, main, parent)
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

// AC14: a subagent whose history has an assistant turn and no child row keeps
// the session key; a fresh one (user turns only) gets a child key.
func TestSubagentWithHistoryKeepsParent(t *testing.T) {
	h, _, db, _ := spreadSetup(t, 1)
	model := "claude-sonnet-5"
	parent := sessionKey(t, "hist")
	spreadSend(t, h, nil, "", spreadBody(model, "hist", false, ""), nil)
	spreadSend(t, h, nil, "late", spreadBody(model, "hist", true, ""), nil)
	if got := lastLoggedConv(t, db); got != parent || boundCred(t, db, router.SubagentKey(parent, "late")) != "" {
		t.Fatalf("subagent with history logged %q", got)
	}
	spreadSend(t, h, nil, "fresh", spreadBody(model, "hist", false, ""), nil)
	child := router.SubagentKey(parent, "fresh")
	if got := lastLoggedConv(t, db); got != child || boundCred(t, db, child) == "" {
		t.Fatalf("fresh subagent logged %q", got)
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
```

Run: `go test -race -run TestSubagent ./internal/proxy/`
Expected: seven failures: `--- FAIL: TestSubagentsSpreadAcrossSubscriptions` (`subagent a1 "", subagent a2 "": want three distinct credentials`), `--- FAIL: TestSubagentSpreadWithTwoSubscriptions`, `--- FAIL: TestSubagentDoesNotMoveSessionRecord`, `--- FAIL: TestSubagentAccountBoundStaysOnChild`, `--- FAIL: TestSubagentWithHistoryKeepsParent`, `--- FAIL: TestSubagentSpreadForAdminAndAnonymous`, `--- FAIL: TestSubagentBindLogFields`, then `FAIL	github.com/p4u/claude-proxy/internal/proxy`. `TestSubagentNotSpreadWhenOffOrNotPortable`, `TestSubagentHeaderIgnoredForOtherProviders` and `TestSubagentHeaderIgnoredWithoutUserIDKey` already pass, because they pin today's behaviour (AC4, AC5, AC16).

Step 2: implement

Create `internal/proxy/subagent.go`:
```go
package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/router"
	"github.com/p4u/claude-proxy/internal/usertoken"
)

// subagentChild returns the child key of a subagent candidate, or "" for any
// other request: an Anthropic, non-custom request keyed by metadata.user_id,
// from a caller with spreading on, carrying a valid X-Claude-Code-Agent-Id.
// The caller has already checked needsSticky.
func subagentChild(r *http.Request, dr router.Result, prov provider.ID, customModel string) string {
	if prov != provider.Anthropic || customModel != "" || dr.Source != router.SourceMetadataUID ||
		!usertoken.SpreadSubagentsFor(usertoken.FromContext(r.Context())) {
		return ""
	}
	return router.SubagentKey(dr.ConvID, r.Header.Get(router.AgentIDHeader))
}

// noAssistantTurn reports whether body's messages hold no assistant turn, as
// in the first request of a fresh subagent. Forks, and subagents already
// running before their child binding existed, carry assistant turns.
func noAssistantTurn(body []byte) bool {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			return false
		}
	}
	return true
}
```

Then apply:
```diff
diff --git a/internal/proxy/proxy.go b/internal/proxy/proxy.go
index 835c9ac..de28566 100644
--- a/internal/proxy/proxy.go
+++ b/internal/proxy/proxy.go
@@ -199,6 +199,14 @@ func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
 			opts.Compaction, opts.ObserveOnly = classifyCompaction(r, body, dr.Source)
 			captureSummary = opts.Compaction.Phase == pool.CompactionSummary && opts.Compaction.Request != ([32]byte{})
 		}
+		// A subagent candidate still passes the session key; the pool picks
+		// the child key when its binding exists or this request may start it
+		// (portable, no assistant turn). For a direct subscription
+		// !accountBound is portableRebalanceRequest.
+		if child := subagentChild(r, dr, prov, custom.Model); child != "" {
+			opts.Family, opts.Child = dr.ConvID, child
+			opts.StartChild = directSubscription && !accountBound && noAssistantTurn(body)
+		}
 		lease, err := h.pool.AcquireScoped(r.Context(), dr.ConvID, prov, custom.Model, custom.CredIDs, opts)
 		if err != nil {
 			h.log.Warn("bind failed", "err", err, "conv", convID, "src", string(convSrc), "provider", string(prov))
@@ -213,21 +221,30 @@ func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
 		}()
 		notice.state, notice.reason = lease.Rebalance, lease.RebalanceReason
 		cred = lease.Credential
+		// Logs, request_log and the captures name the binding the pool used.
+		convID = pool.KeyScoped(lease.ConvID, prov, custom.Model)
+		spread := opts.Child != "" && lease.ConvID == opts.Child
 		// Observe-only requests (haiku background calls, count_tokens) are
 		// skipped: they may be served by a different provider than the
 		// session's main model and would make the reported credential flap.
 		// The route lets POST /v1/claudio/session/switch find this binding.
-		if !opts.ObserveOnly {
+		// A spread subagent is skipped too: the session record keeps
+		// describing the main conversation, which claudio's switch moves.
+		if !opts.ObserveOnly && !spread {
 			h.Sessions.RecordRequest(r, cred.ID, sessionbind.Route{
 				ConvID: dr.ConvID, Provider: prov, Scope: custom.Model, Allowed: custom.CredIDs})
 		}
-		h.log.Info("bind",
+		bindAttrs := []any{
 			"conv", convID, "src", string(convSrc), "new", lease.IsNew,
 			"provider", string(prov),
 			"cred", cred.ID, "label", cred.Label,
 			"sub", cred.SubscriptionType, "weight", cred.Weight,
 			"status", string(cred.Status),
-			"req_count", cred.RequestCount, "path", r.URL.Path)
+			"req_count", cred.RequestCount, "path", r.URL.Path}
+		if spread {
+			bindAttrs = append(bindAttrs, "agent", strings.TrimPrefix(lease.ConvID, opts.Family+"/a_"), "family", opts.Family)
+		}
+		h.log.Info("bind", bindAttrs...)
 	} else {
 		c, err := h.pickForProvider(r.Context(), prov)
 		if err != nil {
```

Run: `go test -race -v -run TestSubagent ./internal/proxy/ && go test -race ./internal/proxy/ ./internal/pool/ ./internal/claudioapi/ ./internal/sessionbind/ && go test -race -count=10 -run TestSubagent ./internal/proxy/ && make lint`
Expected: ten `--- PASS: TestSubagent…` lines, then `ok` for `internal/proxy`, `internal/pool`, `internal/claudioapi` and `internal/sessionbind`, then `ok  	github.com/p4u/claude-proxy/internal/proxy` for the 10 repetitions, then `0 issues.`

Step 3: commit
`git add internal/proxy/subagent.go internal/proxy/subagent_test.go && git commit -m "feat(proxy): spread claude code subagents across subscriptions" -- internal/proxy/proxy.go internal/proxy/subagent.go internal/proxy/subagent_test.go`

Interfaces this task provides to later tasks: none exported. Unexported: `subagentChild(r *http.Request, dr router.Result, prov provider.ID, customModel string) string` and `noAssistantTurn(body []byte) bool`.

### Task 5: the switch in the admin API, web UI, mock, CLI and docs
Model: sonnet   Repo: ~/.cache/claude-proxy/wt/subagent-spread   Files: internal/webui/users.go, internal/webui/webui_test.go, internal/webui/static/js/api.js, internal/webui/static/js/pages/users.js, internal/webui/static/js/mock.js, cmd/claude-proxy/main.go, cmd/claude-proxy/users_test.go, docs/WEBUI.md, README.md
Goal: `POST /api/users/{id}/subagents`, `spread_subagents` in the users list, the Users table's "Subagents" switch with the API client and stateful mock route, `claude-proxy users subagents <id> on|off` and the SUBAGENTS column, and the docs (AC8 and the spec's CLI row).

Step 1: write the failing tests

Append to `internal/webui/webui_test.go`:
```go
// AC8: subagent spreading is on for a new user, the admin route toggles and
// persists it, the list reflects it, and an unknown user is a 404.
func TestUserSubagentsToggle(t *testing.T) {
	db, h := newTestServer(t)
	ctx := context.Background()
	ut, _ := usertoken.Create(ctx, db, "ivan")
	cookie := loginCookie(t, h)

	users := func() []userView {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/users", "", cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("users = %d: %s", w.Code, w.Body.String())
		}
		var out []userView
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return out
	}
	if got := users(); len(got) != 1 || !got[0].SpreadSubagents {
		t.Fatalf("default spread_subagents should be true: %+v", got)
	}

	w := do(t, h, http.MethodPost, "/api/users/"+ut.ID+"/subagents", `{"spread":false}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("subagents = %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		OK              bool `json:"ok"`
		SpreadSubagents bool `json:"spread_subagents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || !res.OK || res.SpreadSubagents {
		t.Fatalf("subagents response = %s", w.Body.String())
	}
	if got := users(); got[0].SpreadSubagents {
		t.Fatal("spread_subagents off not reflected in users list")
	}
	if reloaded, err := usertoken.Get(ctx, db, ut.ID); err != nil || reloaded.SpreadSubagents {
		t.Fatalf("usertoken.Get spread_subagents = %v (err %v)", reloaded, err)
	}

	do(t, h, http.MethodPost, "/api/users/"+ut.ID+"/subagents", `{"spread":true}`, cookie)
	if got := users(); !got[0].SpreadSubagents {
		t.Fatal("spread_subagents not turned back on")
	}
	if w := do(t, h, http.MethodPost, "/api/users/nope/subagents", `{"spread":true}`, cookie); w.Code != http.StatusNotFound {
		t.Fatalf("unknown user subagents = %d, want 404", w.Code)
	}
}
```

Append to `cmd/claude-proxy/users_test.go`:
```go
func TestUsersSubagents(t *testing.T) {
	ctx := context.Background()
	path, db := cliDB(t)
	ut, err := usertoken.Create(ctx, db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	list := func() []string {
		t.Helper()
		var out bytes.Buffer
		if err := printUsers(ctx, db, &out); err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(out.String()), "\n")
	}
	if lines := list(); !strings.Contains(lines[0], "SUBAGENTS") || !strings.Contains(lines[1], " spread ") {
		t.Fatalf("list output:\n%s", strings.Join(lines, "\n"))
	}

	var stdout, stderr bytes.Buffer
	if code := usersSubagents(ctx, []string{ut.ID, "off", "--db", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("off: exit %d, stderr %q", code, stderr.String())
	}
	if got, _ := usertoken.Get(ctx, db, ut.ID); got.SpreadSubagents {
		t.Fatal("off not persisted")
	}
	if lines := list(); !strings.Contains(lines[1], " share ") {
		t.Fatalf("row after off: %q", lines[1])
	}
	if code := usersSubagents(ctx, []string{ut.ID, "on", "--db", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("on: exit %d, stderr %q", code, stderr.String())
	}
	if got, _ := usertoken.Get(ctx, db, ut.ID); !got.SpreadSubagents {
		t.Fatal("on not persisted")
	}

	stderr.Reset()
	if code := usersSubagents(ctx, []string{"utok_missing", "on", "--db", path}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "not found") {
		t.Fatalf("unknown id: exit %d, stderr %q", code, stderr.String())
	}
	if code := usersSubagents(ctx, []string{ut.ID, "maybe", "--db", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("bad value: exit %d", code)
	}
	if code := usersSubagents(ctx, []string{ut.ID}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing value: exit %d", code)
	}
}
```

Run: `go test -race -run TestUserSubagentsToggle ./internal/webui/; go test -race -run TestUsersSubagents ./cmd/claude-proxy/`
Expected: `internal/webui/webui_test.go:1222:46: got[0].SpreadSubagents undefined (type userView has no field or method SpreadSubagents)` with `FAIL	github.com/p4u/claude-proxy/internal/webui [build failed]`, and `cmd/claude-proxy/users_test.go:97:13: undefined: usersSubagents` with `FAIL	github.com/p4u/claude-proxy/cmd/claude-proxy [build failed]`

Step 2: implement
```diff
diff --git a/README.md b/README.md
index 43bd461..9f34fda 100644
--- a/README.md
+++ b/README.md
@@ -1358,6 +1358,32 @@ Only requests whose *last* message opens with the marker match, so quoting the
 suggestion prompt inside a real question is unaffected. If Anthropic renames the
 marker the match stops firing and requests are forwarded as before.
 
+### Subagents on their own subscriptions (per user, on by default)
+
+Claude Code sends every request of a subagent (the Agent tool, workflows) with
+the session's `metadata.user_id`, so without help all of a session's subagents
+run on the main conversation's subscription. The proxy reads the
+`X-Claude-Code-Agent-Id` header Claude Code adds to subagent requests and gives
+each subagent its own sticky binding. Its first binding prefers an Anthropic
+subscription the session is not already using, for the main conversation or
+another live subagent, as long as one has room; with more subagents than
+subscriptions they share. Forks and subagents already running keep the
+session's subscription and its prompt cache.
+
+Each user token carries a `spread_subagents` flag, **on** by default (also for
+the admin token and for deployments without auth). Only Anthropic models and
+subscriptions are affected.
+
+```bash
+claude-proxy users subagents utok_xxx off   # subagents share the session's subscription
+claude-proxy users subagents utok_xxx on    # back to the default
+claude-proxy users list                     # SUBAGENTS column: spread | share
+```
+
+Or flip the **Subagents** switch on the Users page. A subagent shows as its own
+conversation in stats and captured transcripts, and its `bind` log line carries
+`agent` and `family` fields.
+
 ## Operational gotchas
 
 - **Don't reuse a `credentials.json` after import** — see [Generating credentials](#generating-credentials-do-this-once-per-account) for the full explanation. Short version: the proxy owns the token chain after import; any other consumer of the same file causes immediate revocation.
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index 4f52687..700b80b 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -47,6 +47,7 @@ Usage:
   claude-proxy users rm <id>          [--db PATH]
   claude-proxy users refresh <id>     [--db PATH]
   claude-proxy users limit <id>       --tokens 1M --window 24h | --none [--db PATH]
+  claude-proxy users subagents <id> on|off [--db PATH]   # subagents on their own subscriptions (default on)
   claude-proxy plugins list|set|enable|disable|delete|env …  [--db PATH]
                                    # plugins served to claudio; 'plugins' alone for help
   claude-proxy tui                 [--db PATH]   # interactive management UI
@@ -993,7 +994,7 @@ func printUsageBucket(label string, b *usage.Bucket) {
 
 func runUsers(args []string) {
 	if len(args) == 0 {
-		fmt.Fprintln(os.Stderr, "users: missing subcommand (create|list|email|stats|token|disable|enable|rm|refresh|limit|suggestions)")
+		fmt.Fprintln(os.Stderr, "users: missing subcommand (create|list|email|stats|token|disable|enable|rm|refresh|limit|suggestions|subagents)")
 		os.Exit(2)
 	}
 	ctx := context.Background()
@@ -1022,6 +1023,10 @@ func runUsers(args []string) {
 		usersLimit(ctx, args[1:])
 	case "suggestions":
 		usersSuggestions(ctx, args[1:])
+	case "subagents":
+		if code := usersSubagents(ctx, args[1:], os.Stdout, os.Stderr); code != 0 {
+			os.Exit(code)
+		}
 	default:
 		fmt.Fprintf(os.Stderr, "users: unknown subcommand %q\n", args[0])
 		os.Exit(2)
@@ -1106,8 +1111,8 @@ func printUsers(ctx context.Context, db *store.DB, w io.Writer) error {
 		return nil
 	}
 	now := time.Now()
-	fmt.Fprintf(w, "%-22s  %-20s  %-28s  %-8s  %-25s  %-25s  %-6s  %-16s  %s\n",
-		"ID", "NAME", "EMAIL", "STATUS", "CREATED", "LAST_USED", "SUGG", "LIMIT (OUT TOK)", "USED (OUT TOK)")
+	fmt.Fprintf(w, "%-22s  %-20s  %-28s  %-8s  %-25s  %-25s  %-6s  %-9s  %-16s  %s\n",
+		"ID", "NAME", "EMAIL", "STATUS", "CREATED", "LAST_USED", "SUGG", "SUBAGENTS", "LIMIT (OUT TOK)", "USED (OUT TOK)")
 	for _, ut := range list {
 		lastUsed := "-"
 		if ut.LastUsedAt != nil {
@@ -1134,13 +1139,19 @@ func printUsers(ctx context.Context, db *store.DB, w io.Writer) error {
 		if ut.BlockSuggestions {
 			sugg = "block"
 		}
+		// SUBAGENTS reads "spread" when each subagent gets its own binding,
+		// "share" when subagents stay on the session's subscription.
+		agents := "share"
+		if ut.SpreadSubagents {
+			agents = "spread"
+		}
 		email := ut.Email
 		if email == "" {
 			email = "-"
 		}
-		fmt.Fprintf(w, "%-22s  %-20s  %-28s  %-8s  %-25s  %-25s  %-6s  %-16s  %s\n",
+		fmt.Fprintf(w, "%-22s  %-20s  %-28s  %-8s  %-25s  %-25s  %-6s  %-9s  %-16s  %s\n",
 			ut.ID, ut.Name, email, string(ut.Status),
-			ut.CreatedAt.Format(time.RFC3339), lastUsed, sugg, limitCol, usageCol)
+			ut.CreatedAt.Format(time.RFC3339), lastUsed, sugg, agents, limitCol, usageCol)
 	}
 	return nil
 }
@@ -1317,6 +1328,49 @@ func usersToken(ctx context.Context, args []string) {
 	fmt.Println(ut.Token)
 }
 
+// usersSubagents turns subagent spreading on or off for a user: on gives each
+// Claude Code subagent its own binding, preferring a subscription its session
+// is not using; off keeps subagents on the session's subscription.
+func usersSubagents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
+	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
+		fmt.Fprintln(stderr, "users subagents <id> on|off [--db PATH]")
+		fmt.Fprintln(stderr, "  on  = each subagent gets its own binding, preferring an unused subscription (default)")
+		fmt.Fprintln(stderr, "  off = subagents share the session's subscription")
+		return 2
+	}
+	var on bool
+	switch strings.ToLower(args[1]) {
+	case "on", "spread", "true", "1":
+		on = true
+	case "off", "share", "false", "0":
+	default:
+		fmt.Fprintf(stderr, "users subagents: expected on|off, got %q\n", args[1])
+		return 2
+	}
+	fs := flag.NewFlagSet("users subagents", flag.ContinueOnError)
+	fs.SetOutput(stderr)
+	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
+	if err := fs.Parse(args[2:]); err != nil {
+		return 2
+	}
+	db, err := store.Open(*dbPath)
+	if err != nil {
+		fmt.Fprintf(stderr, "open db %s: %v\n", *dbPath, err)
+		return 1
+	}
+	defer db.Close()
+	if err := usertoken.SetSpreadSubagents(ctx, db, args[0], on); err != nil {
+		fmt.Fprintf(stderr, "subagents: %v\n", err)
+		return 1
+	}
+	if on {
+		fmt.Fprintln(stdout, "subagents get their own subscriptions for", args[0])
+	} else {
+		fmt.Fprintln(stdout, "subagents share the session's subscription for", args[0])
+	}
+	return 0
+}
+
 // usersSuggestions toggles whether the proxy answers Claude Code's
 // prompt-suggestion requests locally instead of forwarding them upstream.
 func usersSuggestions(ctx context.Context, args []string) {
diff --git a/docs/WEBUI.md b/docs/WEBUI.md
index 0aa0d69..77d7a20 100644
--- a/docs/WEBUI.md
+++ b/docs/WEBUI.md
@@ -818,3 +818,51 @@ response is an empty completion matching the request's `stream` flag (SSE or
 JSON), reporting zero tokens. The attempt is logged to `request_log` with an
 empty `credential_id`/`conv_id` and zero tokens, and carries
 `X-Router-Reason: suggestion-blocked`.
+
+## v6 additions — subagents on their own subscriptions (2026-11-14)
+
+### Schema
+
+`user_tokens` gains `spread_subagents INTEGER NOT NULL DEFAULT 1` (in
+`CREATE TABLE` and the additive ALTER list). The default is what turns it on
+for every existing user.
+
+### Model
+
+`usertoken.UserToken` and `usertoken.Identity` gain `SpreadSubagents bool`;
+`usertoken.SetSpreadSubagents(ctx, db, id, on)` writes it (`ErrNotFound` for an
+unknown id). `usertoken.SpreadSubagentsFor(id)` is its only reader on the
+request path: true for no identity (no auth configured) and the admin token,
+else the user's flag. `auth.go` copies the flag into the request identity.
+
+### API
+
+- `GET /api/users` — each entry gains `"spread_subagents": bool`.
+- `POST /api/users/{id}/subagents` `{"spread": bool}` ->
+  `{"ok":true,"spread_subagents":bool}`. 404 for an unknown user, mirroring
+  `/suggestions`.
+
+### Frontend
+
+- Users table gains a **Subagents** column after *Block suggestions*, built
+  with `switchToggle` (`subagentsToggle`), and `api.setUserSubagents`.
+- The column header explains it: on, each subagent Claude Code launches gets
+  its own binding, preferring a subscription the session is not using while
+  one has room; off, subagents share the session's subscription; Anthropic
+  subscriptions only.
+- Mock backend: `spread_subagents` on all four fixture users (ci-runner off,
+  so both states render), in the `/users` list, plus a stateful
+  `POST /users/{id}/subagents` route.
+
+### Proxy behaviour
+
+A sticky Anthropic request (no custom host) keyed by `metadata.user_id` that
+carries a valid `X-Claude-Code-Agent-Id` (1-128 of `[0-9A-Za-z_-]`), from a
+caller with the switch on, is a subagent candidate. The pool keys it
+`<session key>/a_<agent id>` when that binding exists or the request may start
+it (portable, no assistant turn), and the first binding avoids the session's
+own credential and those of its subagents seen in the last 15 minutes while
+another subscription keeps headroom ≥ 0.25. Such a request is logged under the
+child key in `request_log`, `prompt_log` and `conversation_message`, does not
+update the `/v1/claudio/session` record, and its `bind` log line adds `agent`
+and `family`.
diff --git a/internal/webui/static/js/api.js b/internal/webui/static/js/api.js
index a3c2fc4..0c73b35 100644
--- a/internal/webui/static/js/api.js
+++ b/internal/webui/static/js/api.js
@@ -152,6 +152,11 @@ export const api = {
   setUserSuggestions: (id, block) =>
     request("POST", `/users/${enc(id)}/suggestions`, { block: !!block }),
 
+  // Per-user subagent spreading. `spread` true ⇒ each Claude Code subagent gets
+  // its own binding, preferring a subscription its session is not using.
+  setUserSubagents: (id, spread) =>
+    request("POST", `/users/${enc(id)}/subagents`, { spread: !!spread }),
+
   // Per-user rolling usage limit, in output tokens. Both values 0 clears the
   // limit; the backend rejects a negative value, or one set and the other zero,
   // with a 400 whose message is meant to be shown to the operator verbatim.
diff --git a/internal/webui/static/js/mock.js b/internal/webui/static/js/mock.js
index 1194139..83571a9 100644
--- a/internal/webui/static/js/mock.js
+++ b/internal/webui/static/js/mock.js
@@ -107,13 +107,13 @@ const pluginView = (p) => ({
 });
 
 const USERS = [
-  { id: "utok_alice", name: "alice", email: "alice@vocdoni.org", status: "active", full_capture: true, block_suggestions: false,
+  { id: "utok_alice", name: "alice", email: "alice@vocdoni.org", status: "active", full_capture: true, block_suggestions: false, spread_subagents: true,
     limit_output_tokens: 1_000_000, limit_window_seconds: 86400, usage_output_tokens: 302_400 },  // ~30%
-  { id: "utok_bob", name: "bob", status: "active", full_capture: false, block_suggestions: true,
+  { id: "utok_bob", name: "bob", status: "active", full_capture: false, block_suggestions: true, spread_subagents: true,
     limit_output_tokens: 400_000, limit_window_seconds: 21600, usage_output_tokens: 344_900 },    // ~86%
-  { id: "utok_carol", name: "carol", status: "disabled", full_capture: false, block_suggestions: false,
+  { id: "utok_carol", name: "carol", status: "disabled", full_capture: false, block_suggestions: false, spread_subagents: true,
     limit_output_tokens: 0, limit_window_seconds: 0, usage_output_tokens: 0 },                     // unlimited
-  { id: "utok_ci", name: "ci-runner", status: "active", full_capture: false, block_suggestions: true,
+  { id: "utok_ci", name: "ci-runner", status: "active", full_capture: false, block_suggestions: true, spread_subagents: false,
     limit_output_tokens: 100_000, limit_window_seconds: 3600, usage_output_tokens: 122_400 },     // ~122%, blocked
 ];
 
@@ -677,7 +677,7 @@ const DB = {
   "/users": () =>
     USERS.map((u, i) => ({
       id: u.id, name: u.name, email: u.email || "", status: u.status, full_capture: u.full_capture,
-      block_suggestions: u.block_suggestions,
+      block_suggestions: u.block_suggestions, spread_subagents: u.spread_subagents,
       ...limitFields(u),
       created_at: now - 86400 * (20 - i * 3),
       last_used_at: u.status === "disabled" ? now - 86400 * 4 : now - 120 * (i + 1),
@@ -1076,6 +1076,15 @@ window.fetch = async (input, init = {}) => {
       return json({ ok: true, block_suggestions: u.block_suggestions });
     }
 
+    // Per-user subagent spreading — stateful for the session.
+    const sa = path.match(/^\/users\/([^/]+)\/subagents$/);
+    if (sa && method === "POST") {
+      const u = userById(decodeURIComponent(sa[1]));
+      if (!u) return json({ error: "unknown user" }, 404);
+      u.spread_subagents = !!JSON.parse(init.body || "{}").spread;
+      return json({ ok: true, spread_subagents: u.spread_subagents });
+    }
+
     // Per-user usage limit — stateful, and validated exactly like the backend:
     // negatives are rejected, and tokens/window must be both set or both zero.
     // (Entering 0 output tokens with a window selected is the reachable path
diff --git a/internal/webui/static/js/pages/users.js b/internal/webui/static/js/pages/users.js
index 373414d..2dff8e4 100644
--- a/internal/webui/static/js/pages/users.js
+++ b/internal/webui/static/js/pages/users.js
@@ -325,6 +325,12 @@ const SUGGESTIONS_HELP =
   "reply: no quota spent, nothing forwarded, and they stop cluttering captured prompts. " +
   "The only visible effect for the user is that typing suggestions stop appearing.";
 
+const SUBAGENTS_HELP =
+  "On (the default), each subagent Claude Code launches gets its own binding, on a " +
+  "subscription the session is not already using while one has room, so parallel " +
+  "subagents spread their load. Off, subagents share the session's subscription. " +
+  "Anthropic subscriptions only.";
+
 const LIMIT_METRIC = "Counts output tokens only — input and cache tokens are ignored.";
 const LIMIT_HELP =
   "Blocks a user once their output tokens over a rolling window exceed the cap. " +
@@ -451,6 +457,10 @@ function buildMgmtTable(users, statById, root) {
         el("span", { text: "Block suggestions" }),
         el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
       ]),
+      el("th", { scope: "col", title: SUBAGENTS_HELP }, [
+        el("span", { text: "Subagents" }),
+        el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
+      ]),
       el("th", { scope: "col", class: "limit-col", title: LIMIT_HELP }, [
         el("span", { text: "Usage limit" }),
         el("span", { class: "th-info", "aria-hidden": "true", text: "?" }),
@@ -534,6 +544,7 @@ function userRow(u, s, root) {
     el("td", {}, statusBadge(u.status)),
     el("td", {}, captureToggle(u)),
     el("td", {}, suggestionsToggle(u)),
+    el("td", {}, subagentsToggle(u)),
     el("td", { class: "limit-col" }, limitCell(u)),
     el("td", { class: "num", text: compactNum(s.requests || 0) }),
     el("td", { class: "num " + errTone, text: fullNum(s.errors || 0) }),
@@ -845,6 +856,28 @@ function suggestionsToggle(u) {
   });
 }
 
+function subagentsToggle(u) {
+  return switchToggle({
+    initial: u.spread_subagents,
+    ariaLabel: `Give ${u.name}'s subagents their own subscriptions`,
+    help: SUBAGENTS_HELP,
+    save: async (want) => {
+      const r = await api.setUserSubagents(u.id, want);
+      const applied =
+        r && typeof r.spread_subagents === "boolean" ? r.spread_subagents : want;
+      u.spread_subagents = applied;
+      return applied;
+    },
+    announce: (on) => [
+      on
+        ? `Subagents spread for ${u.name}: each one prefers a subscription the session is not using.`
+        : `Subagents shared for ${u.name}: they stay on the session's subscription.`,
+      on ? "good" : "warning",
+    ],
+    failure: "Couldn't change subagent handling",
+  });
+}
+
 const PROMPT_LIMIT = 25;
 const CONV_LIMIT = 20;
 const MSG_LIMIT = 20;
diff --git a/internal/webui/users.go b/internal/webui/users.go
index 2f8c186..44011a3 100644
--- a/internal/webui/users.go
+++ b/internal/webui/users.go
@@ -21,6 +21,9 @@ type userView struct {
 	// BlockSuggestions => the proxy answers Claude Code's prompt-suggestion
 	// requests locally instead of spending quota on them.
 	BlockSuggestions bool `json:"block_suggestions"`
+	// SpreadSubagents => each Claude Code subagent gets its own binding,
+	// preferring a subscription its session is not using.
+	SpreadSubagents bool `json:"spread_subagents"`
 
 	// Usage limit (v4). 0/0 => unlimited, in which case usage_output_tokens
 	// and usage_pct are 0 and blocked_until is null.
@@ -94,6 +97,9 @@ func (s *Server) userAction(w http.ResponseWriter, r *http.Request, id, action s
 	case action == "suggestions" && r.Method == http.MethodPost:
 		s.setUserSuggestions(w, r, id)
 		return
+	case action == "subagents" && r.Method == http.MethodPost:
+		s.setUserSubagents(w, r, id)
+		return
 	case action == "limit" && r.Method == http.MethodPost:
 		s.setUserLimit(w, r, id)
 		return
@@ -138,6 +144,7 @@ func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
 			CreatedAt:          ut.CreatedAt.Format(time.RFC3339),
 			FullCapture:        ut.FullCapture,
 			BlockSuggestions:   ut.BlockSuggestions,
+			SpreadSubagents:    ut.SpreadSubagents,
 			LimitOutputTokens:  ut.LimitOutputTokens,
 			LimitWindowSeconds: ut.LimitWindowSeconds,
 		}
@@ -203,6 +210,22 @@ func (s *Server) setUserSuggestions(w http.ResponseWriter, r *http.Request, id s
 	writeJSON(w, map[string]any{"ok": true, "block_suggestions": body.Block})
 }
 
+// setUserSubagents turns subagent spreading on or off for a user.
+func (s *Server) setUserSubagents(w http.ResponseWriter, r *http.Request, id string) {
+	var body struct {
+		Spread bool `json:"spread"`
+	}
+	if err := decodeJSON(w, r, &body); err != nil {
+		writeErr(w, http.StatusBadRequest, err.Error())
+		return
+	}
+	if err := usertoken.SetSpreadSubagents(r.Context(), s.db, id, body.Spread); err != nil {
+		s.userErr(w, err)
+		return
+	}
+	writeJSON(w, map[string]any{"ok": true, "spread_subagents": body.Spread})
+}
+
 // setUserLimit configures or clears a user's rolling-window usage cap.
 // Both fields zero clears the limit; one zero and one non-zero is rejected,
 // since a limit is meaningless without both halves.
```

Run: `go test -race -v -run TestUserSubagentsToggle ./internal/webui/ && go test -race -v -run 'TestUsersSubagents|TestUsersListShowsEmail' ./cmd/claude-proxy/ && go test -race ./internal/webui/ ./cmd/... && node --check internal/webui/static/js/api.js && node --check internal/webui/static/js/pages/users.js && node --check internal/webui/static/js/mock.js && make lint`
Expected: `--- PASS: TestUserSubagentsToggle`, `--- PASS: TestUsersListShowsEmail`, `--- PASS: TestUsersSubagents`, `ok` for `internal/webui` and `cmd/claude-proxy`, nothing from `node --check`, then `0 issues.`

Step 3: commit
`git commit -m "feat(webui): subagents switch in the users table, admin api and cli" -- internal/webui/users.go internal/webui/webui_test.go internal/webui/static/js/api.js internal/webui/static/js/pages/users.js internal/webui/static/js/mock.js cmd/claude-proxy/main.go cmd/claude-proxy/users_test.go docs/WEBUI.md README.md`

Interfaces this task provides to later tasks: `POST /api/users/{id}/subagents` `{"spread": bool}` → `{"ok":true,"spread_subagents":bool}` (404 for an unknown id); `"spread_subagents"` in `GET /api/users`; `api.setUserSubagents(id, spread)`; `claude-proxy users subagents <id> on|off [--db PATH]`.

## Merge and release
1. Full suites in the worktree (shell set up per Global Constraints): `make test`, `make lint` and `go build ./...`. Expected: `go test -race ./...` with an `ok` line for every package that has tests (18) and no `FAIL`, then `0 issues.`, and no output from `go build`. This is AC10.
2. Optional UI check: from the worktree, run `python3 -m http.server -d internal/webui/static 8099` and open `http://127.0.0.1:8099/?mock=1#/users`. Confirm that the Users table shows the **Subagents** column (ci-runner off, the rest on) and that toggling it shows a toast. AGENTS.md asks for screenshots of UI changes.
3. Merge (controller), in the main checkout on main: `git -C /home/p4u/claude-proxy merge --no-ff feat/subagent-spread -m "feat(proxy): subagents on their own subscriptions"`. The branch never touches CLAUDE.md, so the owner's uncommitted CLAUDE.md edit does not block the merge.
4. CLAUDE.md docs. Commit the `claude-md` block at the end of this section through the index only, so that the owner's uncommitted edit stays unstaged:
   ```sh
   cd /home/p4u/claude-proxy
   sed -n '/^```diff claude-md$/,/^```$/p' docs/plans/2026-11-14-subagent-spread-plan.md | sed '1d;$d' > ~/.cache/claude-proxy/claude-md.patch
   git apply --cached --check ~/.cache/claude-proxy/claude-md.patch && git apply --check ~/.cache/claude-proxy/claude-md.patch
   git apply --cached ~/.cache/claude-proxy/claude-md.patch   # index: HEAD + this hunk
   git apply ~/.cache/claude-proxy/claude-md.patch            # working copy: owner's edit + this hunk
   git diff --cached --name-only                             # expect exactly: CLAUDE.md
   git diff --cached --stat                                  # expect: 1 file changed, 42 insertions(+), 3 deletions(-)
   git commit -m "docs(claude): subagents on their own subscriptions"   # no pathspec: commits the index
   git diff --stat CLAUDE.md                                 # expect the owner's edit alone: 1 file changed, 11 insertions(+), 1 deletion(-)
   ```
   This commit is the one exception to "commit with explicit paths". `git commit -- CLAUDE.md` would commit the working copy, which includes the owner's edit. Both `--check` runs were verified against HEAD's CLAUDE.md and against the owner's current working copy (the fourth hunk lands 10 lines lower there). The patch leaves alone the schema "Tables" paragraph (CLAUDE.md:784), which lists `user_tokens` columns, because it sits within the context of the owner's hunk at :780. The new section documents the column instead.
5. **Owner OK required before:**
   - pushing main, which builds and publishes the `:latest` GHCR image the deployment may follow;
   - creating any `v*` tag, which publishes a versioned image (the repo has no tags yet).
6. Post-deploy check (owner, AC11): in a Claude Code session that launches two parallel subagents, the proxy log shows three `bind` lines with distinct `cred` values. The two subagent lines carry `agent` and the same `family` as the main line's `conv`. Then confirm that `GET /v1/claudio/session` for that session still names the main conversation's credential.
7. Cleanup (controller): after the merge, `git -C /home/p4u/claude-proxy worktree remove ~/.cache/claude-proxy/wt/subagent-spread`.

### CLAUDE.md hunk (step 4)
This covers the request flow, conversation key derivation, the mock toggles, and a new section after "Blocking prompt-suggestion traffic". It applies at column 0, exactly as below.

```diff claude-md
diff --git a/CLAUDE.md b/CLAUDE.md
index d467ea8..fcbc362 100644
--- a/CLAUDE.md
+++ b/CLAUDE.md
@@ -54,9 +54,10 @@ Appending **`?mock=1`** to the UI URL makes `index.html` import
 `static/js/mock.js`, which intercepts `fetch` for `/api/*` and serves realistic
 sample credentials, users, and usage series. That is the fast path for frontend
 work: no proxy, no database, no real subscriptions. The mock keeps per-session
-state for the toggles it exposes (`full_capture`, `block_suggestions`, the usage
-limit) so switch behaviour can be exercised end-to-end. When adding an endpoint
-to `/api`, add it to `mock.js` too or the UI silently loses its offline mode.
+state for the toggles it exposes (`full_capture`, `block_suggestions`,
+`spread_subagents`, the usage limit) so switch behaviour can be exercised
+end-to-end. When adding an endpoint to `/api`, add it to `mock.js` too or the UI
+silently loses its offline mode.
 
 ## Architecture
 
@@ -75,6 +76,10 @@ Claude Code (ANTHROPIC_BASE_URL → proxy)
       → enforceUserLimit(): POST /v1/messages only — 429 + X-Router-Reason: user-quota
         when the user is over their rolling usage cap (internal/proxy/userlimit.go)
       → router.Derive(): compute stable conversation key (4-priority algorithm)
+      → subagentChild(): a subagent candidate (Anthropic, keyed by metadata.user_id,
+        valid X-Claude-Code-Agent-Id, user's spread_subagents on) passes
+        RequestOptions{Family, Child, StartChild}; the pool picks the key
+        (internal/proxy/subagent.go, internal/pool/subagent.go)
       → pool.AcquireScoped(): get/create the sticky binding + request lease;
         emergency rebind for unusable or saturated pins; when enabled, announce
         and later revalidate a proactive rebalance of long-lived Anthropic pins
@@ -629,6 +634,10 @@ Before this, a key added seconds after a refresh stayed invisible for 5 minutes.
 3. SHA256(system_prompt + first_user_message)
 4. SHA256(remote_addr + body[:4096]) — fallback
 
+A subagent request keyed by `metadata.user_id` can bind a child key under the
+session key, `router.SubagentKey(parent, agentID)` = `parent + "/a_" +
+shorten(agentID)`; see [Subagents on their own subscriptions](#subagents-on-their-own-subscriptions-internalpoolsubagentgo).
+
 ### Package Responsibilities
 
 | Package | Role |
@@ -1079,6 +1088,36 @@ body still matches; a **renamed** marker stops matching and requests are
 forwarded exactly as before. That is the intended failure direction — a miss
 costs one request, a false positive would silently swallow a real prompt.
 
+## Subagents on their own subscriptions (`internal/pool/subagent.go`)
+
+Claude Code sends `X-Claude-Code-Agent-Id` on every request a subagent makes,
+with the session's `metadata.user_id`. A request is a **subagent candidate**
+(`proxy.subagentChild`) when it is sticky, Anthropic with no custom host, keyed
+by `metadata.user_id`, carries a valid agent id (1-128 of `[0-9A-Za-z_-]`,
+`router.ValidAgentID`) and `usertoken.SpreadSubagentsFor` is true (no identity
+and the admin token: always; a user token: `user_tokens.spread_subagents`,
+default 1). The proxy still passes the session key; `AcquireScoped` resolves
+the key under `p.mu` (`subagentKey`) and reports it in `Lease.ConvID`:
+
+- the child row exists: the child key (`account_bound` latches on it as on any
+  binding, so the subagent never leaves its account);
+- no child row and `StartChild` (portable and no `assistant` message): the
+  child key, created with the anti-affinity pick;
+- otherwise the session key. Forks and subagents running before their child
+  row existed carry assistant turns and stay on the session's binding.
+
+A new child row excludes the **family's credentials** (`familyQuery`: the
+session row at any age, plus child rows seen in the last 15 minutes, a
+primary-key range `id >= F||'/' AND id < F||'0'` bound as Go strings) while
+another candidate keeps headroom `Room(5h) × Room(7d)^1.5 ≥ 0.25`; candidates
+below the floor are dropped, and with none left the ordinary pick runs. A child
+binding on a `limited` credential (retry_after in the future, not account-bound)
+moves to an active credential on its next request; a main binding keeps waiting
+for claudio's switch. Spread requests skip `Sessions.RecordRequest`, so
+`/v1/claudio/session` keeps describing the main conversation. `request_log`,
+`prompt_log` and `conversation_message` record the child key, and the `bind`
+line adds `agent` and `family`. Rows are pruned only with their credential.
+
 ## Admin API
 
 `AuthMiddleware` accepts either the admin `PROXY_AUTH_TOKEN` or any active per-user token; `/admin/*` routes require the admin token (or no token configured). Identity is carried in request context (`usertoken.Identity`).
```
