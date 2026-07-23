# Web UI — architecture & API contract

Management/monitoring dashboard served by the same `claude-proxy` binary, embedded
via `go:embed`. No Node toolchain: the frontend is hand-written ES modules + a
vendored chart library, committed to the repo under `internal/webui/static/`.

## Auth model

- Env var `CLAUDE_PROXY_UI_PASSWORD` (compose maps `.env` `UI_PASSWORD`). Empty ⇒ UI disabled
  (`/ui/*` returns 404; nothing mounted).
- `POST /ui/api/login` `{"password":"..."}` → constant-time compare → on success sets
  `HttpOnly; SameSite=Strict; Path=/ui` session cookie (`cpui_session`), HMAC-SHA256-signed
  value `expiry|nonce|mac`, key derived at startup: `HMAC(SHA256(password), random-boot-salt)`.
  Sessions last 24h. 429 after 5 failed attempts per IP per minute.
- `POST /ui/api/logout` clears the cookie. `GET /ui/api/session` → `{"authenticated":bool}`.
- All other `/ui/api/*` require a valid cookie → 401 otherwise.
- `proxy.AuthMiddleware` passes `/ui/` through untouched (webui does its own auth);
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

## REST API (all JSON, cookie-auth, prefix `/ui/api`)

Periods: `1h|6h|24h|7d|30d` (reuse `usage.ParsePeriod`). Timeseries endpoints take
`period` + optional `buckets` (default 60, max 200); server buckets rows by
`(now-period, now]` into equal intervals.

### Overview
- `GET /ui/api/overview` → totals for header tiles:
  `{requests_24h, tokens_24h:{input,output,cache_read,cache_creation}, active_conversations,
    credentials:{total,active,limited,errored}, users_total, avg_latency_ms_24h,
    error_rate_24h}`.

### Statistics
- `GET /ui/api/stats/requests?period&buckets&group_by=user|credential|none` →
  `{buckets:[ts...], series:[{id,label,requests,errors,tokens_in,tokens_out}...]}`
  (per bucket arrays; aggregated over `request_log`).
- `GET /ui/api/stats/tokens?period&buckets&group_by=user|credential` → same shape,
  values = token sums.
- `GET /ui/api/stats/users?period` → per-user scalar table (lift the CLI query from
  `cmd/claude-proxy/main.go` `usersStats` into a reusable func):
  `[{id,name,requests,ok,errors,tokens_in,tokens_out,cache_read,cache_creation,
     bytes_sent,bytes_received,avg_latency_ms,conversations}]`.
- `GET /ui/api/stats/latency?period&buckets` → `{buckets:[...], avg_ms:[...], p95_ms:[...]}`.

### Subscription usage (remote limits)
- `GET /ui/api/usage/current` → per credential, latest snapshot + live counters:
  `[{credential_id,label,subscription_type,status,weight,five_hour:{pct,resets_at},
     seven_day:{pct,resets_at},seven_day_sonnet:{pct,resets_at},captured_at}]`
  (from `usage_history` latest row per cred; include resets_at).
- `GET /ui/api/usage/history?period&credential_id?` → time series of pct values per
  credential for charts: `{series:[{credential_id,label,points:[{ts,five_hour_pct,
  seven_day_pct,seven_day_sonnet_pct}]}]}`.

### Credential management (wraps `internal/creds`, `internal/ingest`)
- `GET /ui/api/credentials` → extended `credView` (reuse fields from `internal/admin`).
- `POST /ui/api/credentials` `{credentials_json, label, weight?}` → import pasted
  `.credentials.json` (use `ingest.ImportFromJSON`; verifies liveness, rejects dupes).
- `POST /ui/api/credentials/{id}/disable` | `/enable` (SetStatus disabled/active)
- `POST /ui/api/credentials/{id}/refresh` → force OAuth token refresh (`Refresher.RefreshNow`)
- `POST /ui/api/credentials/{id}/weight` `{weight}` (creds.SetWeight)
- `PUT  /ui/api/credentials/{id}/tokens` `{credentials_json}` → `ingest.UpdateFromFile` logic
- `DELETE /ui/api/credentials/{id}` (creds.Delete)

### Proxy user management (wraps `internal/usertoken`)
- `GET /ui/api/users` → list incl. status, created_at, last_used_at.
- `POST /ui/api/users` `{name}` → `{id,name,token}` (token shown once).
- `POST /ui/api/users/{id}/disable` | `/enable`
- `POST /ui/api/users/{id}/rotate` → `{token}`
- `DELETE /ui/api/users/{id}`

### Conversations
- `GET /ui/api/conversations?limit=100` → recent bindings (reuse admin listConvs query).

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
- Served on the same port 8787 ⇒ existing traefik labels/TLS cover `/ui` automatically.
- Makefile: `make ui-url` prints the UI address (uses BASE), README + CLAUDE.md sections.
