# Claudio plugins and browser login: design

Status: audited   Class: architectural
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
(`internal/store/schema.go:50-67`) and no e-mail, although the web UI already
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
- Per-member secrets. Every enabled plugin's env is served to every active
  user token; disabling or deleting the user (Users page, `users disable`)
  is the only revocation, and claudio's on-disk copy outlives it until its
  next refresh.
- A central logout, refresh tokens, or any change to oauth.vocdoni.org.
- Changing how model traffic is authenticated or routed.

## Current state

- Downstream auth: `internal/proxy/auth.go:28 AuthMiddleware`. Every
  `/v1/*` path is proxied (`:35 isProxy`); a bearer or `X-Api-Key`
  credential is an admin token or a `user_tokens.token`; a user identity is
  allowed on `/v1/*`, `/openai/v1/*`, `/health`, and gets 403 on
  `/admin/*` (`:60-65`). A request with no credential passes only when no
  admin token is set and no user token exists (`:96`); otherwise 401
  (`:101-104`). The only exemption is `/health` (`:30`). The chain is
  `AuthMiddleware → claudioapi.WrapHandler → mux` (`main.go:325`).
- Claudio API: `internal/claudioapi/claudioapi.go`. `New` (:103) registers
  the routes; `WrapHandler` (:145) claims every raw path starting with
  `/v1/claudio` before mux canonicalisation; `ServeHTTP` (:163-206) rejects
  encoded paths, `..` and `//` (`:169-174`), rate-limits 1 req/s burst 10
  per identity before dispatch (`:179-184`; every credential-less caller
  shares the key `"anon"`, `identityKey` `:1001-1013`), and 404s unknown
  paths. `serveStats` (:492) is the one route that requires a user
  identity (`:493-499`, the 403 shape to copy).
- Web UI SSO: `internal/webui/sso.go`. `NewSSO` (:55) builds a
  `vocauth.Auth` from `SSOConfig`; `origin` (:50, :91-97) is the
  scheme://host of the redirect URL; `user()` (:103) runs `Require`
  against a throwaway writer (`:108-111`) and so drops vocauth's 302 to
  `/auth/login?return_to=…` for HTML GETs; `sameOrigin` (:122) checks
  `Origin`, else `Sec-Fetch-Site`; `ssoAuthorize` (:177-191) admits any
  signed-in user, there is no role. Who may sign in is decided once in
  `vocauth.Config` (`main.go:350-362`): `CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS`
  empty → every verified `@vocdoni.org` account; set → only those. The
  session cookie is `SameSite=Lax`; sessions last `vocauth.MaxMaxAge`
  (7 days). Without `CLAUDE_PROXY_OAUTH_CLIENT_ID` SSO is nil and
  `/api/login` is the password route (`webui.go:127-146`). No UI response
  sets `X-Frame-Options` or a CSP.
- User tokens: `internal/usertoken/usertoken.go`. `Create(ctx, db, name)`
  (:89) with `name NOT NULL UNIQUE` (`schema.go:52`), `LookupByToken`
  (:145), `SetStatus` (:161), limits in `limit.go` (parser shared with
  `claude-proxy users limit`, `main.go usersLimit`). CLI
  `claude-proxy users …` opens the SQLite file only (`--db`,
  `main.go:911-971`); there is no HTTP admin mode. Web UI users routes:
  `internal/webui/users.go:38`; `serveAuthenticatedAPI` (`webui.go:165-200`)
  dispatches `/api/*`. `webui.signInLimiter` (`sso.go:147`) is a fixed-window
  per-IP map; `clientIP` is in `internal/webui/auth.go:187`.
- Schema: `schema` runs first (`store.go:60`), then an ALTER list
  (`store.go:64-113`) whose "duplicate column" and "already exists" errors
  are ignored (`isDuplicateColumn`, `:123-126`); `foreign_keys` is on
  (`:45`). `usertoken.scan`/`List` select named columns.

## Design

Three additions to the proxy, behind the existing `/v1/claudio` guard and
the existing SSO, plus one split of the SSO into sign-in and administration.

### 0. Sign-in versus administration

Today every account that can sign in is an administrator. With member
sign-in that cannot stay. `SSOConfig` gains `AdminEmails` (from the
existing `CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS`; unchanged name, new meaning)
and `LoginDomains` (new `CLAUDE_PROXY_LOGIN_DOMAINS`, comma-separated,
default `vocdoni.org`). `NewSSO` passes `AllowedDomains: LoginDomains` and
no `AllowedEmails` to vocauth, so every verified account of a login domain
can hold a session cookie. `ssoAuthorize` (every `/api/*` route)
additionally requires the e-mail to be in `AdminEmails` when that list is
non-empty, else 403 `not an administrator`; when the list is empty every
signed-in user is an admin, as today. `GET`/`POST /claudio/login` require
only a session whose e-mail domain is in `LoginDomains` (re-checked on the
page, not only at sign-in). README: `OAUTH_ALLOWED_EMAILS` now lists the
administrators; with device login enabled it must be set, or every member
becomes an admin (the proxy logs a warning at start when SSO is on and the
list is empty). Deployments that set it today keep the same admins and gain
member sign-in; deployments that did not set it are unchanged.

### 1. E-mail on user tokens

`user_tokens` gets `email TEXT` (NULL for admin-created tokens; stored
lower-cased and trimmed). Migration, appended to the ALTER list in
`store.Open` in this order: `ALTER TABLE user_tokens ADD COLUMN email TEXT`,
then `CREATE UNIQUE INDEX IF NOT EXISTS idx_user_tokens_email ON
user_tokens(email) WHERE email IS NOT NULL`. `UserToken.Email string`. New
functions `usertoken.LookupByEmail(ctx, db, email)` and
`usertoken.CreateForEmail(ctx, db, email)`, which picks `name = email` and,
when that name is taken, appends `-2`, `-3`, … (the e-mail, not the name,
is the identity). The Users page (`/api/users` list includes `email`) and
`claude-proxy users list` show it; `users create` gains `--email`;
`claude-proxy users email <id> <email>` sets it and refuses an address
already on another token.

### 2. Device-style login

Available only when SSO is configured; otherwise the routes answer 404
(like `/api/login`, `README.md:863`).

```
claudio                        proxy                          browser
  │ POST /v1/claudio/login/start │                              │
  │─────────────────────────────▶│ creates {user_code, device_code}
  │◀── {device_code, user_code,  │                              │
  │     verify_url, interval, ttl}                              │
  │ opens / prints verify_url and the code ──────────────────▶ │
  │                              │◀── GET /claudio/login         │
  │                              │  (302 to SSO sign-in if needed)
  │                              │──▶ form: type your code       │
  │                              │◀── POST (lookup, code)        │
  │                              │──▶ page: approve? code UC,    │
  │                              │     signed in as e-mail       │
  │                              │◀── POST (approve, code) same-origin
  │                              │  user = find/create by e-mail │
  │ POST /v1/claudio/login/poll  │  mark code approved           │
  │  {device_code} (every 3 s)   │                              │
  │◀── 200 {token, email, name}  │ code redeemed, deleted        │
```

- **Auth exemption.** `AuthMiddleware` gains one exemption, placed with
  the `/health` check: a request whose `r.URL.EscapedPath()` is exactly
  `/v1/claudio/login/start` or `/v1/claudio/login/poll` is passed to
  `next` with no identity in the context, even when a bearer is present (a
  bearer on these routes is ignored, never validated, so a stale token
  cannot turn a login into a 401). The exemption uses the escaped path so
  the raw-path guard in `claudioapi.ServeHTTP` still sees what the client
  sent. `auth_test.go` gets a case for each route with and without a
  bearer, and one asserting `/v1/claudio/login/startx` and
  `/v1/claudio/login/../poll` are not exempt.
- **Limits.** The two login routes replace, not add to, the per-identity
  bucket: `ServeHTTP` skips `h.allow` for them and applies a per-IP limiter
  (`clientIP` as in `webui/auth.go:187`: first `X-Forwarded-For` entry,
  else `RemoteAddr`). `login/start`: 5 per minute per IP. `login/poll`: 40
  per minute per IP (room for two concurrent logins at 3 s), and per
  `device_code` a poll less than `interval_s` after the previous one
  answers 429 `slow_down` without consuming the IP budget. Both limiters
  are the fixed-window map of `webui.signInLimiter`, copied into
  `claudioapi` (no shared package).
- `POST /v1/claudio/login/start`. Reply `{device_code, user_code,
  verify_url, interval_s: 3, expires_in_s: 600}`. `device_code`: 32 random
  bytes, base64url, the only thing that can redeem the login. `user_code`:
  8 characters from `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`, shown as
  `XXXX-XXXX`. `verify_url` is `SSO.origin` + `/claudio/login`, with no
  code: the member types it. Pending logins live in memory, keyed by
  `SHA-256(device_code)` with `user_code` as the second key of the same
  entry (one map per key, same mutex, plus an insertion-ordered list);
  the plaintext `device_code` exists only in the `start` reply. At most 8
  pending logins per client IP (a ninth start answers 429
  `too_many_pending`) and 4096 in total; past the total, the oldest pending
  entry is evicted (its poll then answers 404). A sweeper runs every
  minute. A restart drops them; the member retries.
- `GET /claudio/login` (browser) is served through `s.sso.auth.Require`
  directly (not `user()`): an unauthenticated GET is redirected to
  `/auth/login?return_to=/claudio/login` and comes back after sign-in. A
  session whose e-mail domain is not in `LoginDomains` gets 403. The page
  is a form with one input for the code (letters and digits; dashes and
  case ignored).
- `POST /claudio/login` (`action`, `code`), through `Require` (a missing
  session answers 401), then `sameOrigin`, then the `LoginDomains` check.
  `action=lookup`: unknown, expired or decided code → a page saying so;
  else the approval page: the code in large type, the signed-in e-mail,
  "Approve only if this code is shown in your terminal", and
  `approve` / `deny` buttons (same-origin POST, code in a hidden field).
  `action=approve`: look the e-mail up; if missing, create the user with
  `CreateForEmail` and the default limit from `CLAUDIO_DEFAULT_LIMIT`
  (exactly the arguments of `claude-proxy users limit`, same parser; unset
  or `0` means unlimited) and log `login: created user <email>`. A user
  whose status is not active is refused ("account disabled, contact an
  admin"). The entry is marked approved with the user's id (never the
  token). `action=deny`: marked denied. Each code is decided once.
- Both `/claudio/login` responses set `Content-Security-Policy: default-src
  'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors
  'none'`, `X-Frame-Options: DENY`, `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`. The page is a Go template with no script.
- `POST /v1/claudio/login/poll` body `{device_code}`. State transitions:
  pending → approved | denied | expired; approved → redeemed (deleted on
  the 200). A denied or expired entry is kept for 10 minutes after the
  decision or expiry so the client sees a stable 403/410, then swept. On
  the 200 the proxy re-reads the user by id: a missing row answers 404
  `unknown_code`; `status != active` answers 403 `denied`; otherwise the
  row's current `token` is returned (an approved login never rotates it)
  and the entry deleted. Replies for a well-formed body are exactly: 428
  `authorization_pending`, 429 `slow_down`, 200 `{token, email, name}`,
  410 `expired`, 403 `denied`, 404 `unknown_code` (never existed, swept,
  evicted, or the proxy restarted; claudio treats it like 410). Error
  bodies are `{"error":"<code>"}` with `Content-Type: application/json`;
  the codes are `authorization_pending`, `slow_down`, `expired`, `denied`,
  `unknown_code`, `too_many_pending`.

The login routes are the only `/v1/claudio` routes without a credential.
They never touch `request_log` and never forward upstream.

### 3. Plugins

Tables:

```sql
CREATE TABLE IF NOT EXISTS plugins (
  name        TEXT PRIMARY KEY,   -- [a-z0-9-]{1,40}
  version     TEXT NOT NULL,      -- 1–64 printable ASCII
  bundle_url  TEXT NOT NULL,      -- https only
  bundle_sha256 TEXT NOT NULL,    -- 64 lowercase hex
  installed_id TEXT NOT NULL DEFAULT '', -- '' or [a-z0-9-]{1,40}@[a-z0-9-]{1,40}
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

One package, `internal/plugins` (`Set`, `Enable`, `Delete`, `SetEnv`,
`UnsetEnv`, `List`, `ListForClient`), owns the validation limits; the CLI,
the web routes and the client endpoint all call it.

Client endpoint `GET /v1/claudio/plugins` (user token only, like
`me/stats`; admin and anonymous get 403):

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

Admin CLI (`claude-proxy plugins …`, local DB only, `--db` like `users`):

```
plugins list
plugins set <name> --version <v> --url <https url> --sha256 <hex> [--installed-id <id>]
plugins enable|disable|delete <name>
plugins env set <name> KEY=- [KEY=-…]            # values read from stdin, one per line
plugins env set <name> --env-file <path>          # KEY=VALUE lines
plugins env set <name> KEY=VALUE                  # accepted; help text says "visible in ps"
plugins env unset <name> KEY…
plugins env list <name>                           # keys only
```

Web UI (`internal/webui/plugins.go`, admin session, `ssoAuthorize`), a
**Plugins** page backed by:

```
GET    /api/plugins                  → {plugins:[{name,version,bundle_url,bundle_sha256,installed_id,enabled,updated_at,env_keys:[…]}]}
PUT    /api/plugins/{name}           body {version,bundle_url,bundle_sha256,installed_id}  (create or replace metadata; env untouched)
POST   /api/plugins/{name}/enable | /disable
DELETE /api/plugins/{name}
PUT    /api/plugins/{name}/env       body {"KEY":"value",…}  (upsert listed keys only)
DELETE /api/plugins/{name}/env/{KEY}
```

Validation errors are 400 with the usual `{"error":…}`; no route ever
returns a value. Values are entered in password fields and never shown
after save (keys only, with a "replace" action).

### Alternatives considered

| Option | Why not |
|---|---|
| claudio as an OIDC client of oauth.vocdoni.org | The service admits confidential web clients only, ID tokens live 10 min, no refresh tokens. |
| Localhost callback instead of polling | Fails over SSH or when the browser is on another machine; polling would be needed anyway. |
| Code carried in the link (`?code=`) | A forwarded link alone would then complete a phished login; typing the code is RFC 8628's `verification_uri`. |
| Proxy stores and serves the zip | Plugin edits would bypass git review; the proxy would grow a file store. |
| Per-member secrets | Not needed for the first plugin; shared team keys only. Can be added as a `user_plugin_env` table later. |

## Compatibility and rollout

- API additions only. Old claudio clients ignore unknown routes; old
  proxies answer the new routes with the existing local 404, which claudio
  (sub-project 2) turns into "proxy too old".
- Schema: two new tables, one `ADD COLUMN` and one partial unique index,
  in the existing ALTER list. Rollback: the previous image selects named
  columns and ignores the extra column and tables.
- SSO: `CLAUDE_PROXY_OAUTH_ALLOWED_EMAILS` keeps its name and, for
  deployments that set it, its effect on the admin UI; it must be set before
  device login is enabled on a shared deployment (warning at start).
- Rollout: deploy the proxy first with `OAUTH_ALLOWED_EMAILS` set to the
  admins (nothing changes for current users), then register the
  `vocdoni-internal` plugin and its env, then ship claudio.
- Without `OAUTH_CLIENT_ID` the login routes are 404 and the plugins route
  still works for admin-created tokens.

## Failure modes and security

- Device-flow phishing: an attacker starts a login and sends the member the
  URL. The member must type the code themselves and the approval page
  shows it with "approve only if this code is shown in your terminal";
  codes expire in 10 min; only the initiator's `device_code` can redeem;
  per-IP limits on start and poll; at most 8 pending per IP. Residual risk
  accepted (standard for device flows).
- Pending logins hold `SHA-256(device_code)`, never the plaintext and never
  a token; a memory dump or debug log cannot redeem a login.
- A denied or expired login cannot be revived; a code is decided once;
  redemption re-checks the user's status.
- Clickjacking and caching of the approval page: CSP `frame-ancestors
  'none'`, `X-Frame-Options: DENY`, `no-store`.
- Auto-created users inherit the default limit; an admin can tighten,
  disable or delete from the Users page as today.
- Validation (in `internal/plugins`): `name` `[a-z0-9-]{1,40}`; `version`
  1–64 printable ASCII; `installed_id` empty or
  `[a-z0-9-]{1,40}@[a-z0-9-]{1,40}`; `bundle_url` `https` only, never
  fetched by the proxy; `bundle_sha256` 64 lowercase hex; env keys
  `[A-Z_][A-Z0-9_]{0,63}`; values ≤ 4 KiB; at most 64 keys per plugin; at
  most 32 plugins.
- The plugins reply contains secrets: `no-store`, user token only, never
  logged. The existing per-identity rate limit applies.
- A proxy restart loses pending logins only.

## Acceptance criteria

- [ ] AC1: `go test ./...` green; new tests in `internal/claudioapi` cover the start/poll state machine (pending, approved once, re-check of a disabled user at redemption, expired kept 10 min, denied, slow_down, unknown, per-IP pending cap, eviction), the per-IP limiters, and the 404 without SSO.
- [ ] AC2: `internal/webui` tests: the login page redirects an unauthenticated GET to `/auth/login?return_to=…`; approving with a signed-in `@vocdoni.org` user that has no token creates one with the default limit and e-mail (name suffixed when taken); a second approval reuses it; a disabled user is refused; a non-admin signed-in user gets 403 on `/api/*` when `AdminEmails` is set; the security headers are present.
- [ ] AC3: `GET /v1/claudio/plugins` returns only enabled plugins with env for a user token, 403 for admin and anonymous, `Cache-Control: no-store`; `internal/plugins` tests cover every validation limit.
- [ ] AC4: `claude-proxy plugins set/env set (stdin)/list` round-trip in a CLI test; `users list` shows e-mail; `users email` refuses a duplicate.
- [ ] AC5: `internal/proxy/auth_test.go`: the two login routes pass without a credential and with a stale bearer; `/v1/claudio/login/startx` and `/v1/claudio/login/../poll` do not.
- [ ] AC6: `internal/webui` test: `/api/plugins` round-trip (set, env set, list shows keys only and never a value, disable hides it from `/v1/claudio/plugins`); `/api/users` list includes `email`.
- [ ] AC7: Manual: on a dev proxy with the dev oauth provider, `curl` the start route, type the code in the browser, approve, poll returns the token; README documents the routes, the env split and the CLI.

## Constraints

- Conventional commits `type(scope): lowercase summary`, no AI attribution or co-author trailers.
- Everything under `/v1/claudio` stays local: never forwarded upstream, never written to `request_log` (CLAUDE.md "Claudio API").
- Secrets never in logs, argv of child processes, or the web UI after save.
- No new external dependencies; reuse `vocauth`, the existing limiter and store patterns.
- Keep `golangci-lint` clean (`make lint`).

## Open questions

- None.
