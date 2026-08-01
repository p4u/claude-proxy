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
  `[{credential_id,label,subscription_type,provider,has_usage_api,status,weight,
     five_hour:{pct,resets_at},seven_day:{pct,resets_at},
     seven_day_sonnet:{pct,resets_at},captured_at,selection:{...}}]`
  (from `usage_history` latest row per cred; include resets_at).
  Credentials with `has_usage_api: false` never have a snapshot, so their
  percentages are 0 and `captured_at` is null. They instead carry
  `metered:{five_hour,seven_day}` — each `{requests,input_tokens,output_tokens,
  cache_read_tokens,cache_creation_tokens}` summed from `request_log` over that
  rolling window. The UI renders these where the meters would go, with no bar:
  there is no published allowance, so there is deliberately no percentage, and
  the figure counts only traffic through this proxy. Credentials with a real
  usage API omit `metered`. `selection.share_pct` is totalled
  **per provider**, matching the pool's provider-scoped candidate set: a lone
  GLM key takes 100% of GLM traffic, not a few percent of the global total.
- `GET /api/usage/history?period&credential_id?` → time series of pct values per
  credential for charts: `{series:[{credential_id,label,points:[{ts,five_hour_pct,
  seven_day_pct,seven_day_sonnet_pct}]}]}`.

### Credential management (wraps `internal/creds`, `internal/ingest`)
- `GET /api/credentials` → extended `credView` (reuse fields from `internal/admin`),
  including `provider` (`anthropic` | `glm`) and `has_usage_api`. The latter is
  false for providers publishing no utilization endpoint; the UI uses it to hide
  the usage meters, the expiry date and the OAuth-only row actions (Refresh,
  Update tokens) rather than showing 0% and a synthetic far-future date.
- `POST /api/credentials` `{credentials_json, label, weight?}` → import pasted
  `.credentials.json` (use `ingest.ImportFromJSON`; verifies liveness, rejects dupes).
- `POST /api/credentials/keys` `{provider, api_key, endpoint?, label?, plan?, weight?}`
  → add a static API key (`ingest.ImportKey`). `provider` is `glm` or `mimo`.
  `endpoint` is a short name from the provider's endpoint list (`sgp`, `ams`,
  `cn`, `payg`, `global`) or a full `https://` URL; empty selects the provider
  default. Verified before storing with a live `max_tokens:1 POST /v1/messages`
  — **not** `GET /v1/models`, which `api.z.ai` answers 200 for any bearer token
  and MiMo does not serve at all. Duplicates rejected by access token. `400` on
  a rejected key (the message names the endpoint that rejected it, since a
  right-key/wrong-cluster mistake otherwise looks identical to a bad key), an
  unknown provider or endpoint, or an OAuth provider (which needs `POST
  /api/credentials` instead).
- `GET /api/credentials/endpoints` → endpoint presets per key-based provider:
  `{glm:{name,default,endpoints:[{name,desc,url}]}, mimo:{...}}`. Served from the
  Go registry so the UI keeps no copy of it. OAuth providers are omitted.
- `POST /api/credentials/{id}/endpoint` `{endpoint}` → move an API-key credential
  to another cluster (`ingest.UpdateKeyEndpoint`). Accepts a preset name or a
  full `https://` URL. The key is **re-verified against the new endpoint before
  anything is written**; on failure the credential is left untouched and the
  `400` names the endpoint that rejected it. On success a `revoked`/`expired`/
  `limited` status is healed to `active`. `400` for OAuth credentials, whose
  endpoint is fixed.
- `POST /api/credentials/probe` `{base_url, api_key?}` → interrogate a candidate
  custom host **without storing anything**, so the modal can show what was found
  and let the operator correct it: `{ok, error?, auth_required, has_models_api,
  has_count_tokens, reported_model, models:[{id,display_name,context_window?,
  max_output?}]}`. `context_window` is absent when the host publishes no
  `/v1/models` — it is undiscoverable there and is never guessed.
- `POST /api/credentials/custom` `{base_url, api_key?, label?, models?, weight?}`
  → add a custom Anthropic-compatible host (`ingest.ImportCustomHost`). The host
  is probed again here rather than trusting the modal's earlier probe, since the
  URL or key may have changed since. `models` overrides discovery; omitted, the
  discovered catalogue is used. `400` if the host is unusable or no model could
  be determined.
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

## v3 additions — full conversation capture (2026-07-24)

Gated by the existing `CLAUDE_PROXY_PROMPT_RETENTION_DAYS` (`0` => no capture of
any kind, per-user flag ignored) and pruned by the same hourly janitor.

### Per-user capture mode

`user_tokens` gains `full_capture INTEGER NOT NULL DEFAULT 0` (additive ALTER,
existing swallow-duplicate mechanism). Default **0** = today's behavior: one
`prompt_log` row per request holding the last user message.

When **1**, the proxy additionally records the whole conversation, both roles:

- **Request side** - `POST /v1/messages` carries the full `messages` array (the
  API is stateless, so every prior turn is re-sent). Store messages not yet
  stored for this `conv_id`: keep a per-conversation `seq` and insert from
  `stored_count` onward. Idempotent by construction.
- **Response side** - extend `internal/proxy/usagecapture.go` to accumulate
  assistant output text (SSE `content_block_delta` of type `text_delta`, and the
  `content[]` text blocks of non-streaming JSON) **only when full capture is on
  for that request**; append it as the next `assistant` message once the stream
  completes. Thinking blocks and tool payloads are NOT stored. Parse failures
  stay silent and must never affect the client stream.
- Per-message cap 32768 runes (truncate, append a truncation marker).
- Document the limitation: server-side context compaction/editing can rewrite
  history, so `seq` alignment may drift on very long conversations.

New table:

    conversation_message(
      id INTEGER PK AUTOINCREMENT,
      conv_id TEXT NOT NULL,
      user_token_id TEXT REFERENCES user_tokens(id) ON DELETE SET NULL,
      seq INTEGER NOT NULL,          -- 0-based position within the conversation
      role TEXT NOT NULL,            -- "user" | "assistant"
      content TEXT NOT NULL DEFAULT '',
      model TEXT NOT NULL DEFAULT '',
      ts INTEGER NOT NULL,
      UNIQUE(conv_id, seq)
    )
    INDEX (conv_id, seq), INDEX (user_token_id, ts), INDEX (ts)

`usertoken.UserToken` and `usertoken.Identity` gain `FullCapture bool`
(populated by the existing `LookupByToken` in AuthMiddleware - no extra query).

### API

- `GET /api/users` - each entry gains `"full_capture": bool`.
- `POST /api/users/{id}/capture` `{"full": bool}` -> `{"ok":true,"full_capture":bool}`.
- `GET /api/users/{id}/prompts?limit&offset` - **paginated over ALL rows** for
  that user (not just the newest 50): `{items:[{ts,conv_id,model,prompt}],
  total, limit, offset, has_more}`. Newest first. `limit` default 50, max 500.
- `GET /api/users/{id}/conversations?limit&offset` ->
  `{items:[{conv_id, first_ts, last_ts, messages, prompts, model, source}],
  total, limit, offset, has_more}` - newest `last_ts` first. `messages` counts
  `conversation_message` rows, `prompts` counts `prompt_log` rows; `source` is
  `"full"` when full messages exist for that conversation, else `"prompts"`.
- `GET /api/conversations/{convID}/messages?limit&offset` ->
  `{items:[{seq,role,content,model,ts}], total, limit, offset, has_more,
  source, conv_id, user:{id,name}}` - ascending `seq`. When no
  `conversation_message` rows exist, fall back to that conversation's
  `prompt_log` rows rendered as `role:"user"` items with `source:"prompts"`, so
  the viewer works in both modes.
- `GET /api/conversations/{convID}/export.md` -> `text/markdown; charset=utf-8`
  with `Content-Disposition: attachment; filename="conversation-<short>.md"`.
  Same full-vs-prompts fallback.

Export format - readable in any markdown viewer. Do **not** fence message bodies
(they already contain code fences); separate turns with `---` and a heading
carrying index, role, and timestamp:

    # Conversation 4f2a9c1b
    
    | | |
    |---|---|
    | **User** | alice |
    | **Model** | claude-fable-5 |
    | **Messages** | 24 |
    | **Started** | 2026-07-24 10:12:03 UTC |
    | **Last activity** | 2026-07-24 10:41:55 UTC |
    | **Exported** | 2026-07-24 11:02:10 UTC |
    | **Source** | full conversation |
    
    ---
    
    ### 1 - User - 2026-07-24 10:12:03 UTC
    
    <content verbatim>
    
    ---
    
    ### 2 - Assistant - 2026-07-24 10:12:20 UTC
    
    <content verbatim>

`Source` renders as `full conversation` or `user prompts only`; in the
prompts-only case add a note under the table stating assistant replies were not
captured for this conversation.

### Frontend (Users page)

- Row gains a **Full capture** toggle -> `POST .../capture`, with a short helper
  making the privacy tradeoff explicit ("stores both sides of every
  conversation, including pasted file contents").
- The existing **Prompts** button opens a modal with two tabs: **Prompts** (all
  rows, paginated - prev/next with a range indicator) and **Conversations**
  (paginated list; click a row -> paginated message viewer, role-labelled,
  whitespace-preserving; a **Download .md** button hitting the export endpoint
  via a normal anchor so the browser saves the file).
- Message content is rendered with `textContent` (never `innerHTML`).

## v4 additions — per-user usage limits (2026-07-26)

Blocks a user once their recent usage exceeds a configured cap. Default is
**no limit** (existing users unaffected).

### Metric: output tokens only

    usage = SUM(output_tokens) over the rolling window

Input, cache-creation and cache-read tokens are deliberately NOT counted. An
earlier revision of this contract specified weighted "billable units"
(`output*5 + input*1 + cache_creation*1.25 + cache_read*0.1`); real traffic
showed cache reads dominating by roughly two orders of magnitude (~380M cache
reads against ~948K output tokens in 24h), which made the cap tens of times
larger than any number on the dashboard and impossible to set sensibly. Output
tokens are already displayed everywhere, so a cap can be reasoned about.
Computed from the `request_log` columns populated by usagecapture.go.

### Rolling window (not calendar)

Usage = SUM(output_tokens) over `request_log` where `user_token_id = ?` AND
`ts >= now - window`. Rolling, so there is no calendar-boundary burst hole, no
counter state to drift, and `request_log` is never pruned so lookback is safe.
The `(user_token_id, ts)` index already exists.

**Unblock time** is exact and must be computed, not estimated: read the window's
rows ascending by ts, accumulate their output tokens, and find the first row
where `total - accumulated < limit`. That row's `ts + window` (+1s) is when the
user drops back under. This value goes in the error message and `Retry-After`.

### Schema

`user_tokens` gains (additive ALTER + CREATE TABLE, `0` = unlimited):

    limit_output_tokens  INTEGER NOT NULL DEFAULT 0
    limit_window_seconds INTEGER NOT NULL DEFAULT 0

A limit is active only when BOTH are > 0. `usertoken.UserToken` and
`usertoken.Identity` gain `LimitOutputTokens int64` and `LimitWindowSeconds
int64`, populated by the existing LookupByToken/List/Get (no extra query).

No deployed database ever carried the earlier `limit_units` column, so it was
renamed rather than migrated. A database that happens to have `limit_units`
simply keeps it as an unused column.

### Enforcement

In `internal/proxy` ServeHTTP, after identity is known and **before**
`pool.Bind`, for `POST /v1/messages` ONLY (`count_tokens` is free; admin-token
identity is exempt). When the identity carries no active limit, skip entirely —
unlimited users must incur zero extra queries.

Over the cap => respond **429** with:
- header `Retry-After: <seconds until unblock>` (minimum 1)
- header `X-Router-Reason: user-quota` (distinguishes this from an upstream 429;
  it must NOT touch credential status or counters)
- Anthropic-shaped body:

    {"type":"error","error":{"type":"rate_limit_error","message":
     "proxy: usage limit reached - 1,000,000 output tokens per 24h (used 1,043,882). Resets at 2026-07-25 14:32 UTC (in 3h 12m)."}}

Log the blocked attempt to `request_log` with status 429, empty `credential_id`
and empty `conv_id`, `bytes_received` 0, and zero token columns — so it appears
in per-user stats as an error, contributes nothing to usage, and never
attributes to a credential.

Known limitation to document: token counts are only known after a response
completes, so a single large response can overshoot the cap; enforcement blocks
the NEXT request. Do not mutate the client's `max_tokens` to compensate.

### API

- `GET /api/users` — each entry gains `limit_output_tokens`,
  `limit_window_seconds`, `usage_output_tokens` (current rolling window, 0 when
  unlimited), `usage_pct` (0-100+, 0 when unlimited), `blocked` (bool),
  `blocked_until` (RFC3339 or null). Do NOT emit a fake reset time when the user
  is under the cap — rolling windows have no reset instant; `blocked_until` is
  non-null only while blocked.
- `POST /api/users/{id}/limit` `{"output_tokens": int, "window_seconds": int}`
  -> `{"ok":true,"limit_output_tokens":int,"limit_window_seconds":int}`. Both 0
  clears the limit. Reject negatives with 400; reject one-zero-one-nonzero with
  400 and a message explaining both are required.
- `GET /api/users/{id}/usage?window_seconds=N` ->
  `{"id":string,"window_seconds":int,"output_tokens":int}`. The same rolling sum
  for an arbitrary window, whether or not a limit is configured. 400 when
  `window_seconds` is missing, non-numeric or <= 0; 404 for an unknown user.
  This exists so the limit editor can show real usage before a cap is chosen.

### CLI

`claude-proxy users limit <id> --tokens 1M --window 24h` (accept K/M/G suffixes
and the existing period vocabulary) and `--none` to clear. `users list` shows
`LIMIT (OUT TOK)` and `USED (OUT TOK)` columns.

### Frontend (Users page)

- **Usage limit** column: a meter reusing the Subscriptions utilization-meter
  styling — `740K / 1M out tok · 74%` — warning tint >=80%, critical >=100%,
  plus `blocked until HH:MM` while blocked. Unlimited users show a muted
  "no limit".
- **Cache read** column next to Tokens in/out, sourced from the existing
  `GET /api/stats/users` `cache_read` field, with a tooltip stating it is not
  counted towards the limit. Cache reads being invisible is what made the
  original units metric so confusing; they must be on screen.
- Row action opens an **Edit limit** modal: an output-token field accepting
  `1M`/`500K` shorthand, window presets (1h/6h/24h/7d), a "No limit" clear
  action, and the caption "Counts output tokens only — input and cache tokens
  are ignored."
- The modal MUST show the user's current usage for the selected window before
  saving — "alice used 948K output tokens in the last 24h" — refetched from
  `GET /api/users/{id}/usage` whenever the window preset changes, with
  out-of-order responses discarded. Without this the operator has no basis for
  a number and the feature is unusable.
- Labels everywhere say "output tokens", never "units".
- Mock coverage for: unlimited user, healthy user, >=80% user, blocked user,
  plus the arbitrary-window usage route (scaled sub-linearly with the window so
  switching presets visibly changes the number).

## v5 additions — blocking prompt-suggestion traffic

### Schema

`user_tokens` gains `block_suggestions INTEGER NOT NULL DEFAULT 0` (additive
ALTER, same swallow-duplicate mechanism as `full_capture`). Existing rows read
as 0, i.e. unchanged behaviour.

### Model

`usertoken.UserToken` and `usertoken.Identity` gain `BlockSuggestions bool`;
`usertoken.SetBlockSuggestions(ctx, db, id, block)` writes it. `auth.go` copies
it into the request identity, so enforcement needs no extra lookup.

### API

- `GET /api/users` — each entry gains `"block_suggestions": bool`.
- `POST /api/users/{id}/suggestions` `{"block": bool}` ->
  `{"ok":true,"block_suggestions":bool}`. 404 for an unknown user, mirroring
  `/capture`.

### Frontend

- Users table gains a **Block suggestions** column between *Full capture* and
  *Usage limit*, using the same switch component. `captureToggle` and
  `suggestionsToggle` are now thin wrappers over a shared `switchToggle`
  helper — optimistic flip, painted from the server's returned value, reverted
  on failure.
- Column header carries the explanation: these are the per-turn requests that
  ask what you might type next, they carry the whole conversation, and blocking
  them costs only the typing suggestions themselves.
- Mock backend: `block_suggestions` on all four fixture users (bob and
  ci-runner on, so both states render) plus a stateful
  `POST /users/{id}/suggestions` route.

### Proxy behaviour

`internal/proxy/suggestions.go` short-circuits in `ServeHTTP` before
`enforceUserLimit` and `pool.Bind`. Detection requires the `[SUGGESTION MODE:`
marker to open the LAST message and that message to be from the user. The
response is an empty completion matching the request's `stream` flag (SSE or
JSON), reporting zero tokens. The attempt is logged to `request_log` with an
empty `credential_id`/`conv_id` and zero tokens, and carries
`X-Router-Reason: suggestion-blocked`.
