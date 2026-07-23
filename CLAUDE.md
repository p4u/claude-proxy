# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Is

`claude-proxy` is a sticky multi-subscription OAuth proxy for Claude Code. It sits between Claude Code clients and `api.anthropic.com`, managing multiple Claude OAuth credentials (subscriptions) with **usage-aware weighted-random selection** and stable per-conversation credential pinning. Built as a single static Go binary backed by SQLite.

It serves `/v1/*` as a transparent pass-through to `api.anthropic.com`, swapping in a managed credential's `Authorization` header. This is what Claude Code clients connect to.

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

## Architecture

### Request Flow (`/v1/*`)

```
Claude Code (ANTHROPIC_BASE_URL → proxy)
  → AuthMiddleware (internal/proxy/auth.go): admin token OR per-user bearer token
  → proxy handler (internal/proxy/)
      → router.Derive(): compute stable conversation key (4-priority algorithm)
      → pool.Bind(): get/create sticky credential for this conversation
      → forward to api.anthropic.com with swapped Authorization header
      → on 401: creds refresher triggers token refresh → retry
      → on 429: mark credential "limited", synthesize Retry-After if missing → pass 429 to client
      → on 200: heal "limited" → "active" immediately
  → SSE response streamed back verbatim
  → request_log row written (user, cred, status, bytes, latency)
```

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
| `internal/proxy/` | Proxy-mode HTTP handler + `AuthMiddleware` (two-tier auth, request logging) |
| `internal/pool/` | Usage-aware weighted-random selection + sticky conversation→credential binding |
| `internal/creds/` | Credential model, status management, proactive/reactive token refresh |
| `internal/router/` | Conversation key derivation |
| `internal/store/` | SQLite wrapper, schema, migrations (WAL mode) |
| `internal/ingest/` | OAuth `.credentials.json` parser/importer |
| `internal/admin/` | Admin REST API (`/admin/*` routes) |
| `internal/usertoken/` | Named per-user bearer tokens; request identity (`Identity{IsAdmin,...}` in context) |
| `internal/usage/` | Anthropic usage API client, background poller, history storage + asciigraph chart |
| `internal/prettylog/` | Custom slog handler with per-credential color output |

### Background Goroutines

- **Credential Refresher** (`internal/creds/refresh.go`): proactively refreshes tokens every 60s when `expires_at < now+5min`; reactively refreshes on 401 and retries
- **Pool Janitor** (`internal/pool/pool.go`): cleans up stale conversation→credential bindings
- **Usage Poller** (`internal/usage/`): periodically fetches 5h/7d utilization per credential into `usage_history` (this feeds the selection score)

### Selection Algorithm (`internal/pool/pool.go`)

`pickActiveLocked` → `weightedRandPick`: each candidate gets `score = weight × room_5h × room_7d^1.5`, where `room_X = max(0, 1 − utilization/100)`. The 5h and 7d windows are **independent ceilings** (a request 429s on whichever it hits first), so their remaining room is **multiplied**, not averaged — saturation on either window drives the score toward zero. The `^1.5` on the 7d term protects the slow-resetting weekly quota harder than the cheap 5h window (`sevenDayExp` constant). `seven_day_sonnet_pct` is intentionally ignored. The most recent `usage_history` snapshot is used regardless of age — stale data beats assuming 0% usage; `headroom=1.0` when no snapshot exists (newly imported creds).

**Hard saturation cutoff:** a credential whose latest snapshot reports either window at **≥100%** is excluded from the active candidate set *before* scoring (`NOT EXISTS` clause in the active query) — a maxed-out subscription is never selected, regardless of weight. If every active credential is saturated, the pool falls through to `limited` credentials (to obtain a real 429 + Retry-After), and returns `ErrNoCredentials` only if there are none.

> Selection stays **weighted-random** rather than greedy-best on purpose: bindings are sticky and usage is only polled every 10 min, so greedy would dump every new conversation onto one cred between polls (thundering herd) and overshoot. Weighted-random spreads load and self-corrects each poll cycle.

### Storage Schema (SQLite)

Six tables (`internal/store/schema.go`): `credentials`, `conversations`, `rr_cursor` (legacy round-robin state), `user_tokens` (named bearer tokens), `request_log` (one row per forwarded request, for per-user stats), `usage_history` (utilization snapshots driving selection). Deleting a credential cascades to `usage_history` (`ON DELETE CASCADE`); `conversations` bindings are cleared inside `creds.Delete`'s transaction, since older DBs created that FK without a cascade clause. `request_log` additionally carries per-request token usage columns (`model`, `input_tokens`, `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`), added via the existing swallow-duplicate `ALTER TABLE` migration mechanism and populated by `internal/proxy/usagecapture.go` from the tee'd response stream (SSE and non-streaming JSON) — this feeds the web UI's token stats and never affects what the client receives. Core dependency: `modernc.org/sqlite` (pure Go, no CGO required); `guptarohit/asciigraph` for usage charts and `charmbracelet/bubbletea`+`lipgloss`+`bubbles` for the management TUI.

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

CLI equivalents live under `claude-proxy users <create|list|stats|token|disable|enable|rm|refresh>`.

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
