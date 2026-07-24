# Web UI — architecture & API contract

Management/monitoring dashboard served by the same `claude-proxy` binary, embedded
via `go:embed`. No Node toolchain: the frontend is hand-written ES modules + a
vendored chart library, committed to the repo under `internal/webui/static/`.

## Auth model

- Env var `CLAUDE_PROXY_UI_PASSWORD` (compose maps `.env` `UI_PASSWORD`). Empty ⇒ UI disabled
  (root serves nothing; pre-UI behavior).
- `POST /api/login` `{"password":"..."}` → constant-time compare → on success sets
  `HttpOnly; SameSite=Strict; Path=/` session cookie (`cpui_session`), HMAC-SHA256-signed
  value `expiry|nonce|mac`, key derived at startup: `HMAC(SHA256(password), random-boot-salt)`.
  Sessions last 24h. 429 after 5 failed attempts per IP per minute.
- `POST /api/logout` clears the cookie. `GET /api/session` → `{"authenticated":bool}`.
- All other `/api/*` require a valid cookie → 401 otherwise.
- **Routing:** the UI is served at the root `/`. Reserved prefixes `/v1/`, `/admin/`, `/health`, `/api/` route to their handlers; every other path serves the SPA (deep-link fallback to index.html). `/ui` and `/ui/*` permanently redirect to `/`. `proxy.AuthMiddleware` passes non-`/v1/` non-`/admin/` paths through untouched when the UI is enabled (webui does its own cookie auth); with the UI disabled, unknown paths keep the pre-UI 401/404 behavior;
  same-origin only, no CORS headers needed.
- If `TLS_DOMAIN` is set the cookie gets `Secure`. (Detected via `X-Forwarded-Proto: https`
  from traefik, or new optional env `CLAUDE_PROXY_UI_SECURE_COOKIES=1`.)

## Token usage capture (proxy change)

`request_log` gains additive columns (existing swallow-duplicate ALTER mechanism in
`internal/store/store.go`): `model TEXT NOT NULL DEFAULT ''`, `input_tokens INTEGER NOT NULL
DEFAULT 0`, `output_tokens INTEGER NOT NULL DEFAULT 0`, `cache_creation_tokens INTEGER NOT
NULL DEFAULT 0`, `cache_read_tokens INTEGER NOT NULL DEFAULT 0`.

`internal/proxy/proxy.go` `forward()` tees the response body while streaming:
- SSE (`text/event-stream`): scan `data:` lines; `message_start` → model +
  `usage.input_tokens` + cache tokens; `message_delta` → final `usage.output_tokens`.
- Non-stream JSON: buffer up to 1 MiB, parse top-level `model` + `usage`.
- `Content-Encoding: gzip` → parse side wraps a streaming gzip reader (client still
  receives original bytes verbatim). Parse failures are silent (usage stays 0) and must
  never affect the client stream.

## REST API (all JSON, cookie-auth, prefix `/api`)

Periods: `1h|6h|24h|7d|30d` (reuse `usage.ParsePeriod`), **or a custom window via
`from`+`to` (unix seconds, `from<to`, span ≤ 90d), which overrides `period`**. This
applies to every endpoint taking `period` (stats/*, usage/history). Timeseries
endpoints take optional `buckets` (default 60, max 200); server buckets rows over
the selected window into equal intervals.

### Overview
- `GET /api/overview` → totals for header tiles:
  `{requests_24h, tokens_24h:{input,output,cache_read,cache_creation}, active_conversations,
    credentials:{total,active,limited,errored}, users_total, avg_latency_ms_24h,
    error_rate_24h}`.

### Statistics
- `GET /api/stats/requests?period&buckets&group_by=user|credential|none` →
  `{buckets:[ts...], series:[{id,label,requests,errors,tokens_in,tokens_out}...]}`
  (per bucket arrays; aggregated over `request_log`).
- `GET /api/stats/tokens?period&buckets&group_by=user|credential` → same shape,
  values = token sums.
- `GET /api/stats/users?period` → per-user scalar table (lift the CLI query from
  `cmd/claude-proxy/main.go` `usersStats` into a reusable func):
  `[{id,name,requests,ok,errors,tokens_in,tokens_out,cache_read,cache_creation,
     bytes_sent,bytes_received,avg_latency_ms,conversations}]`.
- `GET /api/stats/latency?period&buckets` → `{buckets:[...], avg_ms:[...], p95_ms:[...]}`.

### Subscription usage (remote limits)
- `GET /api/usage/current` → per credential, latest snapshot + live counters:
  `[{credential_id,label,subscription_type,status,weight,five_hour:{pct,resets_at},
     seven_day:{pct,resets_at},seven_day_sonnet:{pct,resets_at},captured_at}]`
  (from `usage_history` latest row per cred; include resets_at).
- `GET /api/usage/history?period&credential_id?` → time series of pct values per
  credential for charts: `{series:[{credential_id,label,points:[{ts,five_hour_pct,
  seven_day_pct,seven_day_sonnet_pct}]}]}`.

### Credential management (wraps `internal/creds`, `internal/ingest`)
- `GET /api/credentials` → extended `credView` (reuse fields from `internal/admin`).
- `POST /api/credentials` `{credentials_json, label, weight?}` → import pasted
  `.credentials.json` (use `ingest.ImportFromJSON`; verifies liveness, rejects dupes).
- `POST /api/credentials/{id}/disable` | `/enable` (SetStatus disabled/active)
- `POST /api/credentials/{id}/refresh` → force OAuth token refresh (`Refresher.RefreshNow`)
- `POST /api/credentials/{id}/weight` `{weight}` (creds.SetWeight)
- `PUT  /api/credentials/{id}/tokens` `{credentials_json}` → `ingest.UpdateFromFile` logic
- `DELETE /api/credentials/{id}` (creds.Delete)

### Proxy user management (wraps `internal/usertoken`)
- `GET /api/users` → list incl. status, created_at, last_used_at.
- `POST /api/users` `{name}` → `{id,name,token}` (token shown once).
- `POST /api/users/{id}/disable` | `/enable`
- `POST /api/users/{id}/rotate` → `{token}`
- `DELETE /api/users/{id}`

### Conversations
- `GET /api/conversations?limit=100` → recent bindings (reuse admin listConvs query).

Errors: non-2xx with `{"error":"message"}`. All handlers take context from request;
queries must use the indexes on `request_log(ts)` / `usage_history(credential_id,captured_at)`.

## Frontend layout (SPA, hash-routing: #/dashboard #/usage #/credentials #/users)

- **Login screen**: single password field.
- **Dashboard**: header stat tiles (requests, tokens in/out, error rate, avg latency,
  active convs) + requests-over-time chart (stack by user) + tokens chart + latency chart.
  Global period selector (1h/6h/24h/7d/30d) drives every chart; per-chart group-by toggle.
- **Subscriptions**: per-credential cards with 5h/7d/sonnet utilization meters +
  resets-at countdowns, and the utilization history multi-line chart.
- **Credentials**: table with status badges, weight editing, enable/disable/refresh/
  delete actions, "add credential" modal (paste JSON), "update tokens" modal.
- **Users**: table with per-period stats, create modal (token reveal + copy once),
  rotate/disable/delete.

## Env / compose

- `.env.example` + compose: `UI_PASSWORD=` → container env `CLAUDE_PROXY_UI_PASSWORD`.
- Served on the same port 8787 at `/` ⇒ existing traefik labels/TLS cover it automatically.
- Makefile: `make ui-url` prints the UI address (uses BASE), README + CLAUDE.md sections.

## Chart interactions

- Legend/series click on any multi-series chart **solos** that series (click again to
  restore all); a second series click while soloed switches the solo target.
- The period selector offers the presets plus **Custom** (from/to datetime-local
  inputs), driving all charts via `from`/`to` params.

## v2 additions (2026-07-24)

- **`GET /api/overview`** now takes the standard window params (`period` | `from`+`to`);
  fields lose the `_24h` suffix: `{requests, tokens:{input,output,cache_read,cache_creation},
  active_conversations, credentials:{total,active,limited,errored}, users_total,
  avg_latency_ms, error_rate}`. Tiles reflect the selected window.
- **`GET /api/stats/totals?period|from,to&buckets`** → aggregate (no grouping) series:
  `{buckets:[ts...], requests:[...], errors:[...],
    tokens:{input:[...],output:[...],cache_read:[...],cache_creation:[...]}}`.
- **`GET /api/usage/current`**: each entry gains
  `"selection": {room_5h, room_7d, score, share_pct, saturated}` — score mirrors the
  pool exactly (`weight × room_5h × room_7d^1.5`, sevenDayExp exported/shared from
  internal/pool), `share_pct` = score/Σscore×100 across active credentials,
  `saturated` = latest snapshot ≥100% on either window (excluded from new bindings).
- **`GET /api/stats/selection?period|from,to&buckets`** → how often each credential is
  picked for NEW conversations (from `conversations.created_at`):
  `{buckets:[ts...], series:[{credential_id,label,picks:[...]}],
    totals:[{credential_id,label,picks,share_pct}]}`.
- **`GET /api/usage/history`** response is now an aligned grid (fixes the broken
  multi-credential chart): `{buckets:[ts...], series:[{credential_id,label,
  five_hour_pct:[...], seven_day_pct:[...], seven_day_sonnet_pct:[...]}]}` — one value
  per bucket per series, `null` where a credential has no snapshot in that bucket;
  buckets downsampled to ≤200.
- **Prompt logging**: new table `prompt_log(id, user_token_id→SET NULL, conv_id, ts,
  model, prompt)` — the proxy stores the LAST `role:"user"` text (string or first text
  block, trimmed to 4096 chars) from `POST /v1/messages` request bodies. Never responses.
  Retention: env `CLAUDE_PROXY_PROMPT_RETENTION_DAYS` (`.env` `PROMPT_RETENTION_DAYS`,
  default 7; `0` disables capture entirely); hourly janitor deletes older rows.
  `GET /api/users/{id}/prompts?limit=50` → `[{ts,conv_id,model,prompt}]` (newest first).
- **Pool rebinding**: `Bind()` re-picks when the sticky credential's latest snapshot is
  ≥100% on either window — existing conversations migrate off saturated credentials.
- **Frontend**: overview tiles follow the global window; Subscriptions page scopes its
  period picker to the History + Selection charts only (cards always show latest);
  cards show the selection score/share and a "saturated" badge; new stacked Totals
  chart (tokens by type + requests) on the dashboard; credentials table reads
  `last_request_at` (fixes perpetual "never"); Users page gains a per-user "Prompts"
  button + modal (newest prompts, ts + model).
