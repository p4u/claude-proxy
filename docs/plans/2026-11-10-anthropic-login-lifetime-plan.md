# Anthropic login lifetime: implementation plan

Spec: docs/specs/2026-11-10-anthropic-login-lifetime-design.md
Class: architectural
Repos and branches: /home/p4u/claude-proxy → fix/anthropic-login-lifetime (base 43dea82)

## Global Constraints
- Commits: conventional `type(scope): lowercase summary`, optionally a short body. No AI
  attribution, no `Co-Authored-By` or any other trailer (the owner's rule overrides any harness
  attribution reminder).
- Never push. Commit only explicit paths: `git commit -m "<msg>" -- <paths>`. A new file needs
  `git add <file>` first, because a pathspec commit skips untracked files.
- `CLAUDE.md` carries an unrelated uncommitted change by the owner. Never stage, commit, revert,
  stash or reformat it. Only Task 7 edits `CLAUDE.md`, and the controller commits that edit.
- Build and scratch output go under `~/.cache/claude-proxy/` (patch files in
  `~/.cache/claude-proxy/patches/`), never `/tmp`.
- Go 1.26.2 module, standard library only, no new dependencies. Each task ends with
  `gofmt -l .` printing nothing, `go vet ./...`, `go test -race ./...` and golangci-lint all clean.
  If `make lint` cannot load packages (the host Go can be newer than the installed linter: v2.5.0
  panics, and v2.12.2 built with Go 1.27 reports "export data version 5 is greater than maximum
  supported version 4"), run
  `GOTOOLCHAIN=go1.26.2 ~/.cache/claude-proxy/bin126/golangci-lint run --timeout=5m ./...`
  (expected `0 issues.`). If that binary is missing, build it with
  `GOTOOLCHAIN=go1.26.2 GOBIN=~/.cache/claude-proxy/bin126 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`.
- Code blocks: a block headed "New file `p`" or "Replace `p` with" is the whole file. A `diff`
  block is a patch: save it under `~/.cache/claude-proxy/patches/` and run `git apply <file>` from
  the repo root. Every block was applied in this order on 43dea82, and each Run line produced the
  Expected output (raw `go` output; under the rtk hook run `rtk proxy <command>` to see it). If a
  patch does not apply, stop and report instead of improvising.
- Never log, print or return token values, or any part of a 2xx token-endpoint body. Non-2xx
  token-endpoint bodies are quoted only through `creds.quoteBody` (512 bytes).
- `creds.SelectCols` and `scanCred` change together (the pool reads them).
- Every `/api` change is mirrored in `internal/webui/static/js/mock.js`.
- Anthropic `/v1/*` pass-through stays byte-identical. Only the upstream-401 branch of
  `forward` changes.
- Tests never reach a real Anthropic endpoint. A test that can call the token or profile
  endpoint points `creds.TokenURL`, `creds.ProfileURL` (and `claudeoauth.ProfileURL` for the
  sign-in) at an `httptest` server and restores them. From Task 3 on, a `.credentials.json`
  fixture whose `expiresAt` is more than 24 h away takes the profile-check path, so fixtures use a
  short expiry unless they test that path.
- Unchanged behaviour: non-Anthropic providers, the Codex/Gemini sidecar, pool selection and
  scoring, `GET /admin/credentials`, and the lineage-conditional writes (a stale refresh never
  overwrites or revokes a newer login).

## Review Focus
Line numbers are those of the finished branch.
- `internal/creds/refresh.go:223-277` `Refresh` and `RefreshRejected`: `context.WithoutCancel`
  straight after the per-id lock, `ErrNotRefreshable` before any network call, the skip rule
  (expiry more than 5 min away and status not `revoked`), the step-1 token comparison, and the
  liveness mapping (only `ErrTokenRejected` goes on to refresh; 200 and every other outcome is
  `ErrTokenAlive`), plus the "long-lived token rejected" ERROR.
- `internal/creds/refresh.go:281-329` `refreshLocked`: failure memory checked before the token
  call, 400/401 to `rejected`, every other failure transient, `nextDeadline`, `store.Retry`
  around the write, the ERROR when a 200 is not stored, DEBUG "superseded".
- `internal/creds/refresh.go:333-388` `rejected` (revocation ERROR, one line), `transientFailure`
  (`retry_after = MAX(existing, now+60s)`, one WARN), `recentFailure` (30 s).
- `internal/creds/refresh.go:393-417` lineage-conditional writes: the `refresh_token=?` guard and
  the status CASE that heals only `revoked`/`expired`.
- `internal/creds/refresh.go:122-210` `RefreshTokens` (30 s client), `tokenRequest` (1 MiB
  `LimitReader`), `decodeTokens` (no body in any error), `nextDeadline`, `quoteBody`.
- `internal/creds/refresh.go:433-452` `tick`: no `expired` case; only tokens within 5 min of
  expiry reach `Refresh`, which logs its own outcome.
- `internal/creds/check.go:24` `CheckToken`: 10 s client, the sign-in's headers, 200/401/other.
- `internal/proxy/proxy.go:571-603` upstream-401 branch: `ErrTokenAlive` relays upstream's status,
  headers and full body by falling through to the normal relay (which runs `MarkError`), fixed
  502 bodies, no ERROR line of its own. `internal/proxy/models1m.go:772` reaches the same branch.
- `internal/ingest/import.go:88-108` `verifyFile` (D2a): more than 24 h → `CheckToken`, file
  stored unchanged; otherwise refresh with the file's deadline.
- `internal/webui/claudelogin.go:67` `context.WithoutCancel` for the exchange and the store.
- `internal/creds/creds.go:241` `LoginEndsAt`; `:328-389` `SelectCols`/`scanCred`; `:482`
  `UpdateTokensWithSubscription` replacing the deadline; `internal/store/store.go:116,125-130`
  ALTER and the `expired` heal.

## Task order and parallelism
| Task | Depends on | Repo/files | May run in parallel with |
|---|---|---|---|
| 1 Storage and model | — | `internal/store/{schema,store}.go`, `internal/creds/creds.go`, `internal/ingest/import.go`, tests | — |
| 2 Refresher core | 1 | `internal/creds/{refresh,check}.go`, `internal/ingest/import.go`, `cmd/claude-proxy/main.go`, tests incl. `internal/pool/refresh_backoff_test.go` | — |
| 3 Sign-in and import | 2 | `internal/claudeoauth/`, `internal/webui/claudelogin*.go`, `internal/ingest/`, `internal/tui/tui_test.go`, `cmd/claude-proxy/{main,creds_test}.go` | 4 |
| 4 Proxy 401 branch | 2 | `internal/proxy/{proxy,proxy_test,refresh401_test}.go` | 3 |
| 5 Manual refresh, CLI and TUI | 3, 4 | `internal/webui/{creds,refresh_test}.go`, `internal/tui/{update,tui_test}.go`, `cmd/claude-proxy/{main,creds_test}.go`, `internal/creds/refresh.go` | — |
| 6 Web UI | 5 | `internal/webui/{creds,creds_lifetime_test}.go`, `internal/webui/static/{js/pages/credentials.js,js/mock.js,css/credentials.css}` | — |
| 7 Docs | 6 | `docs/WEBUI.md`, `README.md`, `Makefile`, `CLAUDE.md` | — |

Tasks 3 and 4 share no file. Task 4's patches were also checked on top of Task 2 alone. Run in
parallel, each needs its own worktree, and the controller lands them one after the other.

## Tasks

### Task 1: Store each login's refresh deadline
Model: sonnet   Repo: /home/p4u/claude-proxy   Files: `internal/store/schema.go`,
`internal/store/store.go`, `internal/creds/creds.go`, `internal/creds/creds_test.go`,
`internal/creds/deadline_test.go` (new), `internal/creds/refresh_test.go`,
`internal/ingest/import.go`, `internal/ingest/import_test.go`
Goal: AC2 and the storage half of AC3. Add `credentials.refresh_expires_at` (CREATE TABLE and
ALTER list), `Credential.RefreshExpiresAt` read through `SelectCols`/`scanCred`, the deadline on
inserts and reconnects (a reconnect replaces it), `LoginEndsAt`, the startup heal of legacy
`expired` Anthropic rows, and delete `UpdateTokens` (D2, D4).

Notes:
- `InsertWithRateLimitTier` and `UpdateTokensWithSubscription` gain a trailing
  `refreshExpiresAt time.Time`. The ingest callers pass `time.Time{}` here; Tasks 2 and 3 pass the
  real value.
- The new field sits at the end of the struct, after a blank line, so gofmt does not realign the
  existing fields.

Step 1: write the failing test

New file `internal/creds/deadline_test.go`:

````go
package creds

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// AC2: the deadline column exists on fresh and migrated databases, rows that
// predate it read as unknown, and legacy "expired" Anthropic rows are healed.
func TestRefreshDeadlineMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := insertCred(t, db, "legacy")
	if c.RefreshExpiresAt != (time.Time{}) {
		t.Fatalf("fresh insert deadline = %v, want unknown", c.RefreshExpiresAt)
	}
	key, err := InsertKey(ctx, db, provider.GLM, "glm", "", "glm-key", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE credentials SET status='expired'`,
		`ALTER TABLE credentials DROP COLUMN refresh_expires_at`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var cols int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('credentials') WHERE name='refresh_expires_at'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("refresh_expires_at columns = %d, %v", cols, err)
	}
	got, err := Get(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive || !got.RefreshExpiresAt.IsZero() {
		t.Fatalf("legacy row: status=%q deadline=%v, want active and unknown", got.Status, got.RefreshExpiresAt)
	}
	if k, _ := Get(ctx, db, key.ID); k.Status != StatusExpired {
		t.Fatalf("non-Anthropic row healed to %q; only Anthropic rows are", k.Status)
	}
}

func TestRefreshDeadlineStoredAndReplaced(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	deadline := time.Now().Add(27 * 24 * time.Hour).Truncate(time.Second)
	c, err := InsertWithRateLimitTier(ctx, db, "a", "max", "sk-ant-oat-a", "ref-a", time.Now().Add(8*time.Hour), 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(deadline) {
		t.Fatalf("stored deadline = %v, want %v", got.RefreshExpiresAt, deadline)
	}
	newer := deadline.Add(48 * time.Hour)
	if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-b", "ref-b", time.Now().Add(8*time.Hour), "", "", newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(newer) {
		t.Fatalf("reconnect deadline = %v, want the new login's %v", got.RefreshExpiresAt, newer)
	}
	if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-c", "ref-c", time.Now().Add(8*time.Hour), "", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(ctx, db, c.ID); !got.RefreshExpiresAt.IsZero() {
		t.Fatalf("a login without a deadline must store unknown, got %v", got.RefreshExpiresAt)
	}
}

func TestLoginEndsAt(t *testing.T) {
	now := time.Now()
	year, month := now.AddDate(1, 0, 0), now.Add(27*24*time.Hour)
	for _, tt := range []struct {
		name string
		c    Credential
		want time.Time
		ok   bool
	}{
		{"long-lived token", Credential{Provider: provider.Anthropic, ExpiresAt: year, RefreshExpiresAt: month}, year, true},
		{"8-hour token", Credential{Provider: provider.Anthropic, ExpiresAt: now.Add(8 * time.Hour), RefreshExpiresAt: month}, month, true},
		{"unknown deadline", Credential{Provider: provider.Anthropic, ExpiresAt: year}, time.Time{}, false},
		{"api key", Credential{Provider: provider.GLM, ExpiresAt: year, RefreshExpiresAt: month}, time.Time{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.c.LoginEndsAt()
			if ok != tt.ok || !got.Equal(tt.want) {
				t.Fatalf("LoginEndsAt() = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
````

The existing tests move to the new signatures (`UpdateTokens` → `UpdateTokensWithSubscription`):

`internal/creds/creds_test.go`:

````diff
diff --git a/internal/creds/creds_test.go b/internal/creds/creds_test.go
index c89b5b2..1643bd5 100644
--- a/internal/creds/creds_test.go
+++ b/internal/creds/creds_test.go
@@ -66,7 +66,7 @@ func TestInsertGetList(t *testing.T) {
 func TestRateLimitTierPersistence(t *testing.T) {
 	ctx := context.Background()
 	db := testDB(t)
-	c, err := InsertWithRateLimitTier(ctx, db, "tiered", "max", "sk-ant-oat-access", "ref", time.Now().Add(time.Hour), 0, " default_claude_max_20x ")
+	c, err := InsertWithRateLimitTier(ctx, db, "tiered", "max", "sk-ant-oat-access", "ref", time.Now().Add(time.Hour), 0, " default_claude_max_20x ", time.Time{})
 	if err != nil {
 		t.Fatal(err)
 	}
@@ -153,7 +153,7 @@ func TestRateLimitTierRejectsInvalid(t *testing.T) {
 		if err := SetRateLimitTier(ctx, db, c.ID, tier); !errors.Is(err, ErrInvalidRateLimitTier) {
 			t.Fatalf("set invalid %q: %v", tier, err)
 		}
-		if _, err := InsertWithRateLimitTier(ctx, db, "invalid", "max", "fake", "fake", time.Now(), 5, tier); !errors.Is(err, ErrInvalidRateLimitTier) {
+		if _, err := InsertWithRateLimitTier(ctx, db, "invalid", "max", "fake", "fake", time.Now(), 5, tier, time.Time{}); !errors.Is(err, ErrInvalidRateLimitTier) {
 			t.Fatalf("insert invalid %q: %v", tier, err)
 		}
 	}
@@ -296,7 +296,7 @@ func TestSetStatusAndUpdateTokens(t *testing.T) {
 	}
 
 	exp := time.Now().Add(2 * time.Hour)
-	if err := UpdateTokens(ctx, db, c.ID, "sk-ant-oat-new", "ref-new", exp); err != nil {
+	if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-new", "ref-new", exp, "", "", time.Time{}); err != nil {
 		t.Fatalf("update-tokens: %v", err)
 	}
 	got, _ = Get(ctx, db, c.ID)
@@ -316,7 +316,7 @@ func TestSetStatusAndUpdateTokens(t *testing.T) {
 
 func TestUpdateTokensWithSubscriptionMissing(t *testing.T) {
 	err := UpdateTokensWithSubscription(context.Background(), testDB(t), "cred_missing",
-		"sk-ant-oat-new", "ref-new", time.Now().Add(time.Hour), "max", "20x")
+		"sk-ant-oat-new", "ref-new", time.Now().Add(time.Hour), "max", "20x", time.Time{})
 	if !errors.Is(err, ErrNotFound) {
 		t.Fatalf("missing ID: %v", err)
 	}
````

`internal/creds/refresh_test.go`:

````diff
diff --git a/internal/creds/refresh_test.go b/internal/creds/refresh_test.go
index eb668e3..fd46a08 100644
--- a/internal/creds/refresh_test.go
+++ b/internal/creds/refresh_test.go
@@ -124,7 +124,7 @@ func TestStaleRefreshDoesNotUndoReplacedTokens(t *testing.T) {
 			go func() { _, err := r.RefreshNow(ctx, c.ID); done <- err }()
 
 			<-entered
-			if err := UpdateTokens(ctx, db, c.ID, "sk-ant-oat-relogin", "ref-relogin", time.Now().Add(8*time.Hour)); err != nil {
+			if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-relogin", "ref-relogin", time.Now().Add(8*time.Hour), "", "", time.Time{}); err != nil {
 				t.Fatal(err)
 			}
 			close(release)
````

`internal/ingest/import_test.go`:

````diff
diff --git a/internal/ingest/import_test.go b/internal/ingest/import_test.go
index 8a5662f..06cc775 100644
--- a/internal/ingest/import_test.go
+++ b/internal/ingest/import_test.go
@@ -141,7 +141,7 @@ func TestReconnectTierMetadata(t *testing.T) {
 		} {
 			t.Run(source+"/"+tt.name, func(t *testing.T) {
 				db := testDB(t)
-				c, err := creds.InsertWithRateLimitTier(ctx, db, "old", "max", "sk-ant-oat-old", "ref-old", future(), 11, "20x")
+				c, err := creds.InsertWithRateLimitTier(ctx, db, "old", "max", "sk-ant-oat-old", "ref-old", future(), 11, "20x", time.Time{})
 				if err != nil {
 					t.Fatal(err)
 				}
@@ -182,7 +182,7 @@ func TestImportInvalidTierBeforeRefresh(t *testing.T) {
 	prev := creds.TokenURL
 	creds.SetTokenURL(srv.URL)
 	t.Cleanup(func() { creds.SetTokenURL(prev); srv.Close() })
-	c, err := creds.InsertWithRateLimitTier(ctx, db, "old", "max", "sk-ant-oat-old", "ref-old", future(), 5, "20x")
+	c, err := creds.InsertWithRateLimitTier(ctx, db, "old", "max", "sk-ant-oat-old", "ref-old", future(), 5, "20x", time.Time{})
 	if err != nil {
 		t.Fatal(err)
 	}
````

Run: `go test -race -count=1 ./internal/creds/`
Expected: build failure, first line `internal/creds/creds_test.go:69:148: too many arguments in call to InsertWithRateLimitTier`, then `internal/creds/deadline_test.go:23:7: c.RefreshExpiresAt undefined (type *Credential has no field or method RefreshExpiresAt)`, and `FAIL github.com/p4u/claude-proxy/internal/creds [build failed]`

Step 2: implement

`internal/store/schema.go`:

````diff
diff --git a/internal/store/schema.go b/internal/store/schema.go
index ceae48e..b761844 100644
--- a/internal/store/schema.go
+++ b/internal/store/schema.go
@@ -18,6 +18,9 @@ CREATE TABLE IF NOT EXISTS credentials (
   access_token      TEXT NOT NULL,
   refresh_token     TEXT NOT NULL,
   expires_at        INTEGER NOT NULL,
+  -- Login deadline Anthropic reports as refresh_token_expires_in, in Unix
+  -- seconds; 0 = unknown. Refreshes never move it (creds.nextDeadline).
+  refresh_expires_at INTEGER NOT NULL DEFAULT 0,
   status            TEXT NOT NULL,
   retry_after       INTEGER,
   last_success_at   INTEGER,
````

`internal/store/store.go`:

````diff
diff --git a/internal/store/store.go b/internal/store/store.go
index 953efc6..c57174b 100644
--- a/internal/store/store.go
+++ b/internal/store/store.go
@@ -111,12 +111,22 @@ func Open(path string) (*DB, error) {
 		// and trimmed. The index must follow the column for older databases.
 		`ALTER TABLE user_tokens ADD COLUMN email TEXT`,
 		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_tokens_email ON user_tokens(email) WHERE email IS NOT NULL`,
+		// Login refresh deadline in Unix seconds. The default 0 means unknown,
+		// which is what every row that predates the column reads as.
+		`ALTER TABLE credentials ADD COLUMN refresh_expires_at INTEGER NOT NULL DEFAULT 0`,
 	} {
 		if _, err := sdb.Exec(alter); err != nil && !isDuplicateColumn(err) {
 			_ = sdb.Close()
 			return nil, fmt.Errorf("migrate %q: %w", alter, err)
 		}
 	}
+	// A failed refresh no longer produces "expired": it is either a rejection
+	// (revoked) or transient (status unchanged). Legacy rows go back to active
+	// so the refresher and the 401 path judge their tokens again.
+	if _, err := sdb.Exec(`UPDATE credentials SET status='active' WHERE status='expired' AND provider='anthropic'`); err != nil {
+		_ = sdb.Close()
+		return nil, fmt.Errorf("heal expired credentials: %w", err)
+	}
 	// Must follow the bound_at migration for databases created before it existed.
 	if _, err := sdb.Exec(`CREATE INDEX IF NOT EXISTS idx_conversations_bound_at ON conversations(bound_at)`); err != nil {
 		_ = sdb.Close()
````

`internal/creds/creds.go`:

````diff
diff --git a/internal/creds/creds.go b/internal/creds/creds.go
index 6eb09c0..a765dd3 100644
--- a/internal/creds/creds.go
+++ b/internal/creds/creds.go
@@ -58,6 +58,10 @@ type Credential struct {
 	ErrorCount    int64
 	Weight        int
 	CreatedAt     time.Time
+
+	// RefreshExpiresAt is the login's refresh deadline: after it the token
+	// endpoint refuses the refresh token. Zero when Anthropic never reported it.
+	RefreshExpiresAt time.Time
 }
 
 // Model is one entry in a credential's own catalogue.
@@ -147,13 +151,14 @@ func newID() string {
 // Insert stores an Anthropic OAuth credential (access + refresh token pair).
 // For static API keys from other providers, use InsertKey.
 func Insert(ctx context.Context, db *store.DB, label, subType, access, refresh string, expiresAt time.Time, weight int) (*Credential, error) {
-	return InsertWithRateLimitTier(ctx, db, label, subType, access, refresh, expiresAt, weight, "")
+	return InsertWithRateLimitTier(ctx, db, label, subType, access, refresh, expiresAt, weight, "", time.Time{})
 }
 
 // InsertWithRateLimitTier stores an OAuth credential and its known tier in one
 // write. Insert remains the backward-compatible entry point for unknown tiers.
-func InsertWithRateLimitTier(ctx context.Context, db *store.DB, label, subType, access, refresh string, expiresAt time.Time, weight int, tier string) (*Credential, error) {
-	return insert(ctx, db, provider.Anthropic, "", nil, label, subType, access, refresh, expiresAt, weight, tier)
+// refreshExpiresAt is the login's refresh deadline, zero when unknown.
+func InsertWithRateLimitTier(ctx context.Context, db *store.DB, label, subType, access, refresh string, expiresAt time.Time, weight int, tier string, refreshExpiresAt time.Time) (*Credential, error) {
+	return insert(ctx, db, provider.Anthropic, "", nil, label, subType, access, refresh, expiresAt, refreshExpiresAt, weight, tier)
 }
 
 // keyExpiry is the stored expires_at for API-key credentials. They never
@@ -167,23 +172,23 @@ func keyExpiry() time.Time { return time.Now().AddDate(100, 0, 0) }
 // InsertKey stores a static API-key credential (GLM and any future key-based
 // provider). There is no refresh token and no meaningful expiry.
 func InsertKey(ctx context.Context, db *store.DB, p provider.ID, label, plan, apiKey, baseURL string, weight int) (*Credential, error) {
-	return insert(ctx, db, p, baseURL, nil, label, plan, apiKey, "", keyExpiry(), weight, "")
+	return insert(ctx, db, p, baseURL, nil, label, plan, apiKey, "", keyExpiry(), time.Time{}, weight, "")
 }
 
 // InsertCustomKey stores a key for a custom Anthropic-compatible host, which
 // carries its own base URL and model catalogue.
 func InsertCustomKey(ctx context.Context, db *store.DB, label, apiKey, baseURL string, models []Model, weight int) (*Credential, error) {
-	return insert(ctx, db, provider.Custom, baseURL, models, label, "", apiKey, "", keyExpiry(), weight, "")
+	return insert(ctx, db, provider.Custom, baseURL, models, label, "", apiKey, "", keyExpiry(), time.Time{}, weight, "")
 }
 
 // InsertCustomOpenAIKey stores a bearer token for a custom OpenAI-compatible
 // Chat Completions host. Like a custom Anthropic host, it carries its own base
 // URL and model catalogue.
 func InsertCustomOpenAIKey(ctx context.Context, db *store.DB, label, apiKey, baseURL string, models []Model, weight int) (*Credential, error) {
-	return insert(ctx, db, provider.CustomOpenAI, baseURL, models, label, "", apiKey, "", keyExpiry(), weight, "")
+	return insert(ctx, db, provider.CustomOpenAI, baseURL, models, label, "", apiKey, "", keyExpiry(), time.Time{}, weight, "")
 }
 
-func insert(ctx context.Context, db *store.DB, p provider.ID, baseURL string, models []Model, label, subType, access, refresh string, expiresAt time.Time, weight int, tier string) (*Credential, error) {
+func insert(ctx context.Context, db *store.DB, p provider.ID, baseURL string, models []Model, label, subType, access, refresh string, expiresAt, refreshExpiresAt time.Time, weight int, tier string) (*Credential, error) {
 	tier, err := NormalizeRateLimitTier(tier)
 	if err != nil {
 		return nil, err
@@ -205,15 +210,16 @@ func insert(ctx context.Context, db *store.DB, p provider.ID, baseURL string, mo
 		AccessToken:      access,
 		RefreshToken:     refresh,
 		ExpiresAt:        expiresAt,
+		RefreshExpiresAt: refreshExpiresAt,
 		Status:           StatusActive,
 		Weight:           weight,
 		CreatedAt:        time.Now(),
 	}
 	_, err = db.ExecContext(
 		ctx, `
-		INSERT INTO credentials (id, label, subscription_type, rate_limit_tier, provider, base_url, models, access_token, refresh_token, expires_at, status, weight, created_at)
-		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
-		c.ID, c.Label, c.SubscriptionType, c.RateLimitTier, string(c.Provider), c.BaseURL, encodeModels(c.Models), c.AccessToken, c.RefreshToken, c.ExpiresAt.Unix(), string(c.Status), c.Weight, c.CreatedAt.Unix(),
+		INSERT INTO credentials (id, label, subscription_type, rate_limit_tier, provider, base_url, models, access_token, refresh_token, expires_at, refresh_expires_at, status, weight, created_at)
+		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
+		c.ID, c.Label, c.SubscriptionType, c.RateLimitTier, string(c.Provider), c.BaseURL, encodeModels(c.Models), c.AccessToken, c.RefreshToken, c.ExpiresAt.Unix(), unixOrZero(c.RefreshExpiresAt), string(c.Status), c.Weight, c.CreatedAt.Unix(),
 	)
 	if err != nil {
 		return nil, err
@@ -221,6 +227,27 @@ func insert(ctx context.Context, db *store.DB, p provider.ID, baseURL string, mo
 	return c, nil
 }
 
+// unixOrZero stores an unknown (zero) time as 0 rather than year 1's Unix value.
+func unixOrZero(t time.Time) int64 {
+	if t.IsZero() {
+		return 0
+	}
+	return t.Unix()
+}
+
+// LoginEndsAt reports when an OAuth login stops serving: the later of the
+// access-token expiry and the refresh deadline. ok is false for providers
+// without OAuth refresh and for logins whose deadline is unknown.
+func (c *Credential) LoginEndsAt() (time.Time, bool) {
+	if !provider.Get(c.Provider).Refreshable || c.RefreshExpiresAt.IsZero() {
+		return time.Time{}, false
+	}
+	if c.ExpiresAt.After(c.RefreshExpiresAt) {
+		return c.ExpiresAt, true
+	}
+	return c.RefreshExpiresAt, true
+}
+
 // ErrInvalidRateLimitTier identifies malformed tier metadata, distinct from a
 // storage failure so callers can report a validation error without guessing.
 var ErrInvalidRateLimitTier = errors.New("invalid rate limit tier")
@@ -302,7 +329,8 @@ const SelectCols = `id, COALESCE(label,''), COALESCE(subscription_type,''), rate
        COALESCE(provider,'anthropic'), COALESCE(base_url,''), COALESCE(models,''),
        access_token, refresh_token, expires_at, status,
        retry_after, last_success_at, last_429_at, last_request_at,
-       request_count, success_count, error_count, weight, created_at`
+       request_count, success_count, error_count, weight, created_at,
+       refresh_expires_at`
 
 const credSelectCols = SelectCols
 
@@ -321,7 +349,7 @@ func scanCred(rs interface {
 },
 ) (*Credential, error) {
 	c := &Credential{}
-	var exp, created int64
+	var exp, created, refreshExp int64
 	var ra, ls, l429, lreq sql.NullInt64
 	var status, prov, models string
 	if err := rs.Scan(
@@ -329,12 +357,16 @@ func scanCred(rs interface {
 		&c.AccessToken, &c.RefreshToken, &exp, &status,
 		&ra, &ls, &l429, &lreq,
 		&c.RequestCount, &c.SuccessCount, &c.ErrorCount, &c.Weight, &created,
+		&refreshExp,
 	); err != nil {
 		return nil, err
 	}
 	c.Provider = provider.ID(prov)
 	c.Models = decodeModels(models)
 	c.ExpiresAt = time.Unix(exp, 0)
+	if refreshExp > 0 {
+		c.RefreshExpiresAt = time.Unix(refreshExp, 0)
+	}
 	c.Status = Status(status)
 	c.CreatedAt = time.Unix(created, 0)
 	if ra.Valid {
@@ -441,30 +473,25 @@ func SetStatus(ctx context.Context, db *store.DB, id string, s Status) error {
 	return err
 }
 
-func UpdateTokens(ctx context.Context, db *store.DB, id, access, refresh string, expiresAt time.Time) error {
-	_, err := db.ExecContext(ctx, `
-		UPDATE credentials SET access_token=?, refresh_token=?, expires_at=?, status='active'
-		WHERE id=?`, access, refresh, expiresAt.Unix(), id)
-	return err
-}
-
 // UpdateTokensWithSubscription atomically reconnects a credential and records
 // known subscription metadata. Absent values retain existing metadata, except
 // that a changed broad plan clears a stale tier when no replacement is supplied.
-// Weight is never derived from the tier or changed during a reconnect.
-func UpdateTokensWithSubscription(ctx context.Context, db *store.DB, id, access, refresh string, expiresAt time.Time, subType, tier string) error {
+// Weight is never derived from the tier or changed during a reconnect. A new
+// login has its own deadline, so refreshExpiresAt replaces the stored one
+// (zero stores unknown).
+func UpdateTokensWithSubscription(ctx context.Context, db *store.DB, id, access, refresh string, expiresAt time.Time, subType, tier string, refreshExpiresAt time.Time) error {
 	tier, err := NormalizeRateLimitTier(tier)
 	if err != nil {
 		return err
 	}
 	res, err := db.ExecContext(ctx, `UPDATE credentials
-		SET access_token=?, refresh_token=?, expires_at=?, status='active',
+		SET access_token=?, refresh_token=?, expires_at=?, refresh_expires_at=?, status='active',
 		    rate_limit_tier=CASE
 		        WHEN ?<>'' THEN ?
 		        WHEN ?<>'' AND ?<>COALESCE(subscription_type,'') THEN ''
 		        ELSE rate_limit_tier END,
 		    subscription_type=CASE WHEN ?<>'' THEN ? ELSE subscription_type END
-		WHERE id=?`, access, refresh, expiresAt.Unix(), tier, tier, subType, subType, subType, subType, id)
+		WHERE id=?`, access, refresh, expiresAt.Unix(), unixOrZero(refreshExpiresAt), tier, tier, subType, subType, subType, subType, id)
 	if err != nil {
 		return err
 	}
````

`internal/ingest/import.go`:

````diff
diff --git a/internal/ingest/import.go b/internal/ingest/import.go
index a19cb55..a5803e7 100644
--- a/internal/ingest/import.go
+++ b/internal/ingest/import.go
@@ -5,6 +5,7 @@ import (
 	"encoding/json"
 	"fmt"
 	"os"
+	"time"
 
 	"github.com/p4u/claude-proxy/internal/claudeoauth"
 	"github.com/p4u/claude-proxy/internal/creds"
@@ -88,7 +89,7 @@ func insertVerified(ctx context.Context, db *store.DB, o oauthBlock, label strin
 	if label == "" {
 		label = o.SubscriptionType
 	}
-	return creds.InsertWithRateLimitTier(ctx, db, label, o.SubscriptionType, accessToken, refreshToken, expires, weight, o.RateLimitTier)
+	return creds.InsertWithRateLimitTier(ctx, db, label, o.SubscriptionType, accessToken, refreshToken, expires, weight, o.RateLimitTier, time.Time{})
 }
 
 // Import reads a Claude Code .credentials.json and inserts it into the pool.
@@ -150,7 +151,7 @@ func updateVerified(ctx context.Context, db *store.DB, id string, o oauthBlock)
 	if err != nil {
 		return nil, fmt.Errorf("credential is not alive: %w", err)
 	}
-	if err := creds.UpdateTokensWithSubscription(ctx, db, id, accessToken, refreshToken, expires, o.SubscriptionType, o.RateLimitTier); err != nil {
+	if err := creds.UpdateTokensWithSubscription(ctx, db, id, accessToken, refreshToken, expires, o.SubscriptionType, o.RateLimitTier, time.Time{}); err != nil {
 		return nil, err
 	}
 	return creds.Get(ctx, db, id)
@@ -175,7 +176,7 @@ func ImportOAuth(ctx context.Context, db *store.DB, t *claudeoauth.Tokens, label
 	if label == "" {
 		label = t.SubscriptionType
 	}
-	return creds.InsertWithRateLimitTier(ctx, db, label, t.SubscriptionType, t.AccessToken, t.RefreshToken, t.ExpiresAt, weight, t.RateLimitTier)
+	return creds.InsertWithRateLimitTier(ctx, db, label, t.SubscriptionType, t.AccessToken, t.RefreshToken, t.ExpiresAt, weight, t.RateLimitTier, time.Time{})
 }
 
 // UpdateFromOAuth re-points an existing credential at a fresh OAuth sign-in,
@@ -188,7 +189,7 @@ func UpdateFromOAuth(ctx context.Context, db *store.DB, id string, t *claudeoaut
 	if c.Provider != provider.Anthropic {
 		return nil, fmt.Errorf("%s is not an Anthropic subscription", id)
 	}
-	if err := creds.UpdateTokensWithSubscription(ctx, db, id, t.AccessToken, t.RefreshToken, t.ExpiresAt, t.SubscriptionType, t.RateLimitTier); err != nil {
+	if err := creds.UpdateTokensWithSubscription(ctx, db, id, t.AccessToken, t.RefreshToken, t.ExpiresAt, t.SubscriptionType, t.RateLimitTier, time.Time{}); err != nil {
 		return nil, err
 	}
 	return creds.Get(ctx, db, id)
````

Run: `go test -race -count=1 -v -run '^(TestRefreshDeadlineMigration|TestRefreshDeadlineStoredAndReplaced|TestLoginEndsAt|TestSetStatusAndUpdateTokens)$' ./internal/creds/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: 4 `--- PASS` lines (`TestSetStatusAndUpdateTokens`, `TestRefreshDeadlineMigration`, `TestRefreshDeadlineStoredAndReplaced`, `TestLoginEndsAt`), then `ok  github.com/p4u/claude-proxy/internal/creds`

Run: `go vet ./... && go test -race -count=1 ./internal/store/ ./internal/creds/ ./internal/ingest/ ./internal/pool/`
Expected: 4 `ok` lines: `internal/store`, `internal/creds`, `internal/ingest`, `internal/pool`

Step 3: commit
`git add internal/creds/deadline_test.go && git commit -m "feat(creds): store each login's refresh deadline" -- internal/store/schema.go internal/store/store.go internal/creds/creds.go internal/creds/creds_test.go internal/creds/deadline_test.go internal/creds/refresh_test.go internal/ingest/import.go internal/ingest/import_test.go`

Interfaces this task provides to later tasks:
- Column `credentials.refresh_expires_at INTEGER NOT NULL DEFAULT 0` (Unix seconds, 0 = unknown).
- `Credential.RefreshExpiresAt time.Time` (zero = unknown).
- `func (c *Credential) LoginEndsAt() (time.Time, bool)`
- `func InsertWithRateLimitTier(ctx context.Context, db *store.DB, label, subType, access, refresh string, expiresAt time.Time, weight int, tier string, refreshExpiresAt time.Time) (*Credential, error)`
- `func UpdateTokensWithSubscription(ctx context.Context, db *store.DB, id, access, refresh string, expiresAt time.Time, subType, tier string, refreshExpiresAt time.Time) error`
  (replaces the stored deadline; zero stores unknown).
- `func unixOrZero(t time.Time) int64` (package `creds`, unexported).
- `UpdateTokens` no longer exists.

### Task 2: Refresh only tokens that stopped working
Model: opus   Repo: /home/p4u/claude-proxy   Files: `internal/creds/refresh.go`,
`internal/creds/check.go` (new), `internal/creds/refresh_test.go`,
`internal/pool/refresh_backoff_test.go` (new), `internal/ingest/import.go`,
`cmd/claude-proxy/main.go`
Goal: AC4, AC5 (refresher side), AC5b (`tick`), AC6 (refresher side and `activeCandidates`), AC7,
AC8, AC8b. Implements D1's `CheckToken`, D2's refresh rule, D3, D3a's `RefreshRejected`, D3b,
D4, D5 and D6 inside `internal/creds`.

Notes:
- `RefreshNow` stays for now as a forced refresh with its old signature, so the proxy, web UI and
  TUI still compile. Task 4 stops the proxy using it; Task 5 moves the web UI and TUI to `Refresh`
  and deletes it.
- `refresh.go` and `refresh_test.go` are rewritten whole. The old `TestRefresherRefreshNow` and
  `TestRefresherRejectionMarksRevoked` are replaced by `TestRefreshRejected`, and
  `TestStaleRefreshDoesNotUndoReplacedTokens` now drives `Refresh` near expiry.
- `TestRefreshSurvivesBusyDatabase` caps the refresher's only connection at a 100 ms busy wait and
  holds the write lock for 250 ms from a second connection, so the first write attempts fail and
  `store.Retry` stores the result. Without `store.Retry` it fails with `database is locked`.
- `ingest` and `creds refresh` only adapt to the new signatures here. Task 3 passes the file's
  deadline and Task 5 prints the skipped line.

Step 1: write the failing test

Replace `internal/creds/refresh_test.go` with:

````go
package creds

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

// fakeAnthropic serves the token and profile endpoints. Configure it before
// the first request; counters are safe to read at any time.
type fakeAnthropic struct {
	tokenCalls    atomic.Int32
	profileCalls  atomic.Int32
	tokenStatus   int    // 0 answers 200 with fresh tokens
	tokenBody     string // replaces the 200 body when set
	deadlineIn    int    // refresh_token_expires_in in seconds; 0 omits it
	profileStatus int    // 0 answers 200
	onToken       func() // runs before the token endpoint answers
}

func (f *fakeAnthropic) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			f.tokenCalls.Add(1)
			if f.onToken != nil {
				f.onToken()
			}
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
				return
			}
			body := f.tokenBody
			if body == "" {
				extra := ""
				if f.deadlineIn > 0 {
					extra = fmt.Sprintf(`,"refresh_token_expires_in":%d`, f.deadlineIn)
				}
				body = `{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800` + extra + `}`
			}
			_, _ = w.Write([]byte(body))
		case "/profile":
			f.profileCalls.Add(1)
			if f.profileStatus != 0 {
				w.WriteHeader(f.profileStatus)
				return
			}
			_, _ = w.Write([]byte(`{"account":{"email":"a@example.com"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	prevToken, prevProfile := TokenURL, ProfileURL
	TokenURL, ProfileURL = srv.URL+"/token", srv.URL+"/profile"
	t.Cleanup(func() { TokenURL, ProfileURL = prevToken, prevProfile; srv.Close() })
}

// logBuf captures a refresher's log lines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// count returns how many lines are at level and carry label=label.
func (l *logBuf) count(level, label string) int {
	n := 0
	for line := range strings.SplitSeq(l.String(), "\n") {
		fields := strings.Fields(line)
		if hasField(fields, "level="+level) && hasField(fields, "label="+label) {
			n++
		}
	}
	return n
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

func captureLogs(r *Refresher) *logBuf {
	lb := &logBuf{}
	r.SetLogger(slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return lb
}

// insertAt stores an Anthropic credential labelled label with the given
// access-token expiry and refresh deadline (zero = unknown).
func insertAt(t *testing.T, db *store.DB, label string, expires, deadline time.Time) *Credential {
	t.Helper()
	c, err := InsertWithRateLimitTier(context.Background(), db, label, "max", "sk-ant-oat-old", "ref-old", expires, 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func near() time.Time { return time.Now().Add(2 * time.Minute) }

func TestRefreshTokens(t *testing.T) {
	ctx := context.Background()
	f := &fakeAnthropic{deadlineIn: 3600}
	f.serve(t)
	ts, err := RefreshTokens(ctx, "old-ref", time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("refresh-tokens: %v", err)
	}
	if ts.AccessToken != "sk-ant-oat-fresh" || ts.RefreshToken != "ref-fresh" || time.Until(ts.ExpiresAt) < 7*time.Hour {
		t.Fatalf("unexpected tokens: %+v", ts)
	}
	if d := time.Until(ts.RefreshExpiresAt); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("deadline in %v, want the earlier returned one (1h)", d)
	}
	if ts, err = RefreshTokens(ctx, "old-ref", time.Time{}); err != nil || !ts.RefreshExpiresAt.IsZero() {
		t.Fatalf("unknown deadline must stay unknown: %v, %v", ts.RefreshExpiresAt, err)
	}

	rejected := &fakeAnthropic{tokenStatus: http.StatusBadRequest}
	rejected.serve(t)
	if _, err := RefreshTokens(ctx, "bad", time.Time{}); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("400 = %v, want a rejection quoting the OAuth error", err)
	}
}

// AC4: the stored deadline follows Claude Code's min rule.
func TestRefreshDeadlineRule(t *testing.T) {
	day := 24 * time.Hour
	for _, tt := range []struct {
		name       string
		stored     time.Duration // from now; 0 = unknown
		deadlineIn int           // seconds returned; 0 = none
		want       time.Duration // from now; 0 = unknown
	}{
		{"known keeps the earlier stored value", 10 * day, int((20 * day).Seconds()), 10 * day},
		{"known takes an earlier returned value", 20 * day, int((10 * day).Seconds()), 10 * day},
		{"known without a returned value", 10 * day, 0, 10 * day},
		{"unknown stays unknown", 0, int((10 * day).Seconds()), 0},
		{"passed is replaced", -day, int((10 * day).Seconds()), 10 * day},
		{"passed without a returned value becomes unknown", -day, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			(&fakeAnthropic{deadlineIn: tt.deadlineIn}).serve(t)
			var stored time.Time
			if tt.stored != 0 {
				stored = time.Now().Add(tt.stored)
			}
			c := insertAt(t, db, "a", near(), stored)
			got, refreshed, err := NewRefresher(db).Refresh(ctx, c.ID)
			if err != nil || !refreshed {
				t.Fatalf("refresh = %v, refreshed=%v", err, refreshed)
			}
			if tt.want == 0 {
				if !got.RefreshExpiresAt.IsZero() {
					t.Fatalf("deadline = %v, want unknown", got.RefreshExpiresAt)
				}
				return
			}
			if d := time.Until(got.RefreshExpiresAt) - tt.want; d < -time.Minute || d > time.Minute {
				t.Fatalf("deadline in %v, want about %v", time.Until(got.RefreshExpiresAt), tt.want)
			}
		})
	}
}

// AC5: the non-forced Refresh leaves a working token alone.
func TestRefreshSkipsLiveToken(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		status  Status
		expires time.Time
		call    bool
	}{
		{StatusActive, time.Now().Add(time.Hour), false},
		{StatusLimited, time.Now().Add(time.Hour), false},
		{StatusDisabled, time.Now().Add(time.Hour), false},
		{StatusRevoked, time.Now().Add(time.Hour), true},
		{StatusActive, near(), true},
	} {
		t.Run(fmt.Sprintf("%s/call=%v", tt.status, tt.call), func(t *testing.T) {
			db := testDB(t)
			f := &fakeAnthropic{}
			f.serve(t)
			c := insertAt(t, db, "a", tt.expires, time.Time{})
			if err := SetStatus(ctx, db, c.ID, tt.status); err != nil {
				t.Fatal(err)
			}
			_, refreshed, err := NewRefresher(db).Refresh(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if calls := f.tokenCalls.Load(); (calls == 1) != tt.call || refreshed != tt.call {
				t.Fatalf("token calls = %d, refreshed = %v; want call=%v", calls, refreshed, tt.call)
			}
		})
	}
	t.Run("not refreshable", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		key, err := InsertKey(ctx, db, provider.GLM, "glm", "", "glm-key", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		_ = SetStatus(ctx, db, key.ID, StatusRevoked)
		r := NewRefresher(db)
		if _, _, err := r.Refresh(ctx, key.ID); !errors.Is(err, ErrNotRefreshable) {
			t.Fatalf("Refresh = %v, want ErrNotRefreshable", err)
		}
		if _, _, err := r.RefreshRejected(ctx, key.ID, "glm-key"); !errors.Is(err, ErrNotRefreshable) {
			t.Fatalf("RefreshRejected = %v, want ErrNotRefreshable", err)
		}
		if f.tokenCalls.Load()+f.profileCalls.Load() != 0 {
			t.Fatal("a non-refreshable credential reached Anthropic")
		}
	})
}

// AC5b: the background loop never touches a long-lived token.
func TestTickLeavesLongLivedTokenAlone(t *testing.T) {
	db := testDB(t)
	f := &fakeAnthropic{}
	f.serve(t)
	insertAt(t, db, "a", time.Now().AddDate(1, 0, 0), time.Now().Add(27*24*time.Hour))
	NewRefresher(db).tick(context.Background())
	if n := f.tokenCalls.Load(); n != 0 {
		t.Fatalf("tick made %d token calls for a year-long token", n)
	}
}

// AC6 (refresher side): a 401 refreshes only a token the profile endpoint
// also rejects.
func TestRefreshRejected(t *testing.T) {
	ctx := context.Background()
	t.Run("alive token is not refreshed", func(t *testing.T) {
		for _, status := range []int{0, http.StatusForbidden, http.StatusServiceUnavailable} {
			db := testDB(t)
			f := &fakeAnthropic{profileStatus: status}
			f.serve(t)
			c := insertAt(t, db, "a", time.Now().AddDate(1, 0, 0), time.Time{})
			_, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken)
			if !errors.Is(err, ErrTokenAlive) || refreshed {
				t.Fatalf("profile %d: err = %v, refreshed = %v; want ErrTokenAlive", status, err, refreshed)
			}
			got, _ := Get(ctx, db, c.ID)
			if f.tokenCalls.Load() != 0 || got.AccessToken != c.AccessToken || got.Status != StatusActive || !got.ExpiresAt.Equal(c.ExpiresAt.Truncate(time.Second)) {
				t.Fatalf("profile %d: token calls %d, stored %+v", status, f.tokenCalls.Load(), got)
			}
		}
	})
	t.Run("confirmed dead token is refreshed", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		got, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken)
		if err != nil || !refreshed || got.AccessToken != "sk-ant-oat-fresh" || f.tokenCalls.Load() != 1 {
			t.Fatalf("err=%v refreshed=%v got=%+v calls=%d", err, refreshed, got, f.tokenCalls.Load())
		}
	})
	t.Run("near expiry skips the profile check", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		c := insertAt(t, db, "a", near(), time.Time{})
		if _, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken); err != nil || !refreshed {
			t.Fatalf("err=%v refreshed=%v", err, refreshed)
		}
		if f.profileCalls.Load() != 0 || f.tokenCalls.Load() != 1 {
			t.Fatalf("profile calls %d, token calls %d", f.profileCalls.Load(), f.tokenCalls.Load())
		}
	})
	t.Run("token already replaced", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		got, refreshed, err := NewRefresher(db).RefreshRejected(ctx, c.ID, "sk-ant-oat-an-older-one")
		if err != nil || refreshed || got.AccessToken != c.AccessToken || f.tokenCalls.Load()+f.profileCalls.Load() != 0 {
			t.Fatalf("err=%v refreshed=%v calls=%d/%d", err, refreshed, f.tokenCalls.Load(), f.profileCalls.Load())
		}
	})
	t.Run("concurrent 401s refresh once", func(t *testing.T) {
		db := testDB(t)
		release := make(chan struct{})
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized, onToken: func() { <-release }}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		r := NewRefresher(db)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Go(func() {
				_, _, err := r.RefreshRejected(ctx, c.ID, c.AccessToken)
				errs <- err
			})
		}
		for f.tokenCalls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		close(release)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := f.tokenCalls.Load(); n != 1 {
			t.Fatalf("token calls = %d, want 1", n)
		}
	})
	t.Run("rejected login is revoked", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized, tokenStatus: http.StatusBadRequest}
		f.serve(t)
		c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
		if _, _, err := NewRefresher(db).RefreshRejected(ctx, c.ID, c.AccessToken); !errors.Is(err, ErrLoginRejected) {
			t.Fatalf("err = %v, want ErrLoginRejected", err)
		}
		if got, _ := Get(ctx, db, c.ID); got.Status != StatusRevoked {
			t.Fatalf("status = %q, want revoked", got.Status)
		}
	})
}

// AC6 (refresher side): a transient failure leaves the status alone, sets a
// one-minute retry_after and is replayed for 30 s without a token call.
func TestTransientRefreshFailure(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		setup func(f *fakeAnthropic, r *Refresher)
	}{
		{"server error", func(f *fakeAnthropic, _ *Refresher) { f.tokenStatus = http.StatusInternalServerError }},
		{"timeout", func(f *fakeAnthropic, r *Refresher) {
			f.onToken = func() { time.Sleep(300 * time.Millisecond) }
			SetTokenClient(r, &http.Client{Timeout: 50 * time.Millisecond})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			r := NewRefresher(db)
			f := &fakeAnthropic{profileStatus: http.StatusUnauthorized}
			tt.setup(f, r)
			f.serve(t)
			c := insertAt(t, db, "a", time.Now().Add(time.Hour), time.Time{})
			_, _, err := r.RefreshRejected(ctx, c.ID, c.AccessToken)
			if err == nil || errors.Is(err, ErrLoginRejected) || errors.Is(err, ErrTokenAlive) {
				t.Fatalf("err = %v, want a transient error", err)
			}
			got, _ := Get(ctx, db, c.ID)
			if got.Status != StatusActive || got.RetryAfter == nil {
				t.Fatalf("status %q retry_after %v, want active with a retry_after", got.Status, got.RetryAfter)
			}
			if d := time.Until(*got.RetryAfter); d < 50*time.Second || d > 61*time.Second {
				t.Fatalf("retry_after in %v, want about 60s", d)
			}
			if _, _, err2 := r.RefreshRejected(ctx, c.ID, c.AccessToken); err2 == nil || f.tokenCalls.Load() != 1 {
				t.Fatalf("second 401: err=%v, token calls %d; want the remembered error and no new call", err2, f.tokenCalls.Load())
			}
		})
	}
}

// AC7: one WARN per failed refresh and one ERROR per revocation, with the
// label, and a 200 without a refresh token leaks nothing of its body.
func TestRefreshLogging(t *testing.T) {
	ctx := context.Background()
	t.Run("transient", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{tokenStatus: http.StatusBadGateway}).serve(t)
		c := insertAt(t, db, "acct-warn", near(), time.Time{})
		r := NewRefresher(db)
		logs := captureLogs(r)
		_, _, _ = r.Refresh(ctx, c.ID)
		if logs.count("WARN", "acct-warn") != 1 || logs.count("ERROR", "acct-warn") != 0 {
			t.Fatalf("logs:\n%s", logs)
		}
	})
	t.Run("revocation", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{tokenStatus: http.StatusBadRequest}).serve(t)
		c := insertAt(t, db, "acct-err", near(), time.Now().Add(-time.Hour))
		r := NewRefresher(db)
		logs := captureLogs(r)
		_, _, _ = r.Refresh(ctx, c.ID)
		if logs.count("ERROR", "acct-err") != 1 || logs.count("WARN", "acct-err") != 0 || !strings.Contains(logs.String(), "deadline_passed=true") {
			t.Fatalf("logs:\n%s", logs)
		}
	})
	t.Run("missing refresh token", func(t *testing.T) {
		db := testDB(t)
		body := `{"access_token":"sk-ant-oat-leaked-secret","expires_in":28800,"scope":"user:inference"}`
		(&fakeAnthropic{tokenBody: body}).serve(t)
		c := insertAt(t, db, "acct-leak", near(), time.Time{})
		r := NewRefresher(db)
		logs := captureLogs(r)
		_, _, err := r.Refresh(ctx, c.ID)
		if err == nil {
			t.Fatal("a 200 without a refresh token must fail")
		}
		for _, part := range []string{"sk-ant-oat-leaked-secret", "access_token", "expires_in", "28800", "user:inference"} {
			if strings.Contains(err.Error(), part) || strings.Contains(logs.String(), part) {
				t.Fatalf("%q leaked: err=%q logs=%q", part, err, logs)
			}
		}
	})
	t.Run("long-lived token rejected", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{profileStatus: http.StatusUnauthorized}).serve(t)
		c := insertAt(t, db, "acct-long", time.Now().AddDate(1, 0, 0), time.Now().Add(20*24*time.Hour))
		r := NewRefresher(db)
		logs := captureLogs(r)
		if _, _, err := r.RefreshRejected(ctx, c.ID, c.AccessToken); err != nil {
			t.Fatal(err)
		}
		if logs.count("ERROR", "acct-long") != 1 || !strings.Contains(logs.String(), "long-lived token rejected before its expiry") {
			t.Fatalf("logs:\n%s", logs)
		}
	})
}

// AC8: a refresh Anthropic has answered is stored even when the caller left.
func TestRefreshSurvivesCallerCancel(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeAnthropic{onToken: cancel}
	f.serve(t)
	c := insertAt(t, db, "a", near(), time.Time{})
	if _, _, err := NewRefresher(db).Refresh(ctx, c.ID); err != nil {
		t.Fatalf("refresh after cancel: %v", err)
	}
	if got, _ := Get(context.Background(), db, c.ID); got.AccessToken != "sk-ant-oat-fresh" {
		t.Fatalf("tokens lost after the caller cancelled: %q", got.AccessToken)
	}
}

// AC8b: a briefly busy database does not lose a completed refresh. The
// refresher's only connection waits 100 ms per attempt; the lock is held for
// 250 ms, so the first two write attempts fail and a later one succeeds.
func TestRefreshSurvivesBusyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 100`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	c := insertAt(t, db, "a", near(), time.Time{})

	var tx *sql.Tx
	f := &fakeAnthropic{onToken: func() {
		var lerr error
		if tx, lerr = locker.Begin(); lerr != nil {
			t.Error(lerr)
			return
		}
		time.AfterFunc(250*time.Millisecond, func() { _ = tx.Rollback() })
	}}
	f.serve(t)
	if _, _, err := NewRefresher(db).Refresh(context.Background(), c.ID); err != nil {
		t.Fatalf("refresh with a busy database: %v", err)
	}
	if got, _ := Get(context.Background(), db, c.ID); got.AccessToken != "sk-ant-oat-fresh" {
		t.Fatalf("tokens not stored: %q", got.AccessToken)
	}
}

// A refresh that started before the tokens were replaced (Update tokens or a
// browser reconnect) must not revoke or overwrite the newer login when its
// stale response finally arrives.
func TestStaleRefreshDoesNotUndoReplacedTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rejected", http.StatusBadRequest},
		{"succeeded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			entered, release := make(chan struct{}), make(chan struct{})
			f := &fakeAnthropic{tokenStatus: tc.status, onToken: func() { close(entered); <-release }}
			f.serve(t)
			c := insertAt(t, db, "a", near(), time.Time{})
			r := NewRefresher(db)
			done := make(chan error, 1)
			go func() { _, _, err := r.Refresh(ctx, c.ID); done <- err }()

			<-entered
			if err := UpdateTokensWithSubscription(ctx, db, c.ID, "sk-ant-oat-relogin", "ref-relogin", time.Now().Add(8*time.Hour), "", "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("stale refresh reported %v; the credential is healthy", err)
			}
			got, _ := Get(ctx, db, c.ID)
			if got.RefreshToken != "ref-relogin" || got.AccessToken != "sk-ant-oat-relogin" || got.Status != StatusActive {
				t.Fatalf("newer login undone: %+v", got)
			}
		})
	}
}

// A stored refresh heals revoked but leaves disabled and limited alone.
func TestRefreshKeepsOperatorStatus(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct{ from, want Status }{
		{StatusRevoked, StatusActive},
		{StatusDisabled, StatusDisabled},
		{StatusLimited, StatusLimited},
	} {
		db := testDB(t)
		(&fakeAnthropic{}).serve(t)
		c := insertAt(t, db, "a", near(), time.Time{})
		_ = SetStatus(ctx, db, c.ID, tt.from)
		got, refreshed, err := NewRefresher(db).Refresh(ctx, c.ID)
		if err != nil || !refreshed || got.Status != tt.want {
			t.Fatalf("%s: err=%v refreshed=%v status=%q, want %q", tt.from, err, refreshed, got.Status, tt.want)
		}
	}
}
````

New file `internal/pool/refresh_backoff_test.go`:

````go
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
	entries, err := activeCandidates(ctx, db, provider.Anthropic, nil, "", time.Now())
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
````

Run: `go test -race -count=1 ./internal/creds/ ./internal/pool/`
Expected: both packages `[build failed]`; first lines `internal/creds/refresh_test.go:69:38: undefined: ProfileURL` and `internal/pool/refresh_backoff_test.go:29:18: assignment mismatch: 3 variables but creds.NewRefresher(db).Refresh returns 2 values`

Step 2: implement

New file `internal/creds/check.go`:

````go
package creds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProfileURL is the endpoint CheckToken reads. A var so tests can point it at
// a fake server.
var ProfileURL = "https://api.anthropic.com/api/oauth/profile"

// ErrTokenRejected means Anthropic answered 401 to the access token itself.
var ErrTokenRejected = errors.New("access token rejected by Anthropic")

var checkClient = &http.Client{Timeout: 10 * time.Second}

// CheckToken asks Anthropic's profile endpoint whether accessToken still
// works, with the headers the web sign-in sends. nil means 200 and
// ErrTokenRejected means 401; any other error says nothing about the token.
func CheckToken(ctx context.Context, accessToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProfileURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := checkClient.Do(req)
	if err != nil {
		return fmt.Errorf("profile check: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTokenBody))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrTokenRejected
	}
	return fmt.Errorf("profile check: unexpected status %d", resp.StatusCode)
}
````

Replace `internal/creds/refresh.go` with:

````go
package creds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/p4u/claude-proxy/internal/provider"
	"github.com/p4u/claude-proxy/internal/store"
)

const ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// TokenURL is a var (not const) so tests can rewrite it.
var TokenURL = "https://platform.claude.com/v1/oauth/token"

const (
	// refreshWindow is how close to its expiry an access token gets refreshed.
	refreshWindow = 5 * time.Minute
	// LongLived marks a long-lived login: its access token outlives this when
	// minted, which an 8-hour token never does.
	LongLived = 24 * time.Hour
	// failureMemory is how long a transient refresh error is returned to later
	// callers without contacting the token endpoint again.
	failureMemory = 30 * time.Second
	// failureBackoff keeps new conversations off a credential whose refresh
	// just failed transiently (retry_after); existing pins keep it.
	failureBackoff = 60 * time.Second
	maxTokenBody   = 1 << 20
	maxQuotedBody  = 512
)

var (
	// ErrLoginRejected means the token endpoint answered 400/401 to the
	// refresh token and the credential was marked revoked.
	ErrLoginRejected = errors.New("login rejected by Anthropic")
	// ErrTokenAlive means RefreshRejected did not refresh: the access token
	// still works, or the liveness check could not prove it dead.
	ErrTokenAlive = errors.New("access token still valid")
	// ErrNotRefreshable means the credential's provider has no OAuth refresh.
	ErrNotRefreshable = errors.New("credential has no OAuth refresh")
)

type Refresher struct {
	db       *store.DB
	client   *http.Client
	log      *slog.Logger
	mus      sync.Map // id -> *sync.Mutex
	failures sync.Map // id -> failure
}

// failure is a credential's last transient refresh error.
type failure struct {
	err error
	at  time.Time
}

func NewRefresher(db *store.DB) *Refresher {
	return &Refresher{
		db:     db,
		client: &http.Client{Timeout: 30 * time.Second},
		log:    slog.New(slog.DiscardHandler),
	}
}

// SetLogger sets where refresh outcomes are logged; without it they are
// discarded. Call it before Loop or any refresh.
func (r *Refresher) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l
	}
}

// SetTokenClient swaps the HTTP client used for refresh requests (test hook).
func SetTokenClient(r *Refresher, c *http.Client) { r.client = c }

// SetTokenURL overrides the global token endpoint (test hook).
func SetTokenURL(u string) { TokenURL = u }

func (r *Refresher) lockFor(id string) *sync.Mutex {
	v, _ := r.mus.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type refreshReq struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

type refreshResp struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope,omitempty"`
	Error                 string `json:"error,omitempty"`
}

// TokenSet is what a refresh yields.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	// RefreshExpiresAt is the login's refresh deadline; zero when unknown.
	RefreshExpiresAt time.Time
}

var importClient = &http.Client{Timeout: 30 * time.Second}

// RefreshTokens performs a stateless refresh (no DB record required), used
// during import to verify a credential is alive before storing it. deadline
// is the login's known refresh deadline, zero when unknown; the result
// carries it updated by the same rule as a stored refresh.
func RefreshTokens(ctx context.Context, refreshToken string, deadline time.Time) (TokenSet, error) {
	now := time.Now()
	status, raw, err := tokenRequest(ctx, importClient, refreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		return TokenSet{}, fmt.Errorf("credential rejected by Anthropic (%d): %s", status, quoteBody(raw))
	}
	if status != http.StatusOK {
		return TokenSet{}, fmt.Errorf("refresh upstream %d: %s", status, quoteBody(raw))
	}
	ts, err := decodeTokens(raw, now)
	if err != nil {
		return TokenSet{}, err
	}
	ts.RefreshExpiresAt = nextDeadline(deadline, ts.RefreshExpiresAt, now)
	return ts, nil
}

// tokenRequest posts a refresh_token grant and returns the status and body,
// read through a 1 MiB cap.
func tokenRequest(ctx context.Context, client *http.Client, refreshToken string) (int, []byte, error) {
	body, _ := json.Marshal(refreshReq{GrantType: "refresh_token", ClientID: ClientID, RefreshToken: refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody))
	if err != nil {
		return 0, nil, fmt.Errorf("refresh read: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// decodeTokens parses a 200 token response. RefreshExpiresAt is the deadline
// the response reports, before nextDeadline. Errors never quote the body: it
// carries the tokens.
func decodeTokens(raw []byte, now time.Time) (TokenSet, error) {
	var rr refreshResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return TokenSet{}, errors.New("refresh response is not a token response")
	}
	if rr.AccessToken == "" || rr.RefreshToken == "" {
		return TokenSet{}, errors.New("refresh response missing tokens")
	}
	ts := TokenSet{
		AccessToken:  rr.AccessToken,
		RefreshToken: rr.RefreshToken,
		ExpiresAt:    now.Add(time.Duration(rr.ExpiresIn)*time.Second - 5*time.Minute),
	}
	if rr.RefreshTokenExpiresIn > 0 {
		ts.RefreshExpiresAt = now.Add(time.Duration(rr.RefreshTokenExpiresIn) * time.Second)
	}
	return ts, nil
}

// nextDeadline is Claude Code's rule for the deadline after a refresh: a known
// deadline only ever moves earlier, and an unknown one stays unknown, because
// for older logins the returned value is not a deadline. A stored deadline
// that has passed while the refresh still succeeded was wrong, so the returned
// one (or unknown) replaces it.
func nextDeadline(stored, returned, now time.Time) time.Time {
	switch {
	case stored.IsZero():
		return time.Time{}
	case !stored.After(now):
		return returned
	case !returned.IsZero() && returned.Before(stored):
		return returned
	}
	return stored
}

// quoteBody trims a non-2xx token-endpoint body (OAuth error JSON) for an
// error or a log line. A 2xx body carries tokens and must never be passed here.
func quoteBody(raw []byte) string {
	if len(raw) > maxQuotedBody {
		raw = raw[:maxQuotedBody]
	}
	return string(raw)
}

// deadlineAttr renders a deadline for a log line.
func deadlineAttr(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// Refresh refreshes a credential whose access token expires within 5 minutes,
// or one marked revoked. Any other credential is returned unchanged with
// refreshed=false. Safe to call concurrently: serialised per id.
func (r *Refresher) Refresh(ctx context.Context, id string) (c *Credential, refreshed bool, err error) {
	mu := r.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	// From here on the work completes even if the caller goes away: a refresh
	// Anthropic has answered must be stored, or the rotated token is lost.
	ctx = context.WithoutCancel(ctx)
	c, err = Get(ctx, r.db, id)
	if err != nil {
		return nil, false, err
	}
	if !provider.Get(c.Provider).Refreshable {
		return c, false, ErrNotRefreshable
	}
	if time.Until(c.ExpiresAt) > refreshWindow && c.Status != StatusRevoked {
		return c, false, nil
	}
	return r.refreshLocked(ctx, c)
}

// RefreshNow forces a refresh. Transitional: its last callers (web UI, TUI)
// move to Refresh in a later change, which deletes it.
func (r *Refresher) RefreshNow(ctx context.Context, id string) (*Credential, error) {
	mu := r.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	c, err := Get(ctx, r.db, id)
	if err != nil {
		return nil, err
	}
	if !provider.Get(c.Provider).Refreshable {
		return c, ErrNotRefreshable
	}
	c, _, err = r.refreshLocked(ctx, c)
	return c, err
}

// RefreshRejected handles an upstream 401 on rejected, the access token the
// request carried. It refreshes only when the token is really dead: another
// request may already have replaced it, and a 401 can describe the request
// rather than the token, so a token far from expiry is first checked against
// the profile endpoint. ErrTokenAlive means nothing was refreshed.
func (r *Refresher) RefreshRejected(ctx context.Context, id, rejected string) (c *Credential, refreshed bool, err error) {
	mu := r.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	c, err = Get(ctx, r.db, id)
	if err != nil {
		return nil, false, err
	}
	if !provider.Get(c.Provider).Refreshable {
		return c, false, ErrNotRefreshable
	}
	if c.AccessToken != rejected {
		return c, false, nil
	}
	if time.Until(c.ExpiresAt) > refreshWindow {
		switch err := CheckToken(ctx, c.AccessToken); {
		case err == nil:
			return c, false, ErrTokenAlive
		case !errors.Is(err, ErrTokenRejected):
			return c, false, fmt.Errorf("%w: %w", ErrTokenAlive, err)
		case time.Until(c.ExpiresAt) > LongLived:
			r.log.Error("long-lived token rejected before its expiry",
				"cred", c.ID, "label", c.Label,
				"expires_at", c.ExpiresAt.UTC().Format(time.RFC3339),
				"refresh_deadline", deadlineAttr(c.RefreshExpiresAt))
		}
	}
	return r.refreshLocked(ctx, c)
}

// refreshLocked calls the token endpoint for c and stores the outcome. The
// caller holds c's mutex and passes a context that is never cancelled.
func (r *Refresher) refreshLocked(ctx context.Context, c *Credential) (*Credential, bool, error) {
	if err := r.recentFailure(c.ID); err != nil {
		return nil, false, err
	}
	now := time.Now()
	status, raw, err := tokenRequest(ctx, r.client, c.RefreshToken)
	if err == nil && (status == http.StatusBadRequest || status == http.StatusUnauthorized) {
		return r.rejected(ctx, c, status, raw)
	}
	var ts TokenSet
	switch {
	case err != nil:
	case status != http.StatusOK:
		err = fmt.Errorf("refresh upstream %d: %s", status, quoteBody(raw))
	default:
		ts, err = decodeTokens(raw, now)
	}
	if err != nil {
		r.transientFailure(ctx, c, err)
		return nil, false, err
	}

	returned := ts.RefreshExpiresAt
	ts.RefreshExpiresAt = nextDeadline(c.RefreshExpiresAt, returned, now)
	var stored bool
	err = store.Retry(ctx, func() error {
		var werr error
		stored, werr = replaceTokensIfLineage(ctx, r.db, c.ID, c.RefreshToken, ts)
		return werr
	})
	if err != nil {
		r.log.Error("refreshed upstream but not stored; the credential will be rejected at its next refresh",
			"cred", c.ID, "label", c.Label, "err", err)
		return nil, false, err
	}
	r.failures.Delete(c.ID)
	if !stored {
		// A reconnect replaced the tokens while this request was in flight.
		r.log.Debug("token refresh superseded by a newer login", "cred", c.ID, "label", c.Label)
		fresh, err := Get(ctx, r.db, c.ID)
		return fresh, false, err
	}
	r.log.Debug("token refreshed", "cred", c.ID, "label", c.Label,
		"expires_at", ts.ExpiresAt.UTC().Format(time.RFC3339),
		"refresh_deadline", deadlineAttr(ts.RefreshExpiresAt),
		"returned_deadline", deadlineAttr(returned))
	fresh, err := Get(ctx, r.db, c.ID)
	return fresh, true, err
}

// rejected handles a 400/401 from the token endpoint: the lineage is dead, so
// the credential is revoked, unless a newer login replaced it meanwhile.
func (r *Refresher) rejected(ctx context.Context, c *Credential, status int, raw []byte) (*Credential, bool, error) {
	var revoked bool
	err := store.Retry(ctx, func() error {
		var werr error
		revoked, werr = revokeIfLineage(ctx, r.db, c.ID, c.RefreshToken)
		return werr
	})
	if err != nil {
		r.log.Warn("token refresh rejected but the revocation was not stored",
			"cred", c.ID, "label", c.Label, "err", err)
		return nil, false, err
	}
	r.failures.Delete(c.ID)
	if !revoked {
		r.log.Debug("token refresh superseded by a newer login", "cred", c.ID, "label", c.Label)
		fresh, err := Get(ctx, r.db, c.ID)
		return fresh, false, err
	}
	passed := !c.RefreshExpiresAt.IsZero() && !c.RefreshExpiresAt.After(time.Now())
	r.log.Error("login rejected by Anthropic; credential revoked",
		"cred", c.ID, "label", c.Label, "status", status,
		"refresh_deadline", deadlineAttr(c.RefreshExpiresAt), "deadline_passed", passed,
		"upstream", quoteBody(raw))
	return nil, false, fmt.Errorf("%w (%d): %s", ErrLoginRejected, status, quoteBody(raw))
}

// transientFailure remembers err so callers queued on the mutex fail fast,
// and keeps new conversations off the credential for failureBackoff. The
// status is left alone: nothing proved the login dead.
func (r *Refresher) transientFailure(ctx context.Context, c *Credential, err error) {
	now := time.Now()
	r.failures.Store(c.ID, failure{err: err, at: now})
	werr := store.Retry(ctx, func() error {
		_, e := r.db.ExecContext(ctx,
			`UPDATE credentials SET retry_after=MAX(COALESCE(retry_after,0),?) WHERE id=?`,
			now.Add(failureBackoff).Unix(), c.ID)
		return e
	})
	r.log.Warn("token refresh failed", "cred", c.ID, "label", c.Label, "err", err,
		"retry_after_stored", werr == nil)
}

// recentFailure returns the credential's last transient error while it is
// younger than failureMemory.
func (r *Refresher) recentFailure(id string) error {
	v, ok := r.failures.Load(id)
	if !ok {
		return nil
	}
	f := v.(failure)
	if time.Since(f.at) >= failureMemory {
		r.failures.Delete(id)
		return nil
	}
	return f.err
}

// replaceTokensIfLineage writes a refresh result only while the credential
// still holds the refresh token the request was made with. It heals only
// token-related states: disabled and limited stay as they are.
func replaceTokensIfLineage(ctx context.Context, db *store.DB, id, oldRefresh string, ts TokenSet) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE credentials
		SET access_token=?, refresh_token=?, expires_at=?, refresh_expires_at=?,
		    status=CASE WHEN status IN ('revoked','expired') THEN 'active' ELSE status END
		WHERE id=? AND refresh_token=?`,
		ts.AccessToken, ts.RefreshToken, ts.ExpiresAt.Unix(), unixOrZero(ts.RefreshExpiresAt), id, oldRefresh)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// revokeIfLineage marks the credential revoked only while it still holds the
// refresh token that was rejected.
func revokeIfLineage(ctx context.Context, db *store.DB, id, oldRefresh string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE credentials SET status=? WHERE id=? AND refresh_token=?`, string(StatusRevoked), id, oldRefresh)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Loop runs proactive refresh in the background until ctx is cancelled.
func (r *Refresher) Loop(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Refresher) tick(ctx context.Context) {
	creds, err := List(ctx, r.db)
	if err != nil {
		r.log.Warn("refresh loop: list credentials", "err", err)
		return
	}
	cutoff := time.Now().Add(refreshWindow)
	for _, c := range creds {
		// Static API keys have no refresh token and no real expiry.
		if !provider.Get(c.Provider).Refreshable {
			continue
		}
		if c.Status == StatusRevoked || c.Status == StatusDisabled {
			continue
		}
		if c.ExpiresAt.Before(cutoff) {
			_, _, _ = r.Refresh(ctx, c.ID) // refresh logs its own outcome
		}
	}
}
````

The callers adapt to `RefreshTokens` returning a `TokenSet` and `Refresh` returning three values:

`internal/ingest/import.go`:

````diff
diff --git a/internal/ingest/import.go b/internal/ingest/import.go
index a5803e7..8685fa7 100644
--- a/internal/ingest/import.go
+++ b/internal/ingest/import.go
@@ -81,7 +81,7 @@ func insertVerified(ctx context.Context, db *store.DB, o oauthBlock, label strin
 	}
 
 	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
-	accessToken, refreshToken, expires, err := creds.RefreshTokens(ctx, o.RefreshToken)
+	ts, err := creds.RefreshTokens(ctx, o.RefreshToken, time.Time{})
 	if err != nil {
 		return nil, fmt.Errorf("credential is not alive: %w", err)
 	}
@@ -89,7 +89,7 @@ func insertVerified(ctx context.Context, db *store.DB, o oauthBlock, label strin
 	if label == "" {
 		label = o.SubscriptionType
 	}
-	return creds.InsertWithRateLimitTier(ctx, db, label, o.SubscriptionType, accessToken, refreshToken, expires, weight, o.RateLimitTier, time.Time{})
+	return creds.InsertWithRateLimitTier(ctx, db, label, o.SubscriptionType, ts.AccessToken, ts.RefreshToken, ts.ExpiresAt, weight, o.RateLimitTier, ts.RefreshExpiresAt)
 }
 
 // Import reads a Claude Code .credentials.json and inserts it into the pool.
@@ -147,11 +147,11 @@ func updateVerified(ctx context.Context, db *store.DB, id string, o oauthBlock)
 		return nil, err
 	}
 	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
-	accessToken, refreshToken, expires, err := creds.RefreshTokens(ctx, o.RefreshToken)
+	ts, err := creds.RefreshTokens(ctx, o.RefreshToken, time.Time{})
 	if err != nil {
 		return nil, fmt.Errorf("credential is not alive: %w", err)
 	}
-	if err := creds.UpdateTokensWithSubscription(ctx, db, id, accessToken, refreshToken, expires, o.SubscriptionType, o.RateLimitTier, time.Time{}); err != nil {
+	if err := creds.UpdateTokensWithSubscription(ctx, db, id, ts.AccessToken, ts.RefreshToken, ts.ExpiresAt, o.SubscriptionType, o.RateLimitTier, ts.RefreshExpiresAt); err != nil {
 		return nil, err
 	}
 	return creds.Get(ctx, db, id)
````

`cmd/claude-proxy/main.go`:

````diff
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index bce94bd..95d773d 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -762,7 +762,7 @@ func credsRefresh(ctx context.Context, args []string) {
 	db := openDB(*dbPath)
 	defer db.Close()
 	r := creds.NewRefresher(db)
-	c, err := r.Refresh(ctx, args[0])
+	c, _, err := r.Refresh(ctx, args[0])
 	if err != nil {
 		fmt.Fprintf(os.Stderr, "refresh: %v\n", err)
 		os.Exit(1)
````

Run: `go test -race -count=1 -v -run '^(TestRefreshTokens|TestRefreshDeadlineRule|TestRefreshSkipsLiveToken|TestTickLeavesLongLivedTokenAlone|TestRefreshRejected|TestTransientRefreshFailure|TestRefreshLogging|TestRefreshSurvivesCallerCancel|TestRefreshSurvivesBusyDatabase|TestStaleRefreshDoesNotUndoReplacedTokens|TestRefreshKeepsOperatorStatus)$' ./internal/creds/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: 11 `--- PASS` lines (`TestRefreshTokens`, `TestRefreshDeadlineRule`, `TestRefreshSkipsLiveToken`, `TestTickLeavesLongLivedTokenAlone`, `TestRefreshRejected`, `TestTransientRefreshFailure`, `TestRefreshLogging`, `TestRefreshSurvivesCallerCancel`, `TestRefreshSurvivesBusyDatabase`, `TestStaleRefreshDoesNotUndoReplacedTokens`, `TestRefreshKeepsOperatorStatus`), then `ok  github.com/p4u/claude-proxy/internal/creds`

Run: `go test -race -count=1 -run TestTransientRefreshFailureLeavesActiveCandidates ./internal/pool/`
Expected: `ok  github.com/p4u/claude-proxy/internal/pool`

Run: `go vet ./... && go test -race -count=1 ./... 2>&1 | grep -c '^ok'`
Expected: `18` (and no `FAIL` line)

Step 3: commit
`git add internal/creds/check.go internal/pool/refresh_backoff_test.go && git commit -m "fix(creds): refresh only tokens that stopped working" -- internal/creds/refresh.go internal/creds/check.go internal/creds/refresh_test.go internal/pool/refresh_backoff_test.go internal/ingest/import.go cmd/claude-proxy/main.go`

Interfaces this task provides to later tasks:
- `var creds.ProfileURL = "https://api.anthropic.com/api/oauth/profile"` (test hook).
- `func CheckToken(ctx context.Context, accessToken string) error`: nil on 200,
  `ErrTokenRejected` on 401, a descriptive error otherwise.
- `var ErrTokenRejected, ErrLoginRejected, ErrTokenAlive, ErrNotRefreshable error`.
- `const LongLived = 24 * time.Hour`.
- `type TokenSet struct { AccessToken, RefreshToken string; ExpiresAt, RefreshExpiresAt time.Time }`
- `func RefreshTokens(ctx context.Context, refreshToken string, deadline time.Time) (TokenSet, error)`:
  `deadline` is the login's known deadline (zero = unknown); the result carries it after the
  refresh rule.
- `func (r *Refresher) Refresh(ctx context.Context, id string) (c *Credential, refreshed bool, err error)`
- `func (r *Refresher) RefreshRejected(ctx context.Context, id, rejected string) (c *Credential, refreshed bool, err error)`
- `func (r *Refresher) SetLogger(l *slog.Logger)`
- Transitional `func (r *Refresher) RefreshNow(ctx context.Context, id string) (*Credential, error)`,
  deleted in Task 5.

### Task 3: Year-long sign-in, and imports that keep long-lived logins
Model: opus   Repo: /home/p4u/claude-proxy   Files: `internal/claudeoauth/claudeoauth.go`,
`internal/claudeoauth/claudeoauth_test.go`, `internal/webui/claudelogin.go`,
`internal/webui/claudelogin_test.go`, `internal/ingest/import.go`,
`internal/ingest/import_test.go`, `internal/ingest/lifetime_test.go` (new),
`internal/tui/tui_test.go`, `cmd/claude-proxy/main.go`, `cmd/claude-proxy/creds_test.go` (new)
Goal: AC1, AC1b, AC3, AC3b. D1 (scopes, `expires_in` at the exchange, the deadline on `Tokens`,
`WithoutCancel` in `exchangeClaudeLogin`), D2's sign-in and file rules, D2a (long-lived files are
checked, never rotated; `creds export` carries the deadline).

Notes:
- Apply the fixture changes in `import_test.go` and `tui_test.go` before the implementation.
  Their files used `expiresAt: 9999999999000`, which after D2a would take the profile-check path
  and reach the real `api.anthropic.com`.
- `credsExport` and `credsImportBulk` keep their behaviour and messages but move their bodies
  into `exportCreds(ctx, db, w)` and `importBulk(ctx, db, in, out)` so the round trip is testable
  in-process (a subprocess could not redirect `creds.ProfileURL`).

Step 1: write the failing test

`internal/ingest/import_test.go`:

````diff
diff --git a/internal/ingest/import_test.go b/internal/ingest/import_test.go
index 06cc775..386da66 100644
--- a/internal/ingest/import_test.go
+++ b/internal/ingest/import_test.go
@@ -38,13 +38,19 @@ func mockToken(t *testing.T) {
 	t.Cleanup(func() { creds.SetTokenURL(prev); srv.Close() })
 }
 
+// shortExpiry is an 8-hour login's expiresAt (ms): such files take the
+// refresh-to-verify path. A far-future value would be a long-lived login.
+func shortExpiry() int64 { return time.Now().Add(time.Hour).UnixMilli() }
+
 // writeCredFile writes a synthetic .credentials.json with mode 0600.
 func writeCredFile(t *testing.T, access, refresh string) string {
 	t.Helper()
 	path := filepath.Join(t.TempDir(), ".credentials.json")
-	body := `{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"` + refresh +
-		`","expiresAt":9999999999000,"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`
-	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
+	body, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
+		"accessToken": access, "refreshToken": refresh, "expiresAt": shortExpiry(),
+		"scopes": []string{"user:inference"}, "subscriptionType": "max", "rateLimitTier": "default_claude_max_20x",
+	}})
+	if err := os.WriteFile(path, body, 0o600); err != nil {
 		t.Fatalf("write cred file: %v", err)
 	}
 	return path
@@ -110,7 +116,7 @@ func TestImportTierSources(t *testing.T) {
 func tierJSON(t *testing.T, plan, tier string) []byte {
 	t.Helper()
 	block := map[string]any{
-		"accessToken": "sk-ant-oat-input", "refreshToken": "ref-input", "expiresAt": 9999999999000,
+		"accessToken": "sk-ant-oat-input", "refreshToken": "ref-input", "expiresAt": shortExpiry(),
 	}
 	if plan != "" {
 		block["subscriptionType"] = plan
````

`internal/tui/tui_test.go`:

````diff
diff --git a/internal/tui/tui_test.go b/internal/tui/tui_test.go
index e6be31d..1a32bb2 100644
--- a/internal/tui/tui_test.go
+++ b/internal/tui/tui_test.go
@@ -2,6 +2,7 @@ package tui
 
 import (
 	"context"
+	"fmt"
 	"net/http"
 	"net/http/httptest"
 	"path/filepath"
@@ -86,7 +87,9 @@ func TestPasteImport(t *testing.T) {
 	}
 
 	// Paste the JSON and submit with ctrl+s → chains to the label prompt.
-	m.paste.SetValue(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-x","refreshToken":"ref-x","expiresAt":9999999999000,"subscriptionType":"max"}}`)
+	// An 8-hour expiry, so the import takes the refresh-to-verify path.
+	m.paste.SetValue(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-x","refreshToken":"ref-x","expiresAt":%d,"subscriptionType":"max"}}`,
+		time.Now().Add(time.Hour).UnixMilli()))
 	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
 	if m.inputKind != inputImportLabel {
 		t.Fatalf("ctrl+s should advance to label prompt, got %v", m.inputKind)
````

`internal/claudeoauth/claudeoauth_test.go`:

````diff
diff --git a/internal/claudeoauth/claudeoauth_test.go b/internal/claudeoauth/claudeoauth_test.go
index 3c4d0df..1fd9375 100644
--- a/internal/claudeoauth/claudeoauth_test.go
+++ b/internal/claudeoauth/claudeoauth_test.go
@@ -26,6 +26,11 @@ type fakeAnthropic struct {
 	scope     string
 	tokenCode int
 	exchanges int
+	// expiresIn and deadlineIn are the granted lifetime and
+	// refresh_token_expires_in; 0 means 28800 and omitted.
+	expiresIn  int
+	deadlineIn int
+	body       map[string]any // the last exchange request
 }
 
 func (f *fakeAnthropic) serve(t *testing.T) *httptest.Server {
@@ -34,11 +39,13 @@ func (f *fakeAnthropic) serve(t *testing.T) *httptest.Server {
 		switch r.URL.Path {
 		case "/v1/oauth/token":
 			f.exchanges++
-			var b map[string]string
+			var b map[string]any
 			_ = json.NewDecoder(r.Body).Decode(&b)
-			sum := sha256.Sum256([]byte(b["code_verifier"]))
-			if b["grant_type"] != "authorization_code" || b["code"] != "the-code" ||
-				b["client_id"] != creds.ClientID || b["redirect_uri"] != RedirectURL ||
+			f.body = b
+			str := func(k string) string { v, _ := b[k].(string); return v }
+			sum := sha256.Sum256([]byte(str("code_verifier")))
+			if str("grant_type") != "authorization_code" || str("code") != "the-code" ||
+				str("client_id") != creds.ClientID || str("redirect_uri") != RedirectURL ||
 				base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
 				w.WriteHeader(http.StatusBadRequest)
 				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code","access_token":"sk-ant-oat-leak"}`))
@@ -49,11 +56,19 @@ func (f *fakeAnthropic) serve(t *testing.T) *httptest.Server {
 				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
 				return
 			}
-			_ = json.NewEncoder(w).Encode(map[string]any{
+			expiresIn := f.expiresIn
+			if expiresIn == 0 {
+				expiresIn = 28800
+			}
+			resp := map[string]any{
 				"access_token": "sk-ant-oat01-new", "refresh_token": "sk-ant-ort01-new",
-				"expires_in": 28800, "scope": f.scope,
+				"expires_in": expiresIn, "scope": f.scope,
 				"account": map[string]string{"email_address": "token@example.com"},
-			})
+			}
+			if f.deadlineIn > 0 {
+				resp["refresh_token_expires_in"] = f.deadlineIn
+			}
+			_ = json.NewEncoder(w).Encode(resp)
 		case "/api/oauth/profile":
 			if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-new" {
 				w.WriteHeader(http.StatusUnauthorized)
@@ -131,6 +146,51 @@ func TestExchangeHappyPath(t *testing.T) {
 	}
 }
 
+// AC1: the sign-in asks for the narrow scope pair and a one-year token at the
+// exchange, and stores the lifetime and deadline Anthropic grants.
+func TestExchangeRequestsLongLivedLogin(t *testing.T) {
+	ctx := context.Background()
+	fake := &fakeAnthropic{orgType: "claude_team", scope: "user:profile user:inference",
+		expiresIn: LoginLifetimeSeconds, deadlineIn: 2352492}
+	fake.serve(t)
+	f := New()
+	_, raw, err := f.Start()
+	if err != nil {
+		t.Fatal(err)
+	}
+	u, _ := url.Parse(raw)
+	if got := u.Query().Get("scope"); got != "user:profile user:inference" {
+		t.Fatalf("scope = %q, want exactly user:profile user:inference", got)
+	}
+
+	id, state, challenge := start(t, f)
+	fake.challenge = challenge
+	tok, err := f.Exchange(ctx, id, "the-code#"+state)
+	if err != nil {
+		t.Fatal(err)
+	}
+	if v, ok := fake.body["expires_in"].(float64); !ok || v != LoginLifetimeSeconds {
+		t.Fatalf("exchange expires_in = %#v, want the number %d", fake.body["expires_in"], LoginLifetimeSeconds)
+	}
+	if d := time.Until(tok.ExpiresAt); d < 364*24*time.Hour || d > 365*24*time.Hour {
+		t.Fatalf("expiry %v from now, want 365 days minus the 5m margin", d)
+	}
+	if d := time.Until(tok.RefreshTokenExpiresAt); d < 27*24*time.Hour || d > 28*24*time.Hour {
+		t.Fatalf("deadline %v from now, want 27.2 days", d)
+	}
+
+	// A shorter grant is stored as granted, and a missing deadline stays unknown.
+	fake.expiresIn, fake.deadlineIn = 28800, 0
+	id, state, challenge = start(t, f)
+	fake.challenge = challenge
+	if tok, err = f.Exchange(ctx, id, "the-code#"+state); err != nil {
+		t.Fatal(err)
+	}
+	if d := time.Until(tok.ExpiresAt); d > 8*time.Hour || !tok.RefreshTokenExpiresAt.IsZero() {
+		t.Fatalf("short grant: expiry in %v, deadline %v", d, tok.RefreshTokenExpiresAt)
+	}
+}
+
 func TestExchangeRateLimitTier(t *testing.T) {
 	for _, tt := range []struct {
 		name    string
````

`internal/webui/claudelogin_test.go`:

````diff
diff --git a/internal/webui/claudelogin_test.go b/internal/webui/claudelogin_test.go
index 4ee1b7f..50ddd6e 100644
--- a/internal/webui/claudelogin_test.go
+++ b/internal/webui/claudelogin_test.go
@@ -120,3 +120,58 @@ func TestClaudeBrowserLogin(t *testing.T) {
 		t.Fatalf("cancelled session = %d %s", rec.Code, rec.Body.String())
 	}
 }
+
+// AC1b: a browser that disconnects once the code is exchanged still gets its
+// credential stored, with the one-year expiry and the login's deadline.
+func TestClaudeBrowserLoginSurvivesDisconnect(t *testing.T) {
+	ctx, cancel := context.WithCancel(context.Background())
+	defer cancel()
+	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
+		switch r.URL.Path {
+		case "/token":
+			_ = json.NewEncoder(w).Encode(map[string]any{
+				"access_token": "sk-ant-oat01-year", "refresh_token": "sk-ant-ort01-year",
+				"expires_in": 31536000, "refresh_token_expires_in": 2352492,
+				"scope": "user:profile user:inference",
+			})
+		case "/profile":
+			cancel() // the browser goes away after the exchange
+			_ = json.NewEncoder(w).Encode(map[string]any{
+				"account":      map[string]string{"email": "owner@example.com"},
+				"organization": map[string]string{"organization_type": "claude_team"},
+			})
+		}
+	}))
+	t.Cleanup(upstream.Close)
+	oldToken, oldProfile := creds.TokenURL, claudeoauth.ProfileURL
+	creds.TokenURL, claudeoauth.ProfileURL = upstream.URL+"/token", upstream.URL+"/profile"
+	t.Cleanup(func() { creds.TokenURL, claudeoauth.ProfileURL = oldToken, oldProfile })
+	db, err := store.Open(filepath.Join(t.TempDir(), "webui.db"))
+	if err != nil {
+		t.Fatal(err)
+	}
+	t.Cleanup(func() { db.Close() })
+	s := &Server{db: db, claudeLogin: claudeoauth.New()}
+
+	rec := httptest.NewRecorder()
+	s.handleCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/credentials/oauth/start", nil), "/oauth/start")
+	var out struct{ Session, URL string }
+	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
+		t.Fatal(err)
+	}
+	u, _ := url.Parse(out.URL)
+	payload, _ := json.Marshal(map[string]any{"session": out.Session, "code": "c#" + u.Query().Get("state")})
+	req := httptest.NewRequest(http.MethodPost, "/api/credentials/oauth/exchange", strings.NewReader(string(payload))).WithContext(ctx)
+	s.handleCredentials(httptest.NewRecorder(), req, "/oauth/exchange")
+
+	list, err := creds.List(context.Background(), db)
+	if err != nil || len(list) != 1 || list[0].AccessToken != "sk-ant-oat01-year" {
+		t.Fatalf("credential not stored after the browser left: %d rows, %v", len(list), err)
+	}
+	if d := time.Until(list[0].ExpiresAt); d < 364*24*time.Hour {
+		t.Fatalf("expiry %v from now, want about a year", d)
+	}
+	if d := time.Until(list[0].RefreshExpiresAt); d < 27*24*time.Hour || d > 28*24*time.Hour {
+		t.Fatalf("deadline %v from now, want 27.2 days", d)
+	}
+}
````

New file `internal/ingest/lifetime_test.go`:

````go
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/claudeoauth"
	"github.com/p4u/claude-proxy/internal/creds"
)

// fakeAnthropic serves the token and profile endpoints and counts calls.
type fakeAnthropic struct {
	tokenCalls, profileCalls atomic.Int32
	deadlineIn               int // refresh_token_expires_in the token endpoint reports; 0 omits it
	profileStatus            int // 0 answers 200
}

func (f *fakeAnthropic) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			f.tokenCalls.Add(1)
			extra := ""
			if f.deadlineIn > 0 {
				extra = fmt.Sprintf(`,"refresh_token_expires_in":%d`, f.deadlineIn)
			}
			_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800` + extra + `}`))
		case "/profile":
			f.profileCalls.Add(1)
			if f.profileStatus != 0 {
				w.WriteHeader(f.profileStatus)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	prevToken, prevProfile := creds.TokenURL, creds.ProfileURL
	creds.TokenURL, creds.ProfileURL = srv.URL+"/token", srv.URL+"/profile"
	t.Cleanup(func() { creds.TokenURL, creds.ProfileURL = prevToken, prevProfile; srv.Close() })
}

// credJSON builds a .credentials.json with the given expiry and deadline
// (zero omits the deadline).
func credJSON(t *testing.T, refresh string, expires, deadline time.Time) []byte {
	t.Helper()
	block := map[string]any{
		"accessToken": "sk-ant-oat-file", "refreshToken": refresh,
		"expiresAt": expires.UnixMilli(), "subscriptionType": "team",
	}
	if !deadline.IsZero() {
		block["refreshTokenExpiresAt"] = deadline.UnixMilli()
	}
	b, err := json.Marshal(map[string]any{"claudeAiOauth": block})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func about(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if want.IsZero() {
		if !got.IsZero() {
			t.Fatalf("%s = %v, want unknown", what, got)
		}
		return
	}
	if d := got.Sub(want); d < -time.Minute || d > time.Minute {
		t.Fatalf("%s = %v, want about %v", what, got, want)
	}
}

// AC3: a pasted 8-hour file stores its deadline, the earlier of the file's
// and the import refresh's, or unknown when the file has none.
func TestImportFileDeadline(t *testing.T) {
	ctx := context.Background()
	day := 24 * time.Hour
	for _, tt := range []struct {
		name       string
		file       time.Duration // 0 = absent
		deadlineIn time.Duration // 0 = absent
		want       time.Duration // 0 = unknown
	}{
		{"file earlier", 10 * day, 20 * day, 10 * day},
		{"refresh earlier", 20 * day, 10 * day, 10 * day},
		{"file only", 10 * day, 0, 10 * day},
		{"absent from the file", 0, 10 * day, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			(&fakeAnthropic{deadlineIn: int(tt.deadlineIn.Seconds())}).serve(t)
			var file, want time.Time
			if tt.file != 0 {
				file = time.Now().Add(tt.file)
			}
			if tt.want != 0 {
				want = time.Now().Add(tt.want)
			}
			c, err := ImportFromJSON(ctx, db, credJSON(t, "ref-"+tt.name, time.Now().Add(time.Hour), file), "", 0)
			if err != nil {
				t.Fatal(err)
			}
			about(t, "deadline", c.RefreshExpiresAt, want)
		})
	}
	t.Run("update replaces the stored deadline", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{}).serve(t)
		old, err := creds.InsertWithRateLimitTier(ctx, db, "old", "team", "sk-ant-oat-old", "ref-old", future(), 5, "", time.Now().Add(2*day))
		if err != nil {
			t.Fatal(err)
		}
		next := time.Now().Add(20 * day)
		c, err := UpdateFromJSON(ctx, db, old.ID, credJSON(t, "ref-new", time.Now().Add(time.Hour), next))
		if err != nil {
			t.Fatal(err)
		}
		about(t, "deadline", c.RefreshExpiresAt, next)
	})
}

// AC3: sign-ins store their deadline and a reconnect replaces the old one.
func TestOAuthDeadline(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	first := time.Now().Add(27 * 24 * time.Hour).Truncate(time.Second)
	c, err := ImportOAuth(ctx, db, &claudeoauth.Tokens{
		AccessToken: "sk-ant-oat-a", RefreshToken: "ref-a", ExpiresAt: time.Now().AddDate(1, 0, 0),
		RefreshTokenExpiresAt: first, SubscriptionType: "team",
	}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := creds.Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(first) {
		t.Fatalf("sign-in deadline = %v, want %v", got.RefreshExpiresAt, first)
	}
	second := first.Add(30 * 24 * time.Hour)
	if _, err := UpdateFromOAuth(ctx, db, c.ID, &claudeoauth.Tokens{
		AccessToken: "sk-ant-oat-b", RefreshToken: "ref-b", ExpiresAt: time.Now().AddDate(1, 0, 0),
		RefreshTokenExpiresAt: second, SubscriptionType: "team",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := creds.Get(ctx, db, c.ID); !got.RefreshExpiresAt.Equal(second) {
		t.Fatalf("reconnect deadline = %v, want the new login's %v", got.RefreshExpiresAt, second)
	}
}

// AC3b (ingest side): a long-lived file is verified against the profile
// endpoint and stored exactly as it is, without a token-endpoint call.
func TestImportLongLivedFileKeepsTokens(t *testing.T) {
	ctx := context.Background()
	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
	deadline := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	t.Run("alive", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{}
		f.serve(t)
		c, err := ImportFromJSON(ctx, db, credJSON(t, "ref-file", expires, deadline), "", 0)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := creds.Get(ctx, db, c.ID)
		if got.AccessToken != "sk-ant-oat-file" || got.RefreshToken != "ref-file" ||
			!got.ExpiresAt.Equal(expires) || !got.RefreshExpiresAt.Equal(deadline) {
			t.Fatalf("stored %+v, want the file's tokens, expiry and deadline", got)
		}
		if _, err := UpdateFromJSON(ctx, db, c.ID, credJSON(t, "ref-file", expires, deadline)); err != nil {
			t.Fatal(err)
		}
		if f.tokenCalls.Load() != 0 || f.profileCalls.Load() != 2 {
			t.Fatalf("token calls %d, profile calls %d; want 0 and 2", f.tokenCalls.Load(), f.profileCalls.Load())
		}
	})
	t.Run("rejected", func(t *testing.T) {
		db := testDB(t)
		f := &fakeAnthropic{profileStatus: http.StatusUnauthorized}
		f.serve(t)
		_, err := ImportFromJSON(ctx, db, credJSON(t, "ref-file", expires, deadline), "", 0)
		if err == nil || !strings.Contains(err.Error(), "credential is not alive") || f.tokenCalls.Load() != 0 {
			t.Fatalf("err = %v, token calls %d", err, f.tokenCalls.Load())
		}
	})
	t.Run("profile unavailable", func(t *testing.T) {
		db := testDB(t)
		(&fakeAnthropic{profileStatus: http.StatusServiceUnavailable}).serve(t)
		_, err := ImportFromJSON(ctx, db, credJSON(t, "ref-file", expires, deadline), "", 0)
		if err == nil || strings.Contains(err.Error(), "not alive") || !strings.Contains(err.Error(), "503") {
			t.Fatalf("err = %v, want the profile check's own error", err)
		}
		if list, _ := creds.List(ctx, db); len(list) != 0 {
			t.Fatal("a failed check stored the credential")
		}
	})
}
````

New file `cmd/claude-proxy/creds_test.go`:

````go
package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

// fakeAnthropicCLI serves the token and profile endpoints for CLI tests.
func fakeAnthropicCLI(t *testing.T) (tokenCalls, profileStatus *atomic.Int32) {
	t.Helper()
	tokenCalls, profileStatus = new(atomic.Int32), new(atomic.Int32)
	profileStatus.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800}`))
		case "/profile":
			w.WriteHeader(int(profileStatus.Load()))
		}
	}))
	prevToken, prevProfile := creds.TokenURL, creds.ProfileURL
	creds.TokenURL, creds.ProfileURL = srv.URL+"/token", srv.URL+"/profile"
	t.Cleanup(func() { creds.TokenURL, creds.ProfileURL = prevToken, prevProfile; srv.Close() })
	return tokenCalls, profileStatus
}

// AC3b: a backup of a year-long login restores without a token-endpoint call
// and keeps its tokens, expiry and deadline; a dead token fails the restore.
func TestCredsExportImportBulkKeepsLongLivedLogin(t *testing.T) {
	ctx := context.Background()
	tokenCalls, profileStatus := fakeAnthropicCLI(t)
	_, src := cliDB(t)
	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
	deadline := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	orig, err := creds.InsertWithRateLimitTier(ctx, src, "team-a", "team", "sk-ant-oat-year", "ref-year", expires, 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err := exportCreds(ctx, src, &backup); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backup.String(), `"refreshTokenExpiresAt":`) {
		t.Fatalf("export lacks the deadline: %s", backup.String())
	}

	_, dst := cliDB(t)
	if n, err := importBulk(ctx, dst, bytes.NewReader(backup.Bytes()), io.Discard); err != nil || n != 1 {
		t.Fatalf("import-bulk: %d, %v", n, err)
	}
	list, _ := creds.List(ctx, dst)
	if len(list) != 1 {
		t.Fatalf("restored %d credentials", len(list))
	}
	got := list[0]
	if got.AccessToken != orig.AccessToken || got.RefreshToken != orig.RefreshToken ||
		!got.ExpiresAt.Equal(expires) || !got.RefreshExpiresAt.Equal(deadline) ||
		got.Label != "team-a" || got.Weight != 5 {
		t.Fatalf("restored %+v, want the exported login unchanged", got)
	}
	if n := tokenCalls.Load(); n != 0 {
		t.Fatalf("restore made %d token-endpoint calls", n)
	}

	profileStatus.Store(http.StatusUnauthorized)
	_, dead := cliDB(t)
	if _, err := importBulk(ctx, dead, bytes.NewReader(backup.Bytes()), io.Discard); err == nil || !strings.Contains(err.Error(), "credential is not alive") {
		t.Fatalf("dead token restore = %v, want credential is not alive", err)
	}
}
````

Run: `go test -race -count=1 ./internal/claudeoauth/`
Expected: `internal/claudeoauth/claudeoauth_test.go:154:14: undefined: LoginLifetimeSeconds` and `FAIL github.com/p4u/claude-proxy/internal/claudeoauth [build failed]`

Run: `go test -race -count=1 -run TestClaudeBrowserLoginSurvivesDisconnect ./internal/webui/`
Expected: `--- FAIL: TestClaudeBrowserLoginSurvivesDisconnect`, with `claudelogin_test.go:169: credential not stored after the browser left: 0 rows, <nil>`

Run: `go test -race -count=1 ./internal/ingest/`
Expected: `internal/ingest/lifetime_test.go:137:3: unknown field RefreshTokenExpiresAt in struct literal of type claudeoauth.Tokens` and `[build failed]`

Run: `go test -race -count=1 ./cmd/claude-proxy/`
Expected: `cmd/claude-proxy/creds_test.go:50:12: undefined: exportCreds` and `[build failed]`

Step 2: implement

`internal/claudeoauth/claudeoauth.go`:

````diff
diff --git a/internal/claudeoauth/claudeoauth.go b/internal/claudeoauth/claudeoauth.go
index 66fd7c8..2f44cdf 100644
--- a/internal/claudeoauth/claudeoauth.go
+++ b/internal/claudeoauth/claudeoauth.go
@@ -31,22 +31,24 @@ import (
 )
 
 // The values below mirror Claude Code's production OAuth configuration
-// (`CLAUDE_AI_AUTHORIZE_URL`, `MANUAL_REDIRECT_URL` and the subscription login
-// scope set). The redirect URI must be the manual one: it is the only
-// registered redirect that is not a localhost port, and Anthropic's page there
-// displays the code for the user to copy.
+// (`CLAUDE_AI_AUTHORIZE_URL`, `MANUAL_REDIRECT_URL`). The redirect URI must be
+// the manual one: it is the only registered redirect that is not a localhost
+// port, and Anthropic's page there displays the code for the user to copy.
 const (
 	AuthorizeURL = "https://claude.com/cai/oauth/authorize"
 	RedirectURL  = "https://platform.claude.com/oauth/code/callback"
 )
 
-// Scopes is the scope set Claude Code requests for a claude.ai subscription
-// login, so a token minted here is interchangeable with one copied out of
-// .credentials.json — the refresher keeps the same grant either way.
-var Scopes = []string{
-	"org:create_api_key", "user:profile", "user:inference",
-	"user:sessions:claude_code", "user:mcp_servers", "user:file_upload", "user:plugins",
-}
+// Scopes deliberately no longer mirror Claude Code's login set. The proxy
+// holds the narrowest grant that serves its traffic: user:inference for
+// /v1/messages and /v1/models, user:profile for plan detection and the usage
+// API. The sessions, MCP, file and plugin scopes are not needed.
+var Scopes = []string{"user:profile", "user:inference"}
+
+// LoginLifetimeSeconds is the access-token lifetime requested at the code
+// exchange (one year). Anthropic honours it only there, never on a refresh,
+// so a login minted here must not be refreshed while its token works.
+const LoginLifetimeSeconds = 31536000
 
 // ProfileURL is a var so tests can point it at a fake server. The token
 // endpoint is shared with the refresher (creds.TokenURL).
@@ -65,6 +67,10 @@ type Tokens struct {
 	SubscriptionType string // "max", "pro", "team", "enterprise"
 	RateLimitTier    string // profile metadata; empty when not published
 	Email            string
+
+	// RefreshTokenExpiresAt is the login's refresh deadline, from the
+	// exchange's refresh_token_expires_in; zero when not reported.
+	RefreshTokenExpiresAt time.Time
 }
 
 type pending struct {
@@ -188,14 +194,21 @@ func (f *Flow) Exchange(ctx context.Context, id, pasted string) (*Tokens, error)
 	if email == "" {
 		email = tok.Account.Email
 	}
+	now := f.now()
+	var deadline time.Time
+	if tok.RefreshTokenExpiresIn > 0 {
+		deadline = now.Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second)
+	}
 	return &Tokens{
 		AccessToken:  tok.AccessToken,
 		RefreshToken: tok.RefreshToken,
-		// Same safety margin the refresher applies to every expiry it stores.
-		ExpiresAt:        f.now().Add(time.Duration(tok.ExpiresIn)*time.Second - 5*time.Minute),
-		SubscriptionType: sub,
-		RateLimitTier:    tier,
-		Email:            email,
+		// The lifetime Anthropic granted, which may be shorter than requested,
+		// with the safety margin the refresher applies to every expiry.
+		ExpiresAt:             now.Add(time.Duration(tok.ExpiresIn)*time.Second - 5*time.Minute),
+		RefreshTokenExpiresAt: deadline,
+		SubscriptionType:      sub,
+		RateLimitTier:         tier,
+		Email:                 email,
 	}, nil
 }
 
@@ -207,16 +220,20 @@ type tokenResp struct {
 	Account      struct {
 		Email string `json:"email_address"`
 	} `json:"account"`
+
+	// The login's refresh deadline in seconds; a refresh never moves it.
+	RefreshTokenExpiresIn int `json:"refresh_token_expires_in"`
 }
 
 func (f *Flow) exchange(ctx context.Context, code string, p pending) (*tokenResp, error) {
-	body, _ := json.Marshal(map[string]string{
+	body, _ := json.Marshal(map[string]any{
 		"grant_type":    "authorization_code",
 		"code":          code,
 		"redirect_uri":  RedirectURL,
 		"client_id":     creds.ClientID,
 		"code_verifier": p.verifier,
 		"state":         p.state,
+		"expires_in":    LoginLifetimeSeconds,
 	})
 	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.TokenURL, bytes.NewReader(body))
 	if err != nil {
````

`internal/webui/claudelogin.go`:

````diff
diff --git a/internal/webui/claudelogin.go b/internal/webui/claudelogin.go
index 50c06cc..eecbbb8 100644
--- a/internal/webui/claudelogin.go
+++ b/internal/webui/claudelogin.go
@@ -1,6 +1,7 @@
 package webui
 
 import (
+	"context"
 	"net/http"
 	"strings"
 
@@ -61,7 +62,9 @@ func (s *Server) exchangeClaudeLogin(w http.ResponseWriter, r *http.Request) {
 		writeErr(w, http.StatusConflict, "sidecar gateways are managed through their accounts panels")
 		return
 	}
-	ctx := r.Context()
+	// The code is single-use and each login needs a human approval, so once
+	// the exchange starts a browser disconnect must not discard its result.
+	ctx := context.WithoutCancel(r.Context())
 	if target != "" {
 		// Fail before consuming the single-use code on a typo'd ID.
 		if _, err := creds.Get(ctx, s.db, target); err != nil {
````

`internal/ingest/import.go`:

````diff
diff --git a/internal/ingest/import.go b/internal/ingest/import.go
index 8685fa7..3676b6b 100644
--- a/internal/ingest/import.go
+++ b/internal/ingest/import.go
@@ -3,6 +3,7 @@ package ingest
 import (
 	"context"
 	"encoding/json"
+	"errors"
 	"fmt"
 	"os"
 	"time"
@@ -20,6 +21,18 @@ type oauthBlock struct {
 	Scopes           []string `json:"scopes"`
 	SubscriptionType string   `json:"subscriptionType"`
 	RateLimitTier    string   `json:"rateLimitTier"`
+
+	// RefreshTokenExpiresAt is the login's refresh deadline in milliseconds,
+	// as Claude Code records it; 0 or absent means unknown.
+	RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt,omitempty"`
+}
+
+// deadline returns the file's refresh deadline, zero when unknown.
+func (o oauthBlock) deadline() time.Time {
+	if o.RefreshTokenExpiresAt <= 0 {
+		return time.Time{}
+	}
+	return time.UnixMilli(o.RefreshTokenExpiresAt)
 }
 
 type credFile struct {
@@ -67,10 +80,36 @@ func parseCredFile(path string) (oauthBlock, error) {
 	return parseCredBytes(raw, path)
 }
 
+// verifyFile proves a file's login is alive and returns the tokens to store.
+// A long-lived login (access token valid for more than a day, which an 8-hour
+// token never is) is checked against the profile endpoint and kept as it is:
+// a refresh would replace its year-long token with an 8-hour one for good.
+// Any other file is refreshed, which also yields an authoritative expiry.
+func verifyFile(ctx context.Context, o oauthBlock) (creds.TokenSet, error) {
+	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
+	expires := time.UnixMilli(o.ExpiresAt)
+	if time.Until(expires) > creds.LongLived {
+		if err := creds.CheckToken(ctx, o.AccessToken); err != nil {
+			if errors.Is(err, creds.ErrTokenRejected) {
+				return creds.TokenSet{}, fmt.Errorf("credential is not alive: %w", err)
+			}
+			return creds.TokenSet{}, err
+		}
+		return creds.TokenSet{
+			AccessToken: o.AccessToken, RefreshToken: o.RefreshToken,
+			ExpiresAt: expires, RefreshExpiresAt: o.deadline(),
+		}, nil
+	}
+	ts, err := creds.RefreshTokens(ctx, o.RefreshToken, o.deadline())
+	if err != nil {
+		return creds.TokenSet{}, fmt.Errorf("credential is not alive: %w", err)
+	}
+	return ts, nil
+}
+
 // insertVerified runs the shared liveness-check + insert path used by Import and
-// ImportFromJSON: it refreshes the token to confirm the credential is alive
-// (yielding a fresh access token + authoritative expiry), rejects duplicates,
-// and inserts. label defaults to the subscription type when empty.
+// ImportFromJSON: it verifies the login (verifyFile), rejects duplicates, and
+// inserts. label defaults to the subscription type when empty.
 func insertVerified(ctx context.Context, db *store.DB, o oauthBlock, label string, weight int) (*creds.Credential, error) {
 	dup, err := creds.HasRefreshToken(ctx, db, o.RefreshToken)
 	if err != nil {
@@ -80,10 +119,9 @@ func insertVerified(ctx context.Context, db *store.DB, o oauthBlock, label strin
 		return nil, fmt.Errorf("credential already imported (refresh token already exists in the pool)")
 	}
 
-	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
-	ts, err := creds.RefreshTokens(ctx, o.RefreshToken, time.Time{})
+	ts, err := verifyFile(ctx, o)
 	if err != nil {
-		return nil, fmt.Errorf("credential is not alive: %w", err)
+		return nil, err
 	}
 
 	if label == "" {
@@ -115,9 +153,9 @@ func ImportFromJSON(ctx context.Context, db *store.DB, raw []byte, label string,
 
 // UpdateFromFile re-points an existing credential at a freshly re-logged-in
 // .credentials.json — used when a subscription's OAuth lineage was reset and
-// the stored refresh token no longer works. It verifies the new token is alive
-// (which also yields a fresh access token + authoritative expiry) and writes it
-// over the existing credential's tokens, healing its status back to active.
+// the stored refresh token no longer works. It verifies the new login is alive
+// (verifyFile) and writes it over the existing credential's tokens, healing its
+// status back to active.
 //
 // Unlike Import it does not run the duplicate-refresh-token check: a credential
 // legitimately re-uses its own lineage, and the file may even carry the same
@@ -146,10 +184,9 @@ func updateVerified(ctx context.Context, db *store.DB, id string, o oauthBlock)
 	if _, err := creds.Get(ctx, db, id); err != nil {
 		return nil, err
 	}
-	fmt.Fprintf(os.Stderr, "verifying credential with Anthropic...\n")
-	ts, err := creds.RefreshTokens(ctx, o.RefreshToken, time.Time{})
+	ts, err := verifyFile(ctx, o)
 	if err != nil {
-		return nil, fmt.Errorf("credential is not alive: %w", err)
+		return nil, err
 	}
 	if err := creds.UpdateTokensWithSubscription(ctx, db, id, ts.AccessToken, ts.RefreshToken, ts.ExpiresAt, o.SubscriptionType, o.RateLimitTier, ts.RefreshExpiresAt); err != nil {
 		return nil, err
@@ -176,7 +213,7 @@ func ImportOAuth(ctx context.Context, db *store.DB, t *claudeoauth.Tokens, label
 	if label == "" {
 		label = t.SubscriptionType
 	}
-	return creds.InsertWithRateLimitTier(ctx, db, label, t.SubscriptionType, t.AccessToken, t.RefreshToken, t.ExpiresAt, weight, t.RateLimitTier, time.Time{})
+	return creds.InsertWithRateLimitTier(ctx, db, label, t.SubscriptionType, t.AccessToken, t.RefreshToken, t.ExpiresAt, weight, t.RateLimitTier, t.RefreshTokenExpiresAt)
 }
 
 // UpdateFromOAuth re-points an existing credential at a fresh OAuth sign-in,
@@ -189,7 +226,7 @@ func UpdateFromOAuth(ctx context.Context, db *store.DB, id string, t *claudeoaut
 	if c.Provider != provider.Anthropic {
 		return nil, fmt.Errorf("%s is not an Anthropic subscription", id)
 	}
-	if err := creds.UpdateTokensWithSubscription(ctx, db, id, t.AccessToken, t.RefreshToken, t.ExpiresAt, t.SubscriptionType, t.RateLimitTier, time.Time{}); err != nil {
+	if err := creds.UpdateTokensWithSubscription(ctx, db, id, t.AccessToken, t.RefreshToken, t.ExpiresAt, t.SubscriptionType, t.RateLimitTier, t.RefreshTokenExpiresAt); err != nil {
 		return nil, err
 	}
 	return creds.Get(ctx, db, id)
````

`cmd/claude-proxy/main.go`:

````diff
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index 95d773d..93e89b7 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -784,6 +784,9 @@ type exportLine struct {
 		Scopes           []string `json:"scopes"`
 		SubscriptionType string   `json:"subscriptionType"`
 		RateLimitTier    string   `json:"rateLimitTier"`
+
+		// RefreshTokenExpiresAt is the stored login deadline (ms), omitted when unknown.
+		RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt,omitempty"`
 	} `json:"claudeAiOauth"`
 }
 
@@ -797,13 +800,19 @@ func credsExport(ctx context.Context, args []string) {
 	_ = fs.Parse(args)
 	db := openDB(*dbPath)
 	defer db.Close()
+	if err := exportCreds(ctx, db, os.Stdout); err != nil {
+		fmt.Fprintf(os.Stderr, "export: %v\n", err)
+		os.Exit(1)
+	}
+}
 
+// exportCreds writes every credential to w as one exportLine per line.
+func exportCreds(ctx context.Context, db *store.DB, w io.Writer) error {
 	list, err := creds.List(ctx, db)
 	if err != nil {
-		fmt.Fprintf(os.Stderr, "export: %v\n", err)
-		os.Exit(1)
+		return err
 	}
-	enc := json.NewEncoder(os.Stdout)
+	enc := json.NewEncoder(w)
 	for _, c := range list {
 		var line exportLine
 		line.Label = c.Label
@@ -811,14 +820,17 @@ func credsExport(ctx context.Context, args []string) {
 		line.ClaudeAiOauth.AccessToken = c.AccessToken
 		line.ClaudeAiOauth.RefreshToken = c.RefreshToken
 		line.ClaudeAiOauth.ExpiresAt = c.ExpiresAt.UnixMilli()
+		if !c.RefreshExpiresAt.IsZero() {
+			line.ClaudeAiOauth.RefreshTokenExpiresAt = c.RefreshExpiresAt.UnixMilli()
+		}
 		line.ClaudeAiOauth.Scopes = []string{"user:inference", "user:profile"}
 		line.ClaudeAiOauth.SubscriptionType = c.SubscriptionType
 		line.ClaudeAiOauth.RateLimitTier = c.RateLimitTier
 		if err := enc.Encode(line); err != nil {
-			fmt.Fprintf(os.Stderr, "export: encode %s: %v\n", c.ID, err)
-			os.Exit(1)
+			return fmt.Errorf("encode %s: %w", c.ID, err)
 		}
 	}
+	return nil
 }
 
 // credsImportBulk reads JSONL from stdin (produced by creds export) and
@@ -831,8 +843,18 @@ func credsImportBulk(ctx context.Context, args []string) {
 	_ = fs.Parse(args)
 	db := openDB(*dbPath)
 	defer db.Close()
+	n, err := importBulk(ctx, db, os.Stdin, os.Stdout)
+	if err != nil {
+		fmt.Fprintf(os.Stderr, "import-bulk: %v\n", err)
+		os.Exit(1)
+	}
+	fmt.Printf("imported %d credential(s)\n", n)
+}
 
-	scanner := bufio.NewScanner(os.Stdin)
+// importBulk imports the JSONL exportCreds writes, stopping at the first line
+// that fails, and returns how many credentials it imported.
+func importBulk(ctx context.Context, db *store.DB, in io.Reader, out io.Writer) (int, error) {
+	scanner := bufio.NewScanner(in)
 	scanner.Buffer(make([]byte, 4<<20), 4<<20)
 	n := 0
 	for scanner.Scan() {
@@ -842,14 +864,12 @@ func credsImportBulk(ctx context.Context, args []string) {
 		}
 		var el exportLine
 		if err := json.Unmarshal([]byte(line), &el); err != nil {
-			fmt.Fprintf(os.Stderr, "import-bulk: parse line %d: %v\n", n+1, err)
-			os.Exit(1)
+			return n, fmt.Errorf("parse line %d: %w", n+1, err)
 		}
 		// Write to a temp file so ingest.Import can read it.
 		tmp, err := os.CreateTemp("", "claude-proxy-import-*.json")
 		if err != nil {
-			fmt.Fprintf(os.Stderr, "import-bulk: temp file: %v\n", err)
-			os.Exit(1)
+			return n, fmt.Errorf("temp file: %w", err)
 		}
 		// Re-encode only the claudeAiOauth wrapper that ingest expects.
 		type credFile struct {
@@ -858,26 +878,23 @@ func credsImportBulk(ctx context.Context, args []string) {
 		if err := json.NewEncoder(tmp).Encode(credFile{ClaudeAiOauth: el.ClaudeAiOauth}); err != nil {
 			tmp.Close()
 			_ = os.Remove(tmp.Name())
-			fmt.Fprintf(os.Stderr, "import-bulk: write temp: %v\n", err)
-			os.Exit(1)
+			return n, fmt.Errorf("write temp: %w", err)
 		}
 		tmp.Close()
 
 		c, err := ingest.Import(ctx, db, tmp.Name(), el.Label, el.Weight)
 		_ = os.Remove(tmp.Name())
 		if err != nil {
-			fmt.Fprintf(os.Stderr, "import-bulk: import %q: %v\n", el.Label, err)
-			os.Exit(1)
+			return n, fmt.Errorf("import %q: %w", el.Label, err)
 		}
-		fmt.Printf("imported %s  label=%q  sub=%s  weight=%d  expires=%s\n",
+		fmt.Fprintf(out, "imported %s  label=%q  sub=%s  weight=%d  expires=%s\n",
 			c.ID, c.Label, c.SubscriptionType, c.Weight, c.ExpiresAt.Format(time.RFC3339))
 		n++
 	}
 	if err := scanner.Err(); err != nil {
-		fmt.Fprintf(os.Stderr, "import-bulk: read stdin: %v\n", err)
-		os.Exit(1)
+		return n, fmt.Errorf("read stdin: %w", err)
 	}
-	fmt.Printf("imported %d credential(s)\n", n)
+	return n, nil
 }
 
 // credsUsage fetches live usage from Anthropic for each credential (or one).
````

Run: `go test -race -count=1 -v -run '^(TestExchangeRequestsLongLivedLogin|TestExchangeHappyPath|TestClaudeBrowserLoginSurvivesDisconnect|TestClaudeBrowserLogin|TestImportFileDeadline|TestOAuthDeadline|TestImportLongLivedFileKeepsTokens|TestImportSuccess|TestPasteImport|TestCredsExportImportBulkKeepsLongLivedLogin)$' ./internal/claudeoauth/ ./internal/webui/ ./internal/ingest/ ./internal/tui/ ./cmd/claude-proxy/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: 10 `--- PASS` lines (`TestExchangeHappyPath`, `TestExchangeRequestsLongLivedLogin`, `TestClaudeBrowserLogin`, `TestClaudeBrowserLoginSurvivesDisconnect`, `TestImportSuccess`, `TestImportFileDeadline`, `TestOAuthDeadline`, `TestImportLongLivedFileKeepsTokens`, `TestPasteImport`, `TestCredsExportImportBulkKeepsLongLivedLogin`) and 5 `ok` lines (`internal/claudeoauth`, `internal/webui`, `internal/ingest`, `internal/tui`, `cmd/claude-proxy`)

Run: `go vet ./... && go test -race -count=1 ./... 2>&1 | grep -c '^ok'`
Expected: `18` (and no `FAIL` line)

Step 3: commit
`git add internal/ingest/lifetime_test.go cmd/claude-proxy/creds_test.go && git commit -m "feat(claudeoauth): year-long sign-in, and imports that keep long-lived logins" -- internal/claudeoauth/claudeoauth.go internal/claudeoauth/claudeoauth_test.go internal/webui/claudelogin.go internal/webui/claudelogin_test.go internal/ingest/import.go internal/ingest/import_test.go internal/ingest/lifetime_test.go internal/tui/tui_test.go cmd/claude-proxy/main.go cmd/claude-proxy/creds_test.go`

Interfaces this task provides to later tasks:
- `claudeoauth.Scopes = []string{"user:profile", "user:inference"}`;
  `const claudeoauth.LoginLifetimeSeconds = 31536000`.
- `claudeoauth.Tokens.RefreshTokenExpiresAt time.Time` (zero = not reported).
- `func exportCreds(ctx context.Context, db *store.DB, w io.Writer) error` and
  `func importBulk(ctx context.Context, db *store.DB, in io.Reader, out io.Writer) (int, error)`
  in `cmd/claude-proxy`; `exportLine.ClaudeAiOauth.RefreshTokenExpiresAt int64` (ms, omitempty).
- `fakeAnthropicCLI(t) (tokenCalls, profileStatus *atomic.Int32)` test helper in
  `cmd/claude-proxy/creds_test.go`, reused by Task 5.
- `ingest.oauthBlock.RefreshTokenExpiresAt int64` (ms); `verifyFile(ctx, o) (creds.TokenSet, error)`.

### Task 4: Refresh on a 401 only after the token is confirmed dead
Model: opus   Repo: /home/p4u/claude-proxy   Files: `internal/proxy/proxy.go`,
`internal/proxy/proxy_test.go`, `internal/proxy/refresh401_test.go` (new)
Goal: AC6 (proxy side) and AC7b (proxy side). D3a in the proxy: `RefreshRejected` replaces
`RefreshNow`, `ErrTokenAlive` relays upstream's 401 untouched, the fixed 502 bodies, no
`expired`. The background catalogue fetch (`models1m.go:772`) takes the same branch and needs no
code change.

Notes:
- `TestForward401TriggersRefresh` is removed: `TestUpstream401OnDeadTokenRefreshesAndRetries`
  covers it with the liveness check, and the old test never restored `creds.TokenURL`.
- On `ErrTokenAlive` the branch rebuilds `resp.Body` from the bytes it read (up to
  `maxBodyBytes`) and falls through, so the normal relay copies status, headers and body and runs
  `MarkError`. The refresher already logged revocations (ERROR) and transient failures (WARN), so
  the proxy adds no line of its own there.

Step 1: write the failing test

New file `internal/proxy/refresh401_test.go`:

````go
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/store"
)

const upstream401Body = `{"type":"error","error":{"type":"authentication_error","message":"OAuth authentication is currently not supported."}}`

// fake401 is one server playing the upstream, the token endpoint and the
// profile endpoint. The upstream answers 401 to any token but the refreshed
// one. Configure it before the first request.
type fake401 struct {
	tokenCalls    atomic.Int32
	upstreamHits  atomic.Int32
	profileStatus int    // profile endpoint status
	tokenStatus   int    // 0 answers 200 with sk-ant-oat-refreshed
	onStale       func() // runs before a stale-token 401
}

// logLines is a goroutine-safe log sink.
type logLines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// count returns how many lines are at level and carry label=label.
func (l *logLines) count(level, label string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(l.b.String(), "\n") {
		f := " " + line + " "
		if strings.Contains(f, " level="+level+" ") && strings.Contains(f, " label="+label+" ") {
			n++
		}
	}
	return n
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// setup401 builds a proxy with one Anthropic credential, "acct-a", whose
// token expires in an hour, so a 401 on it goes through the liveness check.
func setup401(t *testing.T, f *fake401) (*Handler, *creds.Credential, *store.DB, *logLines) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			f.tokenCalls.Add(1)
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"sk-ant-oat-refreshed","refresh_token":"rt-new","expires_in":28800}`)
		case "/api/oauth/profile":
			w.WriteHeader(f.profileStatus)
		default:
			f.upstreamHits.Add(1)
			if r.Header.Get("Authorization") == "Bearer sk-ant-oat-refreshed" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
				return
			}
			if f.onStale != nil {
				f.onStale()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Upstream-Marker", "relayed")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, upstream401Body)
		}
	}))
	t.Cleanup(ts.Close)
	prevToken, prevProfile := creds.TokenURL, creds.ProfileURL
	creds.SetTokenURL(ts.URL + "/v1/oauth/token")
	creds.ProfileURL = ts.URL + "/api/oauth/profile"
	t.Cleanup(func() { creds.SetTokenURL(prevToken); creds.ProfileURL = prevProfile })

	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := creds.Insert(context.Background(), db, "acct-a", "max", "sk-ant-oat-stale", "rt-A", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logLines{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := creds.NewRefresher(db)
	r.SetLogger(logger)
	h := New(db, pool.New(db), r, logger)
	withUpstream(h, ts)
	return h, c, db, logs
}

func send401(h *Handler) *httptest.ResponseRecorder {
	body := `{"model":"claude-x","metadata":{"user_id":"sess-401"},"messages":[{"role":"user","content":"hi"}]}`
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	return rw
}

// AC6: a 401 on a token the profile endpoint accepts is relayed as it came,
// with no refresh and no change to the credential.
func TestUpstream401OnLiveTokenIsRelayed(t *testing.T) {
	f := &fake401{profileStatus: http.StatusOK}
	h, c, db, logs := setup401(t, f)
	rw := send401(h)
	if rw.Code != http.StatusUnauthorized || rw.Body.String() != upstream401Body || rw.Header().Get("X-Upstream-Marker") != "relayed" {
		t.Fatalf("client got %d %q headers=%v, want upstream's 401", rw.Code, rw.Body.String(), rw.Header())
	}
	got, _ := creds.Get(context.Background(), db, c.ID)
	if f.tokenCalls.Load() != 0 || got.Status != creds.StatusActive || got.AccessToken != c.AccessToken ||
		!got.ExpiresAt.Equal(c.ExpiresAt.Truncate(time.Second)) || got.ErrorCount != 1 {
		t.Fatalf("token calls %d, stored %+v", f.tokenCalls.Load(), got)
	}
	if f.upstreamHits.Load() != 1 || logs.count("WARN", "acct-a") != 1 || !strings.Contains(logs.String(), "path=/v1/messages") {
		t.Fatalf("upstream hits %d, logs:\n%s", f.upstreamHits.Load(), logs)
	}
}

// AC6: a 401 the profile endpoint confirms refreshes once and retries once.
func TestUpstream401OnDeadTokenRefreshesAndRetries(t *testing.T) {
	f := &fake401{profileStatus: http.StatusUnauthorized}
	h, c, db, _ := setup401(t, f)
	if rw := send401(h); rw.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want the retried 200", rw.Code, rw.Body.String())
	}
	if f.upstreamHits.Load() != 2 || f.tokenCalls.Load() != 1 {
		t.Fatalf("upstream hits %d, token calls %d; want 2 and 1", f.upstreamHits.Load(), f.tokenCalls.Load())
	}
	if got, _ := creds.Get(context.Background(), db, c.ID); got.AccessToken != "sk-ant-oat-refreshed" {
		t.Fatalf("token not rotated: %s", got.AccessToken)
	}
}

// AC6: two requests rejected on the same token cost one refresh.
func TestConcurrentUpstream401sRefreshOnce(t *testing.T) {
	var arrived atomic.Int32
	both := make(chan struct{})
	f := &fake401{profileStatus: http.StatusUnauthorized, onStale: func() {
		if arrived.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
		}
	}}
	h, _, _, _ := setup401(t, f)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { codes <- send401(h).Code })
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
	}
	if arrived.Load() != 2 || f.tokenCalls.Load() != 1 {
		t.Fatalf("stale 401s %d, token calls %d; want 2 and 1", arrived.Load(), f.tokenCalls.Load())
	}
}

// AC6: a transient refresh failure is a 502 that leaves the status alone, and
// the next 401 within 30 s does not call the token endpoint again.
func TestUpstream401TransientRefreshFailure(t *testing.T) {
	f := &fake401{profileStatus: http.StatusUnauthorized, tokenStatus: http.StatusInternalServerError}
	h, c, db, _ := setup401(t, f)
	for range 2 {
		rw := send401(h)
		if rw.Code != http.StatusBadGateway || strings.TrimSpace(rw.Body.String()) != "proxy: token refresh failed; retry" {
			t.Fatalf("got %d %q", rw.Code, rw.Body.String())
		}
	}
	got, _ := creds.Get(context.Background(), db, c.ID)
	if got.Status != creds.StatusActive || got.RetryAfter == nil || time.Until(*got.RetryAfter) < 50*time.Second {
		t.Fatalf("status %q retry_after %v, want active and about a minute ahead", got.Status, got.RetryAfter)
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token calls %d, want 1", n)
	}
}

// AC6 and AC7b: a rejected login is revoked (never expired), answered with
// the fixed 502 and logged as one ERROR with the label.
func TestUpstream401RevokedLogin(t *testing.T) {
	f := &fake401{profileStatus: http.StatusUnauthorized, tokenStatus: http.StatusBadRequest}
	h, c, db, logs := setup401(t, f)
	rw := send401(h)
	if rw.Code != http.StatusBadGateway || strings.TrimSpace(rw.Body.String()) != "proxy: credential revoked by Anthropic; sign in again" {
		t.Fatalf("got %d %q", rw.Code, rw.Body.String())
	}
	if got, _ := creds.Get(context.Background(), db, c.ID); got.Status != creds.StatusRevoked {
		t.Fatalf("status %q, want revoked", got.Status)
	}
	if n := logs.count("ERROR", "acct-a"); n != 1 {
		t.Fatalf("%d ERROR lines with the label, want 1:\n%s", n, logs)
	}
}

// AC6: the background catalogue fetch takes the same branch.
func TestCatalogue401OnLiveTokenDoesNotRefresh(t *testing.T) {
	f := &fake401{profileStatus: http.StatusOK}
	h, c, db, _ := setup401(t, f)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	_, _ = io.Copy(io.Discard, rw.Result().Body)
	got, _ := creds.Get(context.Background(), db, c.ID)
	if f.tokenCalls.Load() != 0 || got.AccessToken != c.AccessToken || got.Status != creds.StatusActive {
		t.Fatalf("token calls %d, stored %+v", f.tokenCalls.Load(), got)
	}
}
````

`internal/proxy/proxy_test.go`:

````diff
diff --git a/internal/proxy/proxy_test.go b/internal/proxy/proxy_test.go
index 8abe992..83fb1f0 100644
--- a/internal/proxy/proxy_test.go
+++ b/internal/proxy/proxy_test.go
@@ -201,65 +201,6 @@ func TestForward429MarksLimited(t *testing.T) {
 	}
 }
 
-func TestForward401TriggersRefresh(t *testing.T) {
-	var hits int32
-	// Refresher needs a fake token endpoint too. Build a single mux that handles both.
-	mux := http.NewServeMux()
-	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
-		n := atomic.AddInt32(&hits, 1)
-		if n == 1 {
-			w.WriteHeader(401)
-			fmt.Fprintln(w, `{"error":"expired"}`)
-			return
-		}
-		w.WriteHeader(200)
-		fmt.Fprintln(w, `{"ok":true}`)
-	})
-	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
-		w.Header().Set("Content-Type", "application/json")
-		fmt.Fprintln(w, `{"access_token":"sk-ant-oat-refreshed","refresh_token":"rt-new","expires_in":3600}`)
-	})
-	ts := httptest.NewServer(mux)
-	defer ts.Close()
-
-	dir := t.TempDir()
-	db, _ := store.Open(filepath.Join(dir, "t.db"))
-	defer db.Close()
-	ctx := context.Background()
-	c, _ := creds.Insert(ctx, db, "A", "max", "sk-ant-oat-stale", "rt-A", time.Now().Add(time.Hour), 0)
-
-	// Patch refresher's token URL via direct HTTP override: simplest is to
-	// route a custom client. We'll point a custom refresher at the mux by
-	// rewriting URLs the same way as the proxy.
-	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
-	tsURL, _ := url.Parse(ts.URL)
-	r := creds.NewRefresher(db)
-	// replace internal client via patch helper added below
-	creds.SetTokenClient(r, &http.Client{Transport: &rewriter{base: tsURL, rt: ts.Client().Transport}})
-	creds.SetTokenURL("http://placeholder-host.invalid/v1/oauth/token") // host gets rewritten
-
-	h := New(db, pool.New(db), r, logger)
-	withUpstream(h, ts)
-
-	body := []byte(`{"metadata":{"user_id":"sess-401"},"messages":[{"role":"user","content":"hi"}]}`)
-	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
-	rw := httptest.NewRecorder()
-	h.ServeHTTP(rw, req)
-
-	if rw.Code != 200 {
-		t.Fatalf("expected refreshed retry to succeed, got %d body=%s", rw.Code, rw.Body.String())
-	}
-	if atomic.LoadInt32(&hits) != 2 {
-		t.Fatalf("expected 2 upstream hits (401 then 200), got %d", hits)
-	}
-
-	// Credential's access token must have been rotated.
-	got, _ := creds.Get(ctx, db, c.ID)
-	if got.AccessToken != "sk-ant-oat-refreshed" {
-		t.Fatalf("token not rotated: %s", got.AccessToken)
-	}
-}
-
 // A request carrying X-Claude-Code-Session-Id records which credential served
 // it, scoped to the caller; requests without a (valid) header record nothing.
 func TestSessionBindingRecordedFromHeader(t *testing.T) {
````

Run: `go test -race -count=1 -run 'Upstream401|Catalogue401' ./internal/proxy/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: 5 `--- FAIL` lines (`TestUpstream401OnLiveTokenIsRelayed`, `TestConcurrentUpstream401sRefreshOnce`, `TestUpstream401TransientRefreshFailure`, `TestUpstream401RevokedLogin`, `TestCatalogue401OnLiveTokenDoesNotRefresh`) and `FAIL github.com/p4u/claude-proxy/internal/proxy`. `TestUpstream401OnDeadTokenRefreshesAndRetries` already passes: it pins the behaviour that stays. With `-v` the first failure reads `client got 200 "{\"ok\":true}" ... want upstream's 401`, and the transient and revoked cases get `"proxy: credential expired\n"`

Step 2: implement

`internal/proxy/proxy.go`:

````diff
diff --git a/internal/proxy/proxy.go b/internal/proxy/proxy.go
index 4d73534..835c9ac 100644
--- a/internal/proxy/proxy.go
+++ b/internal/proxy/proxy.go
@@ -383,8 +383,9 @@ type forwardResult struct {
 }
 
 // forward sends the request upstream and streams the response back. If
-// allowRetry is true and upstream returns 401, it refreshes the credential and
-// retries once, before any response bytes have been sent to the client.
+// allowRetry is true and upstream returns 401 on a token the refresher
+// confirms is dead, it refreshes the credential and retries once, before any
+// response bytes have been sent to the client.
 // requestedModel is the model exactly as the client sent it, before the
 // advertising alias was stripped. Successful responses carry it back to the
 // client (see restoreModel below), so what the client records about a turn is
@@ -568,22 +569,37 @@ func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte, r
 	}
 
 	if resp.StatusCode == http.StatusUnauthorized && allowRetry && !up.DelegatedAuth {
-		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
+		// A 401 can describe the request (its headers and path come from the
+		// client) rather than the token, and refreshing a long-lived token
+		// downgrades it for good, so the refresher first confirms the token is
+		// dead. The body is kept whole in case it is relayed.
+		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
 		_ = resp.Body.Close()
 		errorType := upstreamErrorType(raw, resp.Header.Get("Content-Encoding"))
-		h.log.Warn("upstream 401; attempting refresh",
-			"cred", cred.ID,
-			"error_type", errorType)
-		fresh, rerr := h.refresher.RefreshNow(r.Context(), cred.ID)
-		if rerr != nil {
-			_ = creds.SetStatus(r.Context(), h.db, cred.ID, creds.StatusExpired)
+		fresh, refreshed, rerr := h.refresher.RefreshRejected(r.Context(), cred.ID, cred.AccessToken)
+		switch {
+		case rerr == nil:
+			h.log.Info("upstream 401; retrying with the stored token",
+				"cred", cred.ID, "label", cred.Label, "refreshed", refreshed)
+			return h.forward(w, r, body, requestedModel, fresh, false, captureText, captureSummary)
+		case errors.Is(rerr, creds.ErrTokenAlive):
+			// Not the token's fault: relay upstream's answer as it came. The
+			// generic >= 400 branch below runs MarkError; no status changes.
+			h.log.Warn("upstream 401 on a live token; relayed without a refresh",
+				"cred", cred.ID, "label", cred.Label, "path", r.URL.Path,
+				"error_type", errorType, "err", rerr)
+			resp.Body = io.NopCloser(bytes.NewReader(raw))
+		case errors.Is(rerr, creds.ErrLoginRejected):
+			// The refresher logged the revocation.
+			_ = creds.MarkError(r.Context(), h.db, cred.ID)
+			http.Error(w, "proxy: credential revoked by Anthropic; sign in again", http.StatusBadGateway)
+			return forwardResult{status: http.StatusUnauthorized, errorType: errorType}
+		default:
+			// Transient: the refresher logged it and set a short retry_after.
 			_ = creds.MarkError(r.Context(), h.db, cred.ID)
-			h.log.Error("refresh failed after 401; marked expired", "cred", cred.ID, "err", rerr)
-			http.Error(w, "proxy: credential expired", http.StatusBadGateway)
+			http.Error(w, "proxy: token refresh failed; retry", http.StatusBadGateway)
 			return forwardResult{status: http.StatusUnauthorized, errorType: errorType}
 		}
-		h.log.Info("refresh succeeded; retrying upstream", "cred", cred.ID)
-		return h.forward(w, r, body, requestedModel, fresh, false, captureText, captureSummary)
 	}
 
 	if resp.StatusCode == http.StatusTooManyRequests {
````

Run: `go test -race -count=1 -v -run 'Upstream401|Catalogue401' ./internal/proxy/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: 6 `--- PASS` lines (`TestUpstream401OnLiveTokenIsRelayed`, `TestUpstream401OnDeadTokenRefreshesAndRetries`, `TestConcurrentUpstream401sRefreshOnce`, `TestUpstream401TransientRefreshFailure`, `TestUpstream401RevokedLogin`, `TestCatalogue401OnLiveTokenDoesNotRefresh`), then `ok  github.com/p4u/claude-proxy/internal/proxy`

Run: `go vet ./... && go test -race -count=1 ./... 2>&1 | grep -c '^ok'`
Expected: `18` (and no `FAIL` line)

Step 3: commit
`git add internal/proxy/refresh401_test.go && git commit -m "fix(proxy): refresh on a 401 only after the token is confirmed dead" -- internal/proxy/proxy.go internal/proxy/proxy_test.go internal/proxy/refresh401_test.go`

Interfaces this task provides to later tasks: none new. After it the proxy no longer calls
`RefreshNow` and never writes `expired`.

### Task 5: Manual refresh skips a working token
Model: opus   Repo: /home/p4u/claude-proxy   Files: `internal/webui/creds.go`,
`internal/webui/refresh_test.go` (new), `internal/tui/update.go`, `internal/tui/tui_test.go`,
`cmd/claude-proxy/main.go`, `cmd/claude-proxy/creds_test.go`, `internal/creds/refresh.go`
Goal: AC5 (web UI endpoint reports `refreshed`), AC5b (TUI `r`), AC7b (web UI side), AC10.
D3 and D3b for every manual surface: the web UI, TUI and `creds refresh` call `Refresh`, the
CLI and TUI print `skipped <id>: access token valid until <RFC3339>`, `creds list` gains
`LOGIN_ENDS` and `REFRESH_UNTIL`, `runServe` calls `SetLogger(logger)` before `Loop` starts, and
`RefreshNow` is deleted.

Notes:
- `TestRefreshEndpointLogsRevocation` already passes before Step 2: the refresher has written the
  revocation ERROR since Task 2. It stays as the AC7b pin for this path.
- `credsList` and `credsRefresh` move their bodies into `printCreds(ctx, db, w)` and
  `refreshCred(ctx, db, id, w)`, as `users list` already does with `printUsers`.

Step 1: write the failing test

New file `internal/webui/refresh_test.go`:

````go
package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// refreshServer is a web UI server whose refresher talks to a fake token
// endpoint answering status (0 = 200 with fresh tokens).
func refreshServer(t *testing.T, status int) (*Server, *store.DB, *atomic.Int32, *syncBuf) {
	t.Helper()
	calls := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800}`))
	}))
	t.Cleanup(srv.Close)
	prev := creds.TokenURL
	creds.SetTokenURL(srv.URL)
	t.Cleanup(func() { creds.SetTokenURL(prev) })
	db, err := store.Open(filepath.Join(t.TempDir(), "webui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logs := &syncBuf{}
	r := creds.NewRefresher(db)
	r.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	return &Server{db: db, refresher: r}, db, calls, logs
}

func postRefresh(s *Server, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	path := "/" + id + "/refresh"
	s.handleCredentials(rec, httptest.NewRequest(http.MethodPost, "/api/credentials"+path, nil), path)
	return rec
}

// AC5: the manual refresh leaves a working token alone and says so.
func TestRefreshEndpointReportsRefreshed(t *testing.T) {
	ctx := context.Background()
	s, db, calls, _ := refreshServer(t, 0)
	for _, tt := range []struct {
		expires   time.Time
		refreshed bool
	}{
		{time.Now().AddDate(1, 0, 0), false},
		{time.Now().Add(time.Minute), true},
	} {
		c, err := creds.Insert(ctx, db, "a", "max", "sk-ant-oat-a", "ref-"+tt.expires.String(), tt.expires, 5)
		if err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		rec := postRefresh(s, c.ID)
		var out struct {
			OK        bool `json:"ok"`
			Refreshed bool `json:"refreshed"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.OK || out.Refreshed != tt.refreshed {
			t.Fatalf("refresh = %d %s, want refreshed=%v", rec.Code, rec.Body.String(), tt.refreshed)
		}
		if made := calls.Load() - before; (made == 1) != tt.refreshed {
			t.Fatalf("token calls %d for refreshed=%v", made, tt.refreshed)
		}
	}
}

// AC7b: a revocation reached through the manual refresh logs one ERROR with
// the label.
func TestRefreshEndpointLogsRevocation(t *testing.T) {
	s, db, _, logs := refreshServer(t, http.StatusBadRequest)
	c, err := creds.Insert(context.Background(), db, "acct-web", "max", "sk-ant-oat-a", "ref-a", time.Now().Add(time.Minute), 5)
	if err != nil {
		t.Fatal(err)
	}
	if rec := postRefresh(s, c.ID); rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh = %d %s, want 502", rec.Code, rec.Body.String())
	}
	n := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "label=acct-web") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d ERROR lines with the label, want 1:\n%s", n, logs)
	}
}
````

`internal/tui/tui_test.go`:

````diff
diff --git a/internal/tui/tui_test.go b/internal/tui/tui_test.go
index 1a32bb2..3754d40 100644
--- a/internal/tui/tui_test.go
+++ b/internal/tui/tui_test.go
@@ -116,3 +116,43 @@ func TestPasteImport(t *testing.T) {
 		t.Fatalf("expected refreshed access token, got %q", list[0].AccessToken)
 	}
 }
+
+// AC5b: the TUI's r leaves a working long-lived token alone and says why.
+func TestRefreshKeyLeavesLiveTokenAlone(t *testing.T) {
+	ctx := context.Background()
+	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
+	if err != nil {
+		t.Fatalf("open: %v", err)
+	}
+	defer db.Close()
+	calls := 0
+	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
+		calls++
+		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-fresh","refresh_token":"ref-fresh","expires_in":28800}`))
+	}))
+	defer srv.Close()
+	prev := creds.TokenURL
+	creds.SetTokenURL(srv.URL)
+	defer creds.SetTokenURL(prev)
+	c, err := creds.Insert(ctx, db, "acct-A", "max", "sk-ant-oat-x", "ref", time.Now().AddDate(1, 0, 0), 5)
+	if err != nil {
+		t.Fatal(err)
+	}
+	stored, _ := creds.Get(ctx, db, c.ID)
+
+	m := &model{db: db, refresher: creds.NewRefresher(db)}
+	m.initTables()
+	m.Update(m.loadCreds()())
+	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
+	if cmd == nil {
+		t.Fatal("r should return a refresh command")
+	}
+	done, ok := cmd().(actionDoneMsg)
+	want := "skipped " + c.ID + ": access token valid until " + stored.ExpiresAt.Format(time.RFC3339)
+	if !ok || done.err != nil || done.msg != want {
+		t.Fatalf("r = %+v, want %q", done, want)
+	}
+	if calls != 0 {
+		t.Fatalf("r made %d token calls for a year-long token", calls)
+	}
+}
````

`cmd/claude-proxy/creds_test.go`:

````diff
diff --git a/cmd/claude-proxy/creds_test.go b/cmd/claude-proxy/creds_test.go
index 2916725..20c3adb 100644
--- a/cmd/claude-proxy/creds_test.go
+++ b/cmd/claude-proxy/creds_test.go
@@ -78,3 +78,45 @@ func TestCredsExportImportBulkKeepsLongLivedLogin(t *testing.T) {
 		t.Fatalf("dead token restore = %v, want credential is not alive", err)
 	}
 }
+
+// AC10: creds list shows the login's end and refresh deadline, and creds
+// refresh reports a working token as skipped.
+func TestCredsListAndRefresh(t *testing.T) {
+	ctx := context.Background()
+	tokenCalls, _ := fakeAnthropicCLI(t)
+	_, db := cliDB(t)
+	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
+	deadline := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
+	c, err := creds.InsertWithRateLimitTier(ctx, db, "team-a", "team", "sk-ant-oat-year", "ref-year", expires, 5, "", deadline)
+	if err != nil {
+		t.Fatal(err)
+	}
+	if _, err := creds.Insert(ctx, db, "legacy", "max", "sk-ant-oat-old", "ref-old", time.Now().Add(time.Hour), 5); err != nil {
+		t.Fatal(err)
+	}
+
+	var out bytes.Buffer
+	if err := printCreds(ctx, db, &out); err != nil {
+		t.Fatal(err)
+	}
+	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
+	header := strings.Fields(lines[0])
+	if len(lines) != 3 || header[6] != "EXPIRES" || header[7] != "LOGIN_ENDS" || header[8] != "REFRESH_UNTIL" {
+		t.Fatalf("list output:\n%s", out.String())
+	}
+	if f := strings.Fields(lines[1]); f[6] != expires.Format(time.RFC3339) || f[7] != expires.Format(time.RFC3339) || f[8] != deadline.Format(time.RFC3339) {
+		t.Fatalf("long-lived row: %q", lines[1])
+	}
+	if f := strings.Fields(lines[2]); f[7] != "-" || f[8] != "-" {
+		t.Fatalf("legacy row should show - for unknown: %q", lines[2])
+	}
+
+	out.Reset()
+	if err := refreshCred(ctx, db, c.ID, &out); err != nil {
+		t.Fatal(err)
+	}
+	want := "skipped " + c.ID + ": access token valid until " + expires.Format(time.RFC3339) + "\n"
+	if out.String() != want || tokenCalls.Load() != 0 {
+		t.Fatalf("refresh printed %q (token calls %d), want %q", out.String(), tokenCalls.Load(), want)
+	}
+}
````

Run: `go test -race -count=1 -run 'TestRefreshEndpoint' ./internal/webui/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: `--- FAIL: TestRefreshEndpointReportsRefreshed` (with `-v`: `token calls 1 for refreshed=false`) and `FAIL github.com/p4u/claude-proxy/internal/webui`

Run: `go test -race -count=1 -run TestRefreshKeyLeavesLiveTokenAlone ./internal/tui/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: `--- FAIL: TestRefreshKeyLeavesLiveTokenAlone` (with `-v`: `r = {msg:refreshed cred_… (expires HH:MM) err:<nil>}, want "skipped cred_…: access token valid until …"`)

Run: `go test -race -count=1 ./cmd/claude-proxy/`
Expected: `cmd/claude-proxy/creds_test.go:99:12: undefined: printCreds` and `[build failed]`

Step 2: implement

`internal/webui/creds.go`:

````diff
diff --git a/internal/webui/creds.go b/internal/webui/creds.go
index 9e2dbc9..53a219e 100644
--- a/internal/webui/creds.go
+++ b/internal/webui/creds.go
@@ -99,12 +99,18 @@ func (s *Server) credAction(w http.ResponseWriter, r *http.Request, id, action s
 		}
 		writeJSON(w, map[string]any{"ok": true, "id": id})
 	case action == "refresh" && r.Method == http.MethodPost:
-		c, err := s.refresher.RefreshNow(ctx, id)
+		// Not forced: refreshing a working long-lived token would replace it
+		// with an 8-hour one for good, so only a token near expiry (or a
+		// revoked login) reaches the token endpoint.
+		c, refreshed, err := s.refresher.Refresh(ctx, id)
 		if err != nil {
 			writeErr(w, http.StatusBadGateway, err.Error())
 			return
 		}
-		writeJSON(w, map[string]any{"ok": true, "id": id, "expires_at": c.ExpiresAt.UTC().Format(time.RFC3339)})
+		writeJSON(w, map[string]any{
+			"ok": true, "id": id, "refreshed": refreshed,
+			"expires_at": c.ExpiresAt.UTC().Format(time.RFC3339),
+		})
 	case action == "endpoint" && r.Method == http.MethodPost:
 		var body struct {
 			Endpoint string `json:"endpoint"`
````

`internal/tui/update.go`:

````diff
diff --git a/internal/tui/update.go b/internal/tui/update.go
index 25289a3..9ac25b0 100644
--- a/internal/tui/update.go
+++ b/internal/tui/update.go
@@ -5,6 +5,7 @@ import (
 	"fmt"
 	"strconv"
 	"strings"
+	"time"
 
 	"github.com/charmbracelet/bubbles/table"
 	tea "github.com/charmbracelet/bubbletea"
@@ -186,7 +187,7 @@ func (m *model) handleCredKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
 		return m, cmd
 	}
 	switch msg.String() {
-	case "r": // force token refresh
+	case "r": // refresh the token if it is near expiry or revoked
 		m.busy = true
 		m.status = "refreshing " + c.ID + "…"
 		return m, m.refreshCred(c.ID)
@@ -409,11 +410,14 @@ func (m *model) submitInput() (tea.Model, tea.Cmd) {
 func (m *model) refreshCred(id string) tea.Cmd {
 	r := m.refresher
 	return func() tea.Msg {
-		c, err := r.RefreshNow(context.Background(), id)
+		c, refreshed, err := r.Refresh(context.Background(), id)
 		if err != nil {
 			return actionDoneMsg{err: fmt.Errorf("refresh %s: %w", id, err)}
 		}
-		return actionDoneMsg{msg: fmt.Sprintf("refreshed %s (expires %s)", id, c.ExpiresAt.Local().Format("15:04"))}
+		if !refreshed {
+			return actionDoneMsg{msg: fmt.Sprintf("skipped %s: access token valid until %s", id, c.ExpiresAt.Format(time.RFC3339))}
+		}
+		return actionDoneMsg{msg: fmt.Sprintf("refreshed %s (expires %s)", id, c.ExpiresAt.Format(time.RFC3339))}
 	}
 }
 
````

`cmd/claude-proxy/main.go`:

````diff
diff --git a/cmd/claude-proxy/main.go b/cmd/claude-proxy/main.go
index 93e89b7..4f52687 100644
--- a/cmd/claude-proxy/main.go
+++ b/cmd/claude-proxy/main.go
@@ -221,6 +221,7 @@ func runServe(args []string) {
 	logger := slog.New(handler)
 
 	r := creds.NewRefresher(db)
+	r.SetLogger(logger)
 	go r.Loop(ctx)
 
 	codexClient, err := codexgateway.New(codexgateway.Config{
@@ -666,17 +667,24 @@ func credsList(ctx context.Context, args []string) {
 	_ = fs.Parse(args)
 	db := openDB(*dbPath)
 	defer db.Close()
-	list, err := creds.List(ctx, db)
-	if err != nil {
+	if err := printCreds(ctx, db, os.Stdout); err != nil {
 		fmt.Fprintf(os.Stderr, "list: %v\n", err)
 		os.Exit(1)
 	}
+}
+
+// printCreds writes the credentials table behind `creds list`.
+func printCreds(ctx context.Context, db *store.DB, w io.Writer) error {
+	list, err := creds.List(ctx, db)
+	if err != nil {
+		return err
+	}
 	if len(list) == 0 {
-		fmt.Println("(no credentials)")
-		return
+		fmt.Fprintln(w, "(no credentials)")
+		return nil
 	}
-	fmt.Printf("%-30s  %-9s  %-10s  %-6s  %3s  %-9s  %-25s  %5s/%5s/%5s  %s\n",
-		"ID", "PROVIDER", "LABEL", "SUB", "WT", "STATUS", "EXPIRES", "REQ", "OK", "ERR", "LAST_REQ")
+	fmt.Fprintf(w, "%-30s  %-9s  %-10s  %-6s  %3s  %-9s  %-25s  %-25s  %-25s  %5s/%5s/%5s  %s\n",
+		"ID", "PROVIDER", "LABEL", "SUB", "WT", "STATUS", "EXPIRES", "LOGIN_ENDS", "REFRESH_UNTIL", "REQ", "OK", "ERR", "LAST_REQ")
 	for _, c := range list {
 		lastReq := "-"
 		if c.LastRequestAt != nil {
@@ -688,11 +696,19 @@ func credsList(ctx context.Context, args []string) {
 		if !provider.Get(c.Provider).Refreshable {
 			expires = "never (api key)"
 		}
-		fmt.Printf("%-30s  %-9s  %-10s  %-6s  %3d  %-9s  %-25s  %5d/%5d/%5d  %s\n",
+		loginEnds, refreshUntil := "-", "-"
+		if t, ok := c.LoginEndsAt(); ok {
+			loginEnds = t.Format(time.RFC3339)
+		}
+		if !c.RefreshExpiresAt.IsZero() {
+			refreshUntil = c.RefreshExpiresAt.Format(time.RFC3339)
+		}
+		fmt.Fprintf(w, "%-30s  %-9s  %-10s  %-6s  %3d  %-9s  %-25s  %-25s  %-25s  %5d/%5d/%5d  %s\n",
 			c.ID, provider.Get(c.Provider).ID, c.Label, c.SubscriptionType, c.Weight, string(c.Status),
-			expires,
+			expires, loginEnds, refreshUntil,
 			c.RequestCount, c.SuccessCount, c.ErrorCount, lastReq)
 	}
+	return nil
 }
 
 func credsDisable(ctx context.Context, args []string) {
@@ -761,13 +777,26 @@ func credsRefresh(ctx context.Context, args []string) {
 	_ = fs.Parse(args[1:])
 	db := openDB(*dbPath)
 	defer db.Close()
-	r := creds.NewRefresher(db)
-	c, _, err := r.Refresh(ctx, args[0])
-	if err != nil {
+	if err := refreshCred(ctx, db, args[0], os.Stdout); err != nil {
 		fmt.Fprintf(os.Stderr, "refresh: %v\n", err)
 		os.Exit(1)
 	}
-	fmt.Printf("refreshed %s expires=%s\n", c.ID, c.ExpiresAt.Format(time.RFC3339))
+}
+
+// refreshCred runs `creds refresh`: the token endpoint is called only for a
+// token near expiry or a revoked login, so a working token is reported as
+// skipped rather than downgraded.
+func refreshCred(ctx context.Context, db *store.DB, id string, w io.Writer) error {
+	c, refreshed, err := creds.NewRefresher(db).Refresh(ctx, id)
+	if err != nil {
+		return err
+	}
+	if !refreshed {
+		fmt.Fprintf(w, "skipped %s: access token valid until %s\n", c.ID, c.ExpiresAt.Format(time.RFC3339))
+		return nil
+	}
+	fmt.Fprintf(w, "refreshed %s expires=%s\n", c.ID, c.ExpiresAt.Format(time.RFC3339))
+	return nil
 }
 
 // exportLine is the JSONL format used by creds export / import-bulk.
````

`internal/creds/refresh.go`:

````diff
diff --git a/internal/creds/refresh.go b/internal/creds/refresh.go
index 89ea1f0..350c349 100644
--- a/internal/creds/refresh.go
+++ b/internal/creds/refresh.go
@@ -240,24 +240,6 @@ func (r *Refresher) Refresh(ctx context.Context, id string) (c *Credential, refr
 	return r.refreshLocked(ctx, c)
 }
 
-// RefreshNow forces a refresh. Transitional: its last callers (web UI, TUI)
-// move to Refresh in a later change, which deletes it.
-func (r *Refresher) RefreshNow(ctx context.Context, id string) (*Credential, error) {
-	mu := r.lockFor(id)
-	mu.Lock()
-	defer mu.Unlock()
-	ctx = context.WithoutCancel(ctx)
-	c, err := Get(ctx, r.db, id)
-	if err != nil {
-		return nil, err
-	}
-	if !provider.Get(c.Provider).Refreshable {
-		return c, ErrNotRefreshable
-	}
-	c, _, err = r.refreshLocked(ctx, c)
-	return c, err
-}
-
 // RefreshRejected handles an upstream 401 on rejected, the access token the
 // request carried. It refreshes only when the token is really dead: another
 // request may already have replaced it, and a 401 can describe the request
````

Run: `go test -race -count=1 -v -run '^(TestRefreshEndpointReportsRefreshed|TestRefreshEndpointLogsRevocation|TestRefreshKeyLeavesLiveTokenAlone|TestCredsListAndRefresh)$' ./internal/webui/ ./internal/tui/ ./cmd/claude-proxy/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: 4 `--- PASS` lines (`TestRefreshEndpointReportsRefreshed`, `TestRefreshEndpointLogsRevocation`, `TestRefreshKeyLeavesLiveTokenAlone`, `TestCredsListAndRefresh`) and 3 `ok` lines (`internal/webui`, `internal/tui`, `cmd/claude-proxy`)

Run: `grep -rn 'RefreshNow' --include='*.go' . ; go vet ./... && go test -race -count=1 ./... 2>&1 | grep -c '^ok'`
Expected: no `RefreshNow` match, then `18` (and no `FAIL` line)

Step 3: commit
`git add internal/webui/refresh_test.go && git commit -m "fix(creds): manual refresh skips a working token" -- internal/webui/creds.go internal/webui/refresh_test.go internal/tui/update.go internal/tui/tui_test.go cmd/claude-proxy/main.go cmd/claude-proxy/creds_test.go internal/creds/refresh.go`

Interfaces this task provides to later tasks:
- `POST /api/credentials/{id}/refresh` → `{ok, id, refreshed, expires_at}`.
- `func printCreds(ctx context.Context, db *store.DB, w io.Writer) error` and
  `func refreshCred(ctx context.Context, db *store.DB, id string, w io.Writer) error` in
  `cmd/claude-proxy`.
- `RefreshNow` no longer exists.

### Task 6: Show when each login ends
Model: sonnet   Repo: /home/p4u/claude-proxy   Files: `internal/webui/creds.go`,
`internal/webui/creds_lifetime_test.go` (new), `internal/webui/static/js/pages/credentials.js`,
`internal/webui/static/js/mock.js`, `internal/webui/static/css/credentials.css`
Goal: AC9. D7: `GET /api/credentials` adds `login_ends_at` and `refresh_deadline_at` (RFC3339
UTC, present only when known); the credentials card shows "login ends" (warning style under 7
days, "login ended — sign in again" once past) and "refresh until"; the refresh toast reads
"Token refreshed" or "Token still valid, nothing to refresh"; the add-credential blurb reads "A
Pro/Max/Team/Enterprise login, valid for up to a year."; `mock.js` serves both fields (one row
inside 7 days) and alternates `refreshed`.

Notes:
- The new `credView` fields sit at the end of the struct after a blank line, so gofmt leaves the
  existing fields alone.
- Gap the spec leaves, filled here: the card's existing "expires" stat used `localTime`, which has
  no year, so a year-long token read as expiring today ("Nov 10, 04:44 PM"). Expiries more than
  24 h away now render with their year (`whenOf`); within 24 h the time of day stays. Drop the
  `whenOf` hunk if the controller wants D7 to the letter.
- `act()` accepts a function for its success message, so the refresh toast can read the response.

Step 1: write the failing test

New file `internal/webui/creds_lifetime_test.go`:

````go
package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
)

// AC9: the credential list carries when the login ends and until when it can
// refresh, only for logins whose deadline is known.
func TestListCredsLoginDeadline(t *testing.T) {
	ctx := context.Background()
	db, h := newTestServer(t)
	cookie := loginCookie(t, h)
	expires := time.Now().AddDate(1, 0, 0).Truncate(time.Second)
	deadline := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Second)
	known, err := creds.InsertWithRateLimitTier(ctx, db, "known", "team", "sk-ant-oat-k", "ref-k", expires, 5, "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := creds.Insert(ctx, db, "legacy", "max", "sk-ant-oat-l", "ref-l", time.Now().Add(time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/credentials", "", cookie)
	var list []map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, v := range list {
		byID[v["id"].(string)] = v
	}
	k := byID[known.ID]
	if k["login_ends_at"] != expires.UTC().Format(time.RFC3339) || k["refresh_deadline_at"] != deadline.UTC().Format(time.RFC3339) {
		t.Fatalf("known login: %v", k)
	}
	for _, field := range []string{"login_ends_at", "refresh_deadline_at"} {
		if _, ok := byID[legacy.ID][field]; ok {
			t.Fatalf("legacy login carries %s: %v", field, byID[legacy.ID])
		}
	}
	if strings.Contains(rec.Body.String(), "sk-ant") {
		t.Fatal("the credential list leaked a token")
	}
}
````

Run: `go test -race -count=1 -run TestListCredsLoginDeadline ./internal/webui/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)|creds_lifetime_test'`
Expected: `--- FAIL: TestListCredsLoginDeadline` with `creds_lifetime_test.go:42: known login: map[active_conversations:0 … weight:5]` (no `login_ends_at` key), then `FAIL github.com/p4u/claude-proxy/internal/webui`

Step 2: implement

`internal/webui/creds.go`:

````diff
diff --git a/internal/webui/creds.go b/internal/webui/creds.go
index 53a219e..2d7e70d 100644
--- a/internal/webui/creds.go
+++ b/internal/webui/creds.go
@@ -47,6 +47,11 @@ type credView struct {
 	ErrorCount          int64         `json:"error_count"`
 	Weight              int           `json:"weight"`
 	ActiveConversations int           `json:"active_conversations"`
+
+	// LoginEndsAt (creds.LoginEndsAt) and RefreshDeadlineAt, RFC3339 UTC, are
+	// present only when the login's deadline is known.
+	LoginEndsAt       string `json:"login_ends_at,omitempty"`
+	RefreshDeadlineAt string `json:"refresh_deadline_at,omitempty"`
 }
 
 // handleCredentials routes /credentials and /credentials/{id}/{action}. `rest`
@@ -326,6 +331,12 @@ func (s *Server) listCreds(w http.ResponseWriter, r *http.Request) {
 		if c.LastRequestAt != nil {
 			v.LastRequestAt = c.LastRequestAt.Format(time.RFC3339)
 		}
+		if t, ok := c.LoginEndsAt(); ok {
+			v.LoginEndsAt = t.UTC().Format(time.RFC3339)
+		}
+		if !c.RefreshExpiresAt.IsZero() {
+			v.RefreshDeadlineAt = c.RefreshExpiresAt.UTC().Format(time.RFC3339)
+		}
 		if c.RetryAfter != nil {
 			v.RetryAfter = c.RetryAfter.Format(time.RFC3339)
 		}
````

`internal/webui/static/js/pages/credentials.js`:

````diff
diff --git a/internal/webui/static/js/pages/credentials.js b/internal/webui/static/js/pages/credentials.js
index 495e971..92e1e0e 100644
--- a/internal/webui/static/js/pages/credentials.js
+++ b/internal/webui/static/js/pages/credentials.js
@@ -213,8 +213,8 @@ function endpointLabel(c) {
   try { return new URL(c.endpoint).host; } catch { return c.endpoint; }
 }
 
-function stat(val, label) {
-  return el("div", { class: "cred-stat" }, [
+function stat(val, label, warn = false) {
+  return el("div", { class: "cred-stat" + (warn ? " cred-stat--warn" : "") }, [
     el("span", { class: "cred-stat__val", text: val }),
     el("span", { class: "cred-stat__label", text: label }),
   ]);
@@ -242,7 +242,11 @@ function credCard(c, root) {
     ...(!isKey ? [
       {
         label: "Refresh token",
-        onClick: () => act(() => api.post(`/credentials/${id}/refresh`), "Token refreshed", root),
+        onClick: () => act(
+          () => api.post(`/credentials/${id}/refresh`),
+          (out) => (out && out.refreshed === false ? "Token still valid, nothing to refresh" : "Token refreshed"),
+          root,
+        ),
       },
       {
         label: "Update tokens",
@@ -275,7 +279,7 @@ function credCard(c, root) {
   const typeText = c.subscription_type || (modelIDs ? `${(c.models || []).length} model(s)` : "—");
   const wt = c.weight ?? "—";
   const lastUsed = c.last_request_at ? relTime(tsOf(c.last_request_at)) : "never";
-  const expires = isKey ? "never" : (c.expires_at ? localTime(tsOf(c.expires_at)) : "—");
+  const expires = isKey ? "never" : (c.expires_at ? whenOf(tsOf(c.expires_at)) : "—");
 
   return el("article", { class: "card cred-card" + (disabled ? " cred-card--disabled" : "") }, [
     el("div", { class: "cred-card__identity" }, [
@@ -295,10 +299,36 @@ function credCard(c, root) {
       stat(compactNum(c.request_count ?? c.requests ?? 0), "requests"),
       stat(lastUsed, "last used"),
       stat(expires, "expires"),
+      ...loginStats(c),
     ]),
   ]);
 }
 
+// "login ends" (warning under a week out, and once past) and "refresh until",
+// shown only when the login's deadline is known.
+function loginStats(c) {
+  const out = [];
+  if (c.login_ends_at) {
+    const left = tsOf(c.login_ends_at) - Date.now() / 1000;
+    out.push(left <= 0
+      ? stat("login ended — sign in again", "login ends", true)
+      : stat(dateOf(tsOf(c.login_ends_at)), "login ends", left < 7 * 86400));
+  }
+  if (c.refresh_deadline_at) out.push(stat(dateOf(tsOf(c.refresh_deadline_at)), "refresh until"));
+  return out;
+}
+
+// A date with its year: a login can end a year out, which localTime's
+// month-day form would make read as this year.
+function dateOf(tsSec) {
+  return new Date(tsSec * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
+}
+
+// An expiry: the time of day within the next 24 h, otherwise the dated form.
+function whenOf(tsSec) {
+  return tsSec - Date.now() / 1000 > 86400 ? dateOf(tsSec) : localTime(tsSec);
+}
+
 function sidecarCredCard(c, root) {
   const a = c.sidecar_account;
   const ch = c.sidecar_channel;
@@ -390,10 +420,11 @@ function tsOf(v) {
   return isNaN(t) ? 0 : t / 1000;
 }
 
+// okMsg is a string, or a function of the response returning one.
 async function act(fn, okMsg, root) {
   try {
-    await fn();
-    toast(okMsg, "good");
+    const out = await fn();
+    toast(typeof okMsg === "function" ? okMsg(out) : okMsg, "good");
     render(root);
   } catch (e) {
     toast(e.message, "critical", 6000);
@@ -505,7 +536,7 @@ function weightModal(id, current, root, options = {}) {
 const KINDS = {
   anthropic: {
     label: "Anthropic subscription",
-    blurb: "A Pro/Max/Team/Enterprise login. The proxy keeps its token refreshed for you.",
+    blurb: "A Pro/Max/Team/Enterprise login, valid for up to a year.",
     needsJSON: true,
     claudeLogin: true,
   },
````

`internal/webui/static/css/credentials.css`:

````diff
diff --git a/internal/webui/static/css/credentials.css b/internal/webui/static/css/credentials.css
index 33fc3bb..10d4dbb 100644
--- a/internal/webui/static/css/credentials.css
+++ b/internal/webui/static/css/credentials.css
@@ -40,6 +40,7 @@
 .cred-stat { display: flex; align-items: baseline; gap: 5px; }
 .cred-stat__val { font-family: var(--mono); font-size: 12px; font-weight: 600; }
 .cred-stat__label { font-size: 10px; color: var(--ink-muted); }
+.cred-stat--warn .cred-stat__val { color: var(--warning); }
 @media (max-width: 1100px) {
   .cred-card {
     grid-template-columns: minmax(0, 1fr) auto;
````

`internal/webui/static/js/mock.js`:

````diff
diff --git a/internal/webui/static/js/mock.js b/internal/webui/static/js/mock.js
index d21c6d5..1194139 100644
--- a/internal/webui/static/js/mock.js
+++ b/internal/webui/static/js/mock.js
@@ -70,6 +70,21 @@ const endpointNameOf = (c) => {
   return p ? (p.endpoints.find((e) => e.url === endpointOf(c))?.name || "") : "";
 };
 const hasUsageAPI = (c) => providerOf(c) === "anthropic";
+// Anthropic logins with a known deadline: the first is a year-long sign-in,
+// the second an 8-hour login whose deadline is 3 days out (the warning
+// state); the rest predate the field and omit both.
+const loginDeadlines = (c, i) => {
+  if (providerOf(c) !== "anthropic" || i > 1) return {};
+  const now = Math.floor(Date.now() / 1000);
+  const iso = (s) => new Date(s * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
+  if (i === 0) {
+    const year = now + 365 * 86400;
+    return { expires_at: year, login_ends_at: iso(year), refresh_deadline_at: iso(now + 27 * 86400) };
+  }
+  return { login_ends_at: iso(now + 3 * 86400), refresh_deadline_at: iso(now + 3 * 86400) };
+};
+// The refresh action alternates "refreshed" and "still valid" so both toasts show.
+let mockRefreshes = 0;
 // full_capture and the usage limit are mutated in place by
 // POST /users/{id}/capture and POST /users/{id}/limit, so both toggles stay
 // stateful for the whole mock session.
@@ -651,6 +666,7 @@ const DB = {
       models: c.models || [],
       request_count: [8200, 5400, 1200, 3600, 40, 970][i], last_request_at: c.status === "disabled" ? null : now - 60 * (i + 1),
       expires_at: now + 3600 * (5 - i), created_at: now - 86400 * (30 - i * 4),
+      ...loginDeadlines(c, i),
     })),
   "/credentials/endpoints": () => MOCK_ENDPOINTS,
   "/codex/accounts": () => ({ configured: true, accounts: CODEX_ACCOUNTS }),
@@ -962,6 +978,7 @@ window.fetch = async (input, init = {}) => {
       else if (credentialAction[2] === "enable") c.status = "active";
       else if (credentialAction[2] === "disable") c.status = "disabled";
       else if (credentialAction[2] === "weight") c.weight = JSON.parse(init.body || "{}").weight;
+      else if (credentialAction[2] === "refresh") return json({ ok: true, id: c.id, refreshed: mockRefreshes++ % 2 === 0 });
       return json({ ok: true });
     }
     const mdl = path.match(/^\/credentials\/([^/]+)\/models$/);
````

Run: `go test -race -count=1 -v -run TestListCredsLoginDeadline ./internal/webui/ | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'`
Expected: `--- PASS: TestListCredsLoginDeadline`, then `ok  github.com/p4u/claude-proxy/internal/webui`

Run: `node --check internal/webui/static/js/pages/credentials.js && node --check internal/webui/static/js/mock.js && go vet ./... && go test -race -count=1 ./... 2>&1 | grep -c '^ok'`
Expected: `18` (and no `FAIL` line)

Browser check (the controller or a ui-tester; done once during planning with these results):
1. `mkdir -p ~/.cache/claude-proxy/ui-check && go build -o ~/.cache/claude-proxy/ui-check/claude-proxy ./cmd/claude-proxy`
2. `CLAUDE_PROXY_UI_PASSWORD=check ~/.cache/claude-proxy/ui-check/claude-proxy serve --addr 127.0.0.1:18787 --db ~/.cache/claude-proxy/ui-check/proxy.db`
   (an empty database: nothing is contacted upstream).
3. Open `http://127.0.0.1:18787/?mock=1#/credentials`. Expected: `max-personal` shows
   `expires Nov 10, 2027`, `login ends Nov 10, 2027` and `refresh until Dec 7, 2026` (dates move
   with today); `team-eu` shows `login ends` in the warning colour (`--warning`) and `refresh
   until` three days out; `pro-backup` and the others show neither stat.
4. Actions → Refresh token on `max-personal` twice. Expected toasts: "Token refreshed", then
   "Token still valid, nothing to refresh". No console errors.
5. Add credential → Anthropic subscription. Expected blurb: "A Pro/Max/Team/Enterprise login,
   valid for up to a year."
6. Stop the server. If the browser tool writes a `.playwright-mcp/` directory into the repo,
   delete it; it must not be committed.

Step 3: commit
`git add internal/webui/creds_lifetime_test.go && git commit -m "feat(webui): show when each login ends" -- internal/webui/creds.go internal/webui/creds_lifetime_test.go internal/webui/static/js/pages/credentials.js internal/webui/static/js/mock.js internal/webui/static/css/credentials.css`

Interfaces this task provides to later tasks: `credView.login_ends_at`,
`credView.refresh_deadline_at` (RFC3339 UTC, omitted when unknown), documented in Task 7.

### Task 7: Document long-lived logins
Model: sonnet   Repo: /home/p4u/claude-proxy   Files: `docs/WEBUI.md`, `README.md`, `Makefile`,
`CLAUDE.md` (edited here, committed by the controller)
Goal: D7's docs: `docs/WEBUI.md` (the new fields and the `refreshed` response, the exchange's
scopes and lifetime), the README backup note (exports hold year-long tokens; the proxy cannot
revoke one), the `make refresh` help text, and the CLAUDE.md section *Adding an Anthropic
subscription from the web UI* (scopes, lifetime, the deadline, the never-refresh rule, the
liveness check), plus its request-flow line for the 401 path.

Notes:
- `CLAUDE.md` holds the owner's uncommitted edits elsewhere in the file. Apply this task's
  `CLAUDE.md` patch to the working tree only (`git apply`), do not stage or commit it, and leave
  the patch file at `~/.cache/claude-proxy/patches/t7-claude-md.patch`. The controller commits
  exactly these hunks with
  `git apply --cached ~/.cache/claude-proxy/patches/t7-claude-md.patch && git commit -m "docs(claude): long-lived anthropic logins"`
  (the index still holds HEAD's `CLAUDE.md`, so only these hunks are staged).
- The patch was checked with `git apply --check` against both the base `CLAUDE.md` and the
  owner's working copy.
- Do not edit `CLAUDE.md`'s *Background Goroutines* line about the Credential Refresher ("reactively
  refreshes on 401 and retries"): it sits next to the owner's uncommitted hunk. The controller may
  fix it when committing.

Step 1: no test (documentation only).

Step 2: implement

`docs/WEBUI.md`:

````diff
diff --git a/docs/WEBUI.md b/docs/WEBUI.md
index df34584..0aa0d69 100644
--- a/docs/WEBUI.md
+++ b/docs/WEBUI.md
@@ -212,6 +212,12 @@ value claims.
   false for providers publishing no utilization endpoint; the UI uses it to hide
   the usage meters, the expiry date and the OAuth-only row actions (Refresh,
   Update tokens) rather than showing 0% and a synthetic far-future date.
+  `refresh_deadline_at` and `login_ends_at` (RFC3339 UTC) are present only when
+  the login's refresh deadline is known: the first is that deadline, after
+  which the token endpoint refuses the refresh token; the second is when the
+  login stops serving, the later of the deadline and `expires_at`. The UI shows
+  them as "refresh until" and "login ends" (warning style under 7 days,
+  "login ended — sign in again" once past).
 - `POST /api/credentials` `{credentials_json, label, weight?}` → import pasted
   `.credentials.json` (use `ingest.ImportFromJSON`; verifies liveness, rejects dupes).
 - `POST /api/credentials/{id}/tier` `{tier:string}` → `{ok:true}`. Updates only
@@ -286,7 +292,12 @@ value claims.
   → `{ok,id,label,status,subscription_type,weight}`. `code` is what Anthropic
   displayed (`code#state`), a full callback URL, or a bare code; a state from a
   different sign-in is refused before contacting Anthropic. The code is
-  exchanged at the token endpoint the refresher uses, then
+  exchanged at the token endpoint the refresher uses, for the scopes
+  `user:profile user:inference` and with `expires_in: 31536000` (a one-year
+  access token; Anthropic honours it only at the exchange, never on a
+  refresh), and the login's refresh deadline (`refresh_token_expires_in`) is
+  stored with it. Once the exchange starts it completes even if the browser
+  disconnects, since the code cannot be used twice. Then
   `GET api.anthropic.com/api/oauth/profile` proves the token works and maps
   `organization_type` to the plan (`claude_max` → `max`, …; an account without a
   subscription is refused). Without `credential_id` a new credential is added
@@ -297,7 +308,13 @@ value claims.
   rejected before the code is spent. Responses never contain tokens.
 - `POST /api/credentials/oauth/cancel` `{session}` → forget a pending sign-in.
 - `POST /api/credentials/{id}/disable` | `/enable` (SetStatus disabled/active)
-- `POST /api/credentials/{id}/refresh` → force OAuth token refresh (`Refresher.RefreshNow`)
+- `POST /api/credentials/{id}/refresh` → `{ok,id,refreshed,expires_at}`. Not
+  forced (`Refresher.Refresh`): the token endpoint is called only when the
+  access token expires within 5 minutes or the login is `revoked`, because a
+  refresh replaces a year-long token with an 8-hour one for good. Otherwise
+  nothing changes and `refreshed` is false; the UI toasts "Token refreshed" or
+  "Token still valid, nothing to refresh". `502` with the error on a failed
+  refresh.
 - `POST /api/credentials/{id}/weight` `{weight}` (creds.SetWeight)
 - `POST /api/credentials/{id}/settings` `{label, weight}` → atomically edit the
   native credential's display name and selection weight. Labels are trimmed,
````

`README.md`:

````diff
diff --git a/README.md b/README.md
index 0dd6c1f..43bd461 100644
--- a/README.md
+++ b/README.md
@@ -157,6 +157,14 @@ rm -f ~/cp-creds/acct-A/.credentials.json
 > tokens. Re-importing the original `credentials.json` after the proxy has
 > been running will not work — the original refresh token is long since
 > superseded.
+>
+> Logins added with *Sign in with Claude* carry a one-year access token, so an
+> export holds tokens usable for up to a year: guard `backup.jsonl` like a
+> password file. `import-bulk` restores such a login exactly as exported,
+> verified against Anthropic's profile endpoint and never refreshed. The proxy
+> cannot revoke a token at Anthropic: deleting or reconnecting a credential
+> leaves its old token valid, and whether the account can end that session from
+> its Claude settings is unverified.
 
 Repeat for each account. Then point Claude Code at the proxy:
 
@@ -1081,7 +1089,7 @@ Credentials
   make usage ID=cred_xxx    Fetch usage % for a single credential
   make disable ID=cred_xxx  Mark a credential disabled (excluded from selection)
   make rm ID=cred_xxx       Remove a credential row
-  make refresh ID=cred_xxx  Force-refresh a credential's tokens
+  make refresh ID=cred_xxx  Refresh a credential's tokens if near expiry or revoked
   make weight ID=... W=...  Set the selection weight
 
 User tokens (per-user bearer auth)
````

`Makefile`:

````diff
diff --git a/Makefile b/Makefile
index dd446b5..d60fb7c 100644
--- a/Makefile
+++ b/Makefile
@@ -215,7 +215,7 @@ rm: ## Remove a credential (ID=cred_xxx).
 	$(RUN) creds rm "$(ID)" --db $(DB)
 
 # usage: make refresh ID=cred_xxx
-refresh: ## Force-refresh a credential's tokens (ID=cred_xxx).
+refresh: ## Refresh a credential's tokens if near expiry or revoked (ID=cred_xxx).
 	@if [ -z "$(ID)" ]; then echo "usage: make refresh ID=cred_xxx"; exit 2; fi
 	$(RUN) creds refresh "$(ID)" --db $(DB)
 
````

Save this one as `~/.cache/claude-proxy/patches/t7-claude-md.patch` and apply it with
`git apply ~/.cache/claude-proxy/patches/t7-claude-md.patch`:

`CLAUDE.md`:

````diff
diff --git a/CLAUDE.md b/CLAUDE.md
index 227bf46..bf8d482 100644
--- a/CLAUDE.md
+++ b/CLAUDE.md
@@ -85,7 +85,8 @@ Claude Code (ANTHROPIC_BASE_URL → proxy)
         it strips an incoming Anthropic-Version while querying providers and
         reinjects it only for Anthropic, because the Codex sidecar's translated
         catalogue becomes obfuscated when that header is forwarded
-      → on 401: OAuth credential → refresher triggers token refresh → retry;
+      → on 401: OAuth credential → RefreshRejected: refresh + retry once only
+        if /api/oauth/profile also rejects the token, else relay upstream's 401;
         API key → marked revoked immediately (nothing to refresh);
         delegated-auth gateway (Codex sidecar) → relayed, status untouched
       → on 429: mark credential "limited", synthesize Retry-After if missing → pass 429 to client
@@ -904,14 +905,59 @@ existing credential.
 
 The flow uses Claude Code's client ID and its manual redirect
 (`platform.claude.com/oauth/code/callback`), the only registered redirect that
-is not a localhost port, so it works for a proxy on a remote host; the scopes
-match a Claude Code subscription login, so the result is interchangeable with an
-imported `.credentials.json`. The PKCE verifier never leaves the server, a
-session is single-use and expires after 10 minutes, and the new token must read
-`/api/oauth/profile` — which also yields the plan (`max`/`pro`/`team`/
-`enterprise`) — before anything is stored. A fresh sign-in creates a new grant;
-it does not invalidate the account's other Claude Code logins, unlike
-re-importing a `.credentials.json` that another process also refreshes.
+is not a localhost port, so it works for a proxy on a remote host. The PKCE
+verifier never leaves the server, a session is single-use and expires after 10
+minutes, and the new token must read `/api/oauth/profile` — which also yields
+the plan (`max`/`pro`/`team`/`enterprise`) — before anything is stored. Once
+the exchange starts it completes even if the browser disconnects
+(`context.WithoutCancel`), because the code cannot be used twice. A fresh
+sign-in creates a new grant; it does not invalidate the account's other Claude
+Code logins, unlike re-importing a `.credentials.json` that another process
+also refreshes.
+
+**Long-lived logins.** The sign-in asks only for `user:profile user:inference`
+(`claudeoauth.Scopes`): the narrowest grant that serves `/v1/messages`,
+`/v1/models`, plan detection and the usage API, so it no longer mirrors Claude
+Code's scope set. It sends `expires_in: 31536000` (`LoginLifetimeSeconds`) at
+the code exchange, which Anthropic answers with a one-year access token. Every
+Anthropic login also has an absolute refresh deadline
+(`refresh_token_expires_in`, about 27 days): a refresh never moves it, returns
+an 8-hour token whatever `expires_in` it is sent, and is refused (400) once the
+deadline has passed. The deadline is stored in `credentials.refresh_expires_at`
+(0 = unknown, as for every login that predates the column) with Claude Code's
+rule (`creds.nextDeadline`): a new login replaces it, a refresh only ever
+lowers it, and an unknown one stays unknown. The web UI shows "login ends"
+(`Credential.LoginEndsAt`, the later of the token expiry and the deadline,
+warning under 7 days) and "refresh until"; `creds list` prints `LOGIN_ENDS` and
+`REFRESH_UNTIL`; `creds export` carries `refreshTokenExpiresAt`.
+
+**A working token is never refreshed**, since one refresh drops a year-long
+token to 8 hours for good. The refresher calls the token endpoint only when the
+stored expiry is within 5 minutes, for a `revoked` login, or after an upstream
+401 that a liveness check confirms (`Refresher.RefreshRejected`). A 401 can
+describe the request rather than the token (its headers and path come from the
+client), so the proxy first reads `/api/oauth/profile` with the stored token
+(`creds.CheckToken`, the sign-in's headers): a 200, or any answer other than
+401, relays upstream's 401 and changes nothing; only a confirmed 401 refreshes
+and retries once, and concurrent 401s on one token share one refresh. Manual
+refresh (web UI, TUI `r`, `creds refresh`) follows the same rule and reports
+the token as still valid instead. Imports never rotate a long-lived file: a
+`.credentials.json` or export line whose `expiresAt` is more than a day away is
+checked with `CheckToken` and stored as it is. A token-endpoint 400/401 revokes
+the credential (`creds.ErrLoginRejected`, logged at ERROR); every other failure
+is transient: the status stays, `retry_after` moves 60 s out so new
+conversations avoid the credential, and the error is replayed for 30 s without
+another token call. A refresh Anthropic has answered is stored on a context the
+caller cannot cancel, with `store.Retry`. `expired` is no longer produced, and
+legacy `expired` Anthropic rows are healed to `active` at startup.
+
+> Not yet observed: a one-year token serving past its login's refresh deadline.
+> Until a production credential passes its deadline (the canary, about 27 days
+> after the first reconnect; record the result here), "refresh until" stays
+> visible and an early rejection logs at ERROR "long-lived token rejected
+> before its expiry". The proxy cannot revoke a token at Anthropic: deleting or
+> reconnecting a credential leaves its old token valid, and whether the account
+> can end that session from its Claude settings is unverified.
 
 ```bash
 make import FROM=path/to/.credentials.json LABEL=myaccount [WEIGHT=5]
@@ -923,7 +969,7 @@ make usage ID=cred_xxx           # Fetch Anthropic usage for one credential
 make usage-history PERIOD=24h    # Chart stored usage history (optional ID=cred_xxx)
 make disable ID=...              # Exclude from selection
 make rm ID=...                   # Delete credential
-make refresh ID=...              # Force token refresh
+make refresh ID=...              # Refresh a token near expiry (skips a working one)
 make weight ID=... W=N           # Adjust selection weight
 make export-credentials > f.jsonl  # Backup current tokens to file
 cat f.jsonl | make import-credentials  # Restore from backup
````

Run: `make help | grep -c "Refresh a credential's tokens if near expiry or revoked"`
Expected: `1`

Step 3: commit
`git commit -m "docs: long-lived anthropic logins and visible login deadlines" -- docs/WEBUI.md README.md Makefile`
(`CLAUDE.md` is committed by the controller, see Notes.) Afterwards `git status --short` shows
only ` M CLAUDE.md`.

Interfaces this task provides to later tasks: none.

## Merge and release
1. Full suites on the branch: `make test` → `ok` for all 18 packages with tests, no `FAIL`
   (`go test -race -count=1 ./... 2>&1 | grep -c '^ok'` → `18`); `make lint` → `0 issues.` (or the
   `GOTOOLCHAIN=go1.26.2` command from Global Constraints); `gofmt -l .` → no output. Also check
   that no test reaches Anthropic: temporarily make `creds.CheckToken` and `tokenRequest` panic
   when `ProfileURL`/`TokenURL` hold their production values, run `go test -count=1 ./...`
   (expected: 18 `ok`, no panic), then revert that edit (done during planning, passed).
2. AC12 live check (owner, at the gate, on the branch, before merging):
   1. `mkdir -p ~/.cache/claude-proxy/ac12 && go build -o ~/.cache/claude-proxy/ac12/claude-proxy ./cmd/claude-proxy`
   2. `CLAUDE_PROXY_UI_PASSWORD=<pw> ~/.cache/claude-proxy/ac12/claude-proxy serve --addr 127.0.0.1:8788 --db ~/.cache/claude-proxy/ac12/proxy.db`
   3. In `http://127.0.0.1:8788/`: Add credential → Anthropic subscription → Sign in with Claude,
      approve, paste the code.
   4. `~/.cache/claude-proxy/ac12/claude-proxy creds list --db ~/.cache/claude-proxy/ac12/proxy.db`
      → `EXPIRES` about a year away, `LOGIN_ENDS` the same, `REFRESH_UNTIL` about 27 days away.
   5. `~/.cache/claude-proxy/ac12/claude-proxy users create --name ac12 --db ~/.cache/claude-proxy/ac12/proxy.db`,
      then `ANTHROPIC_BASE_URL=http://127.0.0.1:8788 ANTHROPIC_AUTH_TOKEN=<printed token> claude -p "Reply with OK"`
      → a normal answer; `creds list` shows the request and `EXPIRES` unchanged (no refresh).
   6. Stop the server and delete `~/.cache/claude-proxy/ac12`. The token stays valid at Anthropic
      (the proxy cannot revoke it); that is the accepted exposure.
3. Merge order: `fix/anthropic-login-lifetime` → `main`, fast-forward
   (`git checkout main && git merge --ff-only fix/anthropic-login-lifetime`); history is linear.
   The owner's uncommitted `CLAUDE.md` edit travels with the checkout and stays uncommitted.
4. Release: no version bump, changelog or tag. CI publishes `ghcr.io/p4u/claude-proxy:latest`
   from `main`, and Watchtower deploys it to production (claude.vocdoni.net) automatically.
5. **Owner OK required before:**
   - `git push origin main` (publishes `:latest`; Watchtower deploys it to production).
   - Any production reconnect or `creds` write (the owner does the reconnects).
6. Post-release checks:
   - `docker exec claude-proxy claude-proxy creds list --db /data/proxy.db` prints the
     `LOGIN_ENDS` and `REFRESH_UNTIL` columns; existing rows show `-`; no row is `expired`.
   - The owner reconnects the 9 revoked team credentials and pau@vocdoni.io with *Update tokens →
     Sign in with Claude*. Each then shows status `active`, `EXPIRES` about a year out and a
     `REFRESH_UNTIL` about 27 days out, with its id, weight and history kept.
   - `docker logs --since 1h claude-proxy 2>&1 | grep -E 'token refresh failed|login rejected|long-lived token rejected|refreshed upstream but not stored'`
     → no lines.
   - Canary (spec Rollout): the day after the first reconnected credential's `REFRESH_UNTIL`
     (about 27 days after the reconnects), the owner checks that it still serves: `creds list`
     shows it `active` with `EXPIRES` unchanged, the logs have no "long-lived token rejected
     before its expiry" for it, and `request_log` has no 401 on it since the deadline
     (the host has no sqlite3: `echo "SELECT COUNT(*) FROM request_log WHERE credential_id='<id>' AND status_code=401 AND ts >= <deadline unix>;" | docker run --rm -i -v /home/vocdoni/claude-proxy/data:/data alpine sh -c "apk add -q sqlite >/dev/null 2>&1 && sqlite3 -readonly /data/proxy.db"`
     → `0`). Record the result in CLAUDE.md under *Long-lived logins*. If it died at the deadline,
     "login ends" switches to the deadline (follow-up change).
