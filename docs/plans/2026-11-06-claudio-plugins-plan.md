# Claudio plugins and browser login: implementation plan

Spec: docs/specs/2026-11-06-claudio-plugins-design.md (approved 2026-11-06); consumer contract: claudio/docs/specs/2026-11-06-session-plugins-design.md
Class: architectural
Repos and branches: /volumes/repos/claude-proxy → feat/claudio-plugins (base e0c50d9)
Scratch (all eight tasks committed there): ~/.cache/claude-proxy/scratch/claudio-plugins, branch feat/claudio-plugins-scratch (6b51552…5f5c481)
Patches: .delivery/2026-11-06-claudio-plugins/patches/task-<N>.patch, one per task, `git am`-able in order on e0c50d9

Baseline at e0c50d9: `go test -race -count=1 ./...` green, 1002 test results passed (475 top-level + 527 subtests), 0 failed, 20 packages. `make lint` with the installed golangci-lint v2.12.2 panics under the host Go 1.27.3 (`file requires newer Go version go1.27`; it was built with 1.26 and cannot read 1.27 export data); `GOTOOLCHAIN=go1.26.2 make lint` (go.mod's version, already in the module cache) gives `0 issues.` at baseline and after every task. Use that form.
After task 8: 1081 passed (524 + 557), 0 failed, 20 packages ok (the new internal/plugins included).

## Global Constraints
- Commit convention: conventional commits `type(scope): lowercase summary`, no AI attribution, no Co-Authored-By or similar trailers.
- Never push; commit with explicit paths only (`git commit -m "…" -- <paths>`, new files `git add <path>` first); never `git add -A`.
- Build output under the worktree or ~/.cache/claude-proxy/, never /tmp. Lint as `GOTOOLCHAIN=go1.26.2 make lint` (see header).
- Do not start the proxy against production or any real OAuth provider; tests use httptest, the fake OP in `internal/webui/sso_test.go` and temp SQLite files.
- Everything under `/v1/claudio` stays local: never forwarded upstream, never written to `request_log` (CLAUDE.md "Claudio API"). The login routes never touch `request_log` and never forward.
- Secrets never in logs, argv of child processes, or the web UI after save. Plugin env values leave the proxy only through `GET /v1/claudio/plugins`; device codes are held only as SHA-256; tokens only appear in the poll 200 (and the existing admin token routes).
- No new external dependencies; reuse `vocauth`, the existing limiter and store patterns.
- Keep `golangci-lint` clean (`make lint`).
- Unchanged behaviour: deployments without `OAUTH_CLIENT_ID` (password UI) work as before and both login routes are 404; deployments that set `OAUTH_ALLOWED_EMAILS` keep the same administrators; with it empty every signed-in account stays an administrator (warned at start). Every existing `/v1/*` auth rule is unchanged except the two exact login paths.
- Schema changes are additive (two tables, one column, one partial unique index) so the previous image still runs on a migrated DB.

## Review Focus
- AuthMiddleware exemption: `internal/proxy/auth.go:40` (exact `EscapedPath` match, no identity, bearer ignored) and the matching exact dispatch before the identity bucket at `internal/claudioapi/claudioapi.go:192`.
- Login state machine, `internal/claudioapi/login.go`: `start` :127 (per-IP pending cap, eviction at :160), `tooSoon` :177 (slow_down before the IP budget), `poll` :204 (approved → redeeming exactly once), `settle` :237, `decidable` :255 (decided once, expiry), `Sweep` :303, `serveLoginPoll` :388 (status re-check at redemption, token never rotated, no-store).
- SSO admin/member split: `internal/webui/sso.go:110` (vocauth gets `AllowedDomains` only), `isAdmin` :126, `ssoAuthorize` :219-:225; startup warning `cmd/claude-proxy/main.go:335`.
- Plugins endpoint auth: `internal/claudioapi/claudioapi.go:538 servePlugins` (403 for admin/anonymous, no-store, log without values); `internal/plugins/plugins.go` is the only place values are read.
- Login page CSRF and headers: `internal/webui/claudiologin.go:59 loginPostSameOrigin` (Origin null only with Sec-Fetch-Site same-origin), `:68 setClaudioLoginHeaders`, `:77 serveClaudioLogin` (Require for GET and POST), `:107 claudioLoginPost` (Check before create, domain re-check), `:168 memberToken` (create + default limit, delete on failure); wiring `cmd/claude-proxy/main.go:309`.
- `usertoken.CreateForEmail` `internal/usertoken/usertoken.go:122` and `isUniqueOn` :185 (name vs e-mail UNIQUE violations).

## Task order and parallelism
The patches are cut in this order and apply cleanly only in this order; run the tasks sequentially unless the column says otherwise.

| Task | Depends on | Repo/files | May run in parallel with |
|---|---|---|---|
| 1. E-mail on user tokens (store, usertoken, users CLI, /api/users) | — | `cmd/claude-proxy/main.go`, `cmd/claude-proxy/users_test.go`, `internal/store/store.go`, `internal/store/store_test.go`, `internal/usertoken/usertoken.go`, `internal/usertoken/usertoken_test.go`, `internal/webui/static/js/mock.js`, `internal/webui/static/js/pages/users.js`, `internal/webui/useremail_test.go`, `internal/webui/users.go` | — (shares main.go with 2, store_test.go with 3) |
| 2. SSO split: administrators vs login domains | — | `.env.example`, `CLAUDE.md`, `README.md`, `cmd/claude-proxy/main.go`, `cmd/claude-proxy/sso_test.go`, `docs/WEBUI.md`, `internal/webui/sso.go`, `internal/webui/sso_test.go` | 3 |
| 3. internal/plugins package and tables | — | `internal/plugins/plugins.go`, `internal/plugins/plugins_test.go`, `internal/store/schema.go`, `internal/store/store_test.go` | 2 |
| 4. GET /v1/claudio/plugins | 3 | `internal/claudioapi/claudioapi.go`, `internal/claudioapi/claudioapi_test.go`, `internal/claudioapi/plugins_test.go` | — (shares claudioapi.go with 5) |
| 5. Device login state machine, routes and the AuthMiddleware exemption | 1 | `internal/claudioapi/claudioapi.go`, `internal/claudioapi/login.go`, `internal/claudioapi/login_stack_test.go`, `internal/claudioapi/login_test.go`, `internal/proxy/auth.go`, `internal/proxy/auth_test.go` | — (shares claudioapi.go with 4) |
| 6. Browser approval page /claudio/login and serve wiring | 1, 2, 5 | `.env.example`, `cmd/claude-proxy/limit_test.go`, `cmd/claude-proxy/main.go`, `internal/webui/claudiologin.go`, `internal/webui/claudiologin_test.go`, `internal/webui/webui.go` | — |
| 7. Plugins CLI, /api/plugins and the Plugins page | 3, 6 (main.go, webui.go) | `cmd/claude-proxy/main.go`, `cmd/claude-proxy/plugins.go`, `cmd/claude-proxy/plugins_test.go`, `docs/WEBUI.md`, `internal/webui/plugins.go`, `internal/webui/plugins_test.go`, `internal/webui/static/css/plugins.css`, `internal/webui/static/index.html`, `internal/webui/static/js/api.js`, `internal/webui/static/js/app.js`, `internal/webui/static/js/mock.js`, `internal/webui/static/js/pages/plugins.js`, `internal/webui/webui.go` | — |
| 8. Docs: README section and CLAUDE.md Claudio API | 1–7 | `CLAUDE.md`, `README.md` | — |

## Tasks

### Task 1: E-mail on user tokens (store, usertoken, users CLI, /api/users)
Model: standard   Repo: /volumes/repos/claude-proxy   Files: cmd/claude-proxy/main.go, cmd/claude-proxy/users_test.go, internal/store/store.go, internal/store/store_test.go, internal/usertoken/usertoken.go, internal/usertoken/usertoken_test.go, internal/webui/static/js/mock.js, internal/webui/static/js/pages/users.js, internal/webui/useremail_test.go, internal/webui/users.go
Goal: Spec §1: `user_tokens.email` + partial unique index, `CreateForEmail`/`LookupByEmail`/`SetEmail`, `users create --email`, `users email`, e-mail in `users list` and `/api/users` (AC4, AC6 users part).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-1.patch

Notes: The ALTER list runs `ADD COLUMN email` then `CREATE UNIQUE INDEX IF NOT EXISTS … WHERE email IS NOT NULL` (order matters for old files). `isUniqueOn` tells the name and e-mail UNIQUE violations apart by SQLite's message (`UNIQUE constraint failed: user_tokens.<col>`). The Users page shows the e-mail under the name; `mock.js` carries it.

Step 1: write the failing test
````diff
diff --git a/cmd/claude-proxy/users_test.go b/cmd/claude-proxy/users_test.go
new file mode 100644
index 0000000..ebf889f
--- /dev/null
+++ b/cmd/claude-proxy/users_test.go
@@ -0,0 +1,75 @@
+package main
+
+import (
+	"bytes"
+	"context"
+	"path/filepath"
+	"strings"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/store"
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+func cliDB(t *testing.T) (string, *store.DB) {
+	t.Helper()
+	path := filepath.Join(t.TempDir(), "cli.db")
+	db, err := store.Open(path)
+	if err != nil {
+		t.Fatal(err)
+	}
+	t.Cleanup(func() { _ = db.Close() })
+	return path, db
+}
+
+func TestUsersListShowsEmail(t *testing.T) {
+	ctx := context.Background()
+	_, db := cliDB(t)
+	if _, err := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org"); err != nil {
+		t.Fatal(err)
+	}
+	if _, err := usertoken.Create(ctx, db, "ci"); err != nil {
+		t.Fatal(err)
+	}
+	var out bytes.Buffer
+	if err := printUsers(ctx, db, &out); err != nil {
+		t.Fatal(err)
+	}
+	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
+	if len(lines) != 3 || !strings.Contains(lines[0], "EMAIL") {
+		t.Fatalf("list output:\n%s", out.String())
+	}
+	if !strings.Contains(lines[1], "alice@vocdoni.org") {
+		t.Fatalf("alice row has no e-mail: %q", lines[1])
+	}
+	if !strings.Contains(lines[2], " - ") {
+		t.Fatalf("ci row should show '-' for no e-mail: %q", lines[2])
+	}
+}
+
+func TestUsersEmailRefusesDuplicate(t *testing.T) {
+	ctx := context.Background()
+	path, db := cliDB(t)
+	alice, _ := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org")
+	bob, _ := usertoken.Create(ctx, db, "bob")
+
+	var stdout, stderr bytes.Buffer
+	if code := usersEmail(ctx, []string{bob.ID, "alice@vocdoni.org", "--db", path}, &stdout, &stderr); code != 1 {
+		t.Fatalf("duplicate: exit %d, stderr %q", code, stderr.String())
+	}
+	if !strings.Contains(stderr.String(), "another user token") {
+		t.Fatalf("stderr = %q", stderr.String())
+	}
+	stdout.Reset()
+	stderr.Reset()
+	if code := usersEmail(ctx, []string{bob.ID, "Bob@Vocdoni.org", "--db", path}, &stdout, &stderr); code != 0 {
+		t.Fatalf("set: exit %d, stderr %q", code, stderr.String())
+	}
+	got, _ := usertoken.Get(ctx, db, bob.ID)
+	if got.Email != "bob@vocdoni.org" {
+		t.Fatalf("e-mail = %q", got.Email)
+	}
+	if code := usersEmail(ctx, []string{alice.ID}, &stdout, &stderr); code != 2 {
+		t.Fatalf("missing argument: exit %d", code)
+	}
+}
diff --git a/internal/store/store_test.go b/internal/store/store_test.go
index ba51bcf..a624936 100644
--- a/internal/store/store_test.go
+++ b/internal/store/store_test.go
@@ -81,3 +81,35 @@ func TestForeignKeysEnabled(t *testing.T) {
 		t.Fatalf("foreign_keys = %d, want 1", on)
 	}
 }
+
+func TestUserTokenEmailUniqueWhenSet(t *testing.T) {
+	path := filepath.Join(t.TempDir(), "email.db")
+	db, err := Open(path)
+	if err != nil {
+		t.Fatal(err)
+	}
+	insert := func(id, name string, email any) error {
+		_, err := db.Exec(`INSERT INTO user_tokens (id, name, token, created_at, email) VALUES (?, ?, ?, 1, ?)`,
+			id, name, "tok-"+id, email)
+		return err
+	}
+	if err := insert("a", "a", nil); err != nil {
+		t.Fatalf("NULL e-mail: %v", err)
+	}
+	if err := insert("b", "b", nil); err != nil {
+		t.Fatalf("second NULL e-mail must be allowed: %v", err)
+	}
+	if err := insert("c", "c", "x@vocdoni.org"); err != nil {
+		t.Fatalf("first e-mail: %v", err)
+	}
+	if err := insert("d", "d", "x@vocdoni.org"); err == nil {
+		t.Fatal("duplicate e-mail accepted")
+	}
+	_ = db.Close()
+	// The migration is idempotent on an existing file.
+	db, err = Open(path)
+	if err != nil {
+		t.Fatalf("reopen: %v", err)
+	}
+	_ = db.Close()
+}
diff --git a/internal/usertoken/usertoken_test.go b/internal/usertoken/usertoken_test.go
index ff18f6a..68c9529 100644
--- a/internal/usertoken/usertoken_test.go
+++ b/internal/usertoken/usertoken_test.go
@@ -115,3 +115,73 @@ func TestSetStatusRefreshDelete(t *testing.T) {
 		t.Fatalf("delete missing: want ErrNotFound, got %v", err)
 	}
 }
+
+func TestEmailLookupAndSet(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+
+	ut, err := CreateWithEmail(ctx, db, "alice", "  Alice@Vocdoni.org ")
+	if err != nil {
+		t.Fatalf("create with e-mail: %v", err)
+	}
+	if ut.Email != "alice@vocdoni.org" {
+		t.Fatalf("e-mail not normalised: %q", ut.Email)
+	}
+	got, err := LookupByEmail(ctx, db, "ALICE@vocdoni.org")
+	if err != nil || got.ID != ut.ID || got.Email != "alice@vocdoni.org" {
+		t.Fatalf("lookup-by-email: %v %+v", err, got)
+	}
+	if _, err := LookupByEmail(ctx, db, "nobody@vocdoni.org"); err != ErrNotFound {
+		t.Fatalf("want ErrNotFound, got %v", err)
+	}
+
+	bob, _ := Create(ctx, db, "bob")
+	if bob.Email != "" {
+		t.Fatalf("admin-created token has e-mail %q", bob.Email)
+	}
+	if err := SetEmail(ctx, db, bob.ID, "alice@vocdoni.org"); err != ErrEmailTaken {
+		t.Fatalf("duplicate e-mail: want ErrEmailTaken, got %v", err)
+	}
+	if err := SetEmail(ctx, db, bob.ID, "not-an-email"); err == nil {
+		t.Fatal("invalid e-mail accepted")
+	}
+	if err := SetEmail(ctx, db, bob.ID, "Bob@Vocdoni.org"); err != nil {
+		t.Fatalf("set e-mail: %v", err)
+	}
+	if got, _ := Get(ctx, db, bob.ID); got.Email != "bob@vocdoni.org" {
+		t.Fatalf("get after set: %q", got.Email)
+	}
+	if err := SetEmail(ctx, db, "utok_missing", "c@vocdoni.org"); err != ErrNotFound {
+		t.Fatalf("missing id: want ErrNotFound, got %v", err)
+	}
+	if _, err := CreateWithEmail(ctx, db, "alice2", "alice@vocdoni.org"); err != ErrEmailTaken {
+		t.Fatalf("create with taken e-mail: want ErrEmailTaken, got %v", err)
+	}
+}
+
+func TestCreateForEmailSuffixesTakenName(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+
+	// An admin already used the address as a plain name.
+	if _, err := Create(ctx, db, "dave@vocdoni.org"); err != nil {
+		t.Fatal(err)
+	}
+	if _, err := Create(ctx, db, "dave@vocdoni.org-2"); err != nil {
+		t.Fatal(err)
+	}
+	ut, err := CreateForEmail(ctx, db, "Dave@vocdoni.org")
+	if err != nil {
+		t.Fatalf("create for e-mail: %v", err)
+	}
+	if ut.Name != "dave@vocdoni.org-3" || ut.Email != "dave@vocdoni.org" || ut.Status != StatusActive {
+		t.Fatalf("unexpected token: %+v", ut)
+	}
+	if _, err := CreateForEmail(ctx, db, "dave@vocdoni.org"); err != ErrEmailTaken {
+		t.Fatalf("second create: want ErrEmailTaken, got %v", err)
+	}
+	fresh, err := CreateForEmail(ctx, db, "erin@vocdoni.org")
+	if err != nil || fresh.Name != "erin@vocdoni.org" {
+		t.Fatalf("untaken name: %v %+v", err, fresh)
+	}
+}
diff --git a/internal/webui/useremail_test.go b/internal/webui/useremail_test.go
new file mode 100644
index 0000000..7386e2c
--- /dev/null
+++ b/internal/webui/useremail_test.go
@@ -0,0 +1,45 @@
+package webui
+
+import (
+	"context"
+	"encoding/json"
+	"net/http"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+func TestUsersListIncludesEmail(t *testing.T) {
+	db, h := newTestServer(t)
+	ctx := context.Background()
+	if _, err := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org"); err != nil {
+		t.Fatal(err)
+	}
+	if _, err := usertoken.Create(ctx, db, "ci"); err != nil {
+		t.Fatal(err)
+	}
+	w := do(t, h, http.MethodGet, "/api/users", "", loginCookie(t, h))
+	if w.Code != http.StatusOK {
+		t.Fatalf("list: %d %s", w.Code, w.Body.String())
+	}
+	var users []map[string]any
+	if err := json.Unmarshal(w.Body.Bytes(), &users); err != nil {
+		t.Fatal(err)
+	}
+	if len(users) != 2 {
+		t.Fatalf("users = %v", users)
+	}
+	for _, u := range users {
+		email, ok := u["email"]
+		if !ok {
+			t.Fatalf("user %v has no email field", u["name"])
+		}
+		want := ""
+		if u["name"] == "alice" {
+			want = "alice@vocdoni.org"
+		}
+		if email != want {
+			t.Fatalf("%v: email = %q, want %q", u["name"], email, want)
+		}
+	}
+}
````
Run: `go test ./internal/store/ ./internal/usertoken/ ./cmd/claude-proxy/ ./internal/webui/`
Expected: `store_test.go: NULL e-mail: SQL logic error: table user_tokens has no column named email`; `usertoken_test.go: undefined: CreateWithEmail` (build failed); `users_test.go: undefined: usersEmail` (build failed); `--- FAIL: TestUsersListIncludesEmail`

Step 2: implement
````diff
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index 3693334..05345f3 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -36,8 +36,9 @@ const helpText = `claude-proxy — sticky multi-subscription proxy for Claude Co
 
 Usage:
   claude-proxy serve [flags]
-  claude-proxy users create           --name NAME [--db PATH]
+  claude-proxy users create           --name NAME [--email ADDR] [--db PATH]
   claude-proxy users list             [--db PATH]
+  claude-proxy users email <id> <addr> [--db PATH]
   claude-proxy users stats [<id>]     [--period 1h|6h|24h|7d|30d] [--db PATH]
   claude-proxy users token <id>       [--db PATH]
   claude-proxy users disable <id>     [--db PATH]
@@ -910,7 +911,7 @@ func printUsageBucket(label string, b *usage.Bucket) {
 
 func runUsers(args []string) {
 	if len(args) == 0 {
-		fmt.Fprintln(os.Stderr, "users: missing subcommand (create|list|stats|token|disable|enable|rm|refresh|limit|suggestions)")
+		fmt.Fprintln(os.Stderr, "users: missing subcommand (create|list|email|stats|token|disable|enable|rm|refresh|limit|suggestions)")
 		os.Exit(2)
 	}
 	ctx := context.Background()
@@ -919,6 +920,10 @@ func runUsers(args []string) {
 		usersCreate(ctx, args[1:])
 	case "list":
 		usersList(ctx, args[1:])
+	case "email":
+		if code := usersEmail(ctx, args[1:], os.Stdout, os.Stderr); code != 0 {
+			os.Exit(code)
+		}
 	case "stats":
 		usersStats(ctx, args[1:])
 	case "token":
@@ -944,6 +949,7 @@ func runUsers(args []string) {
 func usersCreate(ctx context.Context, args []string) {
 	fs := flag.NewFlagSet("users create", flag.ExitOnError)
 	name := fs.String("name", "", "user name (unique label)")
+	email := fs.String("email", "", "member e-mail the token belongs to (optional, unique)")
 	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
 	_ = fs.Parse(args)
 	if *name == "" {
@@ -952,7 +958,15 @@ func usersCreate(ctx context.Context, args []string) {
 	}
 	db := openDB(*dbPath)
 	defer db.Close()
-	ut, err := usertoken.Create(ctx, db, *name)
+	var (
+		ut  *usertoken.UserToken
+		err error
+	)
+	if *email != "" {
+		ut, err = usertoken.CreateWithEmail(ctx, db, *name, *email)
+	} else {
+		ut, err = usertoken.Create(ctx, db, *name)
+	}
 	if err != nil {
 		fmt.Fprintf(os.Stderr, "create: %v\n", err)
 		os.Exit(1)
@@ -960,24 +974,58 @@ func usersCreate(ctx context.Context, args []string) {
 	fmt.Printf("created %s  name=%q  token=%s\n", ut.ID, ut.Name, ut.Token)
 }
 
+// usersEmail sets the e-mail a user token belongs to. It refuses an address
+// already on another token. Returns the process exit code.
+func usersEmail(ctx context.Context, args []string, stdout, stderr io.Writer) int {
+	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
+		fmt.Fprintln(stderr, "users email <id> <email> [--db PATH]")
+		return 2
+	}
+	fs := flag.NewFlagSet("users email", flag.ContinueOnError)
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
+	if err := usertoken.SetEmail(ctx, db, args[0], args[1]); err != nil {
+		fmt.Fprintf(stderr, "email: %v\n", err)
+		return 1
+	}
+	fmt.Fprintf(stdout, "%s: e-mail set to %s\n", args[0], usertoken.NormalizeEmail(args[1]))
+	return 0
+}
+
 func usersList(ctx context.Context, args []string) {
 	fs := flag.NewFlagSet("users list", flag.ExitOnError)
 	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
 	_ = fs.Parse(args)
 	db := openDB(*dbPath)
 	defer db.Close()
-	list, err := usertoken.List(ctx, db)
-	if err != nil {
+	if err := printUsers(ctx, db, os.Stdout); err != nil {
 		fmt.Fprintf(os.Stderr, "list: %v\n", err)
 		os.Exit(1)
 	}
+}
+
+// printUsers writes the users table of `users list`.
+func printUsers(ctx context.Context, db *store.DB, w io.Writer) error {
+	list, err := usertoken.List(ctx, db)
+	if err != nil {
+		return err
+	}
 	if len(list) == 0 {
-		fmt.Println("(no user tokens)")
-		return
+		fmt.Fprintln(w, "(no user tokens)")
+		return nil
 	}
 	now := time.Now()
-	fmt.Printf("%-22s  %-20s  %-8s  %-25s  %-25s  %-6s  %-16s  %s\n",
-		"ID", "NAME", "STATUS", "CREATED", "LAST_USED", "SUGG", "LIMIT (OUT TOK)", "USED (OUT TOK)")
+	fmt.Fprintf(w, "%-22s  %-20s  %-28s  %-8s  %-25s  %-25s  %-6s  %-16s  %s\n",
+		"ID", "NAME", "EMAIL", "STATUS", "CREATED", "LAST_USED", "SUGG", "LIMIT (OUT TOK)", "USED (OUT TOK)")
 	for _, ut := range list {
 		lastUsed := "-"
 		if ut.LastUsedAt != nil {
@@ -1004,10 +1052,15 @@ func usersList(ctx context.Context, args []string) {
 		if ut.BlockSuggestions {
 			sugg = "block"
 		}
-		fmt.Printf("%-22s  %-20s  %-8s  %-25s  %-25s  %-6s  %-16s  %s\n",
-			ut.ID, ut.Name, string(ut.Status),
+		email := ut.Email
+		if email == "" {
+			email = "-"
+		}
+		fmt.Fprintf(w, "%-22s  %-20s  %-28s  %-8s  %-25s  %-25s  %-6s  %-16s  %s\n",
+			ut.ID, ut.Name, email, string(ut.Status),
 			ut.CreatedAt.Format(time.RFC3339), lastUsed, sugg, limitCol, usageCol)
 	}
+	return nil
 }
 
 // usersLimit sets or clears a user's rolling-window usage limit.
diff --git a/internal/store/store.go b/internal/store/store.go
index 4ec1091..953efc6 100644
--- a/internal/store/store.go
+++ b/internal/store/store.go
@@ -106,6 +106,11 @@ func Open(path string) (*DB, error) {
 		`ALTER TABLE usage_history ADD COLUMN seven_day_scoped_pct REAL`,
 		`ALTER TABLE usage_history ADD COLUMN seven_day_scoped_resets_at INTEGER`,
 		`ALTER TABLE usage_history ADD COLUMN seven_day_scoped_label TEXT`,
+		// The member a user token belongs to, set by the claudio browser login
+		// (or by an admin). NULL for admin-created tokens; stored lower-cased
+		// and trimmed. The index must follow the column for older databases.
+		`ALTER TABLE user_tokens ADD COLUMN email TEXT`,
+		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_tokens_email ON user_tokens(email) WHERE email IS NOT NULL`,
 	} {
 		if _, err := sdb.Exec(alter); err != nil && !isDuplicateColumn(err) {
 			_ = sdb.Close()
diff --git a/internal/usertoken/usertoken.go b/internal/usertoken/usertoken.go
index 423da88..74e1b07 100644
--- a/internal/usertoken/usertoken.go
+++ b/internal/usertoken/usertoken.go
@@ -6,6 +6,8 @@ import (
 	"database/sql"
 	"encoding/hex"
 	"errors"
+	"fmt"
+	"strings"
 	"time"
 
 	"github.com/p4u/claude-proxy/internal/store"
@@ -13,6 +15,13 @@ import (
 
 var ErrNotFound = errors.New("user token not found")
 
+// ErrEmailTaken is returned when an e-mail is already on another user token.
+var ErrEmailTaken = errors.New("e-mail already belongs to another user token")
+
+// columns is the SELECT list scan reads, in order.
+const columns = `id, name, token, status, created_at, last_used_at, full_capture,
+		       limit_output_tokens, limit_window_seconds, block_suggestions, email`
+
 type Status string
 
 const (
@@ -39,6 +48,9 @@ type UserToken struct {
 	// traffic: the proxy answers those requests itself instead of forwarding
 	// them. Default false = forward, matching pre-existing behaviour.
 	BlockSuggestions bool
+	// Email is the member this token belongs to (lower-cased); empty for
+	// admin-created tokens.
+	Email string
 }
 
 // contextKey is unexported to prevent collisions across packages.
@@ -87,29 +99,103 @@ func GenerateToken() string {
 
 // Create inserts a new user token and returns it.
 func Create(ctx context.Context, db *store.DB, name string) (*UserToken, error) {
+	return insert(ctx, db, name, "")
+}
+
+// CreateWithEmail inserts a new user token that belongs to email. It returns
+// ErrEmailTaken when the address is already on another token.
+func CreateWithEmail(ctx context.Context, db *store.DB, name, email string) (*UserToken, error) {
+	email, err := checkEmail(email)
+	if err != nil {
+		return nil, err
+	}
+	return insert(ctx, db, name, email)
+}
+
+// maxNameSuffix bounds CreateForEmail's search for a free name.
+const maxNameSuffix = 100
+
+// CreateForEmail inserts a user token for a member who signed in with email.
+// The name is the e-mail; when an admin already used that name, "-2", "-3",
+// ... is appended (the e-mail, not the name, is the identity). It returns
+// ErrEmailTaken when the address is already on a token.
+func CreateForEmail(ctx context.Context, db *store.DB, email string) (*UserToken, error) {
+	email, err := checkEmail(email)
+	if err != nil {
+		return nil, err
+	}
+	for i := 1; i <= maxNameSuffix; i++ {
+		name := email
+		if i > 1 {
+			name = fmt.Sprintf("%s-%d", email, i)
+		}
+		ut, err := insert(ctx, db, name, email)
+		if err == nil || !isUniqueOn(err, "name") {
+			return ut, err
+		}
+	}
+	return nil, fmt.Errorf("no free user name for %s", email)
+}
+
+// insert stores a new active token; email "" is stored as NULL.
+func insert(ctx context.Context, db *store.DB, name, email string) (*UserToken, error) {
 	ut := &UserToken{
 		ID:        newID(),
 		Name:      name,
 		Token:     GenerateToken(),
 		Status:    StatusActive,
 		CreatedAt: time.Now(),
+		Email:     email,
 	}
 	_, err := db.ExecContext(ctx, `
-		INSERT INTO user_tokens (id, name, token, status, created_at)
-		VALUES (?, ?, ?, ?, ?)`,
-		ut.ID, ut.Name, ut.Token, string(ut.Status), ut.CreatedAt.Unix())
+		INSERT INTO user_tokens (id, name, token, status, created_at, email)
+		VALUES (?, ?, ?, ?, ?, ?)`,
+		ut.ID, ut.Name, ut.Token, string(ut.Status), ut.CreatedAt.Unix(), nullable(email))
 	if err != nil {
+		if isUniqueOn(err, "email") {
+			return nil, ErrEmailTaken
+		}
 		return nil, err
 	}
 	return ut, nil
 }
 
+// NormalizeEmail is the stored form of an e-mail: trimmed and lower-cased.
+func NormalizeEmail(s string) string {
+	return strings.ToLower(strings.TrimSpace(s))
+}
+
+// checkEmail normalises s and rejects anything that is not local@domain
+// without spaces or control characters.
+func checkEmail(s string) (string, error) {
+	e := NormalizeEmail(s)
+	at := strings.LastIndexByte(e, '@')
+	if at <= 0 || at == len(e)-1 || len(e) > 254 {
+		return "", fmt.Errorf("invalid e-mail %q", s)
+	}
+	for i := 0; i < len(e); i++ {
+		if e[i] <= ' ' || e[i] == 0x7f {
+			return "", fmt.Errorf("invalid e-mail %q", s)
+		}
+	}
+	return e, nil
+}
+
+// isUniqueOn reports whether err is SQLite's UNIQUE violation on column.
+func isUniqueOn(err error, column string) bool {
+	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: user_tokens."+column)
+}
+
+func nullable(s string) any {
+	if s == "" {
+		return nil
+	}
+	return s
+}
+
 // List returns all user tokens ordered by creation time.
 func List(ctx context.Context, db *store.DB) ([]*UserToken, error) {
-	rows, err := db.QueryContext(ctx, `
-		SELECT id, name, token, status, created_at, last_used_at, full_capture,
-		       limit_output_tokens, limit_window_seconds, block_suggestions
-		FROM user_tokens ORDER BY created_at`)
+	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens ORDER BY created_at`)
 	if err != nil {
 		return nil, err
 	}
@@ -127,10 +213,7 @@ func List(ctx context.Context, db *store.DB) ([]*UserToken, error) {
 
 // Get returns a single user token by ID.
 func Get(ctx context.Context, db *store.DB, id string) (*UserToken, error) {
-	rows, err := db.QueryContext(ctx, `
-		SELECT id, name, token, status, created_at, last_used_at, full_capture,
-		       limit_output_tokens, limit_window_seconds, block_suggestions
-		FROM user_tokens WHERE id=?`, id)
+	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens WHERE id=?`, id)
 	if err != nil {
 		return nil, err
 	}
@@ -143,10 +226,20 @@ func Get(ctx context.Context, db *store.DB, id string) (*UserToken, error) {
 
 // LookupByToken finds a user token by its bearer value (used in hot path).
 func LookupByToken(ctx context.Context, db *store.DB, token string) (*UserToken, error) {
-	rows, err := db.QueryContext(ctx, `
-		SELECT id, name, token, status, created_at, last_used_at, full_capture,
-		       limit_output_tokens, limit_window_seconds, block_suggestions
-		FROM user_tokens WHERE token=?`, token)
+	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens WHERE token=?`, token)
+	if err != nil {
+		return nil, err
+	}
+	defer rows.Close()
+	if !rows.Next() {
+		return nil, ErrNotFound
+	}
+	return scan(rows)
+}
+
+// LookupByEmail finds the user token that belongs to email.
+func LookupByEmail(ctx context.Context, db *store.DB, email string) (*UserToken, error) {
+	rows, err := db.QueryContext(ctx, `SELECT `+columns+` FROM user_tokens WHERE email=?`, NormalizeEmail(email))
 	if err != nil {
 		return nil, err
 	}
@@ -157,6 +250,26 @@ func LookupByToken(ctx context.Context, db *store.DB, token string) (*UserToken,
 	return scan(rows)
 }
 
+// SetEmail sets the e-mail a token belongs to. It returns ErrEmailTaken when
+// the address is already on another token.
+func SetEmail(ctx context.Context, db *store.DB, id, email string) error {
+	e, err := checkEmail(email)
+	if err != nil {
+		return err
+	}
+	res, err := db.ExecContext(ctx, `UPDATE user_tokens SET email=? WHERE id=?`, e, id)
+	if err != nil {
+		if isUniqueOn(err, "email") {
+			return ErrEmailTaken
+		}
+		return err
+	}
+	if n, _ := res.RowsAffected(); n == 0 {
+		return ErrNotFound
+	}
+	return nil
+}
+
 // SetStatus sets a token's status to active or disabled.
 func SetStatus(ctx context.Context, db *store.DB, id string, s Status) error {
 	res, err := db.ExecContext(ctx,
@@ -246,12 +359,14 @@ func scan(rs interface{ Scan(...any) error }) (*UserToken, error) {
 	ut := &UserToken{}
 	var created int64
 	var lu sql.NullInt64
+	var email sql.NullString
 	var status string
 	if err := rs.Scan(&ut.ID, &ut.Name, &ut.Token, &status, &created, &lu, &ut.FullCapture,
-		&ut.LimitOutputTokens, &ut.LimitWindowSeconds, &ut.BlockSuggestions); err != nil {
+		&ut.LimitOutputTokens, &ut.LimitWindowSeconds, &ut.BlockSuggestions, &email); err != nil {
 		return nil, err
 	}
 	ut.Status = Status(status)
+	ut.Email = email.String
 	ut.CreatedAt = time.Unix(created, 0)
 	if lu.Valid {
 		t := time.Unix(lu.Int64, 0)
diff --git a/internal/webui/static/js/mock.js b/internal/webui/static/js/mock.js
index 5fbfae5..d9b8372 100644
--- a/internal/webui/static/js/mock.js
+++ b/internal/webui/static/js/mock.js
@@ -78,7 +78,7 @@ const hasUsageAPI = (c) => providerOf(c) === "anthropic";
 // meter. The four users cover every limit state: healthy, near-limit,
 // unlimited, blocked.
 const USERS = [
-  { id: "utok_alice", name: "alice", status: "active", full_capture: true, block_suggestions: false,
+  { id: "utok_alice", name: "alice", email: "alice@vocdoni.org", status: "active", full_capture: true, block_suggestions: false,
     limit_output_tokens: 1_000_000, limit_window_seconds: 86400, usage_output_tokens: 302_400 },  // ~30%
   { id: "utok_bob", name: "bob", status: "active", full_capture: false, block_suggestions: true,
     limit_output_tokens: 400_000, limit_window_seconds: 21600, usage_output_tokens: 344_900 },    // ~86%
@@ -645,7 +645,7 @@ const DB = {
   "/gemini/oauth/status": () => ({ status: ++sidecarOAuthPolls.gemini > 2 ? "ok" : "wait" }),
   "/users": () =>
     USERS.map((u, i) => ({
-      id: u.id, name: u.name, status: u.status, full_capture: u.full_capture,
+      id: u.id, name: u.name, email: u.email || "", status: u.status, full_capture: u.full_capture,
       block_suggestions: u.block_suggestions,
       ...limitFields(u),
       created_at: now - 86400 * (20 - i * 3),
diff --git a/internal/webui/static/js/pages/users.js b/internal/webui/static/js/pages/users.js
index 4bc5a95..373414d 100644
--- a/internal/webui/static/js/pages/users.js
+++ b/internal/webui/static/js/pages/users.js
@@ -526,7 +526,11 @@ function userRow(u, s, root) {
     }),
   ]);
   return el("tr", {}, [
-    el("td", {}, [el("span", { class: "cell-strong", text: u.name }), el("span", { class: "cell-id", text: u.id })]),
+    el("td", {}, [
+      el("span", { class: "cell-strong", text: u.name }),
+      u.email ? el("span", { class: "cell-id", text: u.email }) : null,
+      el("span", { class: "cell-id", text: u.id }),
+    ]),
     el("td", {}, statusBadge(u.status)),
     el("td", {}, captureToggle(u)),
     el("td", {}, suggestionsToggle(u)),
diff --git a/internal/webui/users.go b/internal/webui/users.go
index 1ddcb6d..2f8c186 100644
--- a/internal/webui/users.go
+++ b/internal/webui/users.go
@@ -13,6 +13,7 @@ import (
 type userView struct {
 	ID          string  `json:"id"`
 	Name        string  `json:"name"`
+	Email       string  `json:"email"` // member e-mail; "" for admin-created tokens
 	Status      string  `json:"status"`
 	CreatedAt   string  `json:"created_at"`
 	LastUsedAt  *string `json:"last_used_at"`
@@ -132,6 +133,7 @@ func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
 		v := userView{
 			ID:                 ut.ID,
 			Name:               ut.Name,
+			Email:              ut.Email,
 			Status:             string(ut.Status),
 			CreatedAt:          ut.CreatedAt.Format(time.RFC3339),
 			FullCapture:        ut.FullCapture,
````
Run: `go test ./internal/store/ ./internal/usertoken/ ./cmd/claude-proxy/ ./internal/webui/ && GOTOOLCHAIN=go1.26.2 make lint`
Expected: four `ok  	github.com/p4u/claude-proxy/<pkg>` lines; lint `0 issues.`

Step 3: commit
`git add cmd/claude-proxy/users_test.go internal/webui/useremail_test.go` then `git commit -m "feat(usertoken): store the member e-mail on user tokens" -- cmd/claude-proxy/main.go cmd/claude-proxy/users_test.go internal/store/store.go internal/store/store_test.go internal/usertoken/usertoken.go internal/usertoken/usertoken_test.go internal/webui/static/js/mock.js internal/webui/static/js/pages/users.js internal/webui/useremail_test.go internal/webui/users.go`

Interfaces this task provides to later tasks: `usertoken.UserToken.Email`; `usertoken.ErrEmailTaken`; `CreateWithEmail(ctx, db, name, email)`; `CreateForEmail(ctx, db, email) (*UserToken, error)` (name = e-mail, `-2`, `-3`… when taken, ≤ 100); `LookupByEmail(ctx, db, email)`; `SetEmail(ctx, db, id, email)`; `NormalizeEmail(s)`; `main.printUsers(ctx, db, w)`, `main.usersEmail(ctx, args, stdout, stderr) int`; test helper `cliDB(t)` in `cmd/claude-proxy/users_test.go`.

### Task 2: SSO split: administrators vs login domains
Model: strong   Repo: /volumes/repos/claude-proxy   Files: .env.example, CLAUDE.md, README.md, cmd/claude-proxy/main.go, cmd/claude-proxy/sso_test.go, docs/WEBUI.md, internal/webui/sso.go, internal/webui/sso_test.go
Goal: Spec §0: `SSOConfig.AdminEmails` (from `CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS`) and `LoginDomains` (`CLAUDE_PROXY_LOGIN_DOMAINS`, default `vocdoni.org`); vocauth gets `AllowedDomains` only; `ssoAuthorize` 403s non-admins; startup warning; README/.env.example/WEBUI.md (AC2 admin part).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-2.patch

Notes: `AllowedEmails` is no longer passed to vocauth (spec): an administrator must also be in a login domain to sign in at all (true for every oauth.vocdoni.org account). `/api/session` stays open to a member (it only reports the session). No docker-compose change: `CLAUDE_PROXY_LOGIN_DOMAINS` reaches the container through `env_file: .env` as is; adding it to the compose `environment:` block would override a value set in `.env` with an empty one.

Step 1: write the failing test
````diff
diff --git a/cmd/claude-proxy/sso_test.go b/cmd/claude-proxy/sso_test.go
new file mode 100644
index 0000000..3f32e6b
--- /dev/null
+++ b/cmd/claude-proxy/sso_test.go
@@ -0,0 +1,19 @@
+package main
+
+import (
+	"reflect"
+	"testing"
+)
+
+func TestSplitList(t *testing.T) {
+	for in, want := range map[string][]string{
+		"":                               nil,
+		" , ,":                           nil,
+		"vocdoni.org":                    {"vocdoni.org"},
+		" a@vocdoni.org , b@vocdoni.org": {"a@vocdoni.org", "b@vocdoni.org"},
+	} {
+		if got := splitList(in); !reflect.DeepEqual(got, want) {
+			t.Errorf("splitList(%q) = %#v, want %#v", in, got, want)
+		}
+	}
+}
diff --git a/internal/webui/sso_test.go b/internal/webui/sso_test.go
index d2dfd3e..abec920 100644
--- a/internal/webui/sso_test.go
+++ b/internal/webui/sso_test.go
@@ -97,13 +97,23 @@ func (op *fakeOP) sign(t *testing.T, claims map[string]any) string {
 }
 
 func newSSOServer(t *testing.T, issuer string) (*store.DB, http.Handler) {
+	t.Helper()
+	return newSSOServerWith(t, issuer, nil)
+}
+
+// newSSOServerWith is newSSOServer with a hook to adjust the SSO config.
+func newSSOServerWith(t *testing.T, issuer string, mut func(*SSOConfig)) (*store.DB, http.Handler) {
 	t.Helper()
 	db, _ := newTestServer(t)
 	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
-	sso, err := NewSSO(SSOConfig{
+	cfg := SSOConfig{
 		Issuer: issuer, ClientID: ssoClientID, ClientSecret: "secret",
 		RedirectURL: ssoOrigin + "/auth/callback", CookieKeys: keys, Insecure: true,
-	})
+	}
+	if mut != nil {
+		mut(&cfg)
+	}
+	sso, err := NewSSO(cfg)
 	if err != nil {
 		t.Fatal(err)
 	}
@@ -253,6 +263,7 @@ func TestNewSSORejectsBadConfig(t *testing.T) {
 		"short key":     func(c *SSOConfig) { c.CookieKeys = "c2hvcnQ=" },
 		"no secret":     func(c *SSOConfig) { c.ClientSecret = "" },
 		"plain http":    func(c *SSOConfig) { c.RedirectURL = "http://p.test/auth/callback" },
+		"bad domain":    func(c *SSOConfig) { c.LoginDomains = []string{"a@b"} },
 	} {
 		c := good
 		mut(&c)
@@ -261,3 +272,71 @@ func TestNewSSORejectsBadConfig(t *testing.T) {
 		}
 	}
 }
+
+func TestSSOAdminEmailsGateAPI(t *testing.T) {
+	op := newFakeOP(t)
+	_, h := newSSOServerWith(t, op.srv.URL, func(c *SSOConfig) {
+		c.AdminEmails = []string{" Boss@Vocdoni.org "}
+	})
+
+	// A member of the login domain signs in but is not an administrator.
+	member, cb := signIn(t, h, op)
+	if member == nil {
+		t.Fatalf("member sign-in refused: %d %s", cb.Code, cb.Body.String())
+	}
+	w := do(t, h, http.MethodGet, "/api/overview", "", member)
+	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not an administrator") {
+		t.Fatalf("member /api/overview = %d %s, want 403 not an administrator", w.Code, w.Body.String())
+	}
+	if w := do(t, h, http.MethodGet, "/api/users", "", member); w.Code != http.StatusForbidden {
+		t.Fatalf("member /api/users = %d, want 403", w.Code)
+	}
+
+	op.mu.Lock()
+	op.email = "boss@vocdoni.org"
+	op.mu.Unlock()
+	admin, cb := signIn(t, h, op)
+	if admin == nil {
+		t.Fatalf("admin sign-in refused: %d %s", cb.Code, cb.Body.String())
+	}
+	if w := do(t, h, http.MethodGet, "/api/overview", "", admin); w.Code != http.StatusOK {
+		t.Fatalf("admin /api/overview = %d %s", w.Code, w.Body.String())
+	}
+}
+
+func TestSSOLoginDomains(t *testing.T) {
+	op := newFakeOP(t)
+	op.email = "mallory@example.com"
+	_, h := newSSOServerWith(t, op.srv.URL, func(c *SSOConfig) {
+		c.LoginDomains = []string{"example.com"}
+	})
+	if sess, cb := signIn(t, h, op); sess == nil {
+		t.Fatalf("login-domain account refused: %d %s", cb.Code, cb.Body.String())
+	}
+	op.mu.Lock()
+	op.email = "alice@vocdoni.org"
+	op.mu.Unlock()
+	if sess, cb := signIn(t, h, op); sess != nil || cb.Code != http.StatusForbidden {
+		t.Fatalf("account outside the login domains: code %d, session %v", cb.Code, sess)
+	}
+}
+
+func TestSSOAdminListReported(t *testing.T) {
+	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
+	cfg := SSOConfig{ClientID: "c", ClientSecret: "s", RedirectURL: "https://p.test/auth/callback", CookieKeys: keys}
+	open, err := NewSSO(cfg)
+	if err != nil {
+		t.Fatal(err)
+	}
+	if open.HasAdminList() {
+		t.Fatal("no AdminEmails: HasAdminList = true")
+	}
+	cfg.AdminEmails = []string{"boss@vocdoni.org"}
+	closed, err := NewSSO(cfg)
+	if err != nil {
+		t.Fatal(err)
+	}
+	if !closed.HasAdminList() {
+		t.Fatal("AdminEmails set: HasAdminList = false")
+	}
+}
````
Run: `go test ./internal/webui/ ./cmd/claude-proxy/`
Expected: `sso_test.go: open.HasAdminList undefined (type *SSO has no field or method HasAdminList)` and `cfg.AdminEmails undefined` (build failed); `sso_test.go (cmd): undefined: splitList`

Step 2: implement
````diff
diff --git a/.env.example b/.env.example
index d7b524f..9098834 100644
--- a/.env.example
+++ b/.env.example
@@ -56,9 +56,14 @@ UI_PASSWORD=
 # Cookie encryption keys: openssl rand -base64 32 (comma-separated to rotate:
 # the first encrypts, any decrypts).
 # OAUTH_COOKIE_KEYS=
-# Narrow sign-in to these addresses (comma-separated); empty admits every
-# verified account of the provider's domain.
+# Web UI administrators (comma-separated). Every verified account of a login
+# domain can sign in (claudio browser login needs that), but only these may use
+# the web UI. Empty makes everyone who signs in an administrator: set it before
+# members use `claudio proxy login`.
 # OAUTH_ALLOWED_EMAILS=
+# Domains whose verified accounts may sign in (comma-separated, default
+# vocdoni.org). Passed to the container as is.
+# CLAUDE_PROXY_LOGIN_DOMAINS=vocdoni.org
 
 # Force the UI session cookie to be Secure (HTTPS-only). Auto-detected when
 # behind Traefik (TLS_DOMAIN set, forwards X-Forwarded-Proto: https) — only
diff --git a/CLAUDE.md b/CLAUDE.md
index 9494bee..3f78e7b 100644
--- a/CLAUDE.md
+++ b/CLAUDE.md
@@ -837,7 +837,7 @@ instead; it does not load `.env` automatically. Key Compose variables:
 | `MODELS_1M` | _(enabled)_ | Append `[1m]` model variants to `GET /v1/models` for Claude Code gateway model discovery (`CLAUDE_PROXY_MODELS_1M`); set `0` to disable |
 | `REBALANCE_SESSIONS` | `1` | Proactive long-session rebalance for direct Anthropic subscriptions, announced via an API header notice (`CLAUDE_PROXY_REBALANCE_SESSIONS`; `0` disables it). Account-bound conversations never migrate. |
 | `EXPIRY_POLICY` | `shadow` | `off` or `shadow` only (`CLAUDE_PROXY_EXPIRY_POLICY`, `--expiry-policy`). Records bounded unused-quota forecasts in Routing activity; never changes routing. |
-| `OAUTH_CLIENT_ID` (+ `OAUTH_ISSUER`, `_CLIENT_SECRET`, `_REDIRECT_URL`, `_COOKIE_KEYS`, `_ALLOWED_EMAILS`) | _(empty)_ | Web UI single sign-on via OpenID Connect (`CLAUDE_PROXY_OAUTH_*`, env only; issuer default `https://oauth.vocdoni.org`). Set ⇒ replaces the password: `/api/login` 404s, sign-in at `/auth/login` (`internal/webui/sso.go`, `vocauth` kit). Unset ⇒ password login, the open-source default. Contract in `docs/WEBUI.md` |
+| `OAUTH_CLIENT_ID` (+ `OAUTH_ISSUER`, `_CLIENT_SECRET`, `_REDIRECT_URL`, `_COOKIE_KEYS`, `_ALLOWED_EMAILS`) | _(empty)_ | Web UI single sign-on via OpenID Connect (`CLAUDE_PROXY_OAUTH_*`, env only; issuer default `https://oauth.vocdoni.org`). Set ⇒ replaces the password: `/api/login` 404s, sign-in at `/auth/login` (`internal/webui/sso.go`, `vocauth` kit). `_ALLOWED_EMAILS` = the administrators (empty ⇒ every signed-in account, warned at start); `CLAUDE_PROXY_LOGIN_DOMAINS` (default `vocdoni.org`) = who may sign in at all. Unset ⇒ password login, the open-source default. Contract in `docs/WEBUI.md` |
 | `UI_SECURE_COOKIES` | _(empty)_ | Force `Secure` UI session cookies (`CLAUDE_PROXY_UI_SECURE_COOKIES`); auto-detected behind Traefik via `X-Forwarded-Proto` |
 | `PROMPT_RETENTION_DAYS` | `7` | Retain prompt/full-capture rows; `0` disables capture (`CLAUDE_PROXY_PROMPT_RETENTION_DAYS`) |
 | `CLIPROXY_API_KEY` | _(generated)_ | Private proxy-to-CLIProxyAPI API key; never sent to OpenAI |
diff --git a/README.md b/README.md
index 0cbff19..105a008 100644
--- a/README.md
+++ b/README.md
@@ -856,13 +856,20 @@ OAUTH_CLIENT_ID=claude-proxy
 OAUTH_CLIENT_SECRET=...                     # from the provider registration
 OAUTH_REDIRECT_URL=https://<TLS_DOMAIN>/auth/callback
 OAUTH_COOKIE_KEYS=$(openssl rand -base64 32)
-# OAUTH_ALLOWED_EMAILS=alice@example.org,bob@example.org   # optional narrowing
+OAUTH_ALLOWED_EMAILS=alice@vocdoni.org,bob@vocdoni.org     # the administrators
+# CLAUDE_PROXY_LOGIN_DOMAINS=vocdoni.org                   # who may sign in (default)
 ```
 
 With `OAUTH_CLIENT_ID` set the password login is switched off (`UI_PASSWORD`
-is ignored and `/api/login` answers 404); without it nothing changes. By
-default only verified accounts of the provider's domain (`vocdoni.org`) are
-admitted. Sessions last 7 days.
+is ignored and `/api/login` answers 404); without it nothing changes.
+Sign-in and administration are separate: every verified account of a
+`CLAUDE_PROXY_LOGIN_DOMAINS` domain (default `vocdoni.org`) can sign in, which
+is what the claudio browser login needs, but only the addresses in
+`OAUTH_ALLOWED_EMAILS` may use the web UI and its `/api` (anyone else gets
+`403 not an administrator`). When `OAUTH_ALLOWED_EMAILS` is empty every
+signed-in account is an administrator, as before; the proxy logs a warning
+at start in that case, and it must be set before members use `claudio proxy
+login`. Sessions last 7 days.
 
 **Features:**
 
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index 05345f3..2288228 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -309,6 +309,9 @@ func runServe(args []string) {
 			if *uiPassword != "" {
 				logger.Warn("web UI password ignored: single sign-on is configured")
 			}
+			if !sso.HasAdminList() {
+				logger.Warn("CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS is empty: every account that can sign in is a web UI administrator; set it before enabling claudio browser login")
+			}
 		}
 		logger.Info("web UI enabled", "path", "/", "login", login, "secure_cookies", secureCookies)
 	}
@@ -343,26 +346,36 @@ func runServe(args []string) {
 // CLAUDE_PROXY_OAUTH_CLIENT_ID is set and then replaces the password login;
 // unset, it returns nil and the password login applies. They are read from
 // the environment only, never flags, so the client secret stays out of ps.
+//
+// CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS lists the administrators (empty: every
+// signed-in user is one); CLAUDE_PROXY_LOGIN_DOMAINS lists the domains whose
+// verified accounts may sign in at all (default vocdoni.org).
 func ssoFromEnv() (*webui.SSO, error) {
 	clientID := strings.TrimSpace(os.Getenv("CLAUDE_PROXY_OAUTH_CLIENT_ID"))
 	if clientID == "" {
 		return nil, nil
 	}
-	var emails []string
-	for e := range strings.SplitSeq(os.Getenv("CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS"), ",") {
+	return webui.NewSSO(webui.SSOConfig{
+		Issuer:       strings.TrimSpace(os.Getenv("CLAUDE_PROXY_OAUTH_ISSUER")),
+		ClientID:     clientID,
+		ClientSecret: os.Getenv("CLAUDE_PROXY_OAUTH_CLIENT_SECRET"),
+		RedirectURL:  strings.TrimSpace(os.Getenv("CLAUDE_PROXY_OAUTH_REDIRECT_URL")),
+		CookieKeys:   os.Getenv("CLAUDE_PROXY_OAUTH_COOKIE_KEYS"),
+		AdminEmails:  splitList(os.Getenv("CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS")),
+		LoginDomains: splitList(os.Getenv("CLAUDE_PROXY_LOGIN_DOMAINS")),
+		Insecure:     isTruthy(os.Getenv("CLAUDE_PROXY_OAUTH_INSECURE")),
+	})
+}
+
+// splitList splits a comma-separated env value, dropping empty items.
+func splitList(v string) []string {
+	var out []string
+	for e := range strings.SplitSeq(v, ",") {
 		if e = strings.TrimSpace(e); e != "" {
-			emails = append(emails, e)
+			out = append(out, e)
 		}
 	}
-	return webui.NewSSO(webui.SSOConfig{
-		Issuer:        strings.TrimSpace(os.Getenv("CLAUDE_PROXY_OAUTH_ISSUER")),
-		ClientID:      clientID,
-		ClientSecret:  os.Getenv("CLAUDE_PROXY_OAUTH_CLIENT_SECRET"),
-		RedirectURL:   strings.TrimSpace(os.Getenv("CLAUDE_PROXY_OAUTH_REDIRECT_URL")),
-		CookieKeys:    os.Getenv("CLAUDE_PROXY_OAUTH_COOKIE_KEYS"),
-		AllowedEmails: emails,
-		Insecure:      isTruthy(os.Getenv("CLAUDE_PROXY_OAUTH_INSECURE")),
-	})
+	return out
 }
 
 func runTUI(args []string) {
diff --git a/docs/WEBUI.md b/docs/WEBUI.md
index cf87854..b35351b 100644
--- a/docs/WEBUI.md
+++ b/docs/WEBUI.md
@@ -24,10 +24,14 @@ vendored chart library, committed to the repo under `internal/webui/static/`.
   `vocauth` kit (`github.com/vocdoni/oauth/go`, `internal/webui/sso.go`): issuer
   `CLAUDE_PROXY_OAUTH_ISSUER` (default `https://oauth.vocdoni.org`), `_CLIENT_SECRET`,
   `_REDIRECT_URL` (must be `https://<host>/auth/callback`, exactly as registered), `_COOKIE_KEYS`
-  (comma-separated base64 32-byte keys), optional `_ALLOWED_EMAILS` (otherwise every verified
-  `@vocdoni.org` account), `_INSECURE=1` for a localhost dev provider only. Read from the
+  (comma-separated base64 32-byte keys), optional `_ALLOWED_EMAILS` (the administrators;
+  empty makes every signed-in account one), `CLAUDE_PROXY_LOGIN_DOMAINS` (who may sign in at
+  all, default `vocdoni.org`), `_INSECURE=1` for a localhost dev provider only. Read from the
   environment only, never flags. A half-configured SSO stops the server instead of falling back
   to the password. In this mode:
+  - Sign-in and administration are split: every verified account of a login domain gets a
+    session, but every `/api/*` route except `/api/session` answers `403 {"error":"not an
+    administrator"}` unless the e-mail is in `_ALLOWED_EMAILS` (when that list is set).
   - `GET /auth/login` (302 to the provider, PKCE S256), `GET /auth/callback`, `POST /auth/logout`
     (same-origin form only) are the kit's routes; `POST /api/login` and `/api/logout` answer 404
     and no password or `cpui_session` cookie opens anything.
diff --git a/internal/webui/sso.go b/internal/webui/sso.go
index 68ac395..1917409 100644
--- a/internal/webui/sso.go
+++ b/internal/webui/sso.go
@@ -25,9 +25,13 @@ type SSOConfig struct {
 	// CookieKeys is a comma-separated list of base64 32-byte keys; the first
 	// encrypts, any decrypts (rotation).
 	CookieKeys string
-	// AllowedEmails narrows sign-in to these addresses. Empty admits every
-	// verified account of the provider's domain (vocdoni.org).
-	AllowedEmails []string
+	// AdminEmails are the administrators: only they may use /api/*. Empty
+	// makes every signed-in user an administrator (the behaviour before
+	// member sign-in existed).
+	AdminEmails []string
+	// LoginDomains admits every verified account of these domains to a
+	// session (admins and members alike). Empty means DefaultLoginDomain.
+	LoginDomains []string
 	// Insecure allows http:// URLs and drops the Secure cookie attribute.
 	// Local development against a dev provider only.
 	Insecure bool
@@ -36,6 +40,9 @@ type SSOConfig struct {
 // DefaultSSOIssuer is the central Vocdoni sign-in service.
 const DefaultSSOIssuer = "https://oauth.vocdoni.org"
 
+// DefaultLoginDomain is the login domain when SSOConfig.LoginDomains is empty.
+const DefaultLoginDomain = "vocdoni.org"
+
 // ssoSessionAge is the sign-in lifetime. It is absolute, not sliding, so it
 // is set to the kit's maximum: the password session this replaces lasted 30
 // days, and signing in again is one click while the browser's Google session
@@ -48,6 +55,10 @@ type SSO struct {
 	// origin is scheme://host of the redirect URL, the form a browser sends
 	// in the Origin header; state-changing API calls must come from it.
 	origin string
+	// admins is the lower-cased AdminEmails; empty = every signed-in user.
+	admins map[string]bool
+	// domains is the lower-cased LoginDomains.
+	domains map[string]bool
 }
 
 // NewSSO validates cfg. It does no network I/O: the provider is discovered
@@ -68,22 +79,52 @@ func NewSSO(cfg SSOConfig) (*SSO, error) {
 	if u.Path != "/auth/callback" {
 		return nil, errors.New("sso: redirect URL path must be /auth/callback")
 	}
+	domains := make(map[string]bool)
+	for _, d := range cfg.LoginDomains {
+		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
+			domains[d] = true
+		}
+	}
+	if len(domains) == 0 {
+		domains[DefaultLoginDomain] = true
+	}
+	allowed := make([]string, 0, len(domains))
+	for d := range domains {
+		allowed = append(allowed, d)
+	}
+	admins := make(map[string]bool)
+	for _, e := range cfg.AdminEmails {
+		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
+			admins[e] = true
+		}
+	}
 	limiter := newSignInLimiter()
+	// Every verified account of a login domain may hold a session; who may
+	// administer is decided per request by isAdmin, not at sign-in.
 	auth, err := vocauth.New(vocauth.Config{
-		Issuer:        issuer,
-		ClientID:      cfg.ClientID,
-		ClientSecret:  cfg.ClientSecret,
-		RedirectURL:   cfg.RedirectURL,
-		CookieKeys:    keys,
-		AllowedEmails: cfg.AllowedEmails,
-		MaxAge:        ssoSessionAge,
-		Insecure:      cfg.Insecure,
-		Limiter:       func(r *http.Request) bool { return limiter.allow(clientIP(r)) },
+		Issuer:         issuer,
+		ClientID:       cfg.ClientID,
+		ClientSecret:   cfg.ClientSecret,
+		RedirectURL:    cfg.RedirectURL,
+		CookieKeys:     keys,
+		AllowedDomains: allowed,
+		MaxAge:         ssoSessionAge,
+		Insecure:       cfg.Insecure,
+		Limiter:        func(r *http.Request) bool { return limiter.allow(clientIP(r)) },
 	})
 	if err != nil {
 		return nil, err
 	}
-	return &SSO{auth: auth, origin: originOf(u)}, nil
+	return &SSO{auth: auth, origin: originOf(u), admins: admins, domains: domains}, nil
+}
+
+// HasAdminList reports whether AdminEmails was set. Without it every member
+// who can sign in is an administrator.
+func (s *SSO) HasAdminList() bool { return len(s.admins) > 0 }
+
+// isAdmin reports whether email may use the admin API.
+func (s *SSO) isAdmin(email string) bool {
+	return len(s.admins) == 0 || s.admins[strings.ToLower(email)]
 }
 
 // originOf is scheme://host lower-cased and without a default port, the form
@@ -173,13 +214,18 @@ func (s *Server) handleSSOSession(w http.ResponseWriter, r *http.Request) {
 }
 
 // ssoAuthorize gates an /api request in SSO mode and logs who changed what,
-// since the e-mail is now the operator's identity.
+// since the e-mail is now the operator's identity. Only administrators pass:
+// a member signed in for the claudio login gets 403.
 func (s *Server) ssoAuthorize(w http.ResponseWriter, r *http.Request) bool {
 	u, ok := s.sso.user(w, r)
 	if !ok {
 		writeErr(w, http.StatusUnauthorized, "authentication required")
 		return false
 	}
+	if !s.sso.isAdmin(u.Email) {
+		writeErr(w, http.StatusForbidden, "not an administrator")
+		return false
+	}
 	if r.Method != http.MethodGet && r.Method != http.MethodHead {
 		if !s.sso.sameOrigin(r) {
 			writeErr(w, http.StatusForbidden, "cross-origin request refused")
````
Run: `go test ./internal/webui/ ./cmd/claude-proxy/ && GOTOOLCHAIN=go1.26.2 make lint`
Expected: `ok  	github.com/p4u/claude-proxy/internal/webui`, `ok  	github.com/p4u/claude-proxy/cmd/claude-proxy`; lint `0 issues.`

Step 3: commit
`git add cmd/claude-proxy/sso_test.go` then `git commit -m "feat(webui): split sso sign-in from administration" -- .env.example CLAUDE.md README.md cmd/claude-proxy/main.go cmd/claude-proxy/sso_test.go docs/WEBUI.md internal/webui/sso.go internal/webui/sso_test.go`

Interfaces this task provides to later tasks: `webui.SSOConfig.AdminEmails`, `.LoginDomains`; `webui.DefaultLoginDomain`; `(*SSO).HasAdminList() bool`; unexported `(*SSO).isAdmin(email)`, field `SSO.domains map[string]bool` (read by task 6); `main.splitList(v)`; test helper `newSSOServerWith(t, issuer, mut)`.

### Task 3: internal/plugins package and tables
Model: standard   Repo: /volumes/repos/claude-proxy   Files: internal/plugins/plugins.go, internal/plugins/plugins_test.go, internal/store/schema.go, internal/store/store_test.go
Goal: Spec §3 tables and `internal/plugins` (`Set`, `Enable`, `Delete`, `SetEnv`, `UnsetEnv`, `List`, `ListForClient`, `Get`) owning every validation limit (AC3 second half).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-3.patch

Notes: Tables go in `schema.go` (CREATE IF NOT EXISTS, run on every open). One deliberate tightening beyond the spec: values holding NUL or C0 controls other than `\n`/`\t` are refused, because claudio's `admit` refuses the whole plugin for them (consumer spec §2). Errors name keys, never values (`TestErrorsNeverCarryValues`).

Step 1: write the failing test
````diff
diff --git a/internal/plugins/plugins_test.go b/internal/plugins/plugins_test.go
new file mode 100644
index 0000000..45a7556
--- /dev/null
+++ b/internal/plugins/plugins_test.go
@@ -0,0 +1,249 @@
+package plugins
+
+import (
+	"context"
+	"errors"
+	"fmt"
+	"path/filepath"
+	"strings"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/store"
+)
+
+func testDB(t *testing.T) *store.DB {
+	t.Helper()
+	db, err := store.Open(filepath.Join(t.TempDir(), "plugins.db"))
+	if err != nil {
+		t.Fatal(err)
+	}
+	t.Cleanup(func() { _ = db.Close() })
+	return db
+}
+
+var sha = strings.Repeat("ab", 32)
+
+func goodMeta() Meta {
+	return Meta{
+		Version:      "1.4.0",
+		BundleURL:    "https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip",
+		BundleSHA256: sha,
+		InstalledID:  "vocdoni-internal@vocdoni",
+	}
+}
+
+func TestValidation(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+	cases := []struct {
+		name string
+		mut  func(n *string, m *Meta)
+		ok   bool
+	}{
+		{"valid", func(*string, *Meta) {}, true},
+		{"name 40 chars", func(n *string, _ *Meta) { *n = strings.Repeat("a", 40) }, true},
+		{"name 41 chars", func(n *string, _ *Meta) { *n = strings.Repeat("a", 41) }, false},
+		{"name empty", func(n *string, _ *Meta) { *n = "" }, false},
+		{"name upper", func(n *string, _ *Meta) { *n = "Vocdoni" }, false},
+		{"name underscore", func(n *string, _ *Meta) { *n = "a_b" }, false},
+		{"version 64", func(_ *string, m *Meta) { m.Version = strings.Repeat("v", 64) }, true},
+		{"version 65", func(_ *string, m *Meta) { m.Version = strings.Repeat("v", 65) }, false},
+		{"version empty", func(_ *string, m *Meta) { m.Version = "" }, false},
+		{"version non-ascii", func(_ *string, m *Meta) { m.Version = "1.0-ü" }, false},
+		{"version control", func(_ *string, m *Meta) { m.Version = "1.0\n" }, false},
+		{"url http", func(_ *string, m *Meta) { m.BundleURL = "http://example.org/a.zip" }, false},
+		{"url relative", func(_ *string, m *Meta) { m.BundleURL = "/a.zip" }, false},
+		{"url userinfo", func(_ *string, m *Meta) { m.BundleURL = "https://u:p@example.org/a.zip" }, false},
+		{"sha upper", func(_ *string, m *Meta) { m.BundleSHA256 = strings.ToUpper(sha) }, false},
+		{"sha short", func(_ *string, m *Meta) { m.BundleSHA256 = sha[:63] }, false},
+		{"installed empty", func(_ *string, m *Meta) { m.InstalledID = "" }, true},
+		{"installed no at", func(_ *string, m *Meta) { m.InstalledID = "vocdoni-internal" }, false},
+		{"installed upper", func(_ *string, m *Meta) { m.InstalledID = "A@b" }, false},
+		{"installed long", func(_ *string, m *Meta) { m.InstalledID = strings.Repeat("a", 41) + "@b" }, false},
+	}
+	for _, tc := range cases {
+		t.Run(tc.name, func(t *testing.T) {
+			name, m := "vocdoni-internal", goodMeta()
+			tc.mut(&name, &m)
+			err := Set(ctx, db, name, m)
+			if tc.ok && err != nil {
+				t.Fatalf("rejected: %v", err)
+			}
+			if !tc.ok && !errors.Is(err, ErrInvalid) {
+				t.Fatalf("want ErrInvalid, got %v", err)
+			}
+		})
+	}
+}
+
+func TestEnvValidation(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+	if err := Set(ctx, db, "p", goodMeta()); err != nil {
+		t.Fatal(err)
+	}
+	cases := []struct {
+		key, value string
+		ok         bool
+	}{
+		{"VOCDONI_KB_TOKEN", "x", true},
+		{"_A", "x", true},
+		{"A" + strings.Repeat("B", 63), "x", true},
+		{"A" + strings.Repeat("B", 64), "x", false},
+		{"lower", "x", false},
+		{"1ABC", "x", false},
+		{"A-B", "x", false},
+		{"", "x", false},
+		{"BIG", strings.Repeat("v", 4096), true},
+		{"BIG", strings.Repeat("v", 4097), false},
+		{"MULTI", "line one\nline two\tTabbed", true},
+		{"NUL", "a\x00b", false},
+		{"ESC", "a\x1bb", false},
+	}
+	for _, tc := range cases {
+		err := SetEnv(ctx, db, "p", map[string]string{tc.key: tc.value})
+		if tc.ok && err != nil {
+			t.Errorf("%q (%d bytes) rejected: %v", tc.key, len(tc.value), err)
+		}
+		if !tc.ok && !errors.Is(err, ErrInvalid) {
+			t.Errorf("%q (%d bytes): want ErrInvalid, got %v", tc.key, len(tc.value), err)
+		}
+	}
+	// One bad key rejects the whole call: nothing is written.
+	err := SetEnv(ctx, db, "p", map[string]string{"GOOD_NEW": "1", "bad": "2"})
+	if !errors.Is(err, ErrInvalid) {
+		t.Fatalf("mixed batch: %v", err)
+	}
+	p, _ := Get(ctx, db, "p")
+	for _, k := range p.EnvKeys {
+		if k == "GOOD_NEW" {
+			t.Fatal("partial write of a rejected batch")
+		}
+	}
+	if err := SetEnv(ctx, db, "missing", map[string]string{"A": "1"}); !errors.Is(err, ErrNotFound) {
+		t.Fatalf("env on a missing plugin: %v", err)
+	}
+}
+
+func TestCaps(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+	for i := range MaxPlugins {
+		if err := Set(ctx, db, fmt.Sprintf("p%d", i), goodMeta()); err != nil {
+			t.Fatalf("plugin %d: %v", i, err)
+		}
+	}
+	if err := Set(ctx, db, "one-too-many", goodMeta()); !errors.Is(err, ErrInvalid) {
+		t.Fatalf("33rd plugin: %v", err)
+	}
+	// Replacing an existing plugin's metadata is not a new plugin.
+	if err := Set(ctx, db, "p0", goodMeta()); err != nil {
+		t.Fatalf("update at the cap: %v", err)
+	}
+
+	env := make(map[string]string)
+	for i := range MaxEnvKeys {
+		env[fmt.Sprintf("K%d", i)] = "v"
+	}
+	if err := SetEnv(ctx, db, "p0", env); err != nil {
+		t.Fatalf("64 keys: %v", err)
+	}
+	if err := SetEnv(ctx, db, "p0", map[string]string{"K0": "new"}); err != nil {
+		t.Fatalf("overwrite at the cap: %v", err)
+	}
+	if err := SetEnv(ctx, db, "p0", map[string]string{"K64": "v"}); !errors.Is(err, ErrInvalid) {
+		t.Fatalf("65th key: %v", err)
+	}
+	p, _ := Get(ctx, db, "p0")
+	if len(p.EnvKeys) != MaxEnvKeys {
+		t.Fatalf("keys after refused add = %d", len(p.EnvKeys))
+	}
+}
+
+func TestLifecycle(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+	if err := Set(ctx, db, "vocdoni-internal", goodMeta()); err != nil {
+		t.Fatal(err)
+	}
+	if err := SetEnv(ctx, db, "vocdoni-internal", map[string]string{"NOTION_TOKEN": "n-secret", "VOCDONI_KB_TOKEN": "kb-secret"}); err != nil {
+		t.Fatal(err)
+	}
+	if err := Set(ctx, db, "other", Meta{Version: "1", BundleURL: "https://example.org/o.zip", BundleSHA256: sha}); err != nil {
+		t.Fatal(err)
+	}
+
+	list, err := List(ctx, db)
+	if err != nil || len(list) != 2 {
+		t.Fatalf("list: %v %+v", err, list)
+	}
+	vi := list[1]
+	if vi.Name != "vocdoni-internal" || !vi.Enabled || vi.InstalledID != "vocdoni-internal@vocdoni" ||
+		strings.Join(vi.EnvKeys, ",") != "NOTION_TOKEN,VOCDONI_KB_TOKEN" || vi.UpdatedAt.IsZero() {
+		t.Fatalf("list entry: %+v", vi)
+	}
+
+	// Metadata replacement keeps env and the enabled flag.
+	if err := Enable(ctx, db, "vocdoni-internal", false); err != nil {
+		t.Fatal(err)
+	}
+	m := goodMeta()
+	m.Version = "1.5.0"
+	if err := Set(ctx, db, "vocdoni-internal", m); err != nil {
+		t.Fatal(err)
+	}
+	got, err := Get(ctx, db, "vocdoni-internal")
+	if err != nil || got.Version != "1.5.0" || got.Enabled || len(got.EnvKeys) != 2 {
+		t.Fatalf("after replace: %v %+v", err, got)
+	}
+
+	// Disabled plugins are not served to clients.
+	client, err := ListForClient(ctx, db)
+	if err != nil || len(client) != 1 || client[0].Name != "other" || len(client[0].Env) != 0 {
+		t.Fatalf("client list with vocdoni-internal disabled: %v %+v", err, client)
+	}
+	if err := Enable(ctx, db, "vocdoni-internal", true); err != nil {
+		t.Fatal(err)
+	}
+	client, _ = ListForClient(ctx, db)
+	if len(client) != 2 || client[1].Env["VOCDONI_KB_TOKEN"] != "kb-secret" || client[1].BundleSHA256 != sha {
+		t.Fatalf("client list: %+v", client)
+	}
+
+	if err := UnsetEnv(ctx, db, "vocdoni-internal", "NOTION_TOKEN"); err != nil {
+		t.Fatal(err)
+	}
+	if err := UnsetEnv(ctx, db, "vocdoni-internal", "NOTION_TOKEN"); !errors.Is(err, ErrNotFound) {
+		t.Fatalf("unset twice: %v", err)
+	}
+	if err := Delete(ctx, db, "vocdoni-internal"); err != nil {
+		t.Fatal(err)
+	}
+	var n int
+	_ = db.QueryRow(`SELECT COUNT(*) FROM plugin_env`).Scan(&n)
+	if n != 0 {
+		t.Fatalf("env rows survive delete: %d", n)
+	}
+	for _, err := range []error{
+		Delete(ctx, db, "vocdoni-internal"),
+		Enable(ctx, db, "vocdoni-internal", true),
+		UnsetEnv(ctx, db, "vocdoni-internal", "A"),
+	} {
+		if !errors.Is(err, ErrNotFound) {
+			t.Fatalf("missing plugin: %v", err)
+		}
+	}
+	if _, err := Get(ctx, db, "vocdoni-internal"); !errors.Is(err, ErrNotFound) {
+		t.Fatalf("get missing: %v", err)
+	}
+}
+
+func TestErrorsNeverCarryValues(t *testing.T) {
+	ctx := context.Background()
+	db := testDB(t)
+	_ = Set(ctx, db, "p", goodMeta())
+	err := SetEnv(ctx, db, "p", map[string]string{"KEY": "super-secret\x00"})
+	if err == nil || strings.Contains(err.Error(), "super-secret") {
+		t.Fatalf("error leaks the value: %v", err)
+	}
+}
diff --git a/internal/store/store_test.go b/internal/store/store_test.go
index a624936..6f68b85 100644
--- a/internal/store/store_test.go
+++ b/internal/store/store_test.go
@@ -13,7 +13,7 @@ func TestOpenAppliesSchemaAndIsIdempotent(t *testing.T) {
 		t.Fatalf("open: %v", err)
 	}
 	// Schema applied: all expected tables exist.
-	for _, tbl := range []string{"credentials", "conversations", "rr_cursor", "user_tokens", "request_log", "usage_history"} {
+	for _, tbl := range []string{"credentials", "conversations", "rr_cursor", "user_tokens", "request_log", "usage_history", "plugins", "plugin_env"} {
 		var name string
 		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
 		if err != nil {
````
Run: `go test ./internal/plugins/ ./internal/store/`
Expected: `FAIL	github.com/p4u/claude-proxy/internal/plugins [build failed]` (undefined: Set, Meta, ErrInvalid …)

Step 2: implement
````diff
diff --git a/internal/plugins/plugins.go b/internal/plugins/plugins.go
new file mode 100644
index 0000000..939cbd1
--- /dev/null
+++ b/internal/plugins/plugins.go
@@ -0,0 +1,340 @@
+// Package plugins stores the Claude Code plugins the proxy tells claudio to
+// load: for each, a pinned bundle (URL and SHA-256 of a zip released from the
+// content repo) and the environment its MCP servers and hooks need, secrets
+// included.
+//
+// The proxy never fetches or serves the bundle. This package owns every
+// validation limit; the CLI, the web UI routes and GET /v1/claudio/plugins
+// all go through it. Env values are only ever returned by ListForClient.
+package plugins
+
+import (
+	"context"
+	"database/sql"
+	"errors"
+	"fmt"
+	"net/url"
+	"regexp"
+	"time"
+
+	"github.com/p4u/claude-proxy/internal/store"
+)
+
+// Limits.
+const (
+	MaxPlugins    = 32
+	MaxEnvKeys    = 64
+	MaxValueBytes = 4096
+	maxVersionLen = 64
+	maxURLLen     = 2048
+)
+
+var (
+	// ErrNotFound is returned for an unknown plugin or env key.
+	ErrNotFound = errors.New("plugin not found")
+	// ErrInvalid wraps every validation and limit error (a 400 for callers).
+	ErrInvalid = errors.New("invalid plugin")
+
+	nameRE      = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
+	installedRE = regexp.MustCompile(`^[a-z0-9-]{1,40}@[a-z0-9-]{1,40}$`)
+	shaRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
+	keyRE       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
+)
+
+// Meta is a plugin's bundle reference, the part PUT /api/plugins/{name} and
+// `plugins set` replace.
+type Meta struct {
+	Version      string
+	BundleURL    string
+	BundleSHA256 string
+	InstalledID  string
+}
+
+// Plugin is a stored plugin as admins see it: env keys, never values.
+type Plugin struct {
+	Name string
+	Meta
+	Enabled   bool
+	UpdatedAt time.Time
+	EnvKeys   []string // sorted
+}
+
+// ClientPlugin is an enabled plugin as served to claudio, env values included.
+type ClientPlugin struct {
+	Name string
+	Meta
+	Env map[string]string
+}
+
+func invalid(format string, args ...any) error {
+	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
+}
+
+// ValidateName checks a plugin name: [a-z0-9-]{1,40}.
+func ValidateName(name string) error {
+	if !nameRE.MatchString(name) {
+		return invalid("name must match [a-z0-9-]{1,40}")
+	}
+	return nil
+}
+
+// Validate checks a bundle reference.
+func (m Meta) Validate() error {
+	if len(m.Version) < 1 || len(m.Version) > maxVersionLen || !printableASCII(m.Version) {
+		return invalid("version must be 1-%d printable ASCII characters", maxVersionLen)
+	}
+	u, err := url.Parse(m.BundleURL)
+	if len(m.BundleURL) > maxURLLen || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
+		return invalid("bundle_url must be an https URL without credentials")
+	}
+	if !shaRE.MatchString(m.BundleSHA256) {
+		return invalid("bundle_sha256 must be 64 lowercase hex characters")
+	}
+	if m.InstalledID != "" && !installedRE.MatchString(m.InstalledID) {
+		return invalid("installed_id must be empty or name@marketplace ([a-z0-9-]{1,40} each)")
+	}
+	return nil
+}
+
+// ValidateEnv checks env keys and values. Errors name the key, never the value.
+func ValidateEnv(env map[string]string) error {
+	for k, v := range env {
+		if !keyRE.MatchString(k) {
+			return invalid("env key %q must match [A-Z_][A-Z0-9_]{0,63}", k)
+		}
+		if len(v) > MaxValueBytes {
+			return invalid("env value of %s is over %d bytes", k, MaxValueBytes)
+		}
+		// claudio refuses a plugin whose values hold NUL or C0 controls other
+		// than \n and \t; refuse them here so an admin finds out at save time.
+		for i := 0; i < len(v); i++ {
+			if c := v[i]; c < 0x20 && c != '\n' && c != '\t' {
+				return invalid("env value of %s holds a control character", k)
+			}
+		}
+	}
+	return nil
+}
+
+func printableASCII(s string) bool {
+	for i := 0; i < len(s); i++ {
+		if s[i] < 0x20 || s[i] > 0x7e {
+			return false
+		}
+	}
+	return true
+}
+
+// Set creates a plugin or replaces its bundle reference. A new plugin is
+// enabled; an existing one keeps its enabled flag and env.
+func Set(ctx context.Context, db *store.DB, name string, m Meta) error {
+	if err := ValidateName(name); err != nil {
+		return err
+	}
+	if err := m.Validate(); err != nil {
+		return err
+	}
+	tx, err := db.BeginTx(ctx, nil)
+	if err != nil {
+		return err
+	}
+	defer tx.Rollback()
+	var exists, total int
+	if err := tx.QueryRowContext(ctx,
+		`SELECT COALESCE(SUM(name = ?), 0), COUNT(*) FROM plugins`, name).Scan(&exists, &total); err != nil {
+		return err
+	}
+	if exists == 0 && total >= MaxPlugins {
+		return invalid("at most %d plugins", MaxPlugins)
+	}
+	if _, err := tx.ExecContext(ctx, `
+		INSERT INTO plugins (name, version, bundle_url, bundle_sha256, installed_id, updated_at)
+		VALUES (?, ?, ?, ?, ?, ?)
+		ON CONFLICT(name) DO UPDATE SET version=excluded.version, bundle_url=excluded.bundle_url,
+			bundle_sha256=excluded.bundle_sha256, installed_id=excluded.installed_id,
+			updated_at=excluded.updated_at`,
+		name, m.Version, m.BundleURL, m.BundleSHA256, m.InstalledID, time.Now().Unix()); err != nil {
+		return err
+	}
+	return tx.Commit()
+}
+
+// Enable turns a plugin on or off for clients.
+func Enable(ctx context.Context, db *store.DB, name string, on bool) error {
+	v := 0
+	if on {
+		v = 1
+	}
+	res, err := db.ExecContext(ctx, `UPDATE plugins SET enabled=?, updated_at=? WHERE name=?`,
+		v, time.Now().Unix(), name)
+	return affected(res, err)
+}
+
+// Delete removes a plugin and, by cascade, its env.
+func Delete(ctx context.Context, db *store.DB, name string) error {
+	res, err := db.ExecContext(ctx, `DELETE FROM plugins WHERE name=?`, name)
+	return affected(res, err)
+}
+
+// SetEnv upserts the listed keys of a plugin's env, leaving the others. The
+// whole call is validated first and applied in one transaction.
+func SetEnv(ctx context.Context, db *store.DB, name string, env map[string]string) error {
+	if err := ValidateEnv(env); err != nil {
+		return err
+	}
+	tx, err := db.BeginTx(ctx, nil)
+	if err != nil {
+		return err
+	}
+	defer tx.Rollback()
+	res, err := tx.ExecContext(ctx, `UPDATE plugins SET updated_at=? WHERE name=?`, time.Now().Unix(), name)
+	if err := affected(res, err); err != nil {
+		return err
+	}
+	for k, v := range env {
+		if _, err := tx.ExecContext(ctx, `
+			INSERT INTO plugin_env (plugin, key, value) VALUES (?, ?, ?)
+			ON CONFLICT(plugin, key) DO UPDATE SET value=excluded.value`, name, k, v); err != nil {
+			return err
+		}
+	}
+	var n int
+	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_env WHERE plugin=?`, name).Scan(&n); err != nil {
+		return err
+	}
+	if n > MaxEnvKeys {
+		return invalid("at most %d env keys per plugin", MaxEnvKeys)
+	}
+	return tx.Commit()
+}
+
+// UnsetEnv removes env keys of a plugin. ErrNotFound when the plugin is
+// unknown or none of the keys existed.
+func UnsetEnv(ctx context.Context, db *store.DB, name string, keys ...string) error {
+	tx, err := db.BeginTx(ctx, nil)
+	if err != nil {
+		return err
+	}
+	defer tx.Rollback()
+	var removed int64
+	for _, k := range keys {
+		res, err := tx.ExecContext(ctx, `DELETE FROM plugin_env WHERE plugin=? AND key=?`, name, k)
+		if err != nil {
+			return err
+		}
+		n, _ := res.RowsAffected()
+		removed += n
+	}
+	if removed == 0 {
+		return ErrNotFound
+	}
+	if _, err := tx.ExecContext(ctx, `UPDATE plugins SET updated_at=? WHERE name=?`, time.Now().Unix(), name); err != nil {
+		return err
+	}
+	return tx.Commit()
+}
+
+// Get returns one plugin with its env keys.
+func Get(ctx context.Context, db *store.DB, name string) (*Plugin, error) {
+	found, err := list(ctx, db, name)
+	if err != nil {
+		return nil, err
+	}
+	if len(found) == 0 {
+		return nil, ErrNotFound
+	}
+	return &found[0], nil
+}
+
+// List returns every plugin, by name, with env keys and no values.
+func List(ctx context.Context, db *store.DB) ([]Plugin, error) {
+	return list(ctx, db, "")
+}
+
+// list reads the plugins (all when only is "") and their env keys.
+func list(ctx context.Context, db *store.DB, only string) ([]Plugin, error) {
+	rows, err := db.QueryContext(ctx, `
+		SELECT name, version, bundle_url, bundle_sha256, installed_id, enabled, updated_at
+		FROM plugins WHERE ? = '' OR name = ? ORDER BY name`, only, only)
+	if err != nil {
+		return nil, err
+	}
+	var out []Plugin
+	idx := make(map[string]int)
+	for rows.Next() {
+		var p Plugin
+		var updated int64
+		if err := rows.Scan(&p.Name, &p.Version, &p.BundleURL, &p.BundleSHA256, &p.InstalledID,
+			&p.Enabled, &updated); err != nil {
+			rows.Close()
+			return nil, err
+		}
+		p.UpdatedAt = time.Unix(updated, 0)
+		p.EnvKeys = []string{}
+		idx[p.Name] = len(out)
+		out = append(out, p)
+	}
+	rows.Close()
+	if err := rows.Err(); err != nil {
+		return nil, err
+	}
+	keys, err := db.QueryContext(ctx,
+		`SELECT plugin, key FROM plugin_env WHERE ? = '' OR plugin = ? ORDER BY plugin, key`, only, only)
+	if err != nil {
+		return nil, err
+	}
+	defer keys.Close()
+	for keys.Next() {
+		var plugin, key string
+		if err := keys.Scan(&plugin, &key); err != nil {
+			return nil, err
+		}
+		if i, ok := idx[plugin]; ok {
+			out[i].EnvKeys = append(out[i].EnvKeys, key)
+		}
+	}
+	return out, keys.Err()
+}
+
+// ListForClient returns the enabled plugins, by name, with their env values.
+// Only GET /v1/claudio/plugins may call it.
+func ListForClient(ctx context.Context, db *store.DB) ([]ClientPlugin, error) {
+	all, err := List(ctx, db)
+	if err != nil {
+		return nil, err
+	}
+	out := make([]ClientPlugin, 0, len(all))
+	idx := make(map[string]int)
+	for _, p := range all {
+		if !p.Enabled {
+			continue
+		}
+		idx[p.Name] = len(out)
+		out = append(out, ClientPlugin{Name: p.Name, Meta: p.Meta, Env: map[string]string{}})
+	}
+	rows, err := db.QueryContext(ctx, `SELECT plugin, key, value FROM plugin_env`)
+	if err != nil {
+		return nil, err
+	}
+	defer rows.Close()
+	for rows.Next() {
+		var plugin, key, value string
+		if err := rows.Scan(&plugin, &key, &value); err != nil {
+			return nil, err
+		}
+		if i, ok := idx[plugin]; ok {
+			out[i].Env[key] = value
+		}
+	}
+	return out, rows.Err()
+}
+
+func affected(res sql.Result, err error) error {
+	if err != nil {
+		return err
+	}
+	if n, _ := res.RowsAffected(); n == 0 {
+		return ErrNotFound
+	}
+	return nil
+}
diff --git a/internal/store/schema.go b/internal/store/schema.go
index d1f7964..ceae48e 100644
--- a/internal/store/schema.go
+++ b/internal/store/schema.go
@@ -157,6 +157,27 @@ CREATE TABLE IF NOT EXISTS codex_account_weight (
   weight  INTEGER NOT NULL DEFAULT 1
 );
 
+-- Claude Code plugins served to claudio (GET /v1/claudio/plugins): a pinned
+-- bundle reference per plugin, never the bundle itself (internal/plugins).
+CREATE TABLE IF NOT EXISTS plugins (
+  name          TEXT PRIMARY KEY,
+  version       TEXT NOT NULL,
+  bundle_url    TEXT NOT NULL,
+  bundle_sha256 TEXT NOT NULL,
+  -- '' or the installed Claude Code plugin id that makes the bundle redundant.
+  installed_id  TEXT NOT NULL DEFAULT '',
+  enabled       INTEGER NOT NULL DEFAULT 1,
+  updated_at    INTEGER NOT NULL
+);
+-- Environment a plugin's MCP servers and hooks need, secrets included; stored
+-- like the other secrets in this file. Never returned by the admin API.
+CREATE TABLE IF NOT EXISTS plugin_env (
+  plugin TEXT NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
+  key    TEXT NOT NULL,
+  value  TEXT NOT NULL,
+  PRIMARY KEY (plugin, key)
+);
+
 -- Random secrets generated once and kept across restarts (e.g. the web UI
 -- session-signing secret). Never exposed through any API.
 CREATE TABLE IF NOT EXISTS app_secret (
````
Run: `go test ./internal/plugins/ ./internal/store/ && GOTOOLCHAIN=go1.26.2 make lint`
Expected: `ok  	github.com/p4u/claude-proxy/internal/plugins`, `ok  	github.com/p4u/claude-proxy/internal/store`; lint `0 issues.`

Step 3: commit
`git add internal/plugins/plugins.go internal/plugins/plugins_test.go` then `git commit -m "feat(plugins): store plugin bundle references and env" -- internal/plugins/plugins.go internal/plugins/plugins_test.go internal/store/schema.go internal/store/store_test.go`

Interfaces this task provides to later tasks: `plugins.Meta{Version,BundleURL,BundleSHA256,InstalledID}` + `Validate()`; `plugins.Plugin{Name, Meta, Enabled, UpdatedAt, EnvKeys}`; `plugins.ClientPlugin{Name, Meta, Env}`; `ErrNotFound`, `ErrInvalid` (wraps every validation/limit error → 400); `MaxPlugins=32`, `MaxEnvKeys=64`, `MaxValueBytes=4096`; `ValidateName`, `ValidateEnv`; `Set`, `Enable(ctx, db, name, on)`, `Delete`, `SetEnv(ctx, db, name, map)`, `UnsetEnv(ctx, db, name, keys...)`, `Get`, `List`, `ListForClient`.

### Task 4: GET /v1/claudio/plugins
Model: standard   Repo: /volumes/repos/claude-proxy   Files: internal/claudioapi/claudioapi.go, internal/claudioapi/claudioapi_test.go, internal/claudioapi/plugins_test.go
Goal: Spec §3 client endpoint: user token only (admin/anonymous 403, `serveStats` shape), enabled plugins only, `Cache-Control: no-store`, debug log without values (AC3 first half).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-4.patch

Notes: The existing per-identity bucket applies unchanged. `/v1/claudio/plugins` joins `TestNoRequestLogWrite`. Adding `"plugins"` to the discovery capabilities is additive (tests check membership, not the exact list).

Step 1: write the failing test
````diff
diff --git a/internal/claudioapi/claudioapi_test.go b/internal/claudioapi/claudioapi_test.go
index 3be2501..d0e894f 100644
--- a/internal/claudioapi/claudioapi_test.go
+++ b/internal/claudioapi/claudioapi_test.go
@@ -114,6 +114,7 @@ func TestNoRequestLogWrite(t *testing.T) {
 		"/v1/claudio/config",
 		"/v1/claudio/models",
 		"/v1/claudio/pool/health",
+		"/v1/claudio/plugins",
 	} {
 		req := httptest.NewRequest(http.MethodGet, path, nil)
 		rw := httptest.NewRecorder()
diff --git a/internal/claudioapi/plugins_test.go b/internal/claudioapi/plugins_test.go
new file mode 100644
index 0000000..a7990ca
--- /dev/null
+++ b/internal/claudioapi/plugins_test.go
@@ -0,0 +1,107 @@
+package claudioapi_test
+
+import (
+	"context"
+	"encoding/json"
+	"net/http"
+	"net/http/httptest"
+	"strings"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/plugins"
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+func getPlugins(t *testing.T, mux http.Handler, id *usertoken.Identity) *httptest.ResponseRecorder {
+	t.Helper()
+	req := httptest.NewRequest(http.MethodGet, "/v1/claudio/plugins", nil)
+	if id != nil {
+		req = withIdentity(req, id)
+	}
+	rw := httptest.NewRecorder()
+	mux.ServeHTTP(rw, req)
+	return rw
+}
+
+func TestPluginsRequireUserToken(t *testing.T) {
+	mux, _ := setupHandler(t, &fakeCatalogue{})
+	for name, id := range map[string]*usertoken.Identity{
+		"anonymous":  nil,
+		"admin":      {IsAdmin: true},
+		"empty user": {UserTokenID: ""},
+	} {
+		if rw := getPlugins(t, mux, id); rw.Code != http.StatusForbidden {
+			t.Errorf("%s: got %d, want 403; body=%s", name, rw.Code, rw.Body.String())
+		}
+	}
+}
+
+func TestPluginsServesEnabledWithEnv(t *testing.T) {
+	mux, db := setupHandler(t, &fakeCatalogue{})
+	ctx := context.Background()
+	user := &usertoken.Identity{UserTokenID: "utok_a", UserName: "alice"}
+
+	rw := getPlugins(t, mux, user)
+	if rw.Code != http.StatusOK || strings.TrimSpace(rw.Body.String()) != `{"plugins":[]}` {
+		t.Fatalf("empty: %d %s", rw.Code, rw.Body.String())
+	}
+
+	sha := strings.Repeat("0f", 32)
+	if err := plugins.Set(ctx, db, "vocdoni-internal", plugins.Meta{
+		Version: "1.4.0", BundleURL: "https://example.org/vi.zip", BundleSHA256: sha,
+		InstalledID: "vocdoni-internal@vocdoni",
+	}); err != nil {
+		t.Fatal(err)
+	}
+	if err := plugins.SetEnv(ctx, db, "vocdoni-internal", map[string]string{"VOCDONI_KB_TOKEN": "kb-secret"}); err != nil {
+		t.Fatal(err)
+	}
+	if err := plugins.Set(ctx, db, "off", plugins.Meta{Version: "1", BundleURL: "https://example.org/off.zip", BundleSHA256: sha}); err != nil {
+		t.Fatal(err)
+	}
+	if err := plugins.Enable(ctx, db, "off", false); err != nil {
+		t.Fatal(err)
+	}
+
+	rw = getPlugins(t, mux, user)
+	if rw.Code != http.StatusOK {
+		t.Fatalf("status %d %s", rw.Code, rw.Body.String())
+	}
+	if cc := rw.Header().Get("Cache-Control"); cc != "no-store" {
+		t.Fatalf("Cache-Control = %q", cc)
+	}
+	var body struct {
+		Plugins []struct {
+			Name   string `json:"name"`
+			Ver    string `json:"version"`
+			Bundle struct {
+				URL    string `json:"url"`
+				SHA256 string `json:"sha256"`
+			} `json:"bundle"`
+			InstalledID string            `json:"installed_id"`
+			Env         map[string]string `json:"env"`
+		} `json:"plugins"`
+	}
+	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
+		t.Fatal(err)
+	}
+	if len(body.Plugins) != 1 {
+		t.Fatalf("plugins = %+v (disabled one must be omitted)", body.Plugins)
+	}
+	p := body.Plugins[0]
+	if p.Name != "vocdoni-internal" || p.Ver != "1.4.0" || p.Bundle.URL != "https://example.org/vi.zip" ||
+		p.Bundle.SHA256 != sha || p.InstalledID != "vocdoni-internal@vocdoni" || p.Env["VOCDONI_KB_TOKEN"] != "kb-secret" {
+		t.Fatalf("plugin = %+v", p)
+	}
+}
+
+func TestPluginsWrongMethod(t *testing.T) {
+	mux, _ := setupHandler(t, &fakeCatalogue{})
+	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/claudio/plugins", nil),
+		&usertoken.Identity{UserTokenID: "utok_a"})
+	rw := httptest.NewRecorder()
+	mux.ServeHTTP(rw, req)
+	if rw.Code != http.StatusMethodNotAllowed {
+		t.Fatalf("POST = %d", rw.Code)
+	}
+}
````
Run: `go test ./internal/claudioapi/ -run Plugins`
Expected: `--- FAIL: TestPluginsRequireUserToken`, `--- FAIL: TestPluginsServesEnabledWithEnv`, `--- FAIL: TestPluginsWrongMethod` (local 404 for an unknown route)

Step 2: implement
````diff
diff --git a/internal/claudioapi/claudioapi.go b/internal/claudioapi/claudioapi.go
index 5866410..9c1882c 100644
--- a/internal/claudioapi/claudioapi.go
+++ b/internal/claudioapi/claudioapi.go
@@ -16,6 +16,7 @@
 //	GET /v1/claudio/pool/health — coarse per-provider availability
 //	GET /v1/claudio/session?id= — which credential serves a Claude Code session
 //	POST /v1/claudio/session/switch — move a session to another credential
+//	GET /v1/claudio/plugins    — plugins to load, with their env (user token only)
 //
 // Security isolation
 //
@@ -24,8 +25,9 @@
 //     existence is not leaked across users. It exposes the credential's id,
 //     label, provider and plan, never tokens or other secrets. /session/switch
 //     is scoped the same way.
-//   - /me/stats requires a non-empty, non-admin UserTokenID; anonymous
-//     callers and admin tokens receive 403.
+//   - /me/stats and /plugins require a non-empty, non-admin UserTokenID;
+//     anonymous callers and admin tokens receive 403. /plugins carries
+//     secrets: no-store, and logged without values.
 //   - /pool/health exposes no credential IDs, labels, percentages or counts —
 //     only a coarse status string per provider name.
 //   - All caches are keyed by identity so cross-user data is impossible.
@@ -38,12 +40,14 @@ import (
 	"encoding/json"
 	"errors"
 	"io"
+	"log/slog"
 	"net/http"
 	"strings"
 	"sync"
 	"time"
 
 	"github.com/p4u/claude-proxy/internal/creds"
+	"github.com/p4u/claude-proxy/internal/plugins"
 	"github.com/p4u/claude-proxy/internal/pool"
 	"github.com/p4u/claude-proxy/internal/provider"
 	"github.com/p4u/claude-proxy/internal/sessionbind"
@@ -198,6 +202,8 @@ func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
 		h.methodOnly(w, r, http.MethodGet, h.serveSession)
 	case "/v1/claudio/session/switch":
 		h.methodOnly(w, r, http.MethodPost, h.serveSessionSwitch)
+	case "/v1/claudio/plugins":
+		h.methodOnly(w, r, http.MethodGet, h.servePlugins)
 	default:
 		// Includes /v1/claudiox and any other path that starts with
 		// /v1/claudio but does not match a known route.
@@ -228,7 +234,7 @@ type rootResponse struct {
 func (h *Handler) serveRoot(w http.ResponseWriter, r *http.Request) {
 	writeJSON(w, http.StatusOK, rootResponse{
 		Version:      1,
-		Capabilities: []string{"config", "models", "me/stats", "pool/health", "session", "session_switch"},
+		Capabilities: []string{"config", "models", "me/stats", "pool/health", "session", "session_switch", "plugins"},
 	})
 }
 
@@ -489,6 +495,58 @@ func parsePeriod(s string) (time.Duration, string, bool) {
 	return 0, "", false
 }
 
+// ---------------------------------------------------------------------------
+// GET /v1/claudio/plugins
+// ---------------------------------------------------------------------------
+
+type pluginsResponse struct {
+	Plugins []pluginEntry `json:"plugins"`
+}
+
+type pluginEntry struct {
+	Name        string            `json:"name"`
+	Version     string            `json:"version"`
+	Bundle      pluginBundle      `json:"bundle"`
+	InstalledID string            `json:"installed_id"`
+	Env         map[string]string `json:"env"`
+}
+
+type pluginBundle struct {
+	URL    string `json:"url"`
+	SHA256 string `json:"sha256"`
+}
+
+// servePlugins tells a member's claudio which plugins to load. The reply
+// holds the plugins' secrets: user tokens only, never cached, never logged.
+func (h *Handler) servePlugins(w http.ResponseWriter, r *http.Request) {
+	id := usertoken.FromContext(r.Context())
+	if id == nil || id.IsAdmin || id.UserTokenID == "" {
+		writeJSON(w, http.StatusForbidden, apiError("authentication_error",
+			"claudio: /plugins requires a user token identity (admin and anonymous callers not allowed)"))
+		return
+	}
+	list, err := plugins.ListForClient(r.Context(), h.db)
+	if err != nil {
+		writeJSON(w, http.StatusInternalServerError, apiError("api_error", "claudio: plugins query failed"))
+		return
+	}
+	resp := pluginsResponse{Plugins: make([]pluginEntry, 0, len(list))}
+	keys := 0
+	for _, p := range list {
+		resp.Plugins = append(resp.Plugins, pluginEntry{
+			Name:        p.Name,
+			Version:     p.Version,
+			Bundle:      pluginBundle{URL: p.BundleURL, SHA256: p.BundleSHA256},
+			InstalledID: p.InstalledID,
+			Env:         p.Env,
+		})
+		keys += len(p.Env)
+	}
+	slog.Debug("claudio plugins served", "user", id.UserName, "plugins", len(resp.Plugins), "env_keys", keys)
+	w.Header().Set("Cache-Control", "no-store")
+	writeJSON(w, http.StatusOK, resp)
+}
+
 func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
 	id := usertoken.FromContext(r.Context())
 	// Admin, anonymous (nil identity), or empty UserTokenID → 403.
````
Run: `go test ./internal/claudioapi/ && GOTOOLCHAIN=go1.26.2 make lint`
Expected: `ok  	github.com/p4u/claude-proxy/internal/claudioapi`; lint `0 issues.`

Step 3: commit
`git add internal/claudioapi/plugins_test.go` then `git commit -m "feat(claudioapi): serve enabled plugins to user tokens" -- internal/claudioapi/claudioapi.go internal/claudioapi/claudioapi_test.go internal/claudioapi/plugins_test.go`

Interfaces this task provides to later tasks: Route `GET /v1/claudio/plugins` → `{"plugins":[{name,version,bundle:{url,sha256},installed_id,env}]}` (`[]` when none); discovery capabilities gain `"plugins"`.

### Task 5: Device login state machine, routes and the AuthMiddleware exemption
Model: strong   Repo: /volumes/repos/claude-proxy   Files: internal/claudioapi/claudioapi.go, internal/claudioapi/login.go, internal/claudioapi/login_stack_test.go, internal/claudioapi/login_test.go, internal/proxy/auth.go, internal/proxy/auth_test.go
Goal: Spec §2: `POST /v1/claudio/login/start|poll`, in-memory `Logins` keyed by SHA-256(device_code), per-IP limiters replacing the identity bucket, slow_down, pending caps, eviction, sweeper, redeem-once with status re-check, 404 without SSO; AuthMiddleware exemption (AC1, AC5).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-5.patch

Notes: States: pending → approved | denied | expired; approved → redeeming (one poll, under the mutex) → deleted on the 200, or denied (user not active), deleted (user row gone → 404), or back to approved (DB error → 500, retry). Choices the spec leaves open, made here: start over 5/min/IP answers `429 too_many_pending` (claudio's message for it fits); poll over 40/min/IP answers `429 slow_down`; a malformed poll body is `400 {"error":"invalid_request"}`; an approved login not redeemed within the TTL answers 410 like a pending one. Eviction drops the front of the insertion list (oldest entry of any state). Pending-per-IP counts unexpired pending entries only. The exemption compares `r.URL.EscapedPath()` exactly, so `/start/`, `/startx`, `/../poll` and `%2F` variants still need a credential; `ServeHTTP` matches the same exact paths before the identity bucket.

Step 1: write the failing test
````diff
diff --git a/internal/claudioapi/login_stack_test.go b/internal/claudioapi/login_stack_test.go
new file mode 100644
index 0000000..406bc44
--- /dev/null
+++ b/internal/claudioapi/login_stack_test.go
@@ -0,0 +1,68 @@
+package claudioapi_test
+
+import (
+	"context"
+	"net/http"
+	"net/http/httptest"
+	"path/filepath"
+	"strings"
+	"sync/atomic"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/claudioapi"
+	"github.com/p4u/claude-proxy/internal/proxy"
+	"github.com/p4u/claude-proxy/internal/store"
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+// TestLoginThroughFullStack runs the login routes through the production
+// chain (AuthMiddleware → WrapHandler → mux): no credential and a stale one
+// both reach the local handler, nothing is forwarded or logged, and a
+// look-alike path still needs a credential.
+func TestLoginThroughFullStack(t *testing.T) {
+	db, err := store.Open(filepath.Join(t.TempDir(), "stack.db"))
+	if err != nil {
+		t.Fatal(err)
+	}
+	defer db.Close()
+	if _, err := usertoken.Create(context.Background(), db, "someone"); err != nil {
+		t.Fatal(err)
+	}
+	var hits atomic.Int64
+	mux := http.NewServeMux()
+	claudioH := claudioapi.New(mux, db, &fakeCatalogue{})
+	claudioH.SetLogins(claudioapi.NewLogins("https://proxy.test/claudio/login"))
+	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
+	handler := proxy.AuthMiddleware("admin-secret", db, true, claudioH.WrapHandler(mux))
+
+	for _, bearer := range []string{"", "stale-token"} {
+		req := httptest.NewRequest(http.MethodPost, "/v1/claudio/login/start", nil)
+		if bearer != "" {
+			req.Header.Set("Authorization", "Bearer "+bearer)
+		}
+		rw := httptest.NewRecorder()
+		handler.ServeHTTP(rw, req)
+		if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"device_code"`) {
+			t.Fatalf("start with bearer %q: %d %s", bearer, rw.Code, rw.Body.String())
+		}
+		req = httptest.NewRequest(http.MethodPost, "/v1/claudio/login/poll", strings.NewReader(`{"device_code":"nope"}`))
+		rw = httptest.NewRecorder()
+		handler.ServeHTTP(rw, req)
+		if rw.Code != http.StatusNotFound {
+			t.Fatalf("poll unknown: %d %s", rw.Code, rw.Body.String())
+		}
+	}
+	rw := httptest.NewRecorder()
+	handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/v1/claudio/login/../poll", nil))
+	if rw.Code != http.StatusUnauthorized {
+		t.Fatalf("dot-segment login path: %d", rw.Code)
+	}
+	if hits.Load() != 0 {
+		t.Fatalf("forwarded upstream %d times", hits.Load())
+	}
+	var n int
+	_ = db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&n)
+	if n != 0 {
+		t.Fatalf("request_log rows: %d", n)
+	}
+}
diff --git a/internal/claudioapi/login_test.go b/internal/claudioapi/login_test.go
new file mode 100644
index 0000000..5580aae
--- /dev/null
+++ b/internal/claudioapi/login_test.go
@@ -0,0 +1,396 @@
+package claudioapi
+
+import (
+	"context"
+	"encoding/json"
+	"fmt"
+	"net/http"
+	"net/http/httptest"
+	"path/filepath"
+	"regexp"
+	"strings"
+	"testing"
+	"time"
+
+	"github.com/p4u/claude-proxy/internal/store"
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+// loginStack is a claudio handler with device login on and a settable clock.
+type loginStack struct {
+	h      *Handler
+	db     *store.DB
+	logins *Logins
+	now    time.Time
+}
+
+func newLoginStack(t *testing.T) *loginStack {
+	t.Helper()
+	db, err := store.Open(filepath.Join(t.TempDir(), "login.db"))
+	if err != nil {
+		t.Fatal(err)
+	}
+	t.Cleanup(func() { _ = db.Close() })
+	s := &loginStack{db: db, now: time.Unix(1_800_000_000, 0)}
+	clock := func() time.Time { return s.now }
+	s.logins = NewLogins("https://proxy.test/claudio/login")
+	s.logins.now = clock
+	s.h = New(http.NewServeMux(), db, nil)
+	s.h.SetLogins(s.logins)
+	s.h.startLimit.now = clock
+	s.h.pollLimit.now = clock
+	return s
+}
+
+func (s *loginStack) advance(d time.Duration) { s.now = s.now.Add(d) }
+
+func (s *loginStack) post(t *testing.T, path, ip, body string) *httptest.ResponseRecorder {
+	t.Helper()
+	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
+	req.Header.Set("X-Forwarded-For", ip)
+	rw := httptest.NewRecorder()
+	s.h.ServeHTTP(rw, req)
+	return rw
+}
+
+type startReply struct {
+	DeviceCode string `json:"device_code"`
+	UserCode   string `json:"user_code"`
+	VerifyURL  string `json:"verify_url"`
+	IntervalS  int    `json:"interval_s"`
+	ExpiresInS int    `json:"expires_in_s"`
+}
+
+func (s *loginStack) start(t *testing.T, ip string) startReply {
+	t.Helper()
+	rw := s.post(t, "/v1/claudio/login/start", ip, "")
+	if rw.Code != http.StatusOK {
+		t.Fatalf("start: %d %s", rw.Code, rw.Body.String())
+	}
+	var r startReply
+	if err := json.Unmarshal(rw.Body.Bytes(), &r); err != nil {
+		t.Fatal(err)
+	}
+	return r
+}
+
+func (s *loginStack) poll(t *testing.T, ip, deviceCode string) *httptest.ResponseRecorder {
+	t.Helper()
+	b, _ := json.Marshal(map[string]string{"device_code": deviceCode})
+	return s.post(t, "/v1/claudio/login/poll", ip, string(b))
+}
+
+// expectPoll polls after one interval and checks the status and error code.
+func (s *loginStack) expectPoll(t *testing.T, ip, deviceCode string, code int, errCode string) *httptest.ResponseRecorder {
+	t.Helper()
+	s.advance(LoginInterval)
+	rw := s.poll(t, ip, deviceCode)
+	if rw.Code != code {
+		t.Fatalf("poll: %d %s, want %d %s", rw.Code, rw.Body.String(), code, errCode)
+	}
+	if errCode != "" {
+		if got := strings.TrimSpace(rw.Body.String()); got != `{"error":"`+errCode+`"}` {
+			t.Fatalf("poll body = %s, want error %s", got, errCode)
+		}
+		if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
+			t.Fatalf("Content-Type = %q", ct)
+		}
+	}
+	return rw
+}
+
+func TestLoginStartReply(t *testing.T) {
+	s := newLoginStack(t)
+	r := s.start(t, "198.51.100.1")
+	if !regexp.MustCompile(`^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{4}-[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{4}$`).MatchString(r.UserCode) {
+		t.Fatalf("user_code = %q", r.UserCode)
+	}
+	if len(r.DeviceCode) != 43 || strings.ContainsAny(r.DeviceCode, "+/=") {
+		t.Fatalf("device_code = %q, want 32 bytes base64url", r.DeviceCode)
+	}
+	if r.VerifyURL != "https://proxy.test/claudio/login" || r.IntervalS != 3 || r.ExpiresInS != 600 {
+		t.Fatalf("reply = %+v", r)
+	}
+	// Only the hash of the device code is held.
+	s.logins.mu.Lock()
+	defer s.logins.mu.Unlock()
+	if len(s.logins.byHash) != 1 {
+		t.Fatalf("entries = %d", len(s.logins.byHash))
+	}
+	for _, e := range s.logins.byHash {
+		if strings.Contains(fmt.Sprintf("%+v", *e), r.DeviceCode) {
+			t.Fatal("entry holds the plaintext device code")
+		}
+	}
+}
+
+func TestLoginApprovedIsRedeemedOnce(t *testing.T) {
+	s := newLoginStack(t)
+	ctx := context.Background()
+	ut, err := usertoken.CreateWithEmail(ctx, s.db, "alice@vocdoni.org", "alice@vocdoni.org")
+	if err != nil {
+		t.Fatal(err)
+	}
+	r := s.start(t, "198.51.100.1")
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusPreconditionRequired, "authorization_pending")
+
+	if !s.logins.Check(strings.ToLower(r.UserCode)) {
+		t.Fatal("Check: pending code not found (case and dash must be ignored)")
+	}
+	if !s.logins.Approve(strings.ReplaceAll(r.UserCode, "-", ""), ut.ID) {
+		t.Fatal("Approve refused a pending code")
+	}
+	if s.logins.Approve(r.UserCode, ut.ID) || s.logins.Deny(r.UserCode) || s.logins.Check(r.UserCode) {
+		t.Fatal("a decided code was decided again")
+	}
+
+	rw := s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusOK, "")
+	var got struct{ Token, Email, Name string }
+	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
+		t.Fatal(err)
+	}
+	if got.Token != ut.Token || got.Email != "alice@vocdoni.org" || got.Name != "alice@vocdoni.org" {
+		t.Fatalf("200 body = %s", rw.Body.String())
+	}
+	if rw.Header().Get("Cache-Control") != "no-store" {
+		t.Fatal("token reply is cacheable")
+	}
+	// Redeemed: the entry is gone.
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
+	// An approved login never rotates the token.
+	if again, _ := usertoken.Get(ctx, s.db, ut.ID); again.Token != ut.Token {
+		t.Fatal("token rotated")
+	}
+}
+
+func TestLoginRedeemingIsExclusive(t *testing.T) {
+	s := newLoginStack(t)
+	dc, uc, err := s.logins.start("198.51.100.1")
+	if err != nil {
+		t.Fatal(err)
+	}
+	s.logins.Approve(uc, "utok_x")
+	h := hashCode(dc)
+	if st, id := s.logins.poll(h); st != pollApproved || id != "utok_x" {
+		t.Fatalf("first poll = %v %q", st, id)
+	}
+	// A concurrent poll while the first is redeeming must not get the token.
+	if st, _ := s.logins.poll(h); st != pollPending {
+		t.Fatalf("second poll during redemption = %v", st)
+	}
+	// A failed DB read puts it back for the next poll.
+	s.logins.settle(h, outcomeRetry)
+	if st, _ := s.logins.poll(h); st != pollApproved {
+		t.Fatalf("poll after retry = %v", st)
+	}
+}
+
+func TestLoginDisabledUserRefusedAtRedemption(t *testing.T) {
+	s := newLoginStack(t)
+	ctx := context.Background()
+	ut, _ := usertoken.CreateWithEmail(ctx, s.db, "bob", "bob@vocdoni.org")
+	r := s.start(t, "198.51.100.1")
+	s.logins.Approve(r.UserCode, ut.ID)
+	if err := usertoken.SetStatus(ctx, s.db, ut.ID, usertoken.StatusDisabled); err != nil {
+		t.Fatal(err)
+	}
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
+}
+
+func TestLoginDeletedUserAtRedemption(t *testing.T) {
+	s := newLoginStack(t)
+	ctx := context.Background()
+	ut, _ := usertoken.Create(ctx, s.db, "carol")
+	r := s.start(t, "198.51.100.1")
+	s.logins.Approve(r.UserCode, ut.ID)
+	if err := usertoken.Delete(ctx, s.db, ut.ID); err != nil {
+		t.Fatal(err)
+	}
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
+}
+
+func TestLoginDeniedKeptTenMinutes(t *testing.T) {
+	s := newLoginStack(t)
+	r := s.start(t, "198.51.100.1")
+	if !s.logins.Deny(r.UserCode) {
+		t.Fatal("Deny refused a pending code")
+	}
+	if s.logins.Approve(r.UserCode, "utok_x") {
+		t.Fatal("a denied code was approved")
+	}
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
+	s.advance(9 * time.Minute)
+	s.logins.Sweep()
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusForbidden, "denied")
+	s.advance(time.Minute)
+	s.logins.Sweep()
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
+}
+
+func TestLoginExpiredKeptTenMinutes(t *testing.T) {
+	s := newLoginStack(t)
+	r := s.start(t, "198.51.100.1")
+	s.advance(LoginTTL)
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusGone, "expired")
+	if s.logins.Check(r.UserCode) || s.logins.Approve(r.UserCode, "utok_x") {
+		t.Fatal("an expired code can still be decided")
+	}
+	s.advance(9 * time.Minute)
+	s.logins.Sweep()
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusGone, "expired")
+	s.advance(time.Minute)
+	s.logins.Sweep()
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusNotFound, "unknown_code")
+}
+
+func TestLoginApprovedButExpired(t *testing.T) {
+	s := newLoginStack(t)
+	r := s.start(t, "198.51.100.1")
+	s.logins.Approve(r.UserCode, "utok_x")
+	s.advance(LoginTTL)
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusGone, "expired")
+}
+
+func TestLoginSlowDownSparesIPBudget(t *testing.T) {
+	s := newLoginStack(t)
+	r := s.start(t, "198.51.100.1")
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusPreconditionRequired, "authorization_pending")
+	for range 3 * loginPollPerMinute {
+		if rw := s.poll(t, "198.51.100.1", r.DeviceCode); rw.Code != http.StatusTooManyRequests ||
+			strings.TrimSpace(rw.Body.String()) != `{"error":"slow_down"}` {
+			t.Fatalf("early poll: %d %s", rw.Code, rw.Body.String())
+		}
+	}
+	// None of those consumed the per-IP budget.
+	s.expectPoll(t, "198.51.100.1", r.DeviceCode, http.StatusPreconditionRequired, "authorization_pending")
+}
+
+func TestLoginPollIPLimit(t *testing.T) {
+	s := newLoginStack(t)
+	for i := range loginPollPerMinute {
+		if rw := s.poll(t, "198.51.100.1", fmt.Sprintf("unknown-%d", i)); rw.Code != http.StatusNotFound {
+			t.Fatalf("poll %d: %d", i, rw.Code)
+		}
+	}
+	if rw := s.poll(t, "198.51.100.1", "unknown-x"); rw.Code != http.StatusTooManyRequests {
+		t.Fatalf("poll over the IP budget: %d", rw.Code)
+	}
+	// Other addresses have their own budget; the window resets each minute.
+	if rw := s.poll(t, "198.51.100.2", "unknown-x"); rw.Code != http.StatusNotFound {
+		t.Fatalf("other IP: %d", rw.Code)
+	}
+	s.advance(time.Minute)
+	if rw := s.poll(t, "198.51.100.1", "unknown-y"); rw.Code != http.StatusNotFound {
+		t.Fatalf("next window: %d", rw.Code)
+	}
+}
+
+func TestLoginStartLimits(t *testing.T) {
+	s := newLoginStack(t)
+	for range loginStartPerMinute {
+		s.start(t, "198.51.100.1")
+	}
+	if rw := s.post(t, "/v1/claudio/login/start", "198.51.100.1", ""); rw.Code != http.StatusTooManyRequests ||
+		strings.TrimSpace(rw.Body.String()) != `{"error":"too_many_pending"}` {
+		t.Fatalf("6th start in a minute: %d %s", rw.Code, rw.Body.String())
+	}
+	s.advance(time.Minute)
+	for range maxPendingPerIP - loginStartPerMinute {
+		s.start(t, "198.51.100.1")
+	}
+	// Eight pending from this address: a ninth is refused.
+	if rw := s.post(t, "/v1/claudio/login/start", "198.51.100.1", ""); rw.Code != http.StatusTooManyRequests ||
+		strings.TrimSpace(rw.Body.String()) != `{"error":"too_many_pending"}` {
+		t.Fatalf("9th pending: %d %s", rw.Code, rw.Body.String())
+	}
+	s.start(t, "198.51.100.2")
+}
+
+func TestLoginEvictsOldestPastTotal(t *testing.T) {
+	s := newLoginStack(t)
+	first, _, err := s.logins.start("203.0.113.0")
+	if err != nil {
+		t.Fatal(err)
+	}
+	for i := 1; i < maxLogins; i++ {
+		if _, _, err := s.logins.start(fmt.Sprintf("203.0.%d.%d", i/250, i%250)); err != nil {
+			t.Fatalf("start %d: %v", i, err)
+		}
+	}
+	if st, _ := s.logins.poll(hashCode(first)); st != pollPending {
+		t.Fatalf("at the cap the first entry must survive, got %v", st)
+	}
+	if _, _, err := s.logins.start("198.51.100.9"); err != nil {
+		t.Fatal(err)
+	}
+	if st, _ := s.logins.poll(hashCode(first)); st != pollUnknown {
+		t.Fatalf("oldest entry not evicted: %v", st)
+	}
+	s.logins.mu.Lock()
+	n, codes, order := len(s.logins.byHash), len(s.logins.byCode), s.logins.order.Len()
+	s.logins.mu.Unlock()
+	if n != maxLogins || codes != maxLogins || order != maxLogins {
+		t.Fatalf("sizes after eviction: %d %d %d", n, codes, order)
+	}
+}
+
+func TestLoginMalformedAndUnknown(t *testing.T) {
+	s := newLoginStack(t)
+	for _, body := range []string{"", "{", `{"device_code":""}`, `{"device_code":7}`} {
+		if rw := s.post(t, "/v1/claudio/login/poll", "198.51.100.1", body); rw.Code != http.StatusBadRequest {
+			t.Fatalf("body %q: %d", body, rw.Code)
+		}
+	}
+	if rw := s.poll(t, "198.51.100.1", "never-issued"); rw.Code != http.StatusNotFound ||
+		strings.TrimSpace(rw.Body.String()) != `{"error":"unknown_code"}` {
+		t.Fatalf("unknown: %d %s", rw.Code, rw.Body.String())
+	}
+	req := httptest.NewRequest(http.MethodGet, "/v1/claudio/login/start", nil)
+	rw := httptest.NewRecorder()
+	s.h.ServeHTTP(rw, req)
+	if rw.Code != http.StatusMethodNotAllowed {
+		t.Fatalf("GET start: %d", rw.Code)
+	}
+}
+
+func TestLoginRoutes404WithoutSSO(t *testing.T) {
+	db, err := store.Open(filepath.Join(t.TempDir(), "nosso.db"))
+	if err != nil {
+		t.Fatal(err)
+	}
+	defer db.Close()
+	h := New(http.NewServeMux(), db, nil)
+	for _, p := range []string{"/v1/claudio/login/start", "/v1/claudio/login/poll"} {
+		rw := httptest.NewRecorder()
+		h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"device_code":"x"}`)))
+		if rw.Code != http.StatusNotFound {
+			t.Fatalf("%s without SSO: %d", p, rw.Code)
+		}
+	}
+}
+
+func TestLoginRoutesSkipIdentityBucket(t *testing.T) {
+	s := newLoginStack(t)
+	// Exhaust the shared anonymous bucket.
+	for range 20 {
+		rw := httptest.NewRecorder()
+		s.h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/claudio", nil))
+	}
+	rw := httptest.NewRecorder()
+	s.h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/claudio", nil))
+	if rw.Code != http.StatusTooManyRequests {
+		t.Fatalf("anon bucket not exhausted: %d", rw.Code)
+	}
+	s.start(t, "198.51.100.1")
+}
+
+func TestLoginExactPathsOnly(t *testing.T) {
+	s := newLoginStack(t)
+	for _, p := range []string{"/v1/claudio/login/start/", "/v1/claudio/login/startx", "/v1/claudio/login"} {
+		rw := s.post(t, p, "198.51.100.1", "")
+		if rw.Code != http.StatusNotFound {
+			t.Fatalf("%s: %d", p, rw.Code)
+		}
+	}
+}
diff --git a/internal/proxy/auth_test.go b/internal/proxy/auth_test.go
index 222c6ec..0d0b3ab 100644
--- a/internal/proxy/auth_test.go
+++ b/internal/proxy/auth_test.go
@@ -207,3 +207,59 @@ func TestAuthMiddlewareRevokedTokenWithNoAdminToken(t *testing.T) {
 		}
 	})
 }
+
+// TestAuthMiddlewareClaudioLoginExempt: the two device-login routes pass with
+// no credential and with a stale one (ignored, never validated), and carry no
+// identity; look-alike paths get no exemption.
+func TestAuthMiddlewareClaudioLoginExempt(t *testing.T) {
+	ctx := context.Background()
+	db := openDB(t)
+	ut, err := usertoken.Create(ctx, db, "frank")
+	if err != nil {
+		t.Fatal(err)
+	}
+	if err := usertoken.SetStatus(ctx, db, ut.ID, usertoken.StatusDisabled); err != nil {
+		t.Fatal(err)
+	}
+	var hit bool
+	var gotID *usertoken.Identity
+	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
+		hit, gotID = true, usertoken.FromContext(r.Context())
+		w.WriteHeader(http.StatusOK)
+	})
+	h := AuthMiddleware("admin-secret", db, true, inner)
+
+	cases := []struct {
+		name, path, bearer string
+		wantCode           int
+		wantHit            bool
+	}{
+		{"start, no credential", "/v1/claudio/login/start", "", 200, true},
+		{"poll, no credential", "/v1/claudio/login/poll", "", 200, true},
+		{"start, stale bearer", "/v1/claudio/login/start", ut.Token, 200, true},
+		{"poll, stale bearer", "/v1/claudio/login/poll", "garbage", 200, true},
+		{"poll, admin bearer ignored", "/v1/claudio/login/poll", "admin-secret", 200, true},
+		{"startx is not exempt", "/v1/claudio/login/startx", "", 401, false},
+		{"dot segment is not exempt", "/v1/claudio/login/../poll", "", 401, false},
+		{"trailing slash is not exempt", "/v1/claudio/login/start/", "", 401, false},
+		{"encoded slash is not exempt", "/v1/claudio/login%2Fstart", "", 401, false},
+		{"other claudio route still needs auth", "/v1/claudio/plugins", "", 401, false},
+	}
+	for _, tc := range cases {
+		t.Run(tc.name, func(t *testing.T) {
+			hit, gotID = false, nil
+			req := httptest.NewRequest(http.MethodPost, "http://x"+tc.path, nil)
+			if tc.bearer != "" {
+				req.Header.Set("Authorization", "Bearer "+tc.bearer)
+			}
+			rw := httptest.NewRecorder()
+			h.ServeHTTP(rw, req)
+			if rw.Code != tc.wantCode || hit != tc.wantHit {
+				t.Fatalf("got %d hit=%v, want %d hit=%v; body=%s", rw.Code, hit, tc.wantCode, tc.wantHit, rw.Body.String())
+			}
+			if hit && gotID != nil {
+				t.Fatalf("login route carried identity %+v", gotID)
+			}
+		})
+	}
+}
````
Run: `go test ./internal/claudioapi/ ./internal/proxy/`
Expected: `login_test.go:23:10: undefined: Logins` (claudioapi build failed); `--- FAIL: TestAuthMiddlewareClaudioLoginExempt/start,_no_credential` (and the other four exempt cases, 401)

Step 2: implement
````diff
diff --git a/internal/claudioapi/claudioapi.go b/internal/claudioapi/claudioapi.go
index 9c1882c..8fb69eb 100644
--- a/internal/claudioapi/claudioapi.go
+++ b/internal/claudioapi/claudioapi.go
@@ -17,6 +17,7 @@
 //	GET /v1/claudio/session?id= — which credential serves a Claude Code session
 //	POST /v1/claudio/session/switch — move a session to another credential
 //	GET /v1/claudio/plugins    — plugins to load, with their env (user token only)
+//	POST /v1/claudio/login/start, /login/poll — device login (no credential; login.go)
 //
 // Security isolation
 //
@@ -81,6 +82,12 @@ type Handler struct {
 	// per-identity rate limiter
 	rateMu   sync.Mutex
 	rateBkts map[string]*bucket
+
+	// logins backs the device login; nil (no SSO) answers both routes 404.
+	logins *Logins
+	// Per-IP limits of the login routes, which skip the identity bucket.
+	startLimit *ipLimiter
+	pollLimit  *ipLimiter
 }
 
 // CatalogueSource is the subset of the proxy.Handler interface that the
@@ -106,9 +113,11 @@ type CatalogueSource interface {
 // "/v1/claudio" is intercepted before ServeMux sees it.
 func New(mux *http.ServeMux, db *store.DB, cat CatalogueSource) *Handler {
 	h := &Handler{
-		db:       db,
-		cat:      cat,
-		rateBkts: make(map[string]*bucket),
+		db:         db,
+		cat:        cat,
+		rateBkts:   make(map[string]*bucket),
+		startLimit: newIPLimiter(loginStartPerMinute),
+		pollLimit:  newIPLimiter(loginPollPerMinute),
 	}
 	// Belt-and-suspenders: also register on the mux for the plain (non-encoded)
 	// case so that any path-through the guard reaches the correct handler.
@@ -177,6 +186,14 @@ func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
 		return
 	}
 
+	// The device-login routes carry no credential (every caller would share
+	// the "anon" bucket): they replace it with per-IP limits of their own.
+	// Matched exactly, like the AuthMiddleware exemption.
+	if r.URL.Path == loginStartPath || r.URL.Path == loginPollPath {
+		h.serveLogin(w, r)
+		return
+	}
+
 	// Strip trailing slash for matching (GET /v1/claudio/ == GET /v1/claudio).
 	path := strings.TrimRight(r.URL.Path, "/")
 
diff --git a/internal/claudioapi/login.go b/internal/claudioapi/login.go
new file mode 100644
index 0000000..fef06c6
--- /dev/null
+++ b/internal/claudioapi/login.go
@@ -0,0 +1,480 @@
+package claudioapi
+
+// Device-style login for `claudio proxy login` (RFC 8628 shape):
+//
+//	POST /v1/claudio/login/start  → {device_code, user_code, verify_url, interval_s, expires_in_s}
+//	POST /v1/claudio/login/poll   {device_code} → 428 | 429 | 200 {token,email,name} | 410 | 403 | 404
+//
+// The member types user_code on the proxy's /claudio/login page (internal/webui)
+// after signing in with SSO and approves it there; claudio, polling with the
+// device_code only it holds, then receives the member's user token.
+//
+// These are the only /v1/claudio routes without a credential (see
+// proxy.AuthMiddleware). They skip the per-identity bucket for per-IP limits
+// of their own. Pending logins live in memory only, keyed by
+// SHA-256(device_code): neither the plaintext device code nor a token is ever
+// held, so a memory dump or debug log cannot redeem a login.
+
+import (
+	"container/list"
+	"context"
+	"crypto/rand"
+	"crypto/sha256"
+	"encoding/base64"
+	"encoding/json"
+	"errors"
+	"io"
+	"log/slog"
+	"net"
+	"net/http"
+	"strings"
+	"sync"
+	"time"
+
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+const (
+	loginStartPath = "/v1/claudio/login/start"
+	loginPollPath  = "/v1/claudio/login/poll"
+
+	// LoginTTL is how long a code may be approved and redeemed.
+	LoginTTL = 10 * time.Minute
+	// LoginInterval is the minimum gap between two polls of one device code.
+	LoginInterval = 3 * time.Second
+	// loginKeep is how long a denied or expired login still answers 403/410.
+	loginKeep = 10 * time.Minute
+
+	maxPendingPerIP     = 8
+	maxLogins           = 4096
+	loginStartPerMinute = 5
+	// Room for two concurrent logins polling every 3 s.
+	loginPollPerMinute = 40
+	maxPollBody        = 4096
+
+	// userCodeAlphabet has 32 letters (no I, O, 0, 1), so a random byte & 31
+	// picks one without bias.
+	userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
+	userCodeLen      = 8
+)
+
+var errTooManyPending = errors.New("too many pending logins")
+
+type loginState int
+
+const (
+	statePending loginState = iota
+	stateApproved
+	stateRedeeming // one poll is reading the user; others wait
+	stateDenied
+)
+
+type loginEntry struct {
+	hash     [32]byte
+	userCode string // userCodeLen letters, no dash
+	ip       string
+	state    loginState
+	userID   string // set on approval; never the token
+	expires  time.Time
+	decided  time.Time // when denied
+	lastPoll time.Time
+	elem     *list.Element
+}
+
+// Logins holds the pending device logins. It is safe for concurrent use; the
+// web UI's approval page and the claudio API share one instance.
+type Logins struct {
+	verifyURL string
+	now       func() time.Time
+
+	mu     sync.Mutex
+	byHash map[[32]byte]*loginEntry
+	byCode map[string]*loginEntry
+	order  *list.List // *loginEntry, oldest first
+}
+
+// NewLogins creates the pending-login store. verifyURL is the approval page
+// the member opens: the SSO origin + /claudio/login, with no code in it.
+func NewLogins(verifyURL string) *Logins {
+	return &Logins{
+		verifyURL: verifyURL,
+		now:       time.Now,
+		byHash:    make(map[[32]byte]*loginEntry),
+		byCode:    make(map[string]*loginEntry),
+		order:     list.New(),
+	}
+}
+
+func hashCode(deviceCode string) [32]byte { return sha256.Sum256([]byte(deviceCode)) }
+
+// normalizeUserCode upper-cases s and drops dashes and spaces; ok is false
+// unless userCodeLen letters of the alphabet remain.
+func normalizeUserCode(s string) (string, bool) {
+	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
+	if len(s) != userCodeLen {
+		return "", false
+	}
+	for i := 0; i < len(s); i++ {
+		if !strings.ContainsRune(userCodeAlphabet, rune(s[i])) {
+			return "", false
+		}
+	}
+	return s, true
+}
+
+// start creates a pending login for a client at ip and returns the plaintext
+// device code (only ever in this reply) and the user code.
+func (l *Logins) start(ip string) (deviceCode, userCode string, err error) {
+	var raw [32]byte
+	if _, err := rand.Read(raw[:]); err != nil {
+		return "", "", err
+	}
+	deviceCode = base64.RawURLEncoding.EncodeToString(raw[:])
+
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	now := l.now()
+	pending := 0
+	for _, e := range l.byHash {
+		if e.ip == ip && e.state == statePending && now.Before(e.expires) {
+			pending++
+		}
+	}
+	if pending >= maxPendingPerIP {
+		return "", "", errTooManyPending
+	}
+	for {
+		var b [userCodeLen]byte
+		if _, err := rand.Read(b[:]); err != nil {
+			return "", "", err
+		}
+		for i := range b {
+			b[i] = userCodeAlphabet[b[i]&31]
+		}
+		if _, taken := l.byCode[string(b[:])]; !taken {
+			userCode = string(b[:])
+			break
+		}
+	}
+	for len(l.byHash) >= maxLogins {
+		l.removeLocked(l.order.Front().Value.(*loginEntry))
+	}
+	e := &loginEntry{hash: hashCode(deviceCode), userCode: userCode, ip: ip, expires: now.Add(LoginTTL)}
+	e.elem = l.order.PushBack(e)
+	l.byHash[e.hash] = e
+	l.byCode[userCode] = e
+	return deviceCode, userCode, nil
+}
+
+func (l *Logins) removeLocked(e *loginEntry) {
+	delete(l.byHash, e.hash)
+	delete(l.byCode, e.userCode)
+	l.order.Remove(e.elem)
+}
+
+// tooSoon reports whether a poll of this device code comes less than
+// LoginInterval after the previous one; otherwise it records the poll.
+func (l *Logins) tooSoon(hash [32]byte) bool {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	e, ok := l.byHash[hash]
+	if !ok {
+		return false
+	}
+	now := l.now()
+	if !e.lastPoll.IsZero() && now.Sub(e.lastPoll) < LoginInterval {
+		return true
+	}
+	e.lastPoll = now
+	return false
+}
+
+type pollState int
+
+const (
+	pollUnknown pollState = iota
+	pollPending
+	pollApproved // the caller must settle() the entry
+	pollExpired
+	pollDenied
+)
+
+// poll reads a login's state. An approved login moves to redeeming and its
+// user id is returned to exactly one caller, which must then settle it.
+func (l *Logins) poll(hash [32]byte) (pollState, string) {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	e, ok := l.byHash[hash]
+	if !ok {
+		return pollUnknown, ""
+	}
+	switch e.state {
+	case stateDenied:
+		return pollDenied, ""
+	case stateRedeeming:
+		return pollPending, ""
+	}
+	if !l.now().Before(e.expires) {
+		return pollExpired, ""
+	}
+	if e.state == stateApproved {
+		e.state = stateRedeeming
+		return pollApproved, e.userID
+	}
+	return pollPending, ""
+}
+
+type outcome int
+
+const (
+	outcomeRedeemed outcome = iota // token returned: forget the login
+	outcomeGone                    // the user row is gone: forget the login
+	outcomeRefused                 // the user is not active: denied from now on
+	outcomeRetry                   // reading the user failed: approved again
+)
+
+// settle ends a redemption started by poll.
+func (l *Logins) settle(hash [32]byte, o outcome) {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	e, ok := l.byHash[hash]
+	if !ok || e.state != stateRedeeming {
+		return
+	}
+	switch o {
+	case outcomeRedeemed, outcomeGone:
+		l.removeLocked(e)
+	case outcomeRefused:
+		e.state, e.decided = stateDenied, l.now()
+	case outcomeRetry:
+		e.state = stateApproved
+	}
+}
+
+// decidable returns the pending, unexpired login for a typed user code.
+func (l *Logins) decidable(userCode string) *loginEntry {
+	code, ok := normalizeUserCode(userCode)
+	if !ok {
+		return nil
+	}
+	e := l.byCode[code]
+	if e == nil || e.state != statePending || !l.now().Before(e.expires) {
+		return nil
+	}
+	return e
+}
+
+// Check reports whether a typed user code (case, dashes and spaces ignored)
+// names a pending, unexpired login.
+func (l *Logins) Check(userCode string) bool {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	return l.decidable(userCode) != nil
+}
+
+// Approve marks a pending login approved for the user token userID. It
+// returns false when the code is unknown, expired or already decided.
+func (l *Logins) Approve(userCode, userID string) bool {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	e := l.decidable(userCode)
+	if e == nil {
+		return false
+	}
+	e.state, e.userID = stateApproved, userID
+	return true
+}
+
+// Deny marks a pending login denied. It returns false when the code is
+// unknown, expired or already decided.
+func (l *Logins) Deny(userCode string) bool {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	e := l.decidable(userCode)
+	if e == nil {
+		return false
+	}
+	e.state, e.decided = stateDenied, l.now()
+	return true
+}
+
+// Sweep forgets denied logins loginKeep after the decision and every other
+// login loginKeep after it expired.
+func (l *Logins) Sweep() {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	now := l.now()
+	for _, e := range l.byHash {
+		end := e.expires
+		if e.state == stateDenied {
+			end = e.decided
+		}
+		if !now.Before(end.Add(loginKeep)) {
+			l.removeLocked(e)
+		}
+	}
+}
+
+// Run sweeps every minute until ctx ends.
+func (l *Logins) Run(ctx context.Context) {
+	t := time.NewTicker(time.Minute)
+	defer t.Stop()
+	for {
+		select {
+		case <-ctx.Done():
+			return
+		case <-t.C:
+			l.Sweep()
+		}
+	}
+}
+
+// SetLogins turns device login on. Without it both login routes answer 404
+// (no SSO configured).
+func (h *Handler) SetLogins(l *Logins) { h.logins = l }
+
+// serveLogin dispatches the two credential-less login routes.
+func (h *Handler) serveLogin(w http.ResponseWriter, r *http.Request) {
+	if h.logins == nil {
+		writeJSON(w, http.StatusNotFound, apiError("not_found", "claudio: unknown endpoint"))
+		return
+	}
+	if r.URL.Path == loginStartPath {
+		h.methodOnly(w, r, http.MethodPost, h.serveLoginStart)
+		return
+	}
+	h.methodOnly(w, r, http.MethodPost, h.serveLoginPoll)
+}
+
+type loginStartResponse struct {
+	DeviceCode string `json:"device_code"`
+	UserCode   string `json:"user_code"`
+	VerifyURL  string `json:"verify_url"`
+	IntervalS  int    `json:"interval_s"`
+	ExpiresInS int    `json:"expires_in_s"`
+}
+
+func (h *Handler) serveLoginStart(w http.ResponseWriter, r *http.Request) {
+	ip := clientIP(r)
+	if !h.startLimit.allow(ip) {
+		writeLoginErr(w, http.StatusTooManyRequests, "too_many_pending")
+		return
+	}
+	deviceCode, userCode, err := h.logins.start(ip)
+	if errors.Is(err, errTooManyPending) {
+		writeLoginErr(w, http.StatusTooManyRequests, "too_many_pending")
+		return
+	}
+	if err != nil {
+		writeLoginErr(w, http.StatusInternalServerError, "server_error")
+		return
+	}
+	w.Header().Set("Cache-Control", "no-store")
+	writeJSON(w, http.StatusOK, loginStartResponse{
+		DeviceCode: deviceCode,
+		UserCode:   userCode[:4] + "-" + userCode[4:],
+		VerifyURL:  h.logins.verifyURL,
+		IntervalS:  int(LoginInterval / time.Second),
+		ExpiresInS: int(LoginTTL / time.Second),
+	})
+}
+
+type loginTokenResponse struct {
+	Token string `json:"token"`
+	Email string `json:"email"`
+	Name  string `json:"name"`
+}
+
+func (h *Handler) serveLoginPoll(w http.ResponseWriter, r *http.Request) {
+	var body struct {
+		DeviceCode string `json:"device_code"`
+	}
+	if err := json.NewDecoder(io.LimitReader(r.Body, maxPollBody)).Decode(&body); err != nil || body.DeviceCode == "" {
+		writeLoginErr(w, http.StatusBadRequest, "invalid_request")
+		return
+	}
+	hash := hashCode(body.DeviceCode)
+	// Too-fast polls of a live login are answered before the IP budget is
+	// touched, so one eager client cannot starve another on the same NAT.
+	if h.logins.tooSoon(hash) || !h.pollLimit.allow(clientIP(r)) {
+		writeLoginErr(w, http.StatusTooManyRequests, "slow_down")
+		return
+	}
+	st, userID := h.logins.poll(hash)
+	switch st {
+	case pollPending:
+		writeLoginErr(w, http.StatusPreconditionRequired, "authorization_pending")
+	case pollExpired:
+		writeLoginErr(w, http.StatusGone, "expired")
+	case pollDenied:
+		writeLoginErr(w, http.StatusForbidden, "denied")
+	case pollUnknown:
+		writeLoginErr(w, http.StatusNotFound, "unknown_code")
+	case pollApproved:
+		// Re-read the user: it may have been disabled or deleted since the
+		// approval. The current token is returned; approval never rotates it.
+		ut, err := usertoken.Get(r.Context(), h.db, userID)
+		switch {
+		case errors.Is(err, usertoken.ErrNotFound):
+			h.logins.settle(hash, outcomeGone)
+			writeLoginErr(w, http.StatusNotFound, "unknown_code")
+		case err != nil:
+			h.logins.settle(hash, outcomeRetry)
+			writeLoginErr(w, http.StatusInternalServerError, "server_error")
+		case ut.Status != usertoken.StatusActive:
+			h.logins.settle(hash, outcomeRefused)
+			writeLoginErr(w, http.StatusForbidden, "denied")
+		default:
+			h.logins.settle(hash, outcomeRedeemed)
+			slog.Info("claudio login: token issued", "user", ut.Name)
+			w.Header().Set("Cache-Control", "no-store")
+			writeJSON(w, http.StatusOK, loginTokenResponse{Token: ut.Token, Email: ut.Email, Name: ut.Name})
+		}
+	}
+}
+
+// writeLoginErr writes the login routes' error shape: {"error":"<code>"}.
+func writeLoginErr(w http.ResponseWriter, status int, code string) {
+	writeJSON(w, status, map[string]string{"error": code})
+}
+
+// ipLimiter caps requests per client IP in a fixed one-minute window (a copy
+// of webui's signInLimiter: the two packages share no code).
+type ipLimiter struct {
+	max int
+	now func() time.Time
+
+	mu     sync.Mutex
+	start  time.Time
+	counts map[string]int
+}
+
+func newIPLimiter(max int) *ipLimiter {
+	return &ipLimiter{max: max, now: time.Now, counts: make(map[string]int)}
+}
+
+func (l *ipLimiter) allow(ip string) bool {
+	l.mu.Lock()
+	defer l.mu.Unlock()
+	if now := l.now(); now.Sub(l.start) >= time.Minute {
+		l.start, l.counts = now, make(map[string]int)
+	}
+	l.counts[ip]++
+	return l.counts[ip] <= l.max
+}
+
+// clientIP is the first X-Forwarded-For entry, else the peer address (as
+// webui's clientIP: the proxy runs behind Traefik).
+func clientIP(r *http.Request) string {
+	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
+		if i := strings.IndexByte(xff, ','); i >= 0 {
+			return strings.TrimSpace(xff[:i])
+		}
+		return strings.TrimSpace(xff)
+	}
+	host, _, err := net.SplitHostPort(r.RemoteAddr)
+	if err != nil {
+		return r.RemoteAddr
+	}
+	return host
+}
diff --git a/internal/proxy/auth.go b/internal/proxy/auth.go
index 1ed24dc..11a826f 100644
--- a/internal/proxy/auth.go
+++ b/internal/proxy/auth.go
@@ -13,7 +13,9 @@ import (
 // AuthMiddleware authenticates incoming requests.
 //
 // Priority order:
-//  1. /health — always public.
+//  1. /health — always public. So are claudio's two device-login routes
+//     (/v1/claudio/login/start and /poll, exact escaped path): any credential
+//     on them is ignored, never validated, and no identity is attached.
 //  2. When uiEnabled, any path that is not /v1/*, /openai/v1/* or /admin/* is
 //     passed through untouched — the web UI (mounted at "/") does its own
 //     cookie auth.
@@ -31,6 +33,14 @@ func AuthMiddleware(adminToken string, db *store.DB, uiEnabled bool, next http.H
 			next.ServeHTTP(w, r)
 			return
 		}
+		// claudio's login runs before the client holds a token, and a stale
+		// token must not turn it into a 401. The escaped path is compared
+		// exactly, so encoded or dot-segment variants are not exempt and the
+		// raw-path guard in claudioapi still sees what the client sent.
+		if p := r.URL.EscapedPath(); p == "/v1/claudio/login/start" || p == "/v1/claudio/login/poll" {
+			next.ServeHTTP(w, r)
+			return
+		}
 
 		isProxy := strings.HasPrefix(r.URL.Path, "/v1/") || IsOpenAIPath(r.URL.Path)
 		isAdmin := strings.HasPrefix(r.URL.Path, "/admin/")
````
Run: `go test -race ./internal/claudioapi/ ./internal/proxy/ && GOTOOLCHAIN=go1.26.2 make lint`
Expected: `ok  	github.com/p4u/claude-proxy/internal/claudioapi`, `ok  	github.com/p4u/claude-proxy/internal/proxy`; lint `0 issues.`

Step 3: commit
`git add internal/claudioapi/login.go internal/claudioapi/login_stack_test.go internal/claudioapi/login_test.go` then `git commit -m "feat(claudioapi): device login start and poll routes" -- internal/claudioapi/claudioapi.go internal/claudioapi/login.go internal/claudioapi/login_stack_test.go internal/claudioapi/login_test.go internal/proxy/auth.go internal/proxy/auth_test.go`

Interfaces this task provides to later tasks: `claudioapi.NewLogins(verifyURL) *Logins`; `(*Logins).Check(code) bool`, `.Approve(code, userID) bool`, `.Deny(code) bool` (case, dashes and spaces ignored; false when unknown, expired or decided), `.Sweep()`, `.Run(ctx)`; `(*Handler).SetLogins(*Logins)`; `claudioapi.LoginTTL`, `LoginInterval`.

### Task 6: Browser approval page /claudio/login and serve wiring
Model: strong   Repo: /volumes/repos/claude-proxy   Files: .env.example, cmd/claude-proxy/limit_test.go, cmd/claude-proxy/main.go, internal/webui/claudiologin.go, internal/webui/claudiologin_test.go, internal/webui/webui.go
Goal: Spec §2 browser half: GET/POST `/claudio/login` via `vocauth.Require`, same-origin POST, login-domain re-check, lookup/approve/deny, find-or-create the member (`CreateForEmail` + `CLAUDIO_DEFAULT_LIMIT`, same parser as `users limit`), security headers; one `Logins` shared by claudioapi and webui in `main.go` (AC2).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-6.patch

Notes: **Non-obvious:** the spec requires both `Referrer-Policy: no-referrer` and a same-origin POST. With that policy browsers send `Origin: null` on the page's own form posts (Fetch spec, "serializing a request origin"), so `sameOrigin` alone would refuse every approval. `loginPostSameOrigin` accepts `Origin: null` only together with `Sec-Fetch-Site: same-origin` (a forbidden header no page can set); any other Origin goes through the unchanged `sameOrigin`. A typed code is checked with `Check` before a user is created, so a wrong code never creates an account. `SetLimit` after `CreateForEmail` is a second statement; on its failure the new row is deleted, so a member is never left unlimited by accident. `ErrEmailTaken` from a concurrent approval falls back to `LookupByEmail`. Without SSO (or without `Logins`) `/claudio/login` is a 404 (it used to fall through to the SPA). `main.go` passes `Logins` to webui only when non-nil (avoids a non-nil interface holding a nil pointer). `CLAUDIO_DEFAULT_LIMIT` is documented in `.env.example`; like `CLAUDE_PROXY_LOGIN_DOMAINS` it reaches the container through `env_file`.

Step 1: write the failing test
````diff
diff --git a/cmd/claude-proxy/limit_test.go b/cmd/claude-proxy/limit_test.go
new file mode 100644
index 0000000..21e1af7
--- /dev/null
+++ b/cmd/claude-proxy/limit_test.go
@@ -0,0 +1,31 @@
+package main
+
+import "testing"
+
+func TestParseDefaultLimit(t *testing.T) {
+	for _, tc := range []struct {
+		in             string
+		tokens, window int64
+		ok             bool
+	}{
+		{"", 0, 0, true},
+		{"  ", 0, 0, true},
+		{"0", 0, 0, true},
+		{"--none", 0, 0, true},
+		{"--tokens 1M --window 24h", 1_000_000, 86400, true},
+		{" --tokens=500k --window=7d ", 500_000, 7 * 86400, true},
+		{"--tokens 1M", 0, 0, false},
+		{"--window 24h", 0, 0, false},
+		{"--tokens 0 --window 24h", 0, 0, false},
+		{"--tokens 1M --window 2h", 0, 0, false},
+		{"--none --tokens 1M", 0, 0, false},
+		{"--bogus", 0, 0, false},
+		{"1M 24h", 0, 0, false},
+	} {
+		tokens, window, err := parseDefaultLimit(tc.in)
+		if (err == nil) != tc.ok || tokens != tc.tokens || window != tc.window {
+			t.Errorf("parseDefaultLimit(%q) = %d, %d, %v; want %d, %d, ok=%v",
+				tc.in, tokens, window, err, tc.tokens, tc.window, tc.ok)
+		}
+	}
+}
diff --git a/internal/webui/claudiologin_test.go b/internal/webui/claudiologin_test.go
new file mode 100644
index 0000000..472e485
--- /dev/null
+++ b/internal/webui/claudiologin_test.go
@@ -0,0 +1,327 @@
+package webui
+
+import (
+	"context"
+	"encoding/base64"
+	"net/http"
+	"net/http/httptest"
+	"net/url"
+	"strings"
+	"sync"
+	"testing"
+
+	vocauth "github.com/vocdoni/oauth/go"
+
+	"github.com/p4u/claude-proxy/internal/store"
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+// fakeLogins is the claudio API's pending-login store as the page sees it.
+type fakeLogins struct {
+	mu       sync.Mutex
+	pending  map[string]bool
+	approved map[string]string
+	denied   map[string]bool
+}
+
+func newFakeLogins(codes ...string) *fakeLogins {
+	f := &fakeLogins{pending: map[string]bool{}, approved: map[string]string{}, denied: map[string]bool{}}
+	for _, c := range codes {
+		f.pending[c] = true
+	}
+	return f
+}
+
+func fakeNorm(c string) string { return strings.ToUpper(strings.ReplaceAll(c, "-", "")) }
+
+func (f *fakeLogins) Check(c string) bool {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	return f.pending[fakeNorm(c)]
+}
+
+func (f *fakeLogins) Approve(c, userID string) bool {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	if !f.pending[fakeNorm(c)] {
+		return false
+	}
+	delete(f.pending, fakeNorm(c))
+	f.approved[fakeNorm(c)] = userID
+	return true
+}
+
+func (f *fakeLogins) Deny(c string) bool {
+	f.mu.Lock()
+	defer f.mu.Unlock()
+	if !f.pending[fakeNorm(c)] {
+		return false
+	}
+	delete(f.pending, fakeNorm(c))
+	f.denied[fakeNorm(c)] = true
+	return true
+}
+
+// newLoginServer is an SSO server with device login on, boss@vocdoni.org as
+// the only administrator and a 1M/24h default limit for new members.
+func newLoginServer(t *testing.T, issuer string, logins LoginApprover, domains ...string) (*store.DB, http.Handler) {
+	t.Helper()
+	db, _ := newTestServer(t)
+	keys := base64.StdEncoding.EncodeToString(make([]byte, vocauth.KeySize))
+	sso, err := NewSSO(SSOConfig{
+		Issuer: issuer, ClientID: ssoClientID, ClientSecret: "secret",
+		RedirectURL: ssoOrigin + "/auth/callback", CookieKeys: keys, Insecure: true,
+		AdminEmails: []string{"boss@vocdoni.org"}, LoginDomains: domains,
+	})
+	if err != nil {
+		t.Fatal(err)
+	}
+	return db, NewWithOptions(db, nil, Options{
+		SSO: sso, Logins: logins,
+		DefaultLimit: MemberLimit{OutputTokens: 1_000_000, WindowSeconds: 86400},
+	})
+}
+
+// postLogin submits the /claudio/login form the way a browser does from the
+// page itself: Referrer-Policy no-referrer makes it send "Origin: null".
+func postLogin(t *testing.T, h http.Handler, sess *http.Cookie, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
+	t.Helper()
+	r := httptest.NewRequest(http.MethodPost, "/claudio/login", strings.NewReader(form.Encode()))
+	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
+	r.RemoteAddr = "192.0.2.10:5555"
+	if hdr == nil {
+		hdr = map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"}
+	}
+	for k, v := range hdr {
+		r.Header.Set(k, v)
+	}
+	if sess != nil {
+		r.AddCookie(sess)
+	}
+	w := httptest.NewRecorder()
+	h.ServeHTTP(w, r)
+	return w
+}
+
+func getLoginPage(h http.Handler, sess *http.Cookie) *httptest.ResponseRecorder {
+	r := httptest.NewRequest(http.MethodGet, "/claudio/login", nil)
+	r.Header.Set("Accept", "text/html,application/xhtml+xml")
+	if sess != nil {
+		r.AddCookie(sess)
+	}
+	w := httptest.NewRecorder()
+	h.ServeHTTP(w, r)
+	return w
+}
+
+func assertLoginHeaders(t *testing.T, w *httptest.ResponseRecorder) {
+	t.Helper()
+	want := map[string]string{
+		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'",
+		"X-Frame-Options":         "DENY",
+		"Cache-Control":           "no-store",
+		"Referrer-Policy":         "no-referrer",
+	}
+	for k, v := range want {
+		if got := w.Header().Get(k); got != v {
+			t.Errorf("%s = %q, want %q", k, got, v)
+		}
+	}
+}
+
+func TestClaudioLoginRedirectsToSignIn(t *testing.T) {
+	op := newFakeOP(t)
+	_, h := newLoginServer(t, op.srv.URL, newFakeLogins())
+	w := getLoginPage(h, nil)
+	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?return_to=%2Fclaudio%2Flogin" {
+		t.Fatalf("unauthenticated GET = %d %q", w.Code, w.Header().Get("Location"))
+	}
+	assertLoginHeaders(t, w)
+	if w := postLogin(t, h, nil, url.Values{"action": {"lookup"}, "code": {"K7PQ3WXM"}}, nil); w.Code != http.StatusUnauthorized {
+		t.Fatalf("unauthenticated POST = %d", w.Code)
+	}
+}
+
+func TestClaudioLoginPage(t *testing.T) {
+	op := newFakeOP(t)
+	_, h := newLoginServer(t, op.srv.URL, newFakeLogins())
+	sess, _ := signIn(t, h, op)
+	w := getLoginPage(h, sess)
+	if w.Code != http.StatusOK {
+		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
+	}
+	assertLoginHeaders(t, w)
+	body := w.Body.String()
+	for _, s := range []string{`<form method="post" action="/claudio/login">`, `name="code"`, "alice@vocdoni.org"} {
+		if !strings.Contains(body, s) {
+			t.Errorf("page lacks %q", s)
+		}
+	}
+	if strings.Contains(strings.ToLower(body), "<script") {
+		t.Error("page has a script")
+	}
+	// The member is not an administrator of the web UI.
+	if w := do(t, h, http.MethodGet, "/api/users", "", sess); w.Code != http.StatusForbidden {
+		t.Fatalf("member /api/users = %d", w.Code)
+	}
+}
+
+func TestClaudioLoginApproveCreatesMember(t *testing.T) {
+	op := newFakeOP(t)
+	logins := newFakeLogins("K7PQ3WXM", "ABCD2345")
+	db, h := newLoginServer(t, op.srv.URL, logins)
+	ctx := context.Background()
+	// An admin already used the address as a plain name.
+	if _, err := usertoken.Create(ctx, db, "alice@vocdoni.org"); err != nil {
+		t.Fatal(err)
+	}
+	sess, _ := signIn(t, h, op)
+
+	w := postLogin(t, h, sess, url.Values{"action": {"lookup"}, "code": {"k7pq-3wxm"}}, nil)
+	if w.Code != http.StatusOK {
+		t.Fatalf("lookup = %d %s", w.Code, w.Body.String())
+	}
+	assertLoginHeaders(t, w)
+	for _, s := range []string{"K7PQ-3WXM", "alice@vocdoni.org", "Approve only if this code is shown in your terminal",
+		`value="approve"`, `value="deny"`, `type="hidden" name="code" value="K7PQ3WXM"`} {
+		if !strings.Contains(w.Body.String(), s) {
+			t.Errorf("approval page lacks %q", s)
+		}
+	}
+
+	w = postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil)
+	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Approved") {
+		t.Fatalf("approve = %d %s", w.Code, w.Body.String())
+	}
+	ut, err := usertoken.LookupByEmail(ctx, db, "alice@vocdoni.org")
+	if err != nil {
+		t.Fatalf("member not created: %v", err)
+	}
+	if ut.Name != "alice@vocdoni.org-2" || ut.LimitOutputTokens != 1_000_000 || ut.LimitWindowSeconds != 86400 {
+		t.Fatalf("member = %+v", ut)
+	}
+	if logins.approved["K7PQ3WXM"] != ut.ID {
+		t.Fatalf("approved with %q, want %q", logins.approved["K7PQ3WXM"], ut.ID)
+	}
+	if strings.Contains(w.Body.String(), ut.Token) {
+		t.Fatal("the page shows the token")
+	}
+
+	// A second login reuses the member's token.
+	if w := postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"ABCD-2345"}}, nil); w.Code != http.StatusOK {
+		t.Fatalf("second approve = %d %s", w.Code, w.Body.String())
+	}
+	if logins.approved["ABCD2345"] != ut.ID {
+		t.Fatal("second approval used another user")
+	}
+	list, _ := usertoken.List(ctx, db)
+	if len(list) != 2 {
+		t.Fatalf("users = %d, want 2", len(list))
+	}
+
+	// A decided code cannot be decided again.
+	w = postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil)
+	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown, expired or already used") {
+		t.Fatalf("re-approve = %d %s", w.Code, w.Body.String())
+	}
+}
+
+func TestClaudioLoginDisabledMemberRefused(t *testing.T) {
+	op := newFakeOP(t)
+	logins := newFakeLogins("K7PQ3WXM")
+	db, h := newLoginServer(t, op.srv.URL, logins)
+	ctx := context.Background()
+	ut, _ := usertoken.CreateWithEmail(ctx, db, "alice", "alice@vocdoni.org")
+	_ = usertoken.SetStatus(ctx, db, ut.ID, usertoken.StatusDisabled)
+	sess, _ := signIn(t, h, op)
+	w := postLogin(t, h, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil)
+	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "account disabled, contact an admin") {
+		t.Fatalf("disabled member approve = %d %s", w.Code, w.Body.String())
+	}
+	if len(logins.approved) != 0 || !logins.pending["K7PQ3WXM"] {
+		t.Fatal("login decided for a disabled member")
+	}
+}
+
+func TestClaudioLoginDenyAndUnknown(t *testing.T) {
+	op := newFakeOP(t)
+	logins := newFakeLogins("K7PQ3WXM")
+	db, h := newLoginServer(t, op.srv.URL, logins)
+	sess, _ := signIn(t, h, op)
+	w := postLogin(t, h, sess, url.Values{"action": {"lookup"}, "code": {"ZZZZ2222"}}, nil)
+	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown, expired or already used") {
+		t.Fatalf("unknown lookup = %d %s", w.Code, w.Body.String())
+	}
+	w = postLogin(t, h, sess, url.Values{"action": {"deny"}, "code": {"K7PQ3WXM"}}, nil)
+	if w.Code != http.StatusOK || !logins.denied["K7PQ3WXM"] {
+		t.Fatalf("deny = %d %s", w.Code, w.Body.String())
+	}
+	if _, err := usertoken.LookupByEmail(context.Background(), db, "alice@vocdoni.org"); err != usertoken.ErrNotFound {
+		t.Fatalf("deny created a user: %v", err)
+	}
+	if w := postLogin(t, h, sess, url.Values{"action": {"bogus"}, "code": {"K7PQ3WXM"}}, nil); w.Code != http.StatusBadRequest {
+		t.Fatalf("bogus action = %d", w.Code)
+	}
+}
+
+func TestClaudioLoginRefusesCrossOrigin(t *testing.T) {
+	op := newFakeOP(t)
+	logins := newFakeLogins("K7PQ3WXM")
+	_, h := newLoginServer(t, op.srv.URL, logins)
+	sess, _ := signIn(t, h, op)
+	form := url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}
+	for name, hdr := range map[string]map[string]string{
+		"other origin":            {"Origin": "https://evil.test"},
+		"null origin, cross-site": {"Origin": "null", "Sec-Fetch-Site": "cross-site"},
+		"null origin, no fetch":   {"Origin": "null"},
+		"no origin, cross-site":   {"Sec-Fetch-Site": "cross-site"},
+		"nothing":                 {},
+	} {
+		w := postLogin(t, h, sess, form, hdr)
+		if w.Code != http.StatusForbidden {
+			t.Errorf("%s: %d, want 403", name, w.Code)
+		}
+		assertLoginHeaders(t, w)
+	}
+	if len(logins.approved) != 0 {
+		t.Fatal("a cross-origin post approved a login")
+	}
+	if w := postLogin(t, h, sess, form, map[string]string{"Origin": ssoOrigin}); w.Code != http.StatusOK {
+		t.Fatalf("same-origin post = %d %s", w.Code, w.Body.String())
+	}
+}
+
+func TestClaudioLoginRechecksDomain(t *testing.T) {
+	op := newFakeOP(t)
+	op.email = "mallory@example.com"
+	// A session minted while example.com was a login domain...
+	_, wide := newLoginServer(t, op.srv.URL, newFakeLogins(), "vocdoni.org", "example.com")
+	sess, cb := signIn(t, wide, op)
+	if sess == nil {
+		t.Fatalf("sign-in: %d", cb.Code)
+	}
+	// ...is refused once it no longer is (same cookie keys).
+	logins := newFakeLogins("K7PQ3WXM")
+	_, narrow := newLoginServer(t, op.srv.URL, logins)
+	if w := getLoginPage(narrow, sess); w.Code != http.StatusForbidden {
+		t.Fatalf("GET outside the login domains = %d", w.Code)
+	}
+	if w := postLogin(t, narrow, sess, url.Values{"action": {"approve"}, "code": {"K7PQ3WXM"}}, nil); w.Code != http.StatusForbidden {
+		t.Fatalf("POST outside the login domains = %d", w.Code)
+	}
+	if len(logins.approved) != 0 {
+		t.Fatal("approved outside the login domains")
+	}
+}
+
+func TestClaudioLogin404WithoutDeviceLogin(t *testing.T) {
+	_, pw := newTestServer(t)
+	if w := getLoginPage(pw, nil); w.Code != http.StatusNotFound {
+		t.Fatalf("password mode = %d", w.Code)
+	}
+	op := newFakeOP(t)
+	_, h := newSSOServer(t, op.srv.URL)
+	if w := getLoginPage(h, nil); w.Code != http.StatusNotFound {
+		t.Fatalf("SSO without logins = %d", w.Code)
+	}
+}
````
Run: `go test ./internal/webui/ ./cmd/claude-proxy/`
Expected: `claudiologin_test.go:67:57: undefined: LoginApprover`, `unknown field Logins in struct literal of type Options` (build failed); `limit_test.go:25:26: undefined: parseDefaultLimit` (build failed)

Step 2: implement
````diff
diff --git a/.env.example b/.env.example
index 9098834..b67523b 100644
--- a/.env.example
+++ b/.env.example
@@ -64,6 +64,10 @@ UI_PASSWORD=
 # Domains whose verified accounts may sign in (comma-separated, default
 # vocdoni.org). Passed to the container as is.
 # CLAUDE_PROXY_LOGIN_DOMAINS=vocdoni.org
+# Usage limit of members created by their first `claudio proxy login` (same
+# arguments as `claude-proxy users limit`); empty or 0 = unlimited. Passed to
+# the container as is.
+# CLAUDIO_DEFAULT_LIMIT="--tokens 1M --window 24h"
 
 # Force the UI session cookie to be Secure (HTTPS-only). Auto-detected when
 # behind Traefik (TLS_DOMAIN set, forwards X-Forwarded-Proto: https) — only
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index 2288228..cb6783c 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -4,6 +4,7 @@ import (
 	"bufio"
 	"context"
 	"encoding/json"
+	"errors"
 	"flag"
 	"fmt"
 	"io"
@@ -292,11 +293,29 @@ func runServe(args []string) {
 		fmt.Fprintf(os.Stderr, "web UI single sign-on: %v\n", err)
 		os.Exit(1)
 	}
+	memberTokens, memberWindow, err := parseDefaultLimit(os.Getenv("CLAUDIO_DEFAULT_LIMIT"))
+	if err != nil {
+		fmt.Fprintf(os.Stderr, "CLAUDIO_DEFAULT_LIMIT: %v\n", err)
+		os.Exit(1)
+	}
+	// claudio's browser login needs SSO: the member approves the code on
+	// /claudio/login after signing in. Without SSO both login routes are 404.
+	var logins webui.LoginApprover
+	if sso != nil {
+		l := claudioapi.NewLogins(sso.LoginURL())
+		claudioH.SetLogins(l)
+		go l.Run(ctx)
+		logins = l
+		logger.Info("claudio browser login enabled", "verify_url", sso.LoginURL(),
+			"default_limit_tokens", memberTokens, "default_limit_window_s", memberWindow)
+	}
 	uiEnabled := *uiPassword != "" || sso != nil
 	if uiEnabled {
 		secureCookies := isTruthy(os.Getenv("CLAUDE_PROXY_UI_SECURE_COOKIES"))
 		uiH := webui.NewWithOptions(db, r, webui.Options{
 			Password: *uiPassword, SSO: sso, SecureCookies: secureCookies, Codex: codexClient,
+			Logins:       logins,
+			DefaultLimit: webui.MemberLimit{OutputTokens: memberTokens, WindowSeconds: memberWindow},
 		})
 		// The UI owns the root: reserved prefixes (/v1/, /openai/v1/, /admin/, /health) are
 		// registered above and win by ServeMux's longest-match rule; everything
@@ -1085,37 +1104,12 @@ func usersLimit(ctx context.Context, args []string) {
 	id := args[0]
 	fs := flag.NewFlagSet("users limit", flag.ExitOnError)
 	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
-	tokens := fs.String("tokens", "", "limit in output tokens (accepts K/M/G, e.g. 1M)")
-	window := fs.String("window", "", "rolling window: 1h, 6h, 24h, 7d, 30d")
-	none := fs.Bool("none", false, "clear the limit (unlimited)")
+	lf := addLimitFlags(fs)
 	_ = fs.Parse(args[1:])
-
-	var limitTokens, windowSecs int64
-	switch {
-	case *none:
-		if *tokens != "" || *window != "" {
-			fmt.Fprintln(os.Stderr, "--none cannot be combined with --tokens/--window")
-			os.Exit(2)
-		}
-	case *tokens == "" || *window == "":
-		fmt.Fprintln(os.Stderr, "both --tokens and --window are required (or --none to clear)")
+	limitTokens, windowSecs, err := lf.value()
+	if err != nil {
+		fmt.Fprintln(os.Stderr, err)
 		os.Exit(2)
-	default:
-		u, err := usertoken.ParseTokens(*tokens)
-		if err != nil {
-			fmt.Fprintln(os.Stderr, err)
-			os.Exit(2)
-		}
-		dur, err := usage.ParsePeriod(*window)
-		if err != nil {
-			fmt.Fprintln(os.Stderr, err)
-			os.Exit(2)
-		}
-		if u == 0 {
-			fmt.Fprintln(os.Stderr, "--tokens must be greater than 0 (use --none to clear)")
-			os.Exit(2)
-		}
-		limitTokens, windowSecs = u, int64(dur/time.Second)
 	}
 
 	db := openDB(*dbPath)
@@ -1133,6 +1127,66 @@ func usersLimit(ctx context.Context, args []string) {
 		usertoken.FormatWindow(time.Duration(windowSecs)*time.Second))
 }
 
+// limitFlags are the limit arguments of `users limit`, shared with
+// CLAUDIO_DEFAULT_LIMIT so both accept exactly the same text.
+type limitFlags struct {
+	tokens, window *string
+	none           *bool
+}
+
+func addLimitFlags(fs *flag.FlagSet) limitFlags {
+	return limitFlags{
+		tokens: fs.String("tokens", "", "limit in output tokens (accepts K/M/G, e.g. 1M)"),
+		window: fs.String("window", "", "rolling window: 1h, 6h, 24h, 7d, 30d"),
+		none:   fs.Bool("none", false, "clear the limit (unlimited)"),
+	}
+}
+
+// value validates the parsed flags; --none is 0, 0.
+func (f limitFlags) value() (limitTokens, windowSecs int64, err error) {
+	switch {
+	case *f.none:
+		if *f.tokens != "" || *f.window != "" {
+			return 0, 0, errors.New("--none cannot be combined with --tokens/--window")
+		}
+		return 0, 0, nil
+	case *f.tokens == "" || *f.window == "":
+		return 0, 0, errors.New("both --tokens and --window are required (or --none to clear)")
+	}
+	u, err := usertoken.ParseTokens(*f.tokens)
+	if err != nil {
+		return 0, 0, err
+	}
+	dur, err := usage.ParsePeriod(*f.window)
+	if err != nil {
+		return 0, 0, err
+	}
+	if u == 0 {
+		return 0, 0, errors.New("--tokens must be greater than 0 (use --none to clear)")
+	}
+	return u, int64(dur / time.Second), nil
+}
+
+// parseDefaultLimit reads CLAUDIO_DEFAULT_LIMIT, the usage limit of members
+// created by a claudio browser login: the arguments of `users limit` after
+// the id ("--tokens 1M --window 24h" or "--none"). Empty or "0" = unlimited.
+func parseDefaultLimit(v string) (limitTokens, windowSecs int64, err error) {
+	v = strings.TrimSpace(v)
+	if v == "" || v == "0" {
+		return 0, 0, nil
+	}
+	fs := flag.NewFlagSet("CLAUDIO_DEFAULT_LIMIT", flag.ContinueOnError)
+	fs.SetOutput(io.Discard)
+	lf := addLimitFlags(fs)
+	if err := fs.Parse(strings.Fields(v)); err != nil {
+		return 0, 0, err
+	}
+	if fs.NArg() > 0 {
+		return 0, 0, fmt.Errorf("unexpected argument %q", fs.Arg(0))
+	}
+	return lf.value()
+}
+
 func usersStats(ctx context.Context, args []string) {
 	fs := flag.NewFlagSet("users stats", flag.ExitOnError)
 	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
diff --git a/internal/webui/claudiologin.go b/internal/webui/claudiologin.go
new file mode 100644
index 0000000..8fb3dff
--- /dev/null
+++ b/internal/webui/claudiologin.go
@@ -0,0 +1,270 @@
+package webui
+
+// The browser half of `claudio proxy login` (the claudio half is
+// claudioapi/login.go). A member opens /claudio/login, signs in through SSO,
+// types the code their terminal shows and approves it. The page never holds a
+// token: approval stores the member's user-token id on the pending login and
+// claudio's poll reads the token.
+
+import (
+	"context"
+	"errors"
+	"html/template"
+	"log/slog"
+	"net/http"
+	"strings"
+
+	vocauth "github.com/vocdoni/oauth/go"
+
+	"github.com/p4u/claude-proxy/internal/usertoken"
+)
+
+// claudioLoginPath is the approval page; claudioapi's verify_url points here.
+const claudioLoginPath = "/claudio/login"
+
+// LoginApprover is the pending-login store of the claudio API
+// (*claudioapi.Logins). Codes are matched ignoring case, dashes and spaces.
+type LoginApprover interface {
+	// Check reports whether code names a pending, unexpired login.
+	Check(code string) bool
+	// Approve marks the login approved for the user token userID; false when
+	// the code is unknown, expired or already decided.
+	Approve(code, userID string) bool
+	// Deny marks the login denied; false as for Approve.
+	Deny(code string) bool
+}
+
+// MemberLimit is the usage limit given to a member created at their first
+// claudio login (CLAUDIO_DEFAULT_LIMIT). Zero values mean unlimited.
+type MemberLimit struct {
+	OutputTokens  int64
+	WindowSeconds int64
+}
+
+// LoginURL is the claudio approval page as a member's browser reaches it.
+func (s *SSO) LoginURL() string { return s.origin + claudioLoginPath }
+
+// inLoginDomain reports whether email belongs to one of the login domains.
+// The session was admitted against the same list; this re-check keeps a
+// cookie minted under an older configuration from outliving it.
+func (s *SSO) inLoginDomain(email string) bool {
+	at := strings.LastIndexByte(email, '@')
+	return at > 0 && s.domains[strings.ToLower(email[at+1:])]
+}
+
+// loginPostSameOrigin is sameOrigin for the approval form. The page is
+// served with Referrer-Policy: no-referrer, which makes browsers send
+// "Origin: null" on its own form posts, so a null origin is accepted only
+// with Sec-Fetch-Site: same-origin (a header no page can forge).
+func (s *SSO) loginPostSameOrigin(r *http.Request) bool {
+	if r.Header.Get("Origin") == "null" {
+		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
+	}
+	return s.sameOrigin(r)
+}
+
+// setClaudioLoginHeaders keeps the page out of frames and caches and gives it
+// no script, no remote resources and no form target but itself.
+func setClaudioLoginHeaders(h http.Header) {
+	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
+	h.Set("X-Frame-Options", "DENY")
+	h.Set("Cache-Control", "no-store")
+	h.Set("Referrer-Policy", "no-referrer")
+}
+
+// serveClaudioLogin answers GET and POST /claudio/login. Without SSO or the
+// claudio API's login store it is a 404, like /api/login under SSO.
+func (s *Server) serveClaudioLogin(w http.ResponseWriter, r *http.Request) {
+	if s.sso == nil || s.logins == nil {
+		http.NotFound(w, r)
+		return
+	}
+	setClaudioLoginHeaders(w.Header())
+	switch r.Method {
+	case http.MethodGet, http.MethodHead:
+		// Require (not user()): an unauthenticated browser is redirected to
+		// /auth/login?return_to=/claudio/login and comes back signed in.
+		s.sso.auth.Require(http.HandlerFunc(s.claudioLoginForm)).ServeHTTP(w, r)
+	case http.MethodPost:
+		// A missing session answers 401.
+		s.sso.auth.Require(http.HandlerFunc(s.claudioLoginPost)).ServeHTTP(w, r)
+	default:
+		w.Header().Set("Allow", "GET, POST")
+		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
+	}
+}
+
+func (s *Server) claudioLoginForm(w http.ResponseWriter, r *http.Request) {
+	u, _ := vocauth.FromContext(r.Context())
+	if !s.sso.inLoginDomain(u.Email) {
+		renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
+			Message: "This account cannot sign in to claudio."})
+		return
+	}
+	renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "form", Email: u.Email})
+}
+
+func (s *Server) claudioLoginPost(w http.ResponseWriter, r *http.Request) {
+	u, _ := vocauth.FromContext(r.Context())
+	if !s.sso.loginPostSameOrigin(r) {
+		renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
+			Message: "Cross-origin request refused."})
+		return
+	}
+	if !s.sso.inLoginDomain(u.Email) {
+		renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
+			Message: "This account cannot sign in to claudio."})
+		return
+	}
+	r.Body = http.MaxBytesReader(w, r.Body, 4096)
+	if err := r.ParseForm(); err != nil {
+		renderClaudioLogin(w, http.StatusBadRequest, loginPage{Kind: "error", Email: u.Email, Message: "Bad request."})
+		return
+	}
+	code := displayCode(r.PostForm.Get("code"))
+	unknown := loginPage{Kind: "error", Email: u.Email,
+		Message: "This code is unknown, expired or already used. Run claudio proxy login again."}
+	if code == "" || !s.logins.Check(code) {
+		renderClaudioLogin(w, http.StatusBadRequest, unknown)
+		return
+	}
+	switch r.PostForm.Get("action") {
+	case "lookup":
+		renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "confirm", Email: u.Email, Code: code,
+			Hidden: strings.ReplaceAll(code, "-", "")})
+	case "approve":
+		ut, err := s.memberToken(r.Context(), u.Email)
+		if err != nil {
+			slog.Error("claudio login: member lookup failed", "email", u.Email, "err", err)
+			renderClaudioLogin(w, http.StatusInternalServerError, loginPage{Kind: "error", Email: u.Email,
+				Message: "Your account could not be prepared. Try again or contact an admin."})
+			return
+		}
+		if ut.Status != usertoken.StatusActive {
+			renderClaudioLogin(w, http.StatusForbidden, loginPage{Kind: "error", Email: u.Email,
+				Message: "This account is disabled for claudio: account disabled, contact an admin."})
+			return
+		}
+		if !s.logins.Approve(code, ut.ID) {
+			renderClaudioLogin(w, http.StatusBadRequest, unknown)
+			return
+		}
+		renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "done", Email: u.Email,
+			Message: "Approved. Return to your terminal: claudio is signed in."})
+	case "deny":
+		if !s.logins.Deny(code) {
+			renderClaudioLogin(w, http.StatusBadRequest, unknown)
+			return
+		}
+		renderClaudioLogin(w, http.StatusOK, loginPage{Kind: "done", Email: u.Email,
+			Message: "Denied. Nothing was signed in."})
+	default:
+		renderClaudioLogin(w, http.StatusBadRequest, loginPage{Kind: "error", Email: u.Email, Message: "Bad request."})
+	}
+}
+
+// memberToken returns the member's user token, creating it (name = e-mail,
+// default limit) on their first login.
+func (s *Server) memberToken(ctx context.Context, email string) (*usertoken.UserToken, error) {
+	ut, err := usertoken.LookupByEmail(ctx, s.db, email)
+	if !errors.Is(err, usertoken.ErrNotFound) {
+		return ut, err
+	}
+	ut, err = usertoken.CreateForEmail(ctx, s.db, email)
+	if errors.Is(err, usertoken.ErrEmailTaken) {
+		// A concurrent approval created it first.
+		return usertoken.LookupByEmail(ctx, s.db, email)
+	}
+	if err != nil {
+		return nil, err
+	}
+	if l := s.defaultLimit; usertoken.HasLimit(l.OutputTokens, l.WindowSeconds) {
+		if err := usertoken.SetLimit(ctx, s.db, ut.ID, l.OutputTokens, l.WindowSeconds); err != nil {
+			// Never leave a member unlimited by accident.
+			_ = usertoken.Delete(ctx, s.db, ut.ID)
+			return nil, err
+		}
+		ut.LimitOutputTokens, ut.LimitWindowSeconds = l.OutputTokens, l.WindowSeconds
+	}
+	slog.Info("login: created user "+ut.Email, "id", ut.ID, "name", ut.Name)
+	return ut, nil
+}
+
+// displayCode is a typed code as XXXX-XXXX (upper case, dashes and spaces
+// dropped), or "" when it is not 8 letters and digits. The claudio API does
+// the authoritative check.
+func displayCode(s string) string {
+	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
+	if len(s) != 8 {
+		return ""
+	}
+	for i := 0; i < len(s); i++ {
+		if c := s[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
+			return ""
+		}
+	}
+	return s[:4] + "-" + s[4:]
+}
+
+type loginPage struct {
+	Kind    string // form | confirm | done | error
+	Email   string
+	Code    string // XXXX-XXXX, shown
+	Hidden  string // the code as posted back
+	Message string
+}
+
+func renderClaudioLogin(w http.ResponseWriter, status int, p loginPage) {
+	w.Header().Set("Content-Type", "text/html; charset=utf-8")
+	w.WriteHeader(status)
+	if err := claudioLoginTmpl.Execute(w, p); err != nil {
+		slog.Error("claudio login page", "err", err)
+	}
+}
+
+var claudioLoginTmpl = template.Must(template.New("claudio-login").Parse(`<!doctype html>
+<html lang="en">
+<head>
+<meta charset="utf-8">
+<meta name="viewport" content="width=device-width, initial-scale=1">
+<title>claudio sign-in</title>
+<style>
+body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#f6f7f8;color:#1b1f23}
+main{max-width:28rem;margin:4rem auto;padding:2rem;background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.12)}
+h1{font-size:1.3rem;margin:0 0 1rem}
+label{display:block;margin-bottom:.4rem}
+input[name=code]{font:1.4rem ui-monospace,monospace;letter-spacing:.15em;text-transform:uppercase;width:100%;box-sizing:border-box;padding:.5rem}
+button{font:inherit;padding:.5rem 1.2rem;margin:1rem .5rem 0 0;border:0;border-radius:6px;background:#1f6f5c;color:#fff;cursor:pointer}
+button.deny{background:#6b7177}
+.code{font:2.4rem ui-monospace,monospace;letter-spacing:.2em;text-align:center;margin:1rem 0}
+.warn{background:#fff4d6;padding:.6rem .8rem;border-radius:6px}
+.who{color:#5c636a;font-size:.9rem;margin-top:1.5rem}
+</style>
+</head>
+<body>
+<main>
+<h1>claudio sign-in</h1>
+{{if eq .Kind "form"}}
+<form method="post" action="/claudio/login">
+<input type="hidden" name="action" value="lookup">
+<label for="code">Type the code shown in your terminal</label>
+<input id="code" name="code" autocomplete="off" autocapitalize="characters" spellcheck="false" maxlength="16" required autofocus>
+<button type="submit">Continue</button>
+</form>
+{{else if eq .Kind "confirm"}}
+<p class="code">{{.Code}}</p>
+<p class="warn">Approve only if this code is shown in your terminal.</p>
+<p>claudio will be signed in as <strong>{{.Email}}</strong>.</p>
+<form method="post" action="/claudio/login">
+<input type="hidden" name="code" value="{{.Hidden}}">
+<button type="submit" name="action" value="approve">Approve</button>
+<button type="submit" name="action" value="deny" class="deny">Deny</button>
+</form>
+{{else}}
+<p>{{.Message}}</p>
+{{end}}
+{{if .Email}}<p class="who">Signed in as {{.Email}}</p>{{end}}
+</main>
+</body>
+</html>
+`))
diff --git a/internal/webui/webui.go b/internal/webui/webui.go
index 9768724..4240026 100644
--- a/internal/webui/webui.go
+++ b/internal/webui/webui.go
@@ -38,6 +38,11 @@ type Server struct {
 	codex   *codexgateway.Client
 	// claudeLogin holds in-flight Anthropic browser sign-ins.
 	claudeLogin *claudeoauth.Flow
+	// logins is the claudio API's pending device logins, approved on
+	// /claudio/login; nil turns the page off.
+	logins LoginApprover
+	// defaultLimit is given to members created at their first claudio login.
+	defaultLimit MemberLimit
 
 	codexOAuthMu   sync.Mutex
 	codexOAuthLast time.Time
@@ -65,6 +70,11 @@ type Options struct {
 	SSO           *SSO
 	SecureCookies bool
 	Codex         *codexgateway.Client
+	// Logins enables the claudio approval page (/claudio/login); it needs
+	// SSO. Nil leaves the page a 404.
+	Logins LoginApprover
+	// DefaultLimit is the usage limit of members created by a claudio login.
+	DefaultLimit MemberLimit
 }
 
 // NewWithOptions builds the web UI handler.
@@ -91,6 +101,8 @@ func NewWithOptions(db *store.DB, refresher *creds.Refresher, opts Options) http
 		limiter:       newLoginLimiter(),
 		codex:         codex,
 		claudeLogin:   claudeoauth.New(),
+		logins:        opts.Logins,
+		defaultLimit:  opts.DefaultLimit,
 	}
 }
 
@@ -111,6 +123,8 @@ func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
 	case s.sso != nil && strings.HasPrefix(path, "/auth/"):
 		// GET /auth/login, GET /auth/callback, POST /auth/logout.
 		s.sso.auth.Routes().ServeHTTP(w, r)
+	case path == claudioLoginPath:
+		s.serveClaudioLogin(w, r)
 	case path == "/api" || strings.HasPrefix(path, "/api/"):
 		s.serveAPI(w, r, strings.TrimPrefix(path, "/api"))
 	default:
````
Run: `go test ./internal/webui/ ./cmd/claude-proxy/ ./internal/claudioapi/ && go build ./... && GOTOOLCHAIN=go1.26.2 make lint`
Expected: three `ok` lines; lint `0 issues.`

Step 3: commit
`git add cmd/claude-proxy/limit_test.go internal/webui/claudiologin.go internal/webui/claudiologin_test.go` then `git commit -m "feat(webui): claudio login approval page" -- .env.example cmd/claude-proxy/limit_test.go cmd/claude-proxy/main.go internal/webui/claudiologin.go internal/webui/claudiologin_test.go internal/webui/webui.go`

Interfaces this task provides to later tasks: `webui.LoginApprover` (satisfied by `*claudioapi.Logins`; checked at compile time in `main.go`); `webui.MemberLimit{OutputTokens, WindowSeconds}`; `webui.Options.Logins`, `.DefaultLimit`; `(*SSO).LoginURL()`; `main.parseDefaultLimit(v)`, `main.addLimitFlags(fs)` / `limitFlags.value()` (shared with `users limit`).

### Task 7: Plugins CLI, /api/plugins and the Plugins page
Model: standard   Repo: /volumes/repos/claude-proxy   Files: cmd/claude-proxy/main.go, cmd/claude-proxy/plugins.go, cmd/claude-proxy/plugins_test.go, docs/WEBUI.md, internal/webui/plugins.go, internal/webui/plugins_test.go, internal/webui/static/css/plugins.css, internal/webui/static/index.html, internal/webui/static/js/api.js, internal/webui/static/js/app.js, internal/webui/static/js/mock.js, internal/webui/static/js/pages/plugins.js, internal/webui/webui.go
Goal: Spec §3 admin surfaces: `claude-proxy plugins …` (local DB), `/api/plugins` routes behind `ssoAuthorize`, the **Plugins** page with write-only values, `mock.js` and `docs/WEBUI.md` (AC4 plugins part, AC6).
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-7.patch

Notes: Flags may come anywhere (`parseInterleaved`), so `plugins env set <name> KEY=- --db PATH` works. `KEY=-` values are read from stdin one line each in argument order (CR/LF stripped); `--env-file` skips blank and `#` lines and strips one pair of surrounding quotes. CLI and API output never contains a value (`TestPluginsCLIRoundTrip` greps all output for the secrets; the API test greps every body). A JSON decode error on the env body answers a fixed message so the body is never echoed. Manually checked: `index.html?mock=1#/plugins` and `#/users` render (headless Obscura against a static server).

Step 1: write the failing test
````diff
diff --git a/cmd/claude-proxy/plugins_test.go b/cmd/claude-proxy/plugins_test.go
new file mode 100644
index 0000000..33613e7
--- /dev/null
+++ b/cmd/claude-proxy/plugins_test.go
@@ -0,0 +1,124 @@
+package main
+
+import (
+	"bytes"
+	"context"
+	"os"
+	"path/filepath"
+	"strings"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/plugins"
+)
+
+// runCLI runs `claude-proxy plugins <args> --db path` and returns the exit
+// code, stdout and stderr.
+func runCLI(t *testing.T, path, stdin string, args ...string) (int, string, string) {
+	t.Helper()
+	var stdout, stderr bytes.Buffer
+	code := runPlugins(append(args, "--db", path), strings.NewReader(stdin), &stdout, &stderr)
+	return code, stdout.String(), stderr.String()
+}
+
+func TestPluginsCLIRoundTrip(t *testing.T) {
+	ctx := context.Background()
+	path, db := cliDB(t)
+	sha := strings.Repeat("ab", 32)
+	secrets := []string{"kb-secret", "notion-secret", "file-secret", "argv-secret"}
+	var out strings.Builder
+	run := func(stdin string, args ...string) {
+		t.Helper()
+		code, stdout, stderr := runCLI(t, path, stdin, args...)
+		if code != 0 {
+			t.Fatalf("plugins %v: exit %d, stderr %q", args, code, stderr)
+		}
+		out.WriteString(stdout + stderr)
+	}
+
+	run("", "set", "vocdoni-internal", "--version", "1.4.0",
+		"--url", "https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip",
+		"--sha256", sha, "--installed-id", "vocdoni-internal@vocdoni")
+	run("kb-secret\nnotion-secret\n", "env", "set", "vocdoni-internal", "VOCDONI_KB_TOKEN=-", "NOTION_TOKEN=-")
+
+	envFile := filepath.Join(t.TempDir(), "plugin.env")
+	if err := os.WriteFile(envFile, []byte("# comment\n\nFILE_TOKEN=\"file-secret\"\n"), 0o600); err != nil {
+		t.Fatal(err)
+	}
+	run("", "env", "set", "vocdoni-internal", "--env-file", envFile)
+	run("", "env", "set", "vocdoni-internal", "ARGV_TOKEN=argv-secret")
+
+	client, err := plugins.ListForClient(ctx, db)
+	if err != nil || len(client) != 1 {
+		t.Fatalf("client list: %v %+v", err, client)
+	}
+	env := client[0].Env
+	if env["VOCDONI_KB_TOKEN"] != "kb-secret" || env["NOTION_TOKEN"] != "notion-secret" ||
+		env["FILE_TOKEN"] != "file-secret" || env["ARGV_TOKEN"] != "argv-secret" {
+		t.Fatalf("env = %v", env)
+	}
+
+	code, list, _ := runCLI(t, path, "", "list")
+	if code != 0 || !strings.Contains(list, "vocdoni-internal") || !strings.Contains(list, "1.4.0") ||
+		!strings.Contains(list, "vocdoni-internal@vocdoni") || !strings.Contains(list, "on") {
+		t.Fatalf("list:\n%s", list)
+	}
+	out.WriteString(list)
+	code, keys, _ := runCLI(t, path, "", "env", "list", "vocdoni-internal")
+	if code != 0 || strings.TrimSpace(keys) != "ARGV_TOKEN\nFILE_TOKEN\nNOTION_TOKEN\nVOCDONI_KB_TOKEN" {
+		t.Fatalf("env list:\n%s", keys)
+	}
+	out.WriteString(keys)
+
+	run("", "env", "unset", "vocdoni-internal", "ARGV_TOKEN", "FILE_TOKEN")
+	run("", "disable", "vocdoni-internal")
+	if client, _ := plugins.ListForClient(ctx, db); len(client) != 0 {
+		t.Fatal("disabled plugin still served")
+	}
+	run("", "enable", "vocdoni-internal")
+	p, _ := plugins.Get(ctx, db, "vocdoni-internal")
+	if !p.Enabled || strings.Join(p.EnvKeys, ",") != "NOTION_TOKEN,VOCDONI_KB_TOKEN" {
+		t.Fatalf("after unset/enable: %+v", p)
+	}
+	run("", "delete", "vocdoni-internal")
+	if _, err := plugins.Get(ctx, db, "vocdoni-internal"); err != plugins.ErrNotFound {
+		t.Fatalf("after delete: %v", err)
+	}
+
+	for _, s := range secrets {
+		if strings.Contains(out.String(), s) {
+			t.Fatalf("CLI output shows a secret value %q:\n%s", s, out.String())
+		}
+	}
+}
+
+func TestPluginsCLIErrors(t *testing.T) {
+	path, _ := cliDB(t)
+	sha := strings.Repeat("ab", 32)
+	var stderr bytes.Buffer
+	if code := runPlugins(nil, strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 2 ||
+		!strings.Contains(stderr.String(), "missing subcommand") {
+		t.Errorf("no subcommand: exit %d stderr %q", code, stderr.String())
+	}
+	for _, tc := range []struct {
+		stdin string
+		args  []string
+		code  int
+		msg   string
+	}{
+		{"", []string{"bogus"}, 2, "unknown subcommand"},
+		{"", []string{"set", "p", "--version", "1", "--url", "http://x.test/a.zip", "--sha256", sha}, 1, "https"},
+		{"", []string{"set", "p", "--url", "https://x.test/a.zip", "--sha256", sha}, 2, "--version"},
+		{"", []string{"enable", "missing"}, 1, "not found"},
+		{"x\n", []string{"env", "set", "missing", "A=-"}, 1, "not found"},
+		{"one\n", []string{"env", "set", "p", "A=-", "B=-"}, 1, "stdin"},
+		{"", []string{"env", "set", "p", "lower=1"}, 1, "env key"},
+		{"", []string{"env", "set", "p", "NOEQUALS"}, 2, "KEY=VALUE"},
+		{"", []string{"env", "set", "p"}, 2, "no variables"},
+		{"", []string{"env", "list"}, 2, "plugins env list <name>"},
+	} {
+		code, _, stderr := runCLI(t, path, tc.stdin, tc.args...)
+		if code != tc.code || !strings.Contains(stderr, tc.msg) {
+			t.Errorf("plugins %v: exit %d stderr %q, want exit %d containing %q", tc.args, code, stderr, tc.code, tc.msg)
+		}
+	}
+}
diff --git a/internal/webui/plugins_test.go b/internal/webui/plugins_test.go
new file mode 100644
index 0000000..3836c75
--- /dev/null
+++ b/internal/webui/plugins_test.go
@@ -0,0 +1,127 @@
+package webui
+
+import (
+	"context"
+	"encoding/json"
+	"net/http"
+	"strings"
+	"testing"
+
+	"github.com/p4u/claude-proxy/internal/plugins"
+)
+
+func TestPluginsAPIRoundTrip(t *testing.T) {
+	db, h := newTestServer(t)
+	c := loginCookie(t, h)
+	ctx := context.Background()
+	sha := strings.Repeat("cd", 32)
+
+	meta := `{"version":"1.4.0","bundle_url":"https://example.org/vi.zip","bundle_sha256":"` + sha +
+		`","installed_id":"vocdoni-internal@vocdoni"}`
+	if w := do(t, h, http.MethodPut, "/api/plugins/vocdoni-internal", meta, c); w.Code != http.StatusOK {
+		t.Fatalf("PUT meta: %d %s", w.Code, w.Body.String())
+	}
+	w := do(t, h, http.MethodPut, "/api/plugins/vocdoni-internal/env",
+		`{"VOCDONI_KB_TOKEN":"kb-secret","NOTION_TOKEN":"notion-secret"}`, c)
+	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") {
+		t.Fatalf("PUT env: %d %s", w.Code, w.Body.String())
+	}
+
+	w = do(t, h, http.MethodGet, "/api/plugins", "", c)
+	if w.Code != http.StatusOK {
+		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
+	}
+	if strings.Contains(w.Body.String(), "kb-secret") || strings.Contains(w.Body.String(), "notion-secret") {
+		t.Fatalf("list shows a value: %s", w.Body.String())
+	}
+	var list struct {
+		Plugins []struct {
+			Name         string   `json:"name"`
+			Version      string   `json:"version"`
+			BundleURL    string   `json:"bundle_url"`
+			BundleSHA256 string   `json:"bundle_sha256"`
+			InstalledID  string   `json:"installed_id"`
+			Enabled      bool     `json:"enabled"`
+			UpdatedAt    string   `json:"updated_at"`
+			EnvKeys      []string `json:"env_keys"`
+		} `json:"plugins"`
+	}
+	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
+		t.Fatal(err)
+	}
+	if len(list.Plugins) != 1 {
+		t.Fatalf("plugins = %+v", list.Plugins)
+	}
+	p := list.Plugins[0]
+	if p.Name != "vocdoni-internal" || p.Version != "1.4.0" || p.BundleURL != "https://example.org/vi.zip" ||
+		p.BundleSHA256 != sha || p.InstalledID != "vocdoni-internal@vocdoni" || !p.Enabled || p.UpdatedAt == "" ||
+		strings.Join(p.EnvKeys, ",") != "NOTION_TOKEN,VOCDONI_KB_TOKEN" {
+		t.Fatalf("plugin = %+v", p)
+	}
+
+	// Disable hides it from claudio (GET /v1/claudio/plugins serves ListForClient).
+	if w := do(t, h, http.MethodPost, "/api/plugins/vocdoni-internal/disable", "", c); w.Code != http.StatusOK {
+		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
+	}
+	if served, _ := plugins.ListForClient(ctx, db); len(served) != 0 {
+		t.Fatalf("disabled plugin served: %+v", served)
+	}
+	if w := do(t, h, http.MethodPost, "/api/plugins/vocdoni-internal/enable", "", c); w.Code != http.StatusOK {
+		t.Fatalf("enable: %d", w.Code)
+	}
+	served, _ := plugins.ListForClient(ctx, db)
+	if len(served) != 1 || served[0].Env["VOCDONI_KB_TOKEN"] != "kb-secret" {
+		t.Fatalf("served after enable: %+v", served)
+	}
+
+	// Replacing the metadata keeps the env.
+	if w := do(t, h, http.MethodPut, "/api/plugins/vocdoni-internal", strings.Replace(meta, "1.4.0", "1.5.0", 1), c); w.Code != http.StatusOK {
+		t.Fatalf("PUT meta again: %d", w.Code)
+	}
+	if got, _ := plugins.Get(ctx, db, "vocdoni-internal"); got.Version != "1.5.0" || len(got.EnvKeys) != 2 {
+		t.Fatalf("after replace: %+v", got)
+	}
+
+	if w := do(t, h, http.MethodDelete, "/api/plugins/vocdoni-internal/env/NOTION_TOKEN", "", c); w.Code != http.StatusOK {
+		t.Fatalf("DELETE key: %d %s", w.Code, w.Body.String())
+	}
+	if w := do(t, h, http.MethodDelete, "/api/plugins/vocdoni-internal/env/NOTION_TOKEN", "", c); w.Code != http.StatusNotFound {
+		t.Fatalf("DELETE key twice: %d", w.Code)
+	}
+	if w := do(t, h, http.MethodDelete, "/api/plugins/vocdoni-internal", "", c); w.Code != http.StatusOK {
+		t.Fatalf("DELETE: %d", w.Code)
+	}
+	if w := do(t, h, http.MethodPost, "/api/plugins/vocdoni-internal/enable", "", c); w.Code != http.StatusNotFound {
+		t.Fatalf("enable deleted: %d", w.Code)
+	}
+}
+
+func TestPluginsAPIValidation(t *testing.T) {
+	_, h := newTestServer(t)
+	c := loginCookie(t, h)
+	sha := strings.Repeat("cd", 32)
+	for _, tc := range []struct {
+		method, path, body string
+		code               int
+	}{
+		{http.MethodPut, "/api/plugins/vocdoni-internal", `{"version":"1","bundle_url":"http://x.test/a.zip","bundle_sha256":"` + sha + `"}`, 400},
+		{http.MethodPut, "/api/plugins/Bad_Name", `{"version":"1","bundle_url":"https://x.test/a.zip","bundle_sha256":"` + sha + `"}`, 400},
+		{http.MethodPut, "/api/plugins/vocdoni-internal", `{"version":"1"`, 400},
+		{http.MethodPut, "/api/plugins/missing/env", `{"A":"1"}`, 404},
+		{http.MethodPut, "/api/plugins/missing/env", `{"lower":"1"}`, 400},
+		{http.MethodPut, "/api/plugins/missing/env", `{"A":1}`, 400},
+		{http.MethodGet, "/api/plugins/vocdoni-internal/env", "", 404},
+		{http.MethodPost, "/api/plugins/vocdoni-internal/bogus", "", 404},
+	} {
+		w := do(t, h, tc.method, tc.path, tc.body, c)
+		if w.Code != tc.code {
+			t.Errorf("%s %s: %d %s, want %d", tc.method, tc.path, w.Code, w.Body.String(), tc.code)
+		}
+		if tc.code == 400 && !strings.Contains(w.Body.String(), `"error"`) {
+			t.Errorf("%s %s: 400 without an error body: %s", tc.method, tc.path, w.Body.String())
+		}
+	}
+	if w := do(t, h, http.MethodGet, "/api/plugins", "", nil); w.Code != http.StatusUnauthorized {
+		t.Fatalf("unauthenticated: %d", w.Code)
+	}
+}
````
Run: `go test ./cmd/claude-proxy/ ./internal/webui/ -run Plugins`
Expected: `plugins_test.go: undefined: runPlugins` (cmd build failed); `--- FAIL: TestPluginsAPIRoundTrip` (`PUT meta: 404`), `--- FAIL: TestPluginsAPIValidation`

Step 2: implement
````diff
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index cb6783c..bce94bd 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -47,6 +47,8 @@ Usage:
   claude-proxy users rm <id>          [--db PATH]
   claude-proxy users refresh <id>     [--db PATH]
   claude-proxy users limit <id>       --tokens 1M --window 24h | --none [--db PATH]
+  claude-proxy plugins list|set|enable|disable|delete|env …  [--db PATH]
+                                   # plugins served to claudio; 'plugins' alone for help
   claude-proxy tui                 [--db PATH]   # interactive management UI
   claude-proxy creds import        --from FILE [--label NAME] [--weight N]
   claude-proxy creds add-key       --provider glm|mimo|privatemode [--endpoint sgp|https://…]
@@ -83,6 +85,8 @@ func main() {
 		runCreds(os.Args[2:])
 	case "users":
 		runUsers(os.Args[2:])
+	case "plugins":
+		os.Exit(runPlugins(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
 	case "-h", "--help", "help":
 		fmt.Print(helpText)
 	default:
diff --git a/cmd/claude-proxy/plugins.go b/cmd/claude-proxy/plugins.go
new file mode 100644
index 0000000..ad1be14
--- /dev/null
+++ b/cmd/claude-proxy/plugins.go
@@ -0,0 +1,288 @@
+package main
+
+import (
+	"bufio"
+	"context"
+	"errors"
+	"flag"
+	"fmt"
+	"io"
+	"os"
+	"strings"
+
+	"github.com/p4u/claude-proxy/internal/plugins"
+	"github.com/p4u/claude-proxy/internal/store"
+)
+
+const pluginsUsage = `claude-proxy plugins — plugins served to claudio (local DB only)
+
+  plugins list
+  plugins set <name> --version V --url https://… --sha256 HEX [--installed-id name@marketplace]
+  plugins enable|disable|delete <name>
+  plugins env set <name> KEY=- [KEY=-…]     # values read from stdin, one line each, in order
+  plugins env set <name> --env-file PATH     # KEY=VALUE lines
+  plugins env set <name> KEY=VALUE           # accepted, but the value is visible in ps
+  plugins env unset <name> KEY…
+  plugins env list <name>                    # keys only
+
+Every subcommand takes --db PATH (default ./proxy.db). Values are never printed.
+`
+
+// runPlugins is `claude-proxy plugins …`. It returns the exit code: 0, 1 for
+// a failure, 2 for a usage error.
+func runPlugins(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
+	if len(args) == 0 {
+		fmt.Fprint(stderr, "plugins: missing subcommand\n\n"+pluginsUsage)
+		return 2
+	}
+	ctx := context.Background()
+	fs := flag.NewFlagSet("plugins "+args[0], flag.ContinueOnError)
+	fs.SetOutput(stderr)
+	dbPath := fs.String("db", "./proxy.db", "sqlite database path")
+	switch args[0] {
+	case "list":
+		pos, err := parseInterleaved(fs, args[1:])
+		if err != nil || len(pos) != 0 {
+			fmt.Fprintln(stderr, "plugins list [--db PATH]")
+			return 2
+		}
+		return withDB(*dbPath, stderr, func(db *store.DB) error { return pluginsList(ctx, db, stdout) })
+	case "set":
+		version := fs.String("version", "", "bundle version (1-64 printable ASCII)")
+		url := fs.String("url", "", "https URL of the bundle zip")
+		sha := fs.String("sha256", "", "SHA-256 of the zip, 64 lowercase hex")
+		installed := fs.String("installed-id", "", "installed Claude Code plugin that makes the bundle redundant (name@marketplace)")
+		pos, err := parseInterleaved(fs, args[1:])
+		if err != nil || len(pos) != 1 {
+			fmt.Fprintln(stderr, "plugins set <name> --version V --url URL --sha256 HEX [--installed-id ID]")
+			return 2
+		}
+		for flagName, v := range map[string]string{"--version": *version, "--url": *url, "--sha256": *sha} {
+			if v == "" {
+				fmt.Fprintf(stderr, "%s is required\n", flagName)
+				return 2
+			}
+		}
+		meta := plugins.Meta{Version: *version, BundleURL: *url, BundleSHA256: *sha, InstalledID: *installed}
+		return withDB(*dbPath, stderr, func(db *store.DB) error {
+			if err := plugins.Set(ctx, db, pos[0], meta); err != nil {
+				return err
+			}
+			fmt.Fprintf(stdout, "%s: set to %s\n", pos[0], meta.Version)
+			return nil
+		})
+	case "enable", "disable", "delete":
+		pos, err := parseInterleaved(fs, args[1:])
+		if err != nil || len(pos) != 1 {
+			fmt.Fprintf(stderr, "plugins %s <name> [--db PATH]\n", args[0])
+			return 2
+		}
+		return withDB(*dbPath, stderr, func(db *store.DB) error {
+			var err error
+			switch args[0] {
+			case "enable":
+				err = plugins.Enable(ctx, db, pos[0], true)
+			case "disable":
+				err = plugins.Enable(ctx, db, pos[0], false)
+			default:
+				err = plugins.Delete(ctx, db, pos[0])
+			}
+			if err == nil {
+				fmt.Fprintf(stdout, "%s: %sd\n", pos[0], strings.TrimSuffix(args[0], "e"))
+			}
+			return err
+		})
+	case "env":
+		return pluginsEnv(ctx, fs, dbPath, args[1:], stdin, stdout, stderr)
+	default:
+		fmt.Fprintf(stderr, "plugins: unknown subcommand %q\n\n%s", args[0], pluginsUsage)
+		return 2
+	}
+}
+
+func pluginsEnv(ctx context.Context, fs *flag.FlagSet, dbPath *string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
+	if len(args) == 0 {
+		fmt.Fprint(stderr, "plugins env: missing subcommand (set|unset|list)\n")
+		return 2
+	}
+	switch args[0] {
+	case "list":
+		pos, err := parseInterleaved(fs, args[1:])
+		if err != nil || len(pos) != 1 {
+			fmt.Fprintln(stderr, "plugins env list <name> [--db PATH]")
+			return 2
+		}
+		return withDB(*dbPath, stderr, func(db *store.DB) error {
+			p, err := plugins.Get(ctx, db, pos[0])
+			if err != nil {
+				return err
+			}
+			for _, k := range p.EnvKeys {
+				fmt.Fprintln(stdout, k)
+			}
+			return nil
+		})
+	case "unset":
+		pos, err := parseInterleaved(fs, args[1:])
+		if err != nil || len(pos) < 2 {
+			fmt.Fprintln(stderr, "plugins env unset <name> KEY… [--db PATH]")
+			return 2
+		}
+		return withDB(*dbPath, stderr, func(db *store.DB) error {
+			if err := plugins.UnsetEnv(ctx, db, pos[0], pos[1:]...); err != nil {
+				return err
+			}
+			fmt.Fprintf(stdout, "%s: unset %s\n", pos[0], strings.Join(pos[1:], ", "))
+			return nil
+		})
+	case "set":
+		envFile := fs.String("env-file", "", "file of KEY=VALUE lines")
+		pos, err := parseInterleaved(fs, args[1:])
+		if err != nil || len(pos) < 1 {
+			fmt.Fprintln(stderr, "plugins env set <name> KEY=-… | --env-file PATH | KEY=VALUE [--db PATH]")
+			return 2
+		}
+		env := make(map[string]string)
+		if *envFile != "" {
+			if err := readEnvFile(*envFile, env); err != nil {
+				fmt.Fprintf(stderr, "env-file: %v\n", err)
+				return 1
+			}
+		}
+		lines := bufio.NewReader(stdin)
+		for _, a := range pos[1:] {
+			key, value, ok := strings.Cut(a, "=")
+			if !ok {
+				fmt.Fprintf(stderr, "expected KEY=VALUE or KEY=-, got %q\n", key)
+				return 2
+			}
+			if value == "-" {
+				v, err := readLine(lines)
+				if err != nil {
+					fmt.Fprintf(stderr, "stdin ended before the value of %s\n", key)
+					return 1
+				}
+				value = v
+			}
+			env[key] = value
+		}
+		if len(env) == 0 {
+			fmt.Fprintln(stderr, "no variables given")
+			return 2
+		}
+		return withDB(*dbPath, stderr, func(db *store.DB) error {
+			if err := plugins.SetEnv(ctx, db, pos[0], env); err != nil {
+				return err
+			}
+			fmt.Fprintf(stdout, "%s: set %d variable(s)\n", pos[0], len(env))
+			return nil
+		})
+	default:
+		fmt.Fprintf(stderr, "plugins env: unknown subcommand %q\n", args[0])
+		return 2
+	}
+}
+
+// parseInterleaved parses flags that may come before, between or after the
+// positional arguments, which it returns in order.
+func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
+	var pos []string
+	for {
+		if err := fs.Parse(args); err != nil {
+			return nil, err
+		}
+		args = fs.Args()
+		if len(args) == 0 {
+			return pos, nil
+		}
+		pos = append(pos, args[0])
+		args = args[1:]
+	}
+}
+
+// withDB opens the database, runs fn and maps its error to an exit code.
+// Errors name keys at most, never values (internal/plugins guarantees it).
+func withDB(path string, stderr io.Writer, fn func(*store.DB) error) int {
+	db, err := store.Open(path)
+	if err != nil {
+		fmt.Fprintf(stderr, "open db %s: %v\n", path, err)
+		return 1
+	}
+	defer db.Close()
+	if err := fn(db); err != nil {
+		if errors.Is(err, plugins.ErrNotFound) {
+			fmt.Fprintln(stderr, "plugins: not found")
+		} else {
+			fmt.Fprintf(stderr, "plugins: %v\n", err)
+		}
+		return 1
+	}
+	return 0
+}
+
+func pluginsList(ctx context.Context, db *store.DB, w io.Writer) error {
+	list, err := plugins.List(ctx, db)
+	if err != nil {
+		return err
+	}
+	if len(list) == 0 {
+		fmt.Fprintln(w, "(no plugins)")
+		return nil
+	}
+	fmt.Fprintf(w, "%-24s  %-12s  %-3s  %-32s  %-12s  %-25s  %s\n",
+		"NAME", "VERSION", "ON", "INSTALLED_ID", "SHA256", "UPDATED", "ENV KEYS / URL")
+	for _, p := range list {
+		on, installed := "off", p.InstalledID
+		if p.Enabled {
+			on = "on"
+		}
+		if installed == "" {
+			installed = "-"
+		}
+		keys := strings.Join(p.EnvKeys, ",")
+		if keys == "" {
+			keys = "-"
+		}
+		fmt.Fprintf(w, "%-24s  %-12s  %-3s  %-32s  %-12s  %-25s  %s\n",
+			p.Name, p.Version, on, installed, p.BundleSHA256[:12], p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"), keys)
+		fmt.Fprintf(w, "%-24s  %s\n", "", p.BundleURL)
+	}
+	return nil
+}
+
+// readEnvFile adds the KEY=VALUE lines of path to env. Blank lines and lines
+// starting with # are skipped; one pair of surrounding quotes is removed.
+func readEnvFile(path string, env map[string]string) error {
+	f, err := os.Open(path)
+	if err != nil {
+		return err
+	}
+	defer f.Close()
+	sc := bufio.NewScanner(f)
+	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
+	for n := 1; sc.Scan(); n++ {
+		line := strings.TrimSpace(sc.Text())
+		if line == "" || strings.HasPrefix(line, "#") {
+			continue
+		}
+		key, value, ok := strings.Cut(line, "=")
+		if !ok {
+			return fmt.Errorf("line %d: expected KEY=VALUE", n)
+		}
+		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
+			value = value[1 : len(value)-1]
+		}
+		env[strings.TrimSpace(key)] = value
+	}
+	return sc.Err()
+}
+
+// readLine reads one line of stdin without its line ending; EOF before any
+// byte is an error.
+func readLine(r *bufio.Reader) (string, error) {
+	line, err := r.ReadString('\n')
+	if err != nil && (err != io.EOF || line == "") {
+		return "", io.ErrUnexpectedEOF
+	}
+	return strings.TrimRight(line, "\r\n"), nil
+}
diff --git a/docs/WEBUI.md b/docs/WEBUI.md
index b35351b..adaf51b 100644
--- a/docs/WEBUI.md
+++ b/docs/WEBUI.md
@@ -362,7 +362,7 @@ through Antigravity's OAuth, served by the same sidecar
 meter.
 
 ### Proxy user management (wraps `internal/usertoken`)
-- `GET /api/users` → list incl. status, created_at, last_used_at.
+- `GET /api/users` → list incl. `email` (`""` for admin-created tokens), status, created_at, last_used_at.
 - `POST /api/users` `{name}` → `{id,name,token}`.
 - `GET /api/users/{id}/token` → `{id,name,token}`, `Cache-Control: no-store`.
   Backs the Users page *Connect* panel, which builds copy-paste client settings
@@ -373,6 +373,19 @@ meter.
 - `POST /api/users/{id}/rotate` → `{token}`
 - `DELETE /api/users/{id}`
 
+### Plugins served to claudio (wraps `internal/plugins`)
+- `GET /api/plugins` → `{plugins:[{name,version,bundle_url,bundle_sha256,installed_id,enabled,updated_at,env_keys:[…]}]}`.
+- `PUT /api/plugins/{name}` `{version,bundle_url,bundle_sha256,installed_id}` → create or replace the
+  bundle reference; env and `enabled` untouched. Returns the plugin view.
+- `POST /api/plugins/{name}/enable` | `/disable`; `DELETE /api/plugins/{name}` (env cascades).
+- `PUT /api/plugins/{name}/env` `{"KEY":"value",…}` → upsert the listed keys only.
+- `DELETE /api/plugins/{name}/env/{KEY}`.
+- Validation errors are `400 {"error":…}` (limits in `internal/plugins`: name `[a-z0-9-]{1,40}`,
+  version 1–64 printable ASCII, https URL, 64 lowercase hex, `installed_id` empty or
+  `name@marketplace`, keys `[A-Z_][A-Z0-9_]{0,63}`, values ≤ 4 KiB without C0 controls other than
+  `\n`/`\t`, ≤ 64 keys, ≤ 32 plugins); unknown plugin or key `404`. **No route ever returns a
+  value**: the Plugins page shows keys with a *replace* action, values go in password fields.
+
 ### Routing activity
 - `GET /api/routing/events?limit=50&before=123` →
   `{items:[{id,ts,policy,mode,kind,reason,conversation,source_id,target_id,evidence}],next_before,retention_days:7}`.
diff --git a/internal/webui/plugins.go b/internal/webui/plugins.go
new file mode 100644
index 0000000..299e48e
--- /dev/null
+++ b/internal/webui/plugins.go
@@ -0,0 +1,125 @@
+package webui
+
+import (
+	"errors"
+	"net/http"
+	"strings"
+	"time"
+
+	"github.com/p4u/claude-proxy/internal/plugins"
+)
+
+// pluginView is a plugin on the Plugins page: env keys, never values.
+type pluginView struct {
+	Name         string   `json:"name"`
+	Version      string   `json:"version"`
+	BundleURL    string   `json:"bundle_url"`
+	BundleSHA256 string   `json:"bundle_sha256"`
+	InstalledID  string   `json:"installed_id"`
+	Enabled      bool     `json:"enabled"`
+	UpdatedAt    string   `json:"updated_at"`
+	EnvKeys      []string `json:"env_keys"`
+}
+
+func viewOf(p plugins.Plugin) pluginView {
+	return pluginView{
+		Name: p.Name, Version: p.Version, BundleURL: p.BundleURL, BundleSHA256: p.BundleSHA256,
+		InstalledID: p.InstalledID, Enabled: p.Enabled, UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
+		EnvKeys: p.EnvKeys,
+	}
+}
+
+// handlePlugins routes /plugins. `rest` is the path after "/plugins":
+//
+//	GET    ""                 list
+//	PUT    /{name}            create or replace the bundle reference (env untouched)
+//	POST   /{name}/enable     | /{name}/disable
+//	DELETE /{name}
+//	PUT    /{name}/env        upsert the listed keys {"KEY":"value",…}
+//	DELETE /{name}/env/{KEY}
+//
+// No route ever returns an env value.
+func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request, rest string) {
+	ctx := r.Context()
+	if rest == "" {
+		if r.Method != http.MethodGet {
+			writeErr(w, http.StatusNotFound, "not found")
+			return
+		}
+		list, err := plugins.List(ctx, s.db)
+		if err != nil {
+			writeErr(w, http.StatusInternalServerError, err.Error())
+			return
+		}
+		out := make([]pluginView, 0, len(list))
+		for _, p := range list {
+			out = append(out, viewOf(p))
+		}
+		writeJSON(w, map[string]any{"plugins": out})
+		return
+	}
+	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
+	name := parts[0]
+	var err error
+	switch {
+	case len(parts) == 1 && r.Method == http.MethodPut:
+		var body struct {
+			Version      string `json:"version"`
+			BundleURL    string `json:"bundle_url"`
+			BundleSHA256 string `json:"bundle_sha256"`
+			InstalledID  string `json:"installed_id"`
+		}
+		if derr := decodeJSON(w, r, &body); derr != nil {
+			writeErr(w, http.StatusBadRequest, derr.Error())
+			return
+		}
+		err = plugins.Set(ctx, s.db, name, plugins.Meta{
+			Version: body.Version, BundleURL: body.BundleURL,
+			BundleSHA256: body.BundleSHA256, InstalledID: body.InstalledID,
+		})
+	case len(parts) == 1 && r.Method == http.MethodDelete:
+		err = plugins.Delete(ctx, s.db, name)
+	case len(parts) == 2 && parts[1] == "enable" && r.Method == http.MethodPost:
+		err = plugins.Enable(ctx, s.db, name, true)
+	case len(parts) == 2 && parts[1] == "disable" && r.Method == http.MethodPost:
+		err = plugins.Enable(ctx, s.db, name, false)
+	case len(parts) == 2 && parts[1] == "env" && r.Method == http.MethodPut:
+		var env map[string]string
+		if derr := decodeJSON(w, r, &env); derr != nil {
+			// The decoder's message may quote the body; never echo it.
+			writeErr(w, http.StatusBadRequest, "body must be a JSON object of string values")
+			return
+		}
+		err = plugins.SetEnv(ctx, s.db, name, env)
+	case len(parts) == 3 && parts[1] == "env" && r.Method == http.MethodDelete:
+		err = plugins.UnsetEnv(ctx, s.db, name, parts[2])
+	default:
+		writeErr(w, http.StatusNotFound, "not found")
+		return
+	}
+	switch {
+	case errors.Is(err, plugins.ErrInvalid):
+		writeErr(w, http.StatusBadRequest, err.Error())
+	case errors.Is(err, plugins.ErrNotFound):
+		writeErr(w, http.StatusNotFound, err.Error())
+	case err != nil:
+		writeErr(w, http.StatusInternalServerError, err.Error())
+	default:
+		s.writePlugin(w, r, name)
+	}
+}
+
+// writePlugin answers a successful change with the plugin as it now is, or
+// {"ok":true} once it is gone.
+func (s *Server) writePlugin(w http.ResponseWriter, r *http.Request, name string) {
+	p, err := plugins.Get(r.Context(), s.db, name)
+	if errors.Is(err, plugins.ErrNotFound) {
+		writeJSON(w, map[string]any{"ok": true})
+		return
+	}
+	if err != nil {
+		writeErr(w, http.StatusInternalServerError, err.Error())
+		return
+	}
+	writeJSON(w, viewOf(*p))
+}
diff --git a/internal/webui/static/css/plugins.css b/internal/webui/static/css/plugins.css
new file mode 100644
index 0000000..af315cf
--- /dev/null
+++ b/internal/webui/static/css/plugins.css
@@ -0,0 +1,8 @@
+/* Plugins page (js/pages/plugins.js). */
+.plugin-keys { display: flex; flex-wrap: wrap; gap: 6px; max-width: 28rem; }
+.plugin-key { display: inline-flex; align-items: center; gap: 6px; font-family: var(--mono); font-size: 11.5px; padding: 2px 4px 2px 8px; border: 1px solid var(--border-strong); border-radius: var(--r-sm); background: var(--surface-2); }
+.plugin-key__btn { font: inherit; font-size: 10.5px; color: var(--ink-muted); background: none; border: 0; padding: 2px 4px; cursor: pointer; border-radius: var(--r-sm); }
+.plugin-key__btn:hover { color: var(--ink); background: var(--surface-1); }
+.env-rows { display: flex; flex-direction: column; gap: 10px; margin-bottom: 10px; }
+.env-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1.4fr) auto; gap: 8px; align-items: center; }
+.form-hint { margin: 0; font-size: 11.5px; color: var(--ink-muted); }
diff --git a/internal/webui/static/index.html b/internal/webui/static/index.html
index 9cf85a9..36a22fb 100644
--- a/internal/webui/static/index.html
+++ b/internal/webui/static/index.html
@@ -11,6 +11,7 @@
   <link rel="stylesheet" href="./css/charts.css" />
   <link rel="stylesheet" href="./css/credentials.css" />
   <link rel="stylesheet" href="./css/users.css" />
+  <link rel="stylesheet" href="./css/plugins.css" />
   <link rel="stylesheet" href="./css/dashboard.css" />
   <link rel="stylesheet" href="./css/subscription-value.css" />
 </head>
diff --git a/internal/webui/static/js/api.js b/internal/webui/static/js/api.js
index 3799b71..a3c2fc4 100644
--- a/internal/webui/static/js/api.js
+++ b/internal/webui/static/js/api.js
@@ -139,6 +139,11 @@ export const api = {
   }),
   users: () => request("GET", "/users"),
 
+  // Plugins served to claudio. Env values are write-only: the API returns keys.
+  plugins: () => request("GET", "/plugins"),
+  setPlugin: (name, meta) => request("PUT", `/plugins/${enc(name)}`, meta),
+  setPluginEnv: (name, env) => request("PUT", `/plugins/${enc(name)}/env`, env),
+
   // Per-user capture mode. `full` true ⇒ store both sides of every conversation.
   setUserCapture: (id, full) => request("POST", `/users/${enc(id)}/capture`, { full: !!full }),
 
diff --git a/internal/webui/static/js/app.js b/internal/webui/static/js/app.js
index 708ff35..553fb5d 100644
--- a/internal/webui/static/js/app.js
+++ b/internal/webui/static/js/app.js
@@ -9,6 +9,7 @@ import * as usage from "./pages/usage.js";
 import * as subscriptionValue from "./pages/subscription-value.js";
 import * as credentials from "./pages/credentials.js";
 import * as users from "./pages/users.js";
+import * as plugins from "./pages/plugins.js";
 import { seriesColors } from "./charts.js";
 
 window.__seriesColors = seriesColors;
@@ -19,6 +20,7 @@ const ROUTES = {
   "subscription-value": { label: "Subscription value", icon: "value", mod: subscriptionValue },
   credentials: { label: "Credentials", icon: "key", mod: credentials },
   users: { label: "Users", icon: "users", mod: users },
+  plugins: { label: "Plugins", icon: "plugin", mod: plugins },
 };
 
 const appRoot = document.getElementById("app");
@@ -194,6 +196,7 @@ const ICONS = {
   gauge: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M4 15a8 8 0 0 1 16 0"/><path d="M12 15l4-4"/><circle cx="12" cy="15" r="1.3" fill="currentColor" stroke="none"/></svg>',
   key: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="8" cy="12" r="4"/><path d="M12 12h9M18 12v4M21 12v3"/></svg>',
   users: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="9" cy="8" r="3.2"/><path d="M3.5 20a5.5 5.5 0 0 1 11 0"/><path d="M16 5.2a3.2 3.2 0 0 1 0 6M17.5 20a5.5 5.5 0 0 0-3-4.9"/></svg>',
+  plugin: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M9 3v4M15 3v4"/><rect x="6" y="7" width="12" height="6" rx="1.5"/><path d="M8 13v2a4 4 0 0 0 8 0v-2M12 19v2"/></svg>',
   theme: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.4 1.4M17.6 17.6L19 19M19 5l-1.4 1.4M6.4 17.6L5 19"/></svg>',
   logout: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M14 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8"/><path d="M16 8l4 4-4 4M20 12H9"/></svg>',
   menu: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M4 7h16M4 12h16M4 17h16"/></svg>',
diff --git a/internal/webui/static/js/mock.js b/internal/webui/static/js/mock.js
index d9b8372..d21c6d5 100644
--- a/internal/webui/static/js/mock.js
+++ b/internal/webui/static/js/mock.js
@@ -77,6 +77,20 @@ const hasUsageAPI = (c) => providerOf(c) === "anthropic";
 // so usage_pct/blocked are always derived — editing a limit visibly moves the
 // meter. The four users cover every limit state: healthy, near-limit,
 // unlimited, blocked.
+// Plugins served to claudio; mutated in place by the /plugins routes. Values
+// are kept only to mirror the backend: no mock route ever returns them.
+const PLUGINS = [
+  { name: "vocdoni-internal", version: "1.4.0",
+    bundle_url: "https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip",
+    bundle_sha256: "9f2c4e1ab0d37c55e8a1f04b6d2e9c7a3b5f8d1e0c2a4b6d8f0e1c3a5b7d9f1e",
+    installed_id: "vocdoni-internal@vocdoni", enabled: true, updated_at: new Date(Date.now() - 86400e3).toISOString(),
+    env: { VOCDONI_KB_TOKEN: "x", NOTION_TOKEN: "x" } },
+];
+const pluginView = (p) => ({
+  name: p.name, version: p.version, bundle_url: p.bundle_url, bundle_sha256: p.bundle_sha256,
+  installed_id: p.installed_id, enabled: p.enabled, updated_at: p.updated_at, env_keys: Object.keys(p.env).sort(),
+});
+
 const USERS = [
   { id: "utok_alice", name: "alice", email: "alice@vocdoni.org", status: "active", full_capture: true, block_suggestions: false,
     limit_output_tokens: 1_000_000, limit_window_seconds: 86400, usage_output_tokens: 302_400 },  // ~30%
@@ -643,6 +657,7 @@ const DB = {
   "/codex/oauth/status": () => ({ status: ++sidecarOAuthPolls.codex > 2 ? "ok" : "wait" }),
   "/gemini/accounts": () => ({ configured: true, accounts: GEMINI_ACCOUNTS }),
   "/gemini/oauth/status": () => ({ status: ++sidecarOAuthPolls.gemini > 2 ? "ok" : "wait" }),
+  "/plugins": () => ({ plugins: PLUGINS.map(pluginView).sort((a, b) => a.name.localeCompare(b.name)) }),
   "/users": () =>
     USERS.map((u, i) => ({
       id: u.id, name: u.name, email: u.email || "", status: u.status, full_capture: u.full_capture,
@@ -810,6 +825,43 @@ window.fetch = async (input, init = {}) => {
   if (path === "/login") return json({ authenticated: true });
   if (path === "/logout") return json({ authenticated: false });
 
+  // Plugins: validated like internal/plugins, stateful for the session.
+  const plm = path.match(/^\/plugins\/([^/]+)(?:\/(enable|disable|env)(?:\/([^/]+))?)?$/);
+  if (plm && method !== "GET") {
+    const name = decodeURIComponent(plm[1]);
+    const p = PLUGINS.find((x) => x.name === name);
+    const b = JSON.parse(init.body || "{}");
+    const touch = (x) => { x.updated_at = new Date().toISOString(); return json(pluginView(x)); };
+    if (!plm[2] && method === "PUT") {
+      if (!/^[a-z0-9-]{1,40}$/.test(name)) return json({ error: "invalid plugin: name must match [a-z0-9-]{1,40}" }, 400);
+      if (!/^https:\/\/[^/@]+/.test(b.bundle_url || "")) return json({ error: "invalid plugin: bundle_url must be an https URL without credentials" }, 400);
+      if (!/^[0-9a-f]{64}$/.test(b.bundle_sha256 || "")) return json({ error: "invalid plugin: bundle_sha256 must be 64 lowercase hex characters" }, 400);
+      if (!b.version) return json({ error: "invalid plugin: version must be 1-64 printable ASCII characters" }, 400);
+      const meta = { version: b.version, bundle_url: b.bundle_url, bundle_sha256: b.bundle_sha256, installed_id: b.installed_id || "" };
+      if (p) return touch(Object.assign(p, meta));
+      const np = { name, enabled: true, env: {}, ...meta };
+      PLUGINS.push(np);
+      return touch(np);
+    }
+    if (!p) return json({ error: "plugin not found" }, 404);
+    if (!plm[2] && method === "DELETE") { PLUGINS.splice(PLUGINS.indexOf(p), 1); return json({ ok: true }); }
+    if (plm[2] === "enable" || plm[2] === "disable") { p.enabled = plm[2] === "enable"; return touch(p); }
+    if (plm[2] === "env" && !plm[3] && method === "PUT") {
+      for (const k of Object.keys(b)) {
+        if (!/^[A-Z_][A-Z0-9_]{0,63}$/.test(k)) return json({ error: `invalid plugin: env key "${k}" must match [A-Z_][A-Z0-9_]{0,63}` }, 400);
+      }
+      Object.assign(p.env, b);
+      return touch(p);
+    }
+    if (plm[2] === "env" && plm[3] && method === "DELETE") {
+      const k = decodeURIComponent(plm[3]);
+      if (!(k in p.env)) return json({ error: "plugin not found" }, 404);
+      delete p.env[k];
+      return touch(p);
+    }
+    return json({ error: "not found" }, 404);
+  }
+
   // Mutations: acknowledge.
   if (method !== "GET") {
     // Sidecar channels share one handler set: /codex/* and /gemini/*.
diff --git a/internal/webui/static/js/pages/plugins.js b/internal/webui/static/js/pages/plugins.js
new file mode 100644
index 0000000..1bf91f5
--- /dev/null
+++ b/internal/webui/static/js/pages/plugins.js
@@ -0,0 +1,235 @@
+// Plugins page: the Claude Code plugins every active user's claudio loads
+// (GET /v1/claudio/plugins). A plugin is a pinned bundle (release zip URL and
+// SHA-256) plus the environment its MCP servers and hooks need. Env values are
+// write-only: typed in password fields and never shown again, only their keys.
+
+import { api } from "../api.js";
+import { el, clear, spinner, errorState, emptyState, statusBadge, toast, modal, confirmDialog, button } from "../ui.js";
+import { sectionHead } from "../components.js";
+import { relTime } from "../format.js";
+
+let renderToken = 0;
+
+export async function render(root) {
+  const myToken = ++renderToken;
+  clear(root);
+  const head = sectionHead(
+    "Plugins",
+    "Bundles and environment served to every active user's claudio. Values are never shown after saving.",
+    [button("Add plugin", { kind: "primary", onClick: () => metaModal(root, null) })],
+  );
+  const card = el("div", { class: "card table-card" }, spinner("Loading plugins…"));
+  root.append(head, card);
+  try {
+    const res = await api.plugins();
+    if (myToken !== renderToken || !card.isConnected) return;
+    const list = (res && res.plugins) || [];
+    if (!list.length) {
+      clear(card).append(emptyState(
+        "No plugins yet",
+        "Add a plugin: the release zip's URL and SHA-256, then the variables its MCP servers need.",
+      ));
+      return;
+    }
+    clear(card).append(buildTable(list, root));
+  } catch (e) {
+    if (myToken !== renderToken || !card.isConnected) return;
+    clear(card).append(errorState(e.message, () => render(root)));
+  }
+}
+
+function th(label, cls) {
+  return el("th", { scope: "col", class: cls || null, text: label });
+}
+
+function buildTable(list, root) {
+  const table = el("table", { class: "table" }, [
+    el("caption", { class: "sr-only", text: "Plugins served to claudio" }),
+    el("thead", {}, el("tr", {}, [
+      th("Name"), th("Version"), th("Status"), th("Bundle"), th("Environment"), th("Updated"), th("Actions", "actions"),
+    ])),
+  ]);
+  const tb = el("tbody");
+  for (const p of list) tb.append(pluginRow(p, root));
+  table.append(tb);
+  return el("div", { class: "table-scroll" }, table);
+}
+
+function pluginRow(p, root) {
+  const keys = el("div", { class: "plugin-keys" });
+  for (const k of p.env_keys || []) {
+    keys.append(el("span", { class: "plugin-key" }, [
+      el("span", { text: k }),
+      el("button", {
+        class: "plugin-key__btn", type: "button", text: "replace", title: `Replace the value of ${k}`,
+        onClick: () => envModal(root, p, k),
+      }),
+      el("button", {
+        class: "plugin-key__btn", type: "button", text: "remove", title: `Remove ${k}`,
+        onClick: async () => {
+          const ok = await confirmDialog({
+            title: `Remove ${k}?`,
+            message: `${p.name} stops receiving ${k} at the next refresh of each claudio.`,
+            confirmLabel: "Remove",
+          });
+          if (ok) act(() => api.del(`/plugins/${enc(p.name)}/env/${enc(k)}`), `${k} removed`, root);
+        },
+      }),
+    ]));
+  }
+  if (!(p.env_keys || []).length) keys.append(el("span", { class: "cell-muted", text: "none" }));
+
+  const updated = Date.parse(p.updated_at);
+  return el("tr", {}, [
+    el("td", {}, [
+      el("span", { class: "cell-strong", text: p.name }),
+      p.installed_id ? el("span", { class: "cell-id", text: `skipped when ${p.installed_id} is installed` }) : null,
+    ]),
+    el("td", { text: p.version }),
+    el("td", {}, statusBadge(p.enabled ? "enabled" : "disabled")),
+    el("td", {}, [
+      el("span", { class: "cell-id", text: p.bundle_url, title: p.bundle_url }),
+      el("span", { class: "cell-id", text: `sha256 ${p.bundle_sha256.slice(0, 12)}…`, title: p.bundle_sha256 }),
+    ]),
+    el("td", {}, keys),
+    el("td", { text: isNaN(updated) ? "—" : relTime(updated / 1000) }),
+    el("td", { class: "actions" }, [
+      button("Edit", { onClick: () => metaModal(root, p) }),
+      button("Set variables", { onClick: () => envModal(root, p, null) }),
+      button(p.enabled ? "Disable" : "Enable", {
+        onClick: () => act(
+          () => api.post(`/plugins/${enc(p.name)}/${p.enabled ? "disable" : "enable"}`),
+          `${p.name} ${p.enabled ? "disabled" : "enabled"}`, root),
+      }),
+      button("Delete", {
+        kind: "danger-ghost",
+        onClick: async () => {
+          const ok = await confirmDialog({
+            title: "Delete plugin?",
+            message: `"${p.name}" and all its variables are removed. claudio drops it at its next refresh.`,
+            confirmLabel: "Delete",
+          });
+          if (ok) act(() => api.del(`/plugins/${enc(p.name)}`), "Plugin deleted", root);
+        },
+      }),
+    ]),
+  ]);
+}
+
+async function act(fn, okMsg, root) {
+  try {
+    await fn();
+    toast(okMsg, "good");
+    render(root);
+  } catch (e) {
+    toast(e.message, "critical", 6000);
+  }
+}
+
+function field(label, input, hint) {
+  return el("div", { class: "form-row" }, [
+    el("label", { class: "field-label", text: label }),
+    input,
+    hint ? el("p", { class: "form-hint", text: hint }) : null,
+  ]);
+}
+
+function input(value, placeholder) {
+  return el("input", { class: "input", type: "text", value: value || "", placeholder: placeholder || "", autocomplete: "off", spellcheck: "false" });
+}
+
+// Create a plugin (p null) or replace its bundle reference. Env is untouched.
+function metaModal(root, p) {
+  const name = input(p ? p.name : "", "vocdoni-internal");
+  if (p) name.disabled = true;
+  const version = input(p && p.version, "1.4.0");
+  const url = input(p && p.bundle_url, "https://github.com/…/releases/download/v1.4.0/plugin.zip");
+  const sha = input(p && p.bundle_sha256, "64 lowercase hex characters");
+  const installed = input(p && p.installed_id, "vocdoni-internal@vocdoni (optional)");
+  const m = modal({
+    title: p ? `Edit ${p.name}` : "Add plugin",
+    subtitle: "The proxy never downloads the bundle; claudio does, and checks the SHA-256.",
+    body: el("div", { class: "env-rows" }, [
+      field("Name", name, "Lower-case letters, digits and dashes."),
+      field("Version", version),
+      field("Bundle URL", url, "https only."),
+      field("Bundle SHA-256", sha, "sha256sum of the zip."),
+      field("Installed plugin ID", installed, "When this Claude Code plugin is already installed, claudio skips the bundle."),
+    ]),
+    actions: [
+      button("Cancel", { onClick: () => m.close() }),
+      button(p ? "Save" : "Add", {
+        kind: "primary",
+        onClick: async () => {
+          const n = name.value.trim();
+          try {
+            await api.setPlugin(n, {
+              version: version.value.trim(),
+              bundle_url: url.value.trim(),
+              bundle_sha256: sha.value.trim(),
+              installed_id: installed.value.trim(),
+            });
+            m.close();
+            toast(p ? `${n} updated` : `${n} added`, "good");
+            render(root);
+          } catch (e) {
+            toast(e.message, "critical", 6000);
+          }
+        },
+      }),
+    ],
+  });
+}
+
+// Set (or replace, when key is given) env variables. Values go in password
+// fields and are sent once; the page never reads them back.
+function envModal(root, p, key) {
+  const rows = el("div", { class: "env-rows" });
+  const addRow = (k) => {
+    const keyIn = input(k || "", "VOCDONI_KB_TOKEN");
+    if (k) keyIn.disabled = true;
+    const valIn = el("input", { class: "input", type: "password", autocomplete: "new-password", placeholder: "value" });
+    const row = el("div", { class: "env-row" }, [keyIn, valIn]);
+    if (!k) row.append(button("×", { title: "Remove row", onClick: () => row.remove() }));
+    rows.append(row);
+    return { keyIn, valIn };
+  };
+  const first = addRow(key);
+  const m = modal({
+    title: key ? `Replace ${key}` : `Variables of ${p.name}`,
+    subtitle: "Every active user's claudio receives these at its next refresh. Values are never shown after saving.",
+    body: el("div", {}, [
+      rows,
+      key ? null : button("Add variable", { onClick: () => addRow(null).keyIn.focus() }),
+    ]),
+    actions: [
+      button("Cancel", { onClick: () => m.close() }),
+      button("Save", {
+        kind: "primary",
+        onClick: async () => {
+          const env = {};
+          for (const row of rows.children) {
+            const [k, v] = row.querySelectorAll("input");
+            const name = k.value.trim();
+            if (!name) continue;
+            env[name] = v.value;
+          }
+          if (!Object.keys(env).length) return toast("Add at least one variable", "warning");
+          try {
+            await api.setPluginEnv(p.name, env);
+            m.close();
+            toast(`${Object.keys(env).length} variable(s) saved for ${p.name}`, "good");
+            render(root);
+          } catch (e) {
+            toast(e.message, "critical", 6000);
+          }
+        },
+      }),
+    ],
+  });
+  (key ? first.valIn : first.keyIn).focus();
+}
+
+function enc(v) {
+  return encodeURIComponent(String(v == null ? "" : v));
+}
diff --git a/internal/webui/webui.go b/internal/webui/webui.go
index 4240026..0022940 100644
--- a/internal/webui/webui.go
+++ b/internal/webui/webui.go
@@ -212,6 +212,8 @@ func (s *Server) serveAuthenticatedAPI(w http.ResponseWriter, r *http.Request, r
 		s.handleSidecar(w, r, codexgateway.GeminiChannel, strings.TrimPrefix(rest, "/gemini"))
 	case rest == "/users" || strings.HasPrefix(rest, "/users/"):
 		s.handleUsers(w, r, strings.TrimPrefix(rest, "/users"))
+	case rest == "/plugins" || strings.HasPrefix(rest, "/plugins/"):
+		s.handlePlugins(w, r, strings.TrimPrefix(rest, "/plugins"))
 	default:
 		writeErr(w, http.StatusNotFound, "not found")
 	}
````
Run: `go test ./cmd/claude-proxy/ ./internal/webui/ && GOTOOLCHAIN=go1.26.2 make lint`
Expected: `ok  	github.com/p4u/claude-proxy/cmd/claude-proxy`, `ok  	github.com/p4u/claude-proxy/internal/webui`; lint `0 issues.`

Step 3: commit
`git add cmd/claude-proxy/plugins.go cmd/claude-proxy/plugins_test.go internal/webui/plugins.go internal/webui/plugins_test.go internal/webui/static/css/plugins.css internal/webui/static/js/pages/plugins.js` then `git commit -m "feat(plugins): admin cli, web api and plugins page" -- cmd/claude-proxy/main.go cmd/claude-proxy/plugins.go cmd/claude-proxy/plugins_test.go docs/WEBUI.md internal/webui/plugins.go internal/webui/plugins_test.go internal/webui/static/css/plugins.css internal/webui/static/index.html internal/webui/static/js/api.js internal/webui/static/js/app.js internal/webui/static/js/mock.js internal/webui/static/js/pages/plugins.js internal/webui/webui.go`

Interfaces this task provides to later tasks: `main.runPlugins(args, stdin, stdout, stderr) int` (0 ok, 1 failure, 2 usage); `main.parseInterleaved(fs, args)`; `/api/plugins` contract in `docs/WEBUI.md`; `api.plugins()`, `api.setPlugin(name, meta)`, `api.setPluginEnv(name, env)` in `static/js/api.js`.

### Task 8: Docs: README section and CLAUDE.md Claudio API
Model: fast   Repo: /volumes/repos/claude-proxy   Files: CLAUDE.md, README.md
Goal: AC7 documentation half: README "claudio: browser login and plugins" section, CLI reference, storage schema, Web UI features; CLAUDE.md routes table, device-login notes, isolation guarantee 3, package table.
Patch: .delivery/2026-11-06-claudio-plugins/patches/task-8.patch

Step 1: no test (documentation only).

Step 2: write the docs
````diff
diff --git a/CLAUDE.md b/CLAUDE.md
index 3f78e7b..227bf46 100644
--- a/CLAUDE.md
+++ b/CLAUDE.md
@@ -632,7 +632,7 @@ Before this, a key added seconds after a refresh stayed invisible for 5 minutes.
 
 | Package | Role |
 |---|---|
-| `cmd/claude-proxy/` | CLI entry point; `serve`, `tui`, `creds`, and `users` subcommands |
+| `cmd/claude-proxy/` | CLI entry point; `serve`, `tui`, `creds`, `users` and `plugins` subcommands |
 | `internal/tui/` | Interactive Bubble Tea management UI (credentials + users tabs) for `claude-proxy tui` |
 | `internal/webui/` | Embedded browser dashboard (`go:embed`), served at `/` when `CLAUDE_PROXY_UI_PASSWORD` or single sign-on (`CLAUDE_PROXY_OAUTH_*`, replaces the password) is set (old `/ui` 308-redirects); cookie-authenticated JSON API under `/api` (contract: `docs/WEBUI.md`) |
 | `internal/provider/` | Per-upstream behaviour table (base URL, model prefixes, refreshability, usage API, `[1m]` support) |
@@ -645,7 +645,8 @@ Before this, a key added seconds after a refresh stayed invisible for 5 minutes.
 | `internal/ingest/` | OAuth `.credentials.json` parser/importer; stores web OAuth sign-ins (`ImportOAuth`, `UpdateFromOAuth`) |
 | `internal/claudeoauth/` | Anthropic subscription sign-in from the web UI: Claude Code's manual-code OAuth flow (PKCE verifier kept in memory, code exchange, profile-based plan detection) |
 | `internal/admin/` | Admin REST API (`/admin/*` routes) |
-| `internal/usertoken/` | Named per-user bearer tokens; request identity (`Identity{IsAdmin,FullCapture,...}` in context); output-token usage limits (`limit.go`) |
+| `internal/plugins/` | Plugins served to claudio (`plugins`, `plugin_env` tables): every validation limit; CLI, `/api/plugins` and `/v1/claudio/plugins` all call it. Env values only leave through `ListForClient` |
+| `internal/usertoken/` | Named per-user bearer tokens (optional unique `email`, `CreateForEmail`/`LookupByEmail` for the claudio login); request identity (`Identity{IsAdmin,FullCapture,...}` in context); output-token usage limits (`limit.go`) |
 | `internal/usage/` | Anthropic usage API client, background poller, history storage + asciigraph chart |
 | `internal/prettylog/` | Custom slog handler with per-credential color output |
 
@@ -775,7 +776,7 @@ without storing raw upstream error text.
 
 ### Storage Schema (SQLite)
 
-Eight tables (`internal/store/schema.go`): `credentials` (incl. `provider`:
+Tables (`internal/store/schema.go`), among them `plugins` and `plugin_env` (claudio plugins, env values are secrets) and `user_tokens.email` (unique when set, partial index added in the ALTER list): `credentials` (incl. `provider`:
 `anthropic` | `glm` | `mimo` | `codex` | `gemini` | `privatemode` | `custom` | `custom_openai`, added via
 the swallow-duplicate `ALTER TABLE` mechanism — its `DEFAULT 'anthropic'` is
 what migrates existing rows, no backfill needed),
@@ -1072,10 +1073,36 @@ sees them (see "Namespace reservation" below).
 | `GET` | `/v1/claudio/pool/health` | same as /v1/* | Coarse provider availability |
 | `GET` | `/v1/claudio/session?id=<claude_session_id>` | same as /v1/* | Which credential serves a Claude Code session |
 | `POST` | `/v1/claudio/session/switch` | same as /v1/* | Move a Claude Code session to another credential |
+| `GET` | `/v1/claudio/plugins` | user token only | Enabled plugins: bundle `{url,sha256}`, `installed_id`, env (secrets); `no-store` |
+| `POST` | `/v1/claudio/login/start` | **none** (SSO configured, else 404) | Device login: `{device_code,user_code,verify_url,interval_s:3,expires_in_s:600}` |
+| `POST` | `/v1/claudio/login/poll` | **none** (SSO configured, else 404) | `{device_code}` → 428/429/200 `{token,email,name}`/410/403/404 |
 
 Unknown paths return a local 404; wrong methods return 405. Neither is written
 to `request_log`.
 
+### Device login (`login.go`, `internal/webui/claudiologin.go`)
+
+`AuthMiddleware` exempts exactly the escaped paths `/v1/claudio/login/start`
+and `/v1/claudio/login/poll`: no identity is attached and a bearer on them is
+ignored, never validated (a stale token must not 401 a login). `ServeHTTP`
+dispatches them before the per-identity bucket, with per-IP fixed-window
+limits instead (start 5/min, poll 40/min; `clientIP` = first
+`X-Forwarded-For`). `claudioapi.Logins` holds pending logins in memory keyed by
+`SHA-256(device_code)` plus the user code (never the plaintext device code,
+never a token): ≤ 8 pending per IP (`429 too_many_pending`), ≤ 4096 total
+(oldest evicted), swept every minute; denied/expired entries answer 403/410
+for 10 min. A poll < 3 s after the previous one is `429 slow_down` without
+touching the IP budget. Approval happens on the web UI page `/claudio/login`
+(`vocauth.Require`, login-domain re-check, same-origin POST — `Origin: null`
+is accepted only with `Sec-Fetch-Site: same-origin`, because the page's
+`Referrer-Policy: no-referrer` makes browsers send null — CSP/XFO/no-store
+headers, no script): it finds or creates the member's token by e-mail
+(`usertoken.CreateForEmail`, `CLAUDIO_DEFAULT_LIMIT`) and stores only the user
+id on the entry. Redemption moves approved → redeeming under the lock (one
+poll wins), re-reads the user (missing → 404, not active → 403 denied) and
+returns its current token. `main.go` wires one `Logins` into both
+(`claudioH.SetLogins`, `webui.Options.Logins`) only when SSO is on.
+
 ### Session → credential (`/v1/claudio/session`)
 
 Claude Code sends `X-Claude-Code-Session-Id` on every request. After the
@@ -1165,8 +1192,8 @@ anonymous.  The fix:
    never selects a credential, and never makes an outbound HTTP request.
 2. **No `request_log` writes.** The handler does not call `logRequest`. A test
    asserts zero rows after hitting all endpoints (including bypass paths).
-3. **`/me/stats` identity check.** Admin tokens and anonymous callers (nil or
-   empty `UserTokenID`) receive 403. Queries are scoped by `user_token_id` so
+3. **`/me/stats` and `/plugins` identity check.** Admin tokens and anonymous
+   callers (nil or empty `UserTokenID`) receive 403. Queries are scoped by `user_token_id` so
    user A cannot see user B's data.
 4. **`/pool/health` opacity.** Only a coarse status string per provider is
    returned (`ok|busy|saturated|unavailable`). No credential IDs, labels, raw
diff --git a/README.md b/README.md
index 105a008..098c908 100644
--- a/README.md
+++ b/README.md
@@ -890,7 +890,10 @@ login`. Sessions last 7 days.
 - **Credentials** — the same actions as the TUI (import, enable/disable,
   refresh, re-weight, update tokens, delete), plus OpenAI Codex OAuth account
   connection and management, from a browser.
-- **Users** — create/rotate/disable/delete per-user bearer tokens, with
+- **Plugins** — the plugins served to claudio: bundle reference, enable,
+  delete, and write-only environment variables (keys shown, values never).
+- **Users** — create/rotate/disable/delete per-user bearer tokens (with the
+  member's e-mail when the token came from a claudio login), with
   per-user stats, a per-user prompt history, an opt-in full-conversation
   capture toggle with a message viewer and markdown export (see below), and a
   per-user usage limit with a live meter (see below).
@@ -973,6 +976,62 @@ weighted, reauthorized, or deleted. Codex weights compete only with other
 Codex accounts. GPT models are advertised to Claude Code as
 `claude-gpt-*` and restored to `gpt-*` on the sidecar wire.
 
+## claudio: browser login and plugins
+
+[claudio](https://github.com/p4u/claudio) talks to the proxy under
+`/v1/claudio` (local routes, never forwarded upstream nor written to
+`request_log`). Two of them onboard a member without an admin:
+
+**Browser login** (needs single sign-on, see [Web UI](#web-ui); without it
+both routes answer 404). `claudio proxy login` calls
+`POST /v1/claudio/login/start`, prints `https://<host>/claudio/login` and an
+8-character code, and polls `POST /v1/claudio/login/poll` every 3 s. The
+member opens the page, signs in with Google, types the code and approves it.
+A verified account of a `CLAUDE_PROXY_LOGIN_DOMAINS` domain with no user
+token gets one on its first approval (name = e-mail, limit =
+`CLAUDIO_DEFAULT_LIMIT`, e.g. `"--tokens 1M --window 24h"`, the arguments of
+`users limit`; empty = unlimited); later logins return the same token. A
+disabled user is refused. Codes live 10 minutes, in memory only (a restart
+drops pending logins), and only the terminal that started a login can redeem
+it. The two login routes are the only `/v1/claudio` routes without a
+credential; a bearer sent to them is ignored. Limits: 5 starts and 40 polls
+per minute per client IP, 8 pending logins per IP, 4096 in total.
+
+| Poll reply | Meaning |
+|---|---|
+| `428 {"error":"authorization_pending"}` | not approved yet |
+| `429 {"error":"slow_down"}` | polled faster than every 3 s (or the IP budget is spent) |
+| `200 {token,email,name}` | approved; the login is consumed |
+| `410 {"error":"expired"}` / `403 {"error":"denied"}` | expired / denied (or the user is disabled) |
+| `404 {"error":"unknown_code"}` | never existed, already used, or the proxy restarted |
+
+Set `OAUTH_ALLOWED_EMAILS` to the administrators **before** members use the
+browser login, or every member who signs in can use the web UI.
+
+**Plugins.** `GET /v1/claudio/plugins` (user tokens only; admin and
+anonymous get 403; `Cache-Control: no-store`) lists the enabled plugins each
+member's claudio loads: a pinned bundle (a release zip of the content repo,
+URL and SHA-256; the proxy never downloads it) and the environment its MCP
+servers and hooks need, secrets included. Every enabled plugin's env goes to
+every active user token; disabling or deleting the user is the revocation.
+Manage them from the web UI's **Plugins** page (values are entered once and
+never shown again) or the CLI, which opens the database directly:
+
+```bash
+claude-proxy plugins set vocdoni-internal --version 1.4.0 \
+  --url https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip \
+  --sha256 "$(sha256sum vocdoni-internal.zip | cut -d' ' -f1)" \
+  --installed-id vocdoni-internal@vocdoni
+printf '%s\n' "$KB_TOKEN" "$NOTION_TOKEN" | \
+  claude-proxy plugins env set vocdoni-internal VOCDONI_KB_TOKEN=- NOTION_TOKEN=-
+claude-proxy plugins env set vocdoni-internal --env-file ./plugin.env   # KEY=VALUE lines
+claude-proxy plugins env list vocdoni-internal                          # keys only
+claude-proxy plugins list | enable | disable | delete <name>
+```
+
+`KEY=VALUE` on the command line is accepted but visible in `ps`; prefer
+`KEY=-` (stdin) or `--env-file`.
+
 ## Operator workflow with `make`
 
 ```
@@ -1065,12 +1124,20 @@ claude-proxy creds rm            <id>
 claude-proxy creds refresh       <id>
 claude-proxy creds set-weight    <id> <weight>
 
-claude-proxy users create        --name NAME
-claude-proxy users list
+claude-proxy users create        --name NAME [--email ADDR]
+claude-proxy users list                        # includes each token's e-mail
+claude-proxy users email         <id> <addr>   # refuses an address on another token
 claude-proxy users stats         [<id>] [--period 1h|6h|24h|7d|30d]
 claude-proxy users token         <id>
 claude-proxy users disable|enable|rm|refresh  <id>
 claude-proxy users limit         <id> --tokens 1M --window 24h | --none
+
+claude-proxy plugins list
+claude-proxy plugins set         <name> --version V --url https://… --sha256 HEX [--installed-id ID]
+claude-proxy plugins enable|disable|delete <name>
+claude-proxy plugins env set     <name> KEY=-… | --env-file PATH | KEY=VALUE
+claude-proxy plugins env unset   <name> KEY…
+claude-proxy plugins env list    <name>
 ```
 
 `--auth-token` falls back to `CLAUDE_PROXY_AUTH_TOKEN` env var. All `creds`
@@ -1164,8 +1231,12 @@ rr_cursor(k, idx)
 
 user_tokens(
   id, name, token, status, created_at, last_used_at, full_capture,
-  limit_units, limit_window_seconds   -- per-user usage cap; 0/0 = unlimited
+  limit_units, limit_window_seconds,  -- per-user usage cap; 0/0 = unlimited
+  email                               -- member e-mail, unique when set
 )
+
+plugins(name, version, bundle_url, bundle_sha256, installed_id, enabled, updated_at)
+plugin_env(plugin, key, value)        -- secrets served to claudio
 ```
 
 Tokens are stored in plaintext (file mode 0600). KMS / vault integration is
````
Run: `GOTOOLCHAIN=go1.26.2 make lint`
Expected: `0 issues.` (docs only)

Step 3: commit
`git commit -m "docs: claudio browser login and plugins" -- CLAUDE.md README.md`

Interfaces this task provides to later tasks: none

## Merge and release
1. Full suites: in the feature checkout, `go test -race -count=1 ./...` → expected 20 packages `ok`, 1081 test results passed (524 top-level + 557 subtests), 0 failed; `GOTOOLCHAIN=go1.26.2 make lint` → `0 issues.`; `go build ./...` clean.
2. Merge order: apply task-1…task-8 patches in order onto `feat/claudio-plugins` (base e0c50d9; `git am .delivery/2026-11-06-claudio-plugins/patches/task-*.patch`), gate audit, then `feat/claudio-plugins` → `main` (fast-forward or merge commit, per owner). The claudio side (sub-project 2) ships after this proxy is deployed.
3. Release: the repo has no version tags (`git tag --list` is empty) and no changelog; a push to `main` makes CI (`.github/workflows/ci.yml`: lint → test → multi-arch image) publish `ghcr.io/p4u/claude-proxy:latest`, `:main`, `:sha-<short>`. A `vX.Y.Z` tag is optional and only adds semver image tags; propose none unless the owner wants one.
4. **Owner OK required before**, each individually:
   - pushing `feat/claudio-plugins` and/or `main` to GitHub (triggers the image build);
   - creating any `v*.*.*` tag;
   - on prod, setting `OAUTH_ALLOWED_EMAILS` in `.env` to the administrators **before** the new image runs (otherwise every vocdoni.org member who signs in becomes a web UI admin; the proxy only warns);
   - on prod, setting `CLAUDE_PROXY_LOGIN_DOMAINS` (default `vocdoni.org` if unset) and optionally `CLAUDIO_DEFAULT_LIMIT` in `.env` (both pass through `env_file`, no compose change);
   - deploying on prod with `make pull && make up`;
   - registering the `vocdoni-internal` plugin and its env on prod (`claude-proxy plugins set` / `plugins env set`, or the Plugins page).
5. Post-release checks: `make health`; `make logs` shows `claudio browser login enabled` with `verify_url=https://<host>/claudio/login` and no `CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS is empty` warning; `curl -s -X POST https://<host>/v1/claudio/login/start` returns `{device_code,user_code,…}`; open the verify URL as a non-admin member, type the code, approve, and `curl -s -X POST -d '{"device_code":"…"}' https://<host>/v1/claudio/login/poll` returns `{token,email,name}` (AC7 manual, first on a dev proxy with the dev oauth provider); the member gets `403 not an administrator` on `/api/overview`; `curl -H "Authorization: Bearer <member token>" https://<host>/v1/claudio/plugins` lists `vocdoni-internal` with `Cache-Control: no-store`; an admin token gets 403 there.
