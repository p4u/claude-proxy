# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Is

`claude-proxy` is a sticky multi-subscription OAuth proxy for Claude Code. It sits between Claude Code clients and its upstream providers, managing multiple credentials with **usage-aware weighted-random selection** and stable per-conversation credential pinning. Built as a single static Go binary backed by SQLite.

It serves `/v1/*` as a transparent pass-through, swapping in a managed credential's `Authorization` header. This is what Claude Code clients connect to.

Two upstreams are supported (`internal/provider`): **Anthropic** OAuth subscriptions and **Z.AI GLM** coding plans, whose API is Anthropic-compatible. The provider that serves a request is decided by the model the client asked for, so a user picks a GLM model in Claude Code's `/model` picker and it just works. See [Providers](#providers-multi-upstream).

## Build & Test Commands

```bash
# Run tests (host Go toolchain required)
make test           # go test -race ./...
go test -race -run TestName ./internal/pool/   # single test in one package
make lint           # gofmt + go vet + golangci-lint

# Install golangci-lint v2 (CI version)
make lint-install

# Build Docker image from source
make build

# Pull latest published image
make pull
```

Running the proxy (Docker-based workflow):
```bash
make env            # Bootstrap .env from .env.example (run once)
make up             # Start containers
make health         # Verify proxy is running
make logs           # Tail proxy logs
make down           # Stop and remove containers
```

### Web UI development

`internal/webui/static/` is plain ES modules + CSS with **no build step** — there
is no `package.json`, and the tree is compiled in via `go:embed`, so changing a
`.js`/`.css` file requires only rebuilding the Go binary.

Appending **`?mock=1`** to the UI URL makes `index.html` import
`static/js/mock.js`, which intercepts `fetch` for `/api/*` and serves realistic
sample credentials, users, and usage series. That is the fast path for frontend
work: no proxy, no database, no real subscriptions. The mock keeps per-session
state for the toggles it exposes (`full_capture`, `block_suggestions`, the usage
limit) so switch behaviour can be exercised end-to-end. When adding an endpoint
to `/api`, add it to `mock.js` too or the UI silently loses its offline mode.

## Architecture

### Request Flow (`/v1/*`)

```
Claude Code (ANTHROPIC_BASE_URL → proxy)
  → AuthMiddleware (internal/proxy/auth.go): admin token OR per-user bearer token
  → proxy handler (internal/proxy/)
      → providerFor(): the request body's "model" selects the upstream
        (internal/proxy/routing.go); everything below is scoped to it
      → blockSuggestion(): POST /v1/messages only — for users with
        block_suggestions set, Claude Code's prompt-suggestion requests are
        answered locally with an empty completion + X-Router-Reason:
        suggestion-blocked, never forwarded (internal/proxy/suggestions.go)
      → enforceUserLimit(): POST /v1/messages only — 429 + X-Router-Reason: user-quota
        when the user is over their rolling usage cap (internal/proxy/userlimit.go)
      → router.Derive(): compute stable conversation key (4-priority algorithm)
      → pool.Bind(): get/create sticky credential for (conversation, provider)
      → forward to the provider's base URL with swapped Authorization header
      → GET /v1/models: union of every provider that has a usable credential,
        Anthropic entries augmented with "[1m]" variants, cached 5min
        (internal/proxy/models1m.go) for Claude Code gateway model discovery
      → on 401: OAuth credential → refresher triggers token refresh → retry;
        API key → marked revoked immediately (nothing to refresh)
      → on 429: mark credential "limited", synthesize Retry-After if missing → pass 429 to client
      → on 200: heal "limited" → "active" immediately
  → SSE response streamed back verbatim
  → request_log row written (user, cred, status, bytes, latency)
```

### Providers (multi-upstream)

`internal/provider` is the single place per-upstream behaviour is declared;
nothing else tests for a provider by name.

| | Anthropic | Z.AI GLM |
|---|---|---|
| Base URL | `https://api.anthropic.com` | `https://api.z.ai/api/anthropic` |
| Model prefix | `claude-` | `glm-` |
| Credential | OAuth (access + refresh) | static API key |
| Refreshable | yes | no — a 401 means a bad key |
| Usage API | `/api/oauth/usage` | none |
| `[1m]` variants | yes | no — Z.AI 400s on the suffixed form |
| Advertised as | native ID | `claude-`-prefixed alias (see below) |

**GLM models are advertised under a `claude-` alias, and must be.** Claude Code
filters the gateway model list client-side; its `/v1/models` bootstrap runs

```js
.filter(o => /^(claude|anthropic)/i.test(o.id))
.filter(o => { let i = QQ(o.id); return i === null || i === kot })
```

so a bare `glm-4.7` is discarded in the CLI and never reaches the `/model`
picker, no matter what this proxy returns. The second filter admits IDs the CLI
does not recognise (`QQ` returns `null` outside its built-in table), so
`claude-glm-4.7` passes both. `provider.AdvertisePrefix` drives this:
`AdvertisedID` applies it in the merged model list (with the display name tagged
`(Z.AI GLM)` so the picker stays honest about the upstream), and `WireModel`
strips it again in `rewriteModel` before forwarding — Z.AI rejects the prefixed
name, so the alias must never escape the proxy. The native `glm-4.7` keeps
routing normally for clients that address it directly (raw API calls, or
`ANTHROPIC_DEFAULT_SONNET_MODEL=glm-4.7`).

Ordering in `ForModel` is load-bearing: aliases are matched **before** native
prefixes, because `claude-glm-4.7` also starts with Anthropic's `claude-`, so a
naive single pass would hand every GLM model to the wrong upstream.

**The client must be in gateway mode or none of this is reachable.** Claude Code
fetches `GET /v1/models` from the configured base URL only when its provider
mode is `gateway`:

```js
function Hn(){ if(Cy()) return "gateway"; ... return "firstParty" }   // Cy() = gatewayAuth
async function AXi(){ if (Z.CLAUDE_CODE_USE_GATEWAY) { ... U5e({url, jwt, ...}) } }
```

so **both** `CLAUDE_CODE_USE_GATEWAY=1` (which promotes `ANTHROPIC_BASE_URL` +
`ANTHROPIC_AUTH_TOKEN` into a gateway credential) and
`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` (a second gate *inside* the
gateway branch) are required. With only `ANTHROPIC_BASE_URL` set the client is
`firstParty`, never requests the list, and nothing this proxy serves can affect
the picker — which is equally true of the pre-existing `MODELS_1M` feature.
Verified end-to-end: the discovered entries land in `~/.claude.json` under
`additionalModelOptionsCache`, which is what the picker renders.

**GLM needs no translation layer.** Its Anthropic-compatible surface was verified
live against `/v1/models`, `/v1/messages`, `/v1/messages/count_tokens`, SSE
streaming, tool use, `cache_control`, and Claude Code's `anthropic-beta` headers.
Only the base URL and the credential differ, so `usagecapture.go`,
`promptcapture.go`, the 429 handling and the SSE relay all work unmodified.

**Routing is by model, resolved before any credential is chosen.**
`provider.ForModel` prefix-matches the request body's `model` (trimming a
trailing `[1m]`, which Claude Code normally strips client-side). Unknown models
fall to Anthropic, so future Claude IDs keep working without a registry edit.
Provider is then a **hard filter** in `pool.pickActiveLocked` — a GLM model can
only be served by a GLM key, so the two never compete on weight or usage score.
No credential for the requested provider ⇒ 503 +
`X-Router-Reason: provider-unavailable`, naming the provider.

**Sticky keys are provider-scoped** (`pool.Key`). A Claude Code session sends its
main-model turns and its background haiku calls under one derived conversation
ID; once those can land on different providers, a single key is not enough —
each request would find the other provider's credential pinned and migrate it,
so neither would ever be sticky. Anthropic keeps the bare ID (existing rows and
dashboard links are untouched); other providers are prefixed (`glm:<id>`). The
qualified value is used for `conversations`, `request_log` and `prompt_log`
alike, so a GLM and a Claude thread in one session show as two conversations —
which is truthful, they are two independent credential bindings.

**Balancing GLM keys: weight only, reactive to 429.** Z.AI publishes no usage
endpoint and no rate-limit response headers (confirmed by probing; the DevPack
docs describe only a web dashboard). The poller therefore **skips** providers
without a usage API rather than writing zeroed snapshots — a 0% row is
indistinguishable from a genuinely idle credential and would make an exhausted
key look like the most attractive one in the pool. With no snapshot, selection
takes the existing `headroom = 1.0` bootstrap path and picks on weight; a 429
marks the key limited until `Retry-After`. Since provider is a hard filter,
weights only ever compete within one provider, so GLM keys default to weight 1.

> Possible future refinement, deliberately not implemented: Z.AI publishes a
> credit formula (`(in×Min + cached×Mcached + out×Mout)/10000`), per-model
> multipliers, and per-plan 5h/weekly caps. `request_log` already stores all
> four token counts, so a locally metered synthetic utilization could feed the
> existing `usage_history` + `pool.Score` machinery unchanged.

### Conversation Key Derivation (4-priority, `internal/router/`)

1. `X-Router-Conversation-ID` header (explicit override)
2. `$.metadata.user_id` from request JSON body
3. SHA256(system_prompt + first_user_message)
4. SHA256(remote_addr + body[:4096]) — fallback

### Package Responsibilities

| Package | Role |
|---|---|
| `cmd/claude-proxy/` | CLI entry point; `serve`, `tui`, `creds`, and `users` subcommands |
| `internal/tui/` | Interactive Bubble Tea management UI (credentials + users tabs) for `claude-proxy tui` |
| `internal/webui/` | Embedded browser dashboard (`go:embed`), served at `/` when `CLAUDE_PROXY_UI_PASSWORD` is set (old `/ui` 308-redirects); cookie-authenticated JSON API under `/api` (contract: `docs/WEBUI.md`) |
| `internal/provider/` | Per-upstream behaviour table (base URL, model prefixes, refreshability, usage API, `[1m]` support) |
| `internal/proxy/` | Proxy-mode HTTP handler + `AuthMiddleware` (two-tier auth, request logging); `routing.go` maps model → provider |
| `internal/pool/` | Usage-aware weighted-random selection + sticky conversation→credential binding |
| `internal/creds/` | Credential model, status management, proactive/reactive token refresh |
| `internal/router/` | Conversation key derivation |
| `internal/store/` | SQLite wrapper, schema, migrations (WAL mode), lock-contention retry |
| `internal/ingest/` | OAuth `.credentials.json` parser/importer |
| `internal/admin/` | Admin REST API (`/admin/*` routes) |
| `internal/usertoken/` | Named per-user bearer tokens; request identity (`Identity{IsAdmin,FullCapture,...}` in context); output-token usage limits (`limit.go`) |
| `internal/usage/` | Anthropic usage API client, background poller, history storage + asciigraph chart |
| `internal/prettylog/` | Custom slog handler with per-credential color output |

### Background Goroutines

- **Credential Refresher** (`internal/creds/refresh.go`): proactively refreshes tokens every 60s when `expires_at < now+5min`; reactively refreshes on 401 and retries
- **Pool Janitor** (`internal/pool/pool.go`): cleans up stale conversation→credential bindings
- **Usage Poller** (`internal/usage/`): periodically fetches 5h/7d utilization per credential into `usage_history` (this feeds the selection score)

### Selection Algorithm (`internal/pool/pool.go`)

`pickActiveLocked` → `weightedRandPick`: each candidate gets `score = weight × room_5h × room_7d^1.5`, where `room_X = max(0, 1 − utilization/100)`. The 5h and 7d windows are **independent ceilings** (a request 429s on whichever it hits first), so their remaining room is **multiplied**, not averaged — saturation on either window drives the score toward zero. The `^1.5` on the 7d term protects the slow-resetting weekly quota harder than the cheap 5h window (`sevenDayExp` constant). `seven_day_sonnet_pct` is intentionally ignored. The most recent `usage_history` snapshot is used regardless of age — stale data beats assuming 0% usage; `headroom=1.0` when no snapshot exists (newly imported creds).

**Hard saturation cutoff:** a credential whose latest snapshot reports either window at **≥100%** is excluded from the active candidate set *before* scoring (`NOT EXISTS` clause in the active query) — a maxed-out subscription is never selected, regardless of weight. If every active credential is saturated, the pool falls through to `limited` credentials (to obtain a real 429 + Retry-After), and returns `ErrNoCredentials` only if there are none.

**Sticky rebinding:** the ≥100% cutoff also applies to *existing* bindings. On each `Bind()`, if the pinned credential's latest snapshot is saturated, the conversation is migrated onto a fresh non-saturated credential (the `conversations` row's `credential_id` is updated and the caller sees `isNew=true`). When no healthy alternative exists the saturated pin is kept, so the request still reaches Anthropic for a real 429. The scoring math (`Score`, `Room`, `Saturated`, `SevenDayExp`) is exported from `internal/pool` and reused verbatim by the web UI's `/api/usage/current` selection view, so the two can never drift.

> Selection stays **weighted-random** rather than greedy-best on purpose: bindings are sticky and usage is only polled every 10 min, so greedy would dump every new conversation onto one cred between polls (thundering herd) and overshoot. Weighted-random spreads load and self-corrects each poll cycle.

### Storage Schema (SQLite)

Eight tables (`internal/store/schema.go`): `credentials` (incl. `provider`:
`anthropic` | `glm`, added via the swallow-duplicate `ALTER TABLE` mechanism —
its `DEFAULT 'anthropic'` is what migrates existing rows, no backfill needed),
`conversations` (whose `id` is the provider-qualified sticky key), `rr_cursor` (legacy round-robin state), `user_tokens` (named bearer tokens, plus the per-user usage cap `limit_output_tokens` / `limit_window_seconds`, `0` = unlimited, and `block_suggestions` = suppress Claude Code's prompt-suggestion requests), `request_log` (one row per forwarded request, for per-user stats), `usage_history` (utilization snapshots driving selection), `prompt_log` (last user prompt per `POST /v1/messages`, `user_token_id` FK `ON DELETE SET NULL`; never stores responses, gated + retained by `CLAUDE_PROXY_PROMPT_RETENTION_DAYS`, captured in `internal/proxy/promptcapture.go` and pruned by its hourly janitor), `conversation_message` (opt-in whole-conversation capture: `UNIQUE(conv_id, seq)` where `seq` is the message's index in the request's `messages` array, so re-sent history is idempotent; same retention window and janitor as `prompt_log`). Deleting a credential cascades to `usage_history` (`ON DELETE CASCADE`); `conversations` bindings are cleared inside `creds.Delete`'s transaction, since older DBs created that FK without a cascade clause. `request_log` additionally carries per-request token usage columns (`model`, `input_tokens`, `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`), added via the existing swallow-duplicate `ALTER TABLE` migration mechanism and populated by `internal/proxy/usagecapture.go` from the tee'd response stream (SSE and non-streaming JSON) — this feeds the web UI's token stats and never affects what the client receives. Core dependency: `modernc.org/sqlite` (pure Go, no CGO required); `guptarohit/asciigraph` for usage charts and `charmbracelet/bubbletea`+`lipgloss`+`bubbles` for the management TUI.

### Write contention (`internal/store/store.go`, `retry.go`)

SQLite allows exactly one writer at a time, and this proxy has many: the request
logger, the prompt/conversation capture writers, the usage poller, the pool and
credential janitors, and any `claude-proxy` CLI or TUI run against the same file
from another process. Losing a write race is normal; the connection is
configured so that losing one is survivable.

**`_txlock=immediate` is the load-bearing setting.** Every explicit transaction
takes the write lock at `BEGIN`. Without it, a transaction that reads before it
writes — `pool.Bind`, `creds.Delete` — pins a read snapshot, and any commit by
another connection in the gap makes the later write fail with
`SQLITE_BUSY_SNAPSHOT`. That error is returned **instantly and unconditionally**:
`busy_timeout` is never consulted, because no amount of waiting can make a stale
snapshot valid again. It surfaced to clients as `502 proxy: database is locked`,
since `Bind` is the only DB failure `failBind` maps to a 502. Taking the lock up
front converts that unretryable error into an ordinary wait.
`TestDeferredTxFailsOnConcurrentCommit` pins the hazard and
`TestConcurrentBindShapedWritesSucceed` pins the fix — the latter fails within
seconds if the DSN reverts to deferred.

Supporting settings: `synchronous=NORMAL` (the standard WAL pairing — durability
narrows to "the last commits may be lost on power loss", never corruption — and
dropping the per-commit fsync shortens every lock hold), `busy_timeout` 15s,
`SetMaxOpenConns(8)` (writers serialise anyway, so an unbounded pool only
deepens the queue each writer must outwait). `store.Retry` re-runs a
self-contained transaction on `store.IsBusy` with jittered backoff — jitter
matters, since writers backing off on identical schedules keep colliding on
identical schedules. `Bind` is wrapped in it; `ErrNoCredentials` and
`ErrCredentialOrphaned` are not busy errors, so they still fail fast.

**Deletes are batched, not unbounded.** `purgeExpired` removes retention-expired
rows `purgeBatch` at a time with a pause between batches. One unbounded `DELETE`
over a retention window holds the write lock for as long as it takes to finish,
which is precisely how a background janitor starves live requests past their
busy timeout.

## Configuration

All runtime config comes from `.env` (copy from `.env.example`). Key variables:

| Variable | Default | Notes |
|---|---|---|
| `HOST_BIND` | `127.0.0.1` | Bind address; change to `0.0.0.0` only when behind TLS |
| `HOST_PORT` | `8787` | Listening port |
| `PROXY_AUTH_TOKEN` | _(empty)_ | Admin bearer token; empty = no auth. Per-user tokens (see `users`) authenticate alongside this. |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `LOG_FORMAT` | `auto` | `pretty\|text\|json\|auto` |
| `LOG_COLOR` | `auto` | `auto\|always\|never` |
| `UI_PASSWORD` | _(empty)_ | Web UI password (`CLAUDE_PROXY_UI_PASSWORD`); empty = UI disabled. UI is served at `/` (old `/ui/*` paths 308-redirect); API prefixes `/v1/`, `/admin/`, `/api/`, `/health` are reserved |
| `MODELS_1M` | _(enabled)_ | Append `[1m]` model variants to `GET /v1/models` for Claude Code gateway model discovery (`CLAUDE_PROXY_MODELS_1M`); set `0` to disable |
| `UI_SECURE_COOKIES` | _(empty)_ | Force `Secure` UI session cookies (`CLAUDE_PROXY_UI_SECURE_COOKIES`); auto-detected behind Traefik via `X-Forwarded-Proto` |
| `TLS_DOMAIN` | _(empty)_ | Set (with `TLS_EMAIL`) to enable Traefik + Let's Encrypt |
| `CLAUDE_PROXY_IMAGE` | `ghcr.io/p4u/claude-proxy:latest` | Override to use local build |

## Credential Management

For interactive management, prefer the TUI over individual Makefile targets:

```bash
make tui                         # Bubble Tea UI: credentials + users (needs a TTY)
# or, locally without docker:
go run ./cmd/claude-proxy tui --db ./data/proxy.db
```

TUI keys — Credentials tab: `r` refresh token, `u` update token from a fresh login file,
`w` set weight, `d` disable/enable, `x` delete, `i` import from file, `p` paste a
`.credentials.json` directly (multi-line; `ctrl+s` to import). Users tab: `c` create,
`R` rotate token, `d` disable/enable, `x` delete. `tab` switches tabs, `q` quits.

TUI keys also include `k` — add a static API key (GLM).

### Adding a GLM (Z.AI) API key

```bash
# Reads the key from stdin, which keeps it out of shell history and out of
# `ps` output (where a --key flag would be visible to any local user).
echo "$ZAI_KEY" | claude-proxy creds add-key --provider glm --label zai-main --plan pro
make add-key PROVIDER=glm LABEL=zai-main            # prompts for the key
```

The key is verified with a live `GET /v1/models` before it is stored, mirroring
the liveness check `ingest.insertVerified` applies to OAuth imports: a credential
that cannot authenticate must never enter the pool, because the selector would
then hand live conversations to it. Duplicates are rejected by access token
(`creds.HasAccessToken`) — `HasRefreshToken` cannot work here, since every API-key
row stores an empty refresh token and would collide with every other.

Key credentials store `expires_at` ~100 years out: the column is `NOT NULL` and
the refresher compares against it, so a date the refresh window can never reach
is simpler than threading a nullable expiry through every caller. The refresher
also skips non-refreshable providers outright. CLI, TUI and web UI all render it
as "never" rather than a date in 2126.

```bash
make import FROM=path/to/.credentials.json LABEL=myaccount [WEIGHT=5]
make update ID=cred_xxx FROM=path/to/new.credentials.json  # replace tokens from a fresh login
make list                        # Show all credentials with status/counters
make usage                       # Fetch live 5h/7d usage % from Anthropic for all creds
make usage ID=cred_xxx           # Usage % for one credential
make usage-history PERIOD=24h    # Chart stored usage history (optional ID=cred_xxx)
make disable ID=...              # Exclude from selection
make rm ID=...                   # Delete credential
make refresh ID=...              # Force token refresh
make weight ID=... W=N           # Adjust selection weight
make export-credentials > f.jsonl  # Backup current tokens to file
cat f.jsonl | make import-credentials  # Restore from backup
```

Weights default: `max/team/enterprise=5`, `pro=1` (derived from subscription tier in the credential file).

## User Token Management (multi-user auth)

Per-user named bearer tokens authenticate alongside `PROXY_AUTH_TOKEN`. Each forwarded request is attributed to a user in `request_log` for stats.

```bash
make user-create NAME=alice          # Create a user, prints its bearer token
make user-list                       # List all user tokens
make user-stats ID=utok_xxx PERIOD=24h  # Per-user request aggregation (omit ID for all)
make user-token ID=utok_xxx          # Print a user's bearer token
make user-disable / user-enable / user-rm / user-refresh ID=utok_xxx
```

CLI equivalents live under `claude-proxy users <create|list|stats|token|disable|enable|rm|refresh|limit|suggestions>`.

### Blocking prompt suggestions per user

```bash
claude-proxy users suggestions utok_xxx on    # answer them locally, no quota spent
claude-proxy users suggestions utok_xxx off   # forward to Anthropic (default)
claude-proxy users list                       # SUGG column shows block/fwd
```

Also a switch in the web UI's Users table. See the section below for what these
requests are and why suppressing them is safe.

### Per-user usage limits

Each user token can carry a cap on how much they may spend over a **rolling
window**. Default is no limit; existing users are unaffected.

```bash
claude-proxy users limit utok_xxx --tokens 1M --window 24h  # set
claude-proxy users limit utok_xxx --none                     # clear
claude-proxy users list                                      # shows LIMIT + USED
```

**Metric — output tokens only.** `SUM(output_tokens)`; input, cache creation and
cache read are ignored. An earlier version counted weighted "billable units"
across all four token kinds, but cache reads dominate real traffic by two orders
of magnitude, which made the cap unrelatable to anything on the dashboard.
Values come from the `request_log` token columns populated by `usagecapture.go`.
Because no deployed database ever carried the old `limit_units` column, it was
renamed to `limit_output_tokens` rather than migrated.

**Rolling window, not calendar.** Usage is `SUM(output_tokens)` over `request_log` where
`user_token_id = ?` and `ts >= now - window` (the `(user_token_id, ts)` index
already exists). There is no counter state to drift and no calendar-boundary
burst hole; `request_log` is never pruned, so lookback is always safe.

**Unblock time is computed exactly, never estimated.** `usertoken.LimitStatus`
reads the window's rows ascending by `ts`, accumulates their output tokens, and finds
the first row where `total - accumulated < limit`; that row's `ts + window + 1s`
is the instant the user drops back under. It feeds both the `Retry-After` header
and the error message. This second pass only runs when the user is actually
blocked; a user with no limit configured performs **zero** queries.
`GET /api/users/{id}/usage?window_seconds=N` exposes the same rolling sum for an
arbitrary window (no limit required), which is what the web UI's *Edit limit*
modal shows so an operator can size a cap against real traffic.

Enforcement sits in `ServeHTTP` after the identity is known and *before*
`pool.Bind`, so a blocked request never touches credential state — no binding,
no counters, no status change. The blocked attempt is written to `request_log`
with status 429, empty `credential_id`/`conv_id` and zero tokens, so it shows up
as an error in per-user stats while contributing nothing to usage. Metering
errors fail open.

> **Known limitation:** token counts are only known after a response completes,
> so one large response can overshoot the cap — enforcement blocks the *next*
> request rather than truncating the current one. The client's `max_tokens` is
> deliberately never mutated to compensate.

## Blocking prompt-suggestion traffic (`internal/proxy/suggestions.go`)

Claude Code emits an extra `POST /v1/messages` per turn asking the model to
guess what the user will type next. In the CLI bundle it is
`querySource:"prompt_suggestion"`: a fork of the conversation with one appended
user message opening `[SUGGESTION MODE:`. It therefore carries the **entire
history**, costs a real round trip, and — because `promptcapture.go` records the
last user message — buries the user's actual prompts in the capture log.

`user_tokens.block_suggestions` opts a user out. The check sits in `ServeHTTP`
before both `enforceUserLimit` and `pool.Bind`, so a suppressed request spends
no quota, binds no credential, and is never captured. Users without the flag pay
one cheap body check.

Detection requires the marker to open the **last** message and that message to
be from the user — the exact shape the CLI produces. Quoting the marker later in
a prompt (asking about it, say) is deliberately left alone; `TestIsSuggestionRequest`
pins both directions.

The reply is a well-formed but empty completion — SSE or JSON, matching the
request's `stream` flag — reporting zero tokens, which is true since nothing was
sent upstream. Claude Code already treats an empty answer as a normal outcome
(it looks for a text block, finds none, and records `suppressed`, the same branch
as when its own filter rejects a too-long or evaluative suggestion), so nothing
errors and nothing retries. The user simply stops seeing typing suggestions.

Matching only the opening marker rather than the whole prompt means a reworded
body still matches; a **renamed** marker stops matching and requests are
forwarded exactly as before. That is the intended failure direction — a miss
costs one request, a false positive would silently swallow a real prompt.

## Admin API

`AuthMiddleware` accepts either the admin `PROXY_AUTH_TOKEN` or any active per-user token; `/admin/*` routes require the admin token (or no token configured). Identity is carried in request context (`usertoken.Identity`).

- `GET /health` — liveness check
- `GET /admin/credentials` — credential list with status
- `GET /admin/conversations` — active conversation bindings
- `GET /admin/stats` — aggregate counters
- `POST /admin/credentials/:id/disable` — disable a credential
- `DELETE /admin/credentials/:id` — delete credential

A separate, cookie-authenticated API backs the web UI at `/api/*` (password
login, not `PROXY_AUTH_TOKEN`/user tokens); see [`docs/WEBUI.md`](./docs/WEBUI.md)
for the full contract (auth model, endpoints, response shapes).

## CI/CD

GitHub Actions (`.github/workflows/ci.yml`): lint → test → multi-arch Docker image (`linux/amd64` + `linux/arm64`) pushed to GHCR.

Image tags: `:latest` and `:sha-<short>` on `main`; semver tags on `v*.*.*` pushes.
