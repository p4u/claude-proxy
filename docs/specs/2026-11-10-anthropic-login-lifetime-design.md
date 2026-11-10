# Anthropic login lifetime: design

Status: audited (2026-11-10)   Class: architectural
Owner decisions so far:
- 2026-11-10: web sign-in moves to long-lived logins (only the scopes the proxy uses, plus a
  1-year access token at the code exchange), with each login's deadline stored and shown.
- 2026-11-10: delivered through the agentic-dev delivery path.

## Problem

Every Anthropic credential added through the web UI's *Sign in with Claude* dies about four
weeks later and is marked `revoked`. In production (claude.vocdoni.net, queried read-only on
2026-11-10):

- All 9 credentials signed in on 2026-10-06/07 were revoked between 2026-11-03 16:13 and
  2026-11-05 23:21 UTC, 27.4 to 29.4 days after creation. pau@vocdoni.io died the same way
  (last request 2026-11-04 07:43) and was reconnected by the owner at 2026-11-10 15:12.
- The 3 `max` credentials imported from `.credentials.json` in June–July (tarat, ops1,
  logins1) are still active.
- `request_log` shows **zero** upstream 401s for these credentials in the days before they
  died. The access tokens kept working, and each death was the token endpoint refusing the
  refresh token. The only Anthropic path that writes `revoked` is
  `internal/creds/refresh.go:148-160`.
- The revoked rows' `expires_at` values all fall on second `:37`, the phase of that process's
  60 s refresh tick (`refresh.go:208-241`). So the background loop performed the fatal refresh.
  It discards the error (`refresh.go:238`, `_, _ = r.Refresh(...)`), so nothing was logged.

Cause: every Anthropic OAuth login carries an absolute deadline, which the token endpoint
reports as `refresh_token_expires_in`. A refresh issues a new 8-hour access token but never
moves the deadline. Once it passes, the next refresh is refused (400). The proxy ignores the
field, so a credential dies without warning.

Evidence for the mechanism:
- Claude Code 2.1.318 stores `refreshTokenExpiresAt` and on every refresh keeps
  `min(previous, new)`. When no previous value is stored (older logins), it records none.
- anthropics/claude-code#92127 and rynfar/meridian#1260 report the same behaviour.
- Probe on 2026-11-10, run against claudio@vocdoni.io with scopes `user:profile user:inference`
  and `expires_in: 31536000` at the code exchange:
  - The exchange returned `expires_in` 31536000 (365 days), a refresh token, and
    `refresh_token_expires_in` 2352492 (27.23 days).
  - A refresh with `expires_in: 31536000` returned `expires_in` **28800** (8 h), and the
    deadline was unchanged (still 2026-12-07 21:08 UTC).
  - So a long access-token lifetime is honoured **only at the code exchange**, never on a
    refresh. Any refresh permanently drops a long-lived credential back to 8-hour tokens and
    to the ~27-day deadline.

Adjacent defects make deaths silent, permanent when they should not be, or, once tokens are
long-lived, self-inflicted:
- After an upstream 401, any `RefreshNow` failure marks the credential `expired`
  (`internal/proxy/proxy.go:577-584`). That includes transient ones: network errors, 5xx, 429,
  or the client cancelling `r.Context()`. The loop then skips `expired` forever
  (`refresh.go:234`).
- Every upstream 401 forces a refresh whatever caused it (`proxy.go:570-577`). Request headers
  and path come from the client (`copyHeaders`, `proxy.go:443`), so a 401 can describe the
  request rather than the token. The background catalogue fetch takes the same branch
  (`models1m.go:772`).
- The manual refresh in the web UI (`internal/webui/creds.go:102`) and the TUI
  (`internal/tui/update.go:412`) calls the forced `RefreshNow`.
- Every import and backup restore rotates the token before storing it
  (`internal/ingest/import.go:83,149`; `creds import-bulk` goes through `ingest.Import`,
  `cmd/claude-proxy/main.go:866`).

## Goals
1. A credential added or reconnected through *Sign in with Claude* keeps serving for the access
   token's lifetime (one year when Anthropic grants it), with no proactive refresh before then.
2. The proxy never refreshes an Anthropic credential whose access token still works. A refresh
   happens only when the stored expiry is within 5 minutes, or when upstream answered 401
   **and** a liveness check made with the proxy's own headers confirms that the token itself is
   rejected.
3. Each Anthropic credential's login deadline is stored when Anthropic reports it. The web UI
   and `creds list` show when the login ends and until when it can refresh, and the web UI
   warns before the login ends.
4. A transient refresh failure never takes a credential out of the pool permanently. Only the
   token endpoint rejecting the grant does.
5. Refresh failures and revocations are logged with the credential's label and the cause,
   whichever path triggered them.
6. A refresh that Anthropic has completed, or a sign-in it has accepted, cannot be lost because
   a client disconnects or the database is briefly busy.

## Non-goals
- Re-authenticating automatically. A new login always needs a human approval in a browser.
- Extending a login past its deadline. Verified impossible (see Problem).
- Making `.credentials.json` logins long-lived. Those logins come from Claude Code's own
  `/login`; the proxy only records their deadline and, for files that are already long-lived,
  stops rotating them (D2a).
- The TUI's credentials table columns. The web UI and `creds list` are the surfaces. Only the
  TUI's manual refresh and its messages change (D3, D3b).
- Recovering the credentials that are already revoked. Each needs one reconnect after deploy
  (Rollout).
- Serialising refreshes across processes (TUI or CLI against the same DB). This is
  pre-existing, and it matters less once manual refresh no longer forces.
- Any change to non-Anthropic providers' behaviour, the Codex/Gemini sidecar, pool selection or
  scoring, or `GET /admin/credentials`.

## Current state
- Sign-in, `internal/claudeoauth/claudeoauth.go`:
  - `Scopes` (l.46-49) are `org:create_api_key user:profile user:inference
    user:sessions:claude_code user:mcp_servers user:file_upload user:plugins`.
  - `Start` builds the authorize URL (l.113-121).
  - `exchange` (l.212-248) posts a `map[string]string` body with no `expires_in`, and requires
    the access token, the refresh token and `expires_in > 0` (l.244).
  - `tokenResp` (l.202-210) has no `refresh_token_expires_in`.
  - `Tokens` (l.61-68) has no deadline.
  - `Exchange` sets `ExpiresAt = now + expires_in − 5 min` (l.195).
  - `exchangeClaudeLogin` (`internal/webui/claudelogin.go:64-81`) runs the exchange and the store
    on `r.Context()`.
- Storage, `credentials` (`internal/store/schema.go:4-26`):
  - It has `expires_at` but no deadline column.
  - New columns are added in the CREATE TABLE plus the swallow-duplicate ALTER list
    (`internal/store/store.go:64-119`).
  - `creds.SelectCols` and `scanCred` (`internal/creds/creds.go:298-357`) are shared with
    `internal/pool/pool.go:639-642` and must stay in step.
- Writers of Anthropic tokens:
  - `insert` (`creds.go:186-222`), reached through `InsertWithRateLimitTier`.
  - `UpdateTokens` (`creds.go:444-449`), which has no production caller, only tests.
  - `UpdateTokensWithSubscription` (`creds.go:455-476`).
  - `replaceTokensIfLineage` (`refresh.go:184-193`), which always sets `status='active'`.
  - The ingest paths call them: `insertVerified` and `updateVerified`
    (`internal/ingest/import.go:64-95,144-160`), which first rotate the token with the
    stateless `creds.RefreshTokens` (`refresh.go:63-99`, on `http.DefaultClient` with no
    timeout); `ImportOAuth` and `UpdateFromOAuth` (`import.go:162-195`) store sign-in tokens.
  - The `.credentials.json` block (`import.go:15-22`) does not read `refreshTokenExpiresAt`.
  - `creds export` and `creds import-bulk` (`main.go:776-881`) round-trip tokens through
    `ingest.Import`, so they take the `insertVerified` path. `exportLine` has no deadline.
- Refresh, `internal/creds/refresh.go`:
  - `refresh(ctx, id, force)` (l.116-180) skips only when `!force`, the token is valid for more
    than 5 minutes, and `Status == active` (l.125). It never checks `Refreshable`.
  - On 400/401 it revokes when the lineage still matches (l.148-160).
  - It writes new tokens with the caller's `ctx` (l.176) and no retry.
  - A 200 without both tokens returns an error that embeds the raw body (l.170-171).
  - Responses are read without a size cap (l.81,146).
  - `refreshResp` (l.52-58) has no deadline field. The `Refresher` has no logger.
- Callers of the forced `RefreshNow`: `proxy.go:577` (upstream 401), `webui/creds.go:102`
  (`POST /api/credentials/{id}/refresh`) and `tui/update.go:412`. The CLI's `creds refresh`
  (`cmd/claude-proxy/main.go:765`) uses the non-forced `Refresh`.
- Pool: `activeCandidates` (`pool.go:368`) and `rebalanceCandidates` (`rebalance.go:490`) skip
  a credential whose `retry_after` is in the future. `pool.go:206` rebinds off `expired`,
  `revoked` and `disabled`.
- Display:
  - `webui/creds.go` `credView` (l.20-50) has `expires_at` and `status`.
  - `static/js/pages/credentials.js:278,297` renders "expires"; l.245 toasts "Token refreshed";
    l.508 says "The proxy keeps its token refreshed for you".
  - `static/js/mock.js:645-654` mocks `/credentials`; l.957 mocks the refresh action.
  - `creds list` prints an EXPIRES column (`main.go:678-696`).

## Design

### D1. Long-lived sign-in (`internal/claudeoauth`)
- `Scopes = []string{"user:profile", "user:inference"}`. Production traffic over Anthropic
  credentials in the last 30 days was only `/v1/messages`, `/v1/messages/count_tokens` and
  `/v1/models` (449,403 requests). `user:profile` serves `/api/oauth/profile` (plan detection)
  and `/api/oauth/usage` (selection scoring); both verified 200 with this scope pair.
- The code-exchange body becomes `map[string]any` and adds `"expires_in": 31536000` as a JSON
  number. It is a package constant `LoginLifetimeSeconds = 31536000`.
- `tokenResp` gains `RefreshTokenExpiresIn int` (`json:"refresh_token_expires_in"`). `Tokens`
  gains `RefreshTokenExpiresAt time.Time`: `now + refresh_token_expires_in` when the value is
  > 0, otherwise the zero time.
- Validation and `ExpiresAt` are computed as today, from the **response's** `expires_in`. If
  Anthropic grants less than requested, the shorter lifetime is stored and nothing fails.
- The comment on `Scopes` must say why it no longer mirrors Claude Code's set: the sessions
  scope is not needed, and the proxy holds the narrowest grant that serves its traffic.
- `exchangeClaudeLogin` runs the exchange and the store on `context.WithoutCancel(r.Context())`,
  so a browser disconnect cannot discard a login that needs another human approval.
- A liveness helper `creds.CheckToken(ctx, accessToken) error` lives in `internal/creds`,
  because `claudeoauth` already imports `creds` and the refresher needs it:
  - It calls `GET https://api.anthropic.com/api/oauth/profile`, with a 10 s timeout and the
    same headers `claudeoauth.profile()` sends. The URL is a `creds.ProfileURL` var, as a test
    hook.
  - It returns nil on 200, `creds.ErrTokenRejected` on 401, and a descriptive error otherwise.
  - D2a and D3a use it. `claudeoauth.profile()` is unchanged.

### D2. Stored deadline
- New column `credentials.refresh_expires_at INTEGER NOT NULL DEFAULT 0`, in Unix seconds;
  0 means unknown. It is added to the CREATE TABLE and to the ALTER list.
- `Credential.RefreshExpiresAt time.Time`: zero when the column is 0. It is added to
  `SelectCols` and `scanCred` together.
- Write rules, the same as Claude Code's:
  - **Sign-in and reconnect** (`ImportOAuth`, `UpdateFromOAuth`): set it to
    `Tokens.RefreshTokenExpiresAt`, replacing any stored value, because a new login has its
    own deadline. Unknown stores 0.
  - **`.credentials.json` import and update** (`insertVerified`, `updateVerified`): the file's
    `claudeAiOauth.refreshTokenExpiresAt` (Unix ms) when it is > 0. When the import-time
    refresh also returns a deadline, store the earlier of the two. When the file has none,
    store 0. A replacement login replaces the stored value.
  - **Refresh** (`refresh()` and `RefreshTokens`): parse `refresh_token_expires_in`.
    - If the stored value is known and the response carries one, store
      `min(stored, now + value)`.
    - If the stored value has already passed and the refresh succeeded anyway, the stored value
      is wrong: store `now + value` when the response carries one, otherwise 0.
    - If the stored value is unknown, keep it unknown and log the returned value at debug.
    - Apart from the passed-deadline case, a refresh never raises a known deadline.
  - Other inserts (API keys, custom hosts, the Codex/Gemini synthetic rows) leave the column
    default.
- `func (c *Credential) LoginEndsAt() (time.Time, bool)` returns
  `max(ExpiresAt, RefreshExpiresAt)` when the provider is `Refreshable` and `RefreshExpiresAt`
  is known. Otherwise it returns `false`. With a 1-year token this is the access-token expiry
  (an unmeasured premise, see Failure modes); with 8-hour tokens it is the deadline, which may
  be up to 8 h early and errs towards warning sooner.
- `UpdateTokens` is deleted; its tests move to `UpdateTokensWithSubscription`.

### D2a. Imports never rotate a long-lived token
- `oauthBlock` (`import.go:15-22`) and `exportLine` (`main.go:776-788`) gain
  `refreshTokenExpiresAt` (Unix ms, omitted when unknown). `creds export` writes the stored
  deadline.
- In `insertVerified` and `updateVerified`, a file whose `expiresAt` is more than 24 hours away
  is a long-lived login (an 8-hour token never is):
  - It is verified with `creds.CheckToken` using the file's access token, and no
    token-endpoint call is made.
  - The file's access token, refresh token, expiry and deadline are stored unchanged.
  - `creds.ErrTokenRejected` fails the import with "credential is not alive"; any other
    `CheckToken` error fails it with that error.
- Every other file keeps today's refresh-to-verify path.

### D3. Refresh only when the access token stops working
- The non-forced `Refresh` skips the token endpoint whenever the access token is valid for more
  than 5 minutes and the status is not `revoked`. This covers `active`, `limited` and
  `disabled`; today it skips only `active`.
- `refresh()` returns `ErrNotRefreshable` before any network call when
  `provider.Get(c.Provider).Refreshable` is false.
- The web UI's `POST /api/credentials/{id}/refresh`, the TUI's `r` and the CLI's
  `creds refresh` all use `Refresh`. No caller forces a refresh without the D3a check.

### D3a. A 401 refreshes only a token that is really dead
- The upstream-401 branch (`proxy.go:570-587`) calls
  `Refresher.RefreshRejected(ctx, id, rejectedAccessToken)`; `RefreshNow` is removed.
  `RefreshRejected` runs under the per-id mutex:
  1. Re-read the credential. If the stored access token differs from `rejectedAccessToken`,
     another request already refreshed it: return the stored credential, with no token call.
  2. If the stored expiry is more than 5 minutes away, call `creds.CheckToken` with that token.
     - 200: return `ErrTokenAlive`.
     - 401: continue to step 3.
     - Anything else (network error, 403, 429, 5xx): return `ErrTokenAlive` wrapping the cause.
       An unconfirmed 401 never costs a token.
  3. Refresh as `refresh()` does.
- On `ErrTokenAlive` the proxy:
  - relays upstream's 401 status, headers and body to the client;
  - runs `MarkError`, changes no status and does not retry;
  - logs at WARN with `cred`, `label`, `path` and `error_type`.
- The background catalogue fetch (`models1m.go:772`) goes through the same branch and gets the
  same behaviour.

### D3b. Refresher interface
- `Refresh(ctx, id) (c *Credential, refreshed bool, err error)`. `refreshed` is true only when
  this call stored new tokens. `RefreshRejected` returns the same triple.
- Sentinel errors, wrapped with `%w` and matched by callers with `errors.Is`, never by error
  text or a status re-read:
  - `creds.ErrLoginRejected`: the token endpoint answered 400/401 and the credential was marked
    revoked.
  - `creds.ErrTokenAlive` (D3a) and `creds.ErrNotRefreshable` (D3).
  - Every other error is transient.
- The proxy's 502 bodies are fixed strings that carry no refresh error text:
  `proxy: credential revoked by Anthropic; sign in again` and
  `proxy: token refresh failed; retry`.
- The web response adds `"refreshed": bool`, and the UI toast reads "Token refreshed" or
  "Token still valid, nothing to refresh".
- When `refreshed` is false the CLI prints `skipped <id>: access token valid until <RFC3339>`,
  and the TUI shows the same wording. Both print expiries as RFC3339 dates.
- `refresh()` itself writes the D6 log lines, so the loop, the 401 path and manual refresh are
  logged identically.

### D4. Transient failures are not terminal
- A refresh error never sets `StatusExpired`. The status stays whatever `refresh()` left:
  - `revoked` if the token endpoint rejected the lineage (`ErrLoginRejected`);
  - unchanged for network, timeout, 5xx, 429 or decode errors.
- `MarkError` still runs on the 401 path.
- A transient refresh failure sets `retry_after = now + 60 s` on the credential and leaves its
  status alone. `activeCandidates` and `rebalanceCandidates` already skip a credential with a
  future `retry_after`, so new conversations and elective moves avoid it for a minute while
  existing pins keep it.
- The refresher remembers each credential's last transient failure for 30 s and returns it
  without calling the token endpoint, so requests waiting on the per-id mutex fail fast. A
  successful refresh clears it.
- `expired` is no longer produced. At startup, after the ALTER list, one statement heals legacy
  rows: `UPDATE credentials SET status='active' WHERE status='expired' AND
  provider='anthropic'`. A healed row with a dead token is then refreshed or revoked by the
  next `tick` (near expiry) or by its first upstream 401 (D3a). `tick` and `Refresh` need no
  `expired` case.

### D5. A completed refresh is never lost
- After `refresh()` takes the per-id lock it works on `context.WithoutCancel(ctx)`. That covers
  the token request, `revokeIfLineage`, `replaceTokensIfLineage` and the final read.
- Both writes run inside `store.Retry`. The worst case is the 30 s client timeout plus four
  write attempts of up to 15 s each.
- `RefreshTokens` uses a client with the same 30 s timeout.
- The conditional write heals only token-related states:
  `status = CASE WHEN status IN ('revoked','expired') THEN 'active' ELSE status END`.
  `disabled` and `limited` are left as they are.
- If the write still fails after a 200, log at ERROR: "refreshed upstream but not stored; the
  credential will be rejected at its next refresh" (no token values).
- A 200 whose tokens were not stored because the lineage changed is logged at DEBUG as
  "superseded by a newer login", not as a success.
- Not covered: a process stopped between Anthropic's answer and the write. The credential is
  then rejected at its next refresh and needs a reconnect.

### D6. Logging
- `(*Refresher).SetLogger(*slog.Logger)`; without it the refresher discards its logs.
  `NewRefresher(db)` keeps its signature, and `runServe` calls `SetLogger(logger)`.
- `refresh()` logs each failed refresh at WARN (id, label, cause) and each revocation at ERROR
  (id, label, the known deadline, and whether it had passed).
- A successful refresh logs at DEBUG with the new access-token expiry and deadline.
- A liveness-confirmed 401 (D3a) on a credential whose stored expiry is more than 24 hours away
  logs at ERROR "long-lived token rejected before its expiry", with id, label, expiry and
  deadline.
- Errors built by `refresh()` and `RefreshTokens` never include a 2xx body; `refresh.go:171`
  becomes `refresh response missing tokens`. Non-2xx bodies pass through one helper that
  truncates to 512 bytes, and only those appear in errors or logs.
- Token-endpoint responses are read through `io.LimitReader(resp.Body, 1<<20)`.

### D7. Surfaces
- `GET /api/credentials` adds two fields, both RFC3339 UTC:
  - `login_ends_at`, present only when `LoginEndsAt` returns true;
  - `refresh_deadline_at`, present when the deadline is known.
- The credentials page shows a "login ends" stat. It renders with a warning style when under 7
  days away and as "login ended — sign in again" once past.
- Beside it is a "refresh until" stat (the deadline), with no warning style.
- The add-credential blurb (`credentials.js:508`) reads "A Pro/Max/Team/Enterprise login, valid
  for up to a year."
- `mock.js` gains both fields, with one sample row inside 7 days. Its refresh action returns
  `refreshed`, alternating true and false.
- `creds list` adds `LOGIN_ENDS` and `REFRESH_UNTIL` after `EXPIRES`, formatted like it
  (RFC3339), or `-` when unknown.
- `GET /admin/credentials` is unchanged.
- Docs:
  - `docs/WEBUI.md`: the new fields and the `refreshed` response.
  - CLAUDE.md: the *Adding an Anthropic subscription from the web UI* section covers scopes,
    lifetime, the deadline, the never-refresh rule and the liveness check.
  - README backup note: exports now hold year-long tokens.

### Alternatives considered
| Option | Why not |
|---|---|
| Keep 30-day logins and only warn | Rejected by the owner: a re-login per account every ~4 weeks. |
| Send `expires_in` on refresh | Anthropic ignores it (probe 2026-11-10: 28800 returned). |
| Inference-only scope (as in `claude setup-token`) | Loses `user:profile`, so no plan detection and no usage API; selection would run blind. |
| Trust a refresh's deadline for logins with none stored | Claude Code deliberately does not; for older logins the value is not a deadline. |
| Refresh on every upstream 401, as today | A 401 can describe the request (client headers, path), and with 1-year tokens a needless refresh destroys the credential. |

## Compatibility and rollout
- The schema change is additive with a default, so old binaries ignore the column
  (`SelectCols` lists columns explicitly). Rollback is redeploying the previous image, and the
  column stays. While the previous image runs, its manual Refresh button and its 401 path can
  still downgrade a long-lived credential. A reconnect made on it leaves the old deadline in
  place until the next successful refresh after that deadline corrects it.
- `GET /api/credentials` gains optional fields, and the refresh response gains `refreshed`.
  Existing clients ignore both. The `creds export` line gains an optional field that older
  binaries ignore.
- Existing credentials:
  - Legacy logins keep an unknown deadline and behave as today.
  - The 9 revoked team credentials and pau@vocdoni.io need one reconnect each after deploy
    (*Update tokens → Sign in with Claude*). The reconnect keeps id, weight and history.
- Merging to `main` publishes `ghcr.io/p4u/claude-proxy:latest`, which Watchtower deploys to
  production automatically. The push to `main` therefore needs the owner's explicit OK.
- Canary: the day after the first new sign-in's refresh deadline (about 27 days after the
  reconnects), the owner checks that the credential still serves (`creds list`, and
  `request_log` for 401s on it) and records the result in CLAUDE.md. If it died at the
  deadline, "login ends" switches to the deadline.

## Failure modes and security
- If Anthropic stops honouring `expires_in` at the exchange, the credential gets 8-hour tokens,
  its deadline is recorded, and the UI shows the ~27-day end. It degrades to today's behaviour
  but visibly.
- **Unmeasured premise.** No probe has shown a 1-year access token working after its login's
  refresh deadline. Until one production credential passes its deadline and keeps serving,
  Goal 1 is unconfirmed. "Refresh until" stays visible, and an early rejection is logged at
  ERROR (D6), so the failure would not be silent.
- If Anthropic invalidates a long-lived access token early, D3a confirms it and refreshes.
  Before the deadline the credential serves on 8-hour tokens with the deadline shown; after it,
  the credential is revoked and logged.
- If a client sends a request that upstream rejects with 401 for its own reasons, the liveness
  check answers 200 and nothing changes on the credential (D3a).
- **Exposure:**
  - A stored 1-year access token is usable for up to a year if the database leaks, where
    today's refresh token is usable for about 27 days. This is accepted by the owner.
  - The same holds for `creds export` output and backups.
  - The grant is narrower than today's (no `org:create_api_key`, sessions, MCP or plugin
    scopes).
  - The proxy cannot revoke a token at Anthropic, and deleting or reconnecting a credential
    does not invalidate the old token. Whether the account can end such a session from its
    Claude settings is unverified. CLAUDE.md and the README backup note say so.
- Concurrency:
  - The per-id refresh mutex is unchanged.
  - D3a step 1 collapses concurrent 401s on one token into one refresh.
  - The 30 s failure memory (D4) bounds the queue behind a failing token endpoint.
- Logs and errors carry no token values. Only non-2xx token-endpoint bodies (OAuth error JSON)
  are quoted, truncated to 512 bytes.

## Acceptance criteria
- [ ] AC1: The authorize URL's scope is exactly `user:profile user:inference`. The exchange body
  carries `expires_in: 31536000` as a number. `RefreshTokenExpiresAt` and a 365-day
  `ExpiresAt` come from a fake response (`internal/claudeoauth` tests).
- [ ] AC1b: A sign-in whose browser request is cancelled after the exchange still stores the
  credential (`internal/webui` test).
- [ ] AC2: A fresh DB and a DB created without the column both expose `refresh_expires_at`;
  existing rows read as unknown; an `expired` Anthropic row is `active` after startup
  (`internal/store` or `internal/creds` test).
- [ ] AC3: `ImportOAuth` and `UpdateFromOAuth` store the deadline and replace an older one. A
  pasted `.credentials.json` stores `refreshTokenExpiresAt` (min with the import refresh), or
  unknown when absent (`internal/ingest` tests).
- [ ] AC3b: `creds export` then `creds import-bulk` of a credential whose expiry is a year away
  makes no token-endpoint call and preserves its access token, expiry and deadline. A
  profile 401 on such a file fails the import (`internal/ingest` and `cmd/claude-proxy`
  tests).
- [ ] AC4: A refresh with a known deadline stores `min(stored, new)`. A refresh with an unknown
  one stays unknown. A refresh that succeeds after a passed stored deadline stores the new
  value (`internal/creds` tests).
- [ ] AC5: The non-forced `Refresh` makes no token-endpoint call when the token is valid for
  more than 5 min and the status is `active`, `limited` or `disabled`. It does call for
  `revoked`, or near expiry. A non-refreshable provider returns `ErrNotRefreshable` with no
  network call. The web UI refresh endpoint reports `refreshed` (`internal/creds` and
  `internal/webui` tests).
- [ ] AC5b: `tick` makes no token-endpoint call for an `active` credential whose expiry is a
  year away. The TUI's `r` on such a credential makes none either (`internal/creds` and
  `internal/tui` tests).
- [ ] AC6:
  - Upstream 401 while the profile endpoint answers 200: no token-endpoint call. The client
    receives upstream's 401, and the status, access token and expiry are unchanged.
  - Upstream 401 while the profile endpoint answers 401: the refresh runs and the request is
    retried once.
  - Two concurrent upstream 401s on one credential produce exactly one token-endpoint call.
  - Confirmed 401, then a token-endpoint 500 or timeout: the status stays `active`, the
    response is a 502, and `retry_after` is about 60 s ahead. A second confirmed 401 within
    30 s makes no token-endpoint call, and the credential is absent from `activeCandidates`.
  - Confirmed 401, then a token-endpoint 400: the status is `revoked`, never `expired`.
  - Revoked and transient cases return their fixed 502 bodies.
  - (`internal/proxy` and `internal/creds` tests.)
- [ ] AC7: A failed refresh logs one WARN, and a revocation one ERROR, with the label. A
  token-endpoint 200 without a refresh token produces an error and a log line containing
  neither the access token nor any part of the body (`internal/creds` test with a capturing
  slog handler).
- [ ] AC7b: A revocation reached through the upstream-401 path and one reached through
  `POST /api/credentials/{id}/refresh` each log one ERROR line with the label
  (`internal/proxy` and `internal/webui` tests).
- [ ] AC8: The caller's context is cancelled while the fake token endpoint holds the request,
  and the endpoint then answers 200. The new tokens are stored (`internal/creds` test).
- [ ] AC8b: While a second connection holds the write lock for the first two attempts, the
  refresh result is still stored (`internal/creds` test).
- [ ] AC9: `GET /api/credentials` returns `login_ends_at` and `refresh_deadline_at` only when
  known. The credentials page renders both, the under-7-days warning, and the refresh toast
  wording with `?mock=1` (webui test plus a browser check).
- [ ] AC10: `creds list` prints `LOGIN_ENDS` and `REFRESH_UNTIL`. `creds refresh` on a valid
  token prints the skipped line (`cmd/claude-proxy` test or command output).
- [ ] AC11: `make test` and `make lint` pass.
- [ ] AC12: A credential created through the new sign-in flow on a local proxy serves a real
  `claude -p` request, and `creds list` shows an expiry about a year away plus the deadline
  (the owner does the sign-in at the gate).

## Constraints
- Go 1.26.2, standard library first, with no new dependencies. Run `gofmt`, `go vet` and
  `golangci-lint` (`make lint`) and `go test -race ./...` (`make test`).
- Never log or return token values, or any part of a 2xx token-endpoint body. Truncate non-2xx
  bodies to 512 bytes.
- `creds.SelectCols` and `scanCred` change together (the pool reads them).
- The web UI has no build step. Every `/api` change is mirrored in `static/js/mock.js`.
- Anthropic request pass-through stays byte-identical; nothing in `/v1/*` forwarding changes
  except the 401 branch.
- Commits use conventional, lowercase `type(scope): summary` with no AI attribution and no
  `Co-Authored-By` or other trailers. Commit explicit paths only, and never push.

## Open questions
None.
