# Claudio plugins and browser login: design

Status: draft   Class: architectural
Owner decisions so far (2026-11-06):
- Plugin content (skills, agents, hooks, MCP list) lives in a git repo and is released as a zip; the proxy serves only the pinned bundle reference and the secret env.
- `claudio proxy login` uses a device-style flow: one-time code, browser sign-in at the proxy, polling.
- A verified `@vocdoni.org` account with no user token is auto-created on first sign-in.
- The first plugin is named `vocdoni-internal` (a `vocdoni-public` may follow).

This is sub-project 1 of 2. Sub-project 2 is the claudio side
(`claudio/docs/specs/2026-11-06-session-plugins-design.md`), which consumes the
API defined here.

## Problem

A new Vocdoni member who installs claudio today needs an admin to create a
proxy user token on the Users page, paste it into `claudio proxy login`, then
install the team's skills, plugins and MCP servers by hand, and obtain every
MCP server's API key separately. Nothing in the proxy knows which person a
user token belongs to: `user_tokens` has `id`, `name`, `token`, `status`
(`internal/store/schema.go:50-66`) and no e-mail, although the web UI already
signs admins in through oauth.vocdoni.org (`internal/webui/sso.go`,
`cmd/claude-proxy/main.go:345 ssoFromEnv`).

## Goals

1. A member runs `claudio proxy login`, signs in with their Google account in
   the browser, and claudio holds a per-user proxy token for them. No admin
   step for a verified `@vocdoni.org` account.
2. The proxy tells an authenticated claudio which plugins to load: for each,
   a pinned bundle (URL + SHA-256) and the environment the bundle's MCP
   servers and hooks need, including secrets.
3. Admins manage plugins and their secrets from the proxy CLI and the web
   UI, never from a repo.
4. Every user token can carry the member's e-mail, shown on the Users page.

## Non-goals

- Serving or building plugin zips. The bundle is a release asset of the
  content repo; the proxy only stores its URL and hash.
- Per-member secrets (personal GitHub tokens etc.). Plugin env is shared by
  every member with access to the plugin.
- A central logout, refresh tokens, or any change to oauth.vocdoni.org.
- Changing how model traffic is authenticated or routed.

## Current state

- Downstream auth: `internal/proxy/auth.go:28 AuthMiddleware`. A bearer or
  `X-Api-Key` credential is an admin token or a `user_tokens.token`; a user
  identity is allowed on `/v1/*`, `/openai/v1/*`, `/health`, and gets 403 on
  `/admin/*`. An invalid presented credential is always 401.
- Claudio API: `internal/claudioapi/claudioapi.go`. `New` (:103) registers
  the routes; `WrapHandler` (:145) claims every raw path starting with
  `/v1/claudio` before mux canonicalisation; `ServeHTTP` (:163-206) rejects
  encoded paths, `..` and `//`, rate-limits 1 req/s burst 10 per identity
  (:178-181), and 404s unknown paths. `me/stats` (:492) is the one route
  that requires a user identity.
- Web UI SSO: `internal/webui/sso.go`. `NewSSO` (:55) builds a
  `vocauth.Auth` from `SSOConfig`; `handleSSOSession` (:162) and
  `ssoAuthorize` (:177) protect UI routes; `user()` (:103) returns the
  signed-in `vocauth.User` (e-mail, name). Sessions last 7 days. Without
  `OAUTH_CLIENT_ID` the UI falls back to `UI_PASSWORD`.
- User tokens: `internal/usertoken/usertoken.go`. `Create(ctx, db, name)`
  (:89) with a unique `name`, `LookupByToken` (:145), `SetStatus` (:161),
  limits in `limit.go`. CLI: `claude-proxy users create|limit|list|...`
  (`cmd/claude-proxy/main.go:82`). Web UI: `internal/webui/users.go`.
- Schema migrations are `ALTER TABLE … ADD COLUMN` statements run at open,
  each ignored when the column exists (`internal/store/store.go:65-71`).

## Design

Three additions to the proxy, all behind the existing `/v1/claudio` guard
and the existing SSO.

### 1. E-mail on user tokens

`user_tokens` gets `email TEXT` (nullable, unique when not null, stored
lower-cased) and `UserToken.Email string`. New functions:
`usertoken.LookupByEmail(ctx, db, email)` and
`usertoken.CreateForEmail(ctx, db, email, name)`. The Users page and
`claude-proxy users list` show the e-mail. `users create` gains
`--email`. Existing tokens keep a null e-mail; an admin can set one with
`claude-proxy users email <id> <email>`.

### 2. Device-style login

Available only when SSO is configured; otherwise the three routes answer
404 (same as `/api/login` today, `README.md:862`).

```
claudio                        proxy                          browser
  │ POST /v1/claudio/login/start │                              │
  │─────────────────────────────▶│ creates {user_code, device_code}
  │◀── {device_code, user_code,  │                              │
  │     verify_url, interval, ttl}                              │
  │ opens / prints verify_url ───────────────────────────────▶ │
  │                              │◀── GET /claudio/login?code=UC │
  │                              │  (SSO sign-in if needed)     │
  │                              │──▶ page: "Approve claudio    │
  │                              │     sign-in? code UC"        │
  │                              │◀── POST /claudio/login (same-origin, approve)
  │                              │  user = find/create by e-mail│
  │ POST /v1/claudio/login/poll  │  mark code approved          │
  │  {device_code} (every 3 s)   │                              │
  │◀── 200 {token, email, name}  │ code consumed                │
```

- `POST /v1/claudio/login/start` (no credential; per-IP limit 5/min, burst
  5). Reply `{device_code, user_code, verify_url, interval_s: 3,
  expires_in_s: 600}`. `device_code`: 32 random bytes, base64url, the only
  thing that can redeem the login. `user_code`: 8 characters from
  `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`, shown as `XXXX-XXXX`. `verify_url` is
  `<public base URL>/claudio/login?code=<user_code>`. Pending logins live
  in memory (map guarded by a mutex, swept every minute); at most 256
  pending at a time, else 429. A restart drops them; the member retries.
- `GET /claudio/login?code=` (browser, protected by the UI's SSO exactly as
  other UI pages: `ssoAuthorize`). Unknown or expired code: a page saying
  so. Otherwise a page that shows the code in large type, the signed-in
  e-mail, the text "Approve only if this code is shown in your terminal",
  and a same-origin POST form `approve` / `deny` (CSRF as the UI's other
  forms: origin check `sameOrigin`, `sso.go:122`).
- `POST /claudio/login` (`code`, `action`). On `approve`: look the e-mail
  up; if missing, create the user with `name = email` and the default
  limit from `CLAUDIO_DEFAULT_LIMIT` (`<tokens>/<window>`, e.g. `1M/24h`;
  unset means no limit) and log `login: created user <email>`. A disabled
  user is refused ("account disabled, contact an admin"). The pending
  login is marked approved with the user's id. On `deny`: marked denied.
  Each code is decided once.
- `POST /v1/claudio/login/poll` body `{device_code}` (no credential; the
  per-IP limit above, plus the code's `interval_s`: a poll sooner than 3 s
  after the previous one answers 429 `slow_down`). Replies: 428
  `authorization_pending`; 200 `{token, email, name}` exactly once, after
  which the login is deleted; 410 `expired`; 403 `denied`; 404 unknown
  code. The token returned is the user's current token (an approved login
  never rotates it).

The login routes are the only `/v1/claudio` routes without a credential.
They never touch `request_log` and never forward upstream.

### 3. Plugins

Tables:

```sql
CREATE TABLE IF NOT EXISTS plugins (
  name        TEXT PRIMARY KEY,   -- [a-z0-9-]{1,40}
  version     TEXT NOT NULL,      -- free text, e.g. "1.4.0"
  bundle_url  TEXT NOT NULL,      -- https only
  bundle_sha256 TEXT NOT NULL,    -- 64 hex chars
  installed_id TEXT NOT NULL DEFAULT '', -- e.g. "vocdoni-internal@vocdoni"
  enabled     INTEGER NOT NULL DEFAULT 1,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS plugin_env (
  plugin  TEXT NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
  key     TEXT NOT NULL,          -- [A-Z_][A-Z0-9_]{0,63}
  value   TEXT NOT NULL,          -- ≤ 4 KiB
  PRIMARY KEY (plugin, key)
);
```

`installed_id` tells claudio which installed Claude Code plugin makes the
bundle redundant (the member installed it from the marketplace). Values in
`plugin_env` are stored like the other secrets in this DB (user tokens,
provider refresh tokens): plaintext in the SQLite file the proxy already
protects.

Endpoint `GET /v1/claudio/plugins` (user token only, like `me/stats`;
admin and anonymous get 403):

```json
{"plugins": [{
  "name": "vocdoni-internal",
  "version": "1.4.0",
  "bundle": {"url": "https://github.com/vocdoni/skills/releases/download/v1.4.0/vocdoni-internal.zip",
             "sha256": "…64 hex…"},
  "installed_id": "vocdoni-internal@vocdoni",
  "env": {"VOCDONI_KB_TOKEN": "…", "NOTION_TOKEN": "…"}
}]}
```

Disabled plugins are omitted. The reply has `Cache-Control: no-store`.
Reads are logged at debug level without values.

Admin CLI (`claude-proxy plugins …`, admin token or local DB like
`users`):

```
plugins list
plugins set <name> --version <v> --url <https url> --sha256 <hex> [--installed-id <id>]
plugins enable|disable|delete <name>
plugins env set <name> KEY=VALUE [KEY=VALUE…]   # value may also be read from stdin with KEY=-
plugins env unset <name> KEY…
plugins env list <name>                           # keys only
```

Web UI: a **Plugins** page (admin, behind SSO/password like Users) with the
same operations; values are entered in password fields and never displayed
after save (keys only, with a "replace" action).

### Alternatives considered

| Option | Why not |
|---|---|
| claudio as an OIDC client of oauth.vocdoni.org | The service admits confidential web clients only, ID tokens live 10 min, no refresh tokens. |
| Localhost callback instead of polling | Fails over SSH or when the browser is on another machine; polling would be needed anyway. |
| Proxy stores and serves the zip | Plugin edits would bypass git review; the proxy would grow a file store. |
| Per-member secrets | Not needed for the first plugin; shared team keys only. Can be added as a `user_plugin_env` table later. |

## Compatibility and rollout

- API additions only. Old claudio clients ignore unknown routes; old
  proxies answer the new routes with the existing local 404, which claudio
  (sub-project 2) turns into "proxy too old".
- Schema: two new tables and one `ALTER TABLE user_tokens ADD COLUMN email`,
  in the existing migration list. Rollback: the previous image ignores the
  extra column and tables.
- Rollout: deploy the proxy first (nothing changes for current users), then
  register the `vocdoni-internal` plugin and its env, then ship claudio.
- Without `OAUTH_CLIENT_ID` the login routes are 404 and the plugins route
  still works for admin-created tokens.

## Failure modes and security

- Device-flow phishing: an attacker starts a login and sends the member the
  verify URL. Mitigations: the approval page shows the code and tells the
  member to compare it with their terminal; codes expire in 10 min; only the
  initiator's `device_code` can redeem; per-IP limits on start/poll.
  Residual risk accepted (standard for device flows).
- `device_code` is compared in constant time; `user_code` lookups are
  rate-limited and codes are unguessable enough (32^8).
- A denied or expired login cannot be revived; a code is decided once.
- Auto-created users inherit the default limit; an admin can tighten,
  disable or delete from the Users page as today.
- `plugins` URLs must be `https`; the proxy never fetches them. SHA-256
  must be 64 hex. Env keys are validated; values capped at 4 KiB; at most 64
  keys per plugin; at most 32 plugins.
- The plugins reply contains secrets: `no-store`, user token only, never
  logged. The existing per-identity rate limit applies.
- A proxy restart loses pending logins only.

## Acceptance criteria

- [ ] AC1: `go test ./...` green; new tests in `internal/claudioapi` cover start/poll state machine (pending, approved once, expired, denied, slow_down, unknown) and the 404 without SSO.
- [ ] AC2: `internal/webui` test: approving with a signed-in `@vocdoni.org` user that has no token creates one with the default limit and e-mail; a second approval reuses it; a disabled user is refused.
- [ ] AC3: `GET /v1/claudio/plugins` returns only enabled plugins with env for a user token, 403 for admin and anonymous, `Cache-Control: no-store`; store tests cover validation limits.
- [ ] AC4: `claude-proxy plugins set/env set/list` round-trip in a CLI test; `users list` shows e-mail.
- [ ] AC5: Manual: on a dev proxy with the dev oauth provider, `curl` the start route, approve in the browser, poll returns the token; README documents the routes and the CLI.

## Constraints

- Conventional commits `type(scope): lowercase summary`, no AI attribution or co-author trailers.
- Everything under `/v1/claudio` stays local: never forwarded upstream, never written to `request_log` (CLAUDE.md "Claudio API").
- Secrets never in logs, argv of child processes, or the web UI after save.
- No new external dependencies; reuse `vocauth`, the existing limiter and store patterns.
- Keep `golangci-lint` clean (`make lint`).

## Open questions

- None.
