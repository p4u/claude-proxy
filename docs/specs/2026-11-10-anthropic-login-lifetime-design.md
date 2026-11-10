# Anthropic login lifetime: design

Status: draft   Class: architectural
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

Two adjacent defects make deaths silent, or permanent when they should not be:
- After an upstream 401, any `RefreshNow` failure marks the credential `expired`
  (`internal/proxy/proxy.go:577-584`). That includes transient ones: network errors, 5xx, 429,
  or the client cancelling `r.Context()`. The loop then skips `expired` forever
  (`refresh.go:234`).
- The manual refresh in the web UI (`internal/webui/creds.go:102`) and the TUI
  (`internal/tui/update.go:412`) calls the forced `RefreshNow`. On a long-lived credential that
  would destroy its 1-year token.

## Goals
1. A credential added or reconnected through *Sign in with Claude* keeps serving for the access
   token's lifetime (one year when Anthropic grants it), with no proactive refresh before then.
2. The proxy never refreshes an Anthropic credential whose access token is still valid, unless
   upstream rejected that token with a 401.
3. Each Anthropic credential's login deadline is stored when Anthropic reports it. The web UI
   and `creds list` show when the login ends, and the web UI warns ahead of time.
4. A transient refresh failure never takes a credential out of the pool permanently. Only the
   token endpoint rejecting the grant does.
5. Refresh failures and revocations are logged with the credential's label and the cause.
6. A refresh that Anthropic has completed cannot be lost, either because the requesting client
   disconnects or because the database is briefly busy.

## Non-goals
- Re-authenticating automatically. A new login always needs a human approval in a browser.
- Extending a login past its deadline. Verified impossible (see Problem).
- Changing `.credentials.json` imports to long-lived tokens. Those logins come from Claude
  Code's own `/login`; the proxy can only record their deadline.
- The TUI's credentials table. The web UI and `creds list` are the surfaces; the TUI keeps its
  current columns. Only its manual refresh changes (Goal 2).
- Recovering the credentials that are already revoked. Each needs one reconnect after deploy
  (Rollout).
- Serialising refreshes across processes (TUI or CLI against the same DB). This is
  pre-existing, and it matters less once manual refresh no longer forces.
- Any change to non-Anthropic providers, the Codex/Gemini sidecar, pool selection or scoring.

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
- Storage, `credentials` (`internal/store/schema.go:4-26`):
  - It has `expires_at` but no deadline column.
  - New columns are added in the CREATE TABLE plus the swallow-duplicate ALTER list
    (`internal/store/store.go:62-116`).
  - `creds.SelectCols` and `scanCred` (`internal/creds/creds.go:299-357`) are shared with
    `internal/pool/pool.go:635-641` and must stay in step.
- Writers of Anthropic tokens:
  - `insert` (`creds.go:195-221`), reached through `InsertWithRateLimitTier`.
  - `UpdateTokens` (`creds.go:444-449`).
  - `UpdateTokensWithSubscription` (`creds.go:455-476`).
  - `replaceTokensIfLineage` (`refresh.go:184-193`).
  - The ingest paths call them: `insertVerified` and `updateVerified`
    (`internal/ingest/import.go:64-95,144-160`), which first rotate the token with the
    stateless `creds.RefreshTokens` (`refresh.go:63-99`); `ImportOAuth` and `UpdateFromOAuth`
    (`import.go:162-195`) store sign-in tokens.
  - The `.credentials.json` block (`import.go:15-22`) does not read `refreshTokenExpiresAt`.
- Refresh, `internal/creds/refresh.go`:
  - `refresh(ctx, id, force)` (l.116-180) skips only when `!force`, the token is valid for more
    than 5 minutes, and `Status == active` (l.125).
  - On 400/401 it revokes when the lineage still matches (l.148-160).
  - It writes new tokens with the caller's `ctx` (l.176) and no retry.
  - `refreshResp` (l.52-58) has no deadline field.
  - The `Refresher` has no logger.
- Callers of the forced `RefreshNow`: `proxy.go:577` (upstream 401), `webui/creds.go:102`
  (`POST /api/credentials/{id}/refresh`) and `tui/update.go:412`. The CLI's `creds refresh`
  (`cmd/claude-proxy/main.go:765`) uses the non-forced `Refresh`.
- Display:
  - `webui/creds.go` `credView` (l.25-52) has `expires_at` and `status`.
  - `static/js/pages/credentials.js:278,297` renders "expires".
  - `static/js/mock.js:646-655` mocks `/credentials`.
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
    - If the stored value is unknown, keep it unknown and log the returned value at debug.
    - A refresh never raises a known deadline.
  - Other inserts (API keys, custom hosts, the Codex/Gemini synthetic rows) leave the column
    default.
- `func (c *Credential) LoginEndsAt() (time.Time, bool)` returns
  `max(ExpiresAt, RefreshExpiresAt)` when the provider is `Refreshable` and `RefreshExpiresAt`
  is known. Otherwise it returns `false`. With a 1-year token this is the access-token expiry;
  with 8-hour tokens it is the deadline, which may be up to 8 h early and errs towards warning
  sooner.

### D3. Refresh only when the access token stops working
- The non-forced `Refresh` skips the token endpoint whenever the access token is valid for more
  than 5 minutes and the status is neither `revoked` nor `expired`. This covers `active`,
  `limited` and `disabled`; today it skips only `active`.
- `RefreshNow` (forced) is called only by the upstream-401 path in `proxy.go`.
- The web UI's `POST /api/credentials/{id}/refresh` and the TUI's `r` switch to `Refresh`. The
  web response adds `"refreshed": bool`, false when it was skipped, and the UI shows a short
  note when it was. A 401 means the token is already unusable, so refreshing there costs
  nothing and keeps the credential serving until its deadline.

### D4. Transient failures are not terminal
- In the upstream-401 branch (`proxy.go:569-587`), a `RefreshNow` error no longer sets
  `StatusExpired`. The status stays whatever `refresh()` left:
  - `revoked` if the token endpoint rejected the lineage (400/401);
  - unchanged for network, timeout, 5xx, 429 or decode errors.
- `MarkError` still runs. The client gets a 502 whose message distinguishes "credential revoked
  by Anthropic; sign in again" from "token refresh failed; retry".
- `tick` stops skipping `expired`. Nothing produces `expired` any more, so existing `expired`
  rows are retried: a successful refresh heals them to `active`, and a rejected one revokes
  them.

### D5. A completed refresh is never lost
- In `refresh()`, the token request and the write of its result run under
  `context.WithoutCancel(ctx)`, bounded by the existing 30 s client timeout. A client
  disconnecting on the 401 path cannot abort a refresh after Anthropic has rotated the refresh
  token.
- The conditional write (`replaceTokensIfLineage`) runs inside `store.Retry`.
- If the write still fails after a 200, log at error level: "refreshed upstream but not stored;
  the credential will be rejected at its next refresh" (no token values).

### D6. Logging
- `NewRefresher(db, logger)`: the refresher takes a `*slog.Logger`, nil meaning discard.
- `tick` logs each failed refresh at WARN (id, label, cause) and each revocation at ERROR (id,
  label, the known deadline, and whether it had passed).
- A successful refresh logs at DEBUG with the new access-token expiry and deadline.
- Upstream error bodies are truncated to 512 bytes. Token values are never logged.

### D7. Surfaces
- `GET /api/credentials` adds `login_ends_at` (RFC3339 UTC), present only when `LoginEndsAt`
  returns true.
- The credentials page shows a "login ends" stat for those rows. It renders with a warning
  style when under 7 days away and as "login ended — sign in again" once past.
- `mock.js` gains the field, with one sample row inside 7 days.
- `creds list` adds a `LOGIN_ENDS` column: the date, or `-` when unknown.
- Docs:
  - `docs/WEBUI.md`: the new field and the `refreshed` response.
  - CLAUDE.md: the *Adding an Anthropic subscription from the web UI* section covers scopes,
    lifetime, the deadline and the never-refresh rule.

### Alternatives considered
| Option | Why not |
|---|---|
| Keep 30-day logins and only warn | Rejected by the owner: a re-login per account every ~4 weeks. |
| Send `expires_in` on refresh | Anthropic ignores it (probe 2026-11-10: 28800 returned). |
| Inference-only scope (as in `claude setup-token`) | Loses `user:profile`, so no plan detection and no usage API; selection would run blind. |
| Trust a refresh's deadline for logins with none stored | Claude Code deliberately does not; for older logins the value is not a deadline. |

## Compatibility and rollout
- The schema change is additive with a default, so old binaries ignore the column
  (`SelectCols` lists columns explicitly). Rollback is redeploying the previous image; the
  column stays and is harmless.
- `GET /api/credentials` gains an optional field, and the refresh response gains `refreshed`.
  Existing clients ignore both.
- Existing credentials:
  - Legacy logins keep an unknown deadline and behave as today.
  - The 9 revoked team credentials and pau@vocdoni.io need one reconnect each after deploy
    (*Update tokens → Sign in with Claude*). The reconnect keeps id, weight and history.
- Merging to `main` publishes `ghcr.io/p4u/claude-proxy:latest`, which Watchtower deploys to
  production automatically. The push to `main` therefore needs the owner's explicit OK.

## Failure modes and security
- If Anthropic stops honouring `expires_in` at the exchange, the credential gets 8-hour tokens,
  its deadline is recorded, and the UI shows the ~27-day end. It degrades to today's behaviour
  but visibly.
- If Anthropic invalidates a long-lived access token early, the upstream 401 triggers a refresh.
  Before the deadline it serves on 8-hour tokens with the deadline shown; after it, the
  credential is revoked and logged.
- **Exposure:** a stored 1-year access token is usable for up to a year if the database leaks,
  where today's refresh token is usable for about 27 days. This is accepted by the owner. The
  grant is narrower than today's (no `org:create_api_key`, sessions, MCP or plugin scopes).
- Concurrency:
  - The per-id refresh mutex is unchanged.
  - `WithoutCancel` lengthens a refresh at most to the existing 30 s timeout.
  - `store.Retry` uses the existing jittered backoff.
- Logs and errors carry no token values. Truncated upstream bodies are OAuth error JSON.

## Acceptance criteria
- [ ] AC1: The authorize URL's scope is exactly `user:profile user:inference`. The exchange body
  carries `expires_in: 31536000` as a number. `RefreshTokenExpiresAt` and a 365-day
  `ExpiresAt` come from a fake response (`internal/claudeoauth` tests).
- [ ] AC2: A fresh DB and a DB created without the column both expose `refresh_expires_at`, and
  existing rows read as unknown (`internal/store` or `internal/creds` test).
- [ ] AC3: `ImportOAuth` and `UpdateFromOAuth` store the deadline and replace an older one. A
  pasted `.credentials.json` stores `refreshTokenExpiresAt` (min with the import refresh), or
  unknown when absent (`internal/ingest` tests).
- [ ] AC4: A refresh with a known deadline stores `min(stored, new)`. A refresh with an unknown
  one stays unknown (`internal/creds` tests).
- [ ] AC5: The non-forced `Refresh` makes no token-endpoint call when the token is valid for
  more than 5 min and the status is `active`, `limited` or `disabled`. It does call for
  `revoked` or `expired`, or near expiry. The web UI refresh endpoint and the TUI use it, and
  the web response reports `refreshed` (`internal/creds` and `internal/webui` tests).
- [ ] AC6:
  - Upstream 401 then a token-endpoint 500 or timeout leaves the status `active` and returns 502.
  - Upstream 401 then a token-endpoint 400 leaves the status `revoked`, never `expired`.
  - (`internal/proxy` tests.)
- [ ] AC7: `tick` logs a failed refresh at WARN and a revocation at ERROR with the label. An
  `expired` credential is retried (`internal/creds` test with a capturing slog handler).
- [ ] AC8: The caller's context is cancelled while the fake token endpoint holds the request,
  and the endpoint then answers 200. The new tokens are stored (`internal/creds` test).
- [ ] AC9: `GET /api/credentials` returns `login_ends_at` only when known. The credentials page
  renders it and the under-7-days warning with `?mock=1` (webui test plus a browser check).
- [ ] AC10: `creds list` prints `LOGIN_ENDS` (`cmd/claude-proxy` test or command output).
- [ ] AC11: `make test` and `make lint` pass.
- [ ] AC12: A credential created through the new sign-in flow on a local proxy serves a real
  `claude -p` request, and `creds list` shows an expiry about a year away plus the deadline
  (the owner does the sign-in at the gate).

## Constraints
- Go 1.26.2, standard library first, with no new dependencies. Run `gofmt`, `go vet` and
  `golangci-lint` (`make lint`) and `go test -race ./...` (`make test`).
- Never log or return token values. Truncate upstream bodies to 512 bytes.
- `creds.SelectCols` and `scanCred` change together (the pool reads them).
- The web UI has no build step. Every `/api` change is mirrored in `static/js/mock.js`.
- Anthropic request pass-through stays byte-identical; nothing in `/v1/*` forwarding changes
  except the 401 branch.
- Commits use conventional, lowercase `type(scope): summary` with no AI attribution and no
  `Co-Authored-By` or other trailers. Commit explicit paths only, and never push.

## Open questions
None.
