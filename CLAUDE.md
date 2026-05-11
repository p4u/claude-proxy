# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Is

`claude-proxy` is a sticky multi-subscription OAuth proxy for Claude Code. It sits between Claude Code clients and `api.anthropic.com`, managing multiple Claude OAuth credentials (subscriptions) with weighted round-robin selection and stable per-conversation credential pinning. Built as a single static Go binary backed by SQLite.

## Build & Test Commands

```bash
# Run tests (host Go toolchain required)
make test           # go test -race ./...
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

### Request Flow

```
Claude Code (ANTHROPIC_BASE_URL → proxy)
  → proxy handler (internal/proxy/)
      → router.Derive(): compute stable conversation key (4-priority algorithm)
      → pool.Bind(): get/create sticky credential for this conversation
      → forward to api.anthropic.com with swapped Authorization header
      → on 401: creds refresher triggers token refresh → retry
      → on 429: mark credential "rate-limited" → pass 429 to client
  → SSE response streamed back verbatim
```

### Conversation Key Derivation (4-priority, `internal/router/`)

1. `X-Router-Conversation-ID` header (explicit override)
2. `$.metadata.user_id` from request JSON body
3. SHA256(system_prompt + first_user_message)
4. SHA256(remote_addr + body[:4096]) — fallback

### Package Responsibilities

| Package | Role |
|---|---|
| `cmd/claude-proxy/` | CLI entry point; `serve` and `creds` subcommands |
| `internal/proxy/` | HTTP handler: header rewrites, body buffering, SSE passthrough |
| `internal/pool/` | Weighted round-robin selection + sticky conversation→credential binding |
| `internal/creds/` | Credential model, status management, proactive/reactive token refresh |
| `internal/router/` | Conversation key derivation |
| `internal/store/` | SQLite wrapper, schema, migrations (WAL mode) |
| `internal/ingest/` | OAuth `.credentials.json` parser/importer |
| `internal/admin/` | Admin REST API (`/admin/*` routes) |
| `internal/prettylog/` | Custom slog handler with per-credential color output |

### Background Goroutines

- **Credential Refresher** (`internal/creds/refresh.go`): proactively refreshes tokens every 60s when `expires_at < now+5min`; reactively refreshes on 401 and retries
- **Pool Janitor** (`internal/pool/pool.go`): cleans up stale conversation→credential bindings

### Storage Schema (SQLite)

Three tables: `credentials` (21 cols), `conversations` (6 cols), `rr_cursor` (round-robin state). Single dependency: `modernc.org/sqlite` (pure Go, no CGO required).

## Configuration

All runtime config comes from `.env` (copy from `.env.example`). Key variables:

| Variable | Default | Notes |
|---|---|---|
| `HOST_BIND` | `127.0.0.1` | Bind address; change to `0.0.0.0` only when behind TLS |
| `HOST_PORT` | `8787` | Listening port |
| `PROXY_AUTH_TOKEN` | _(empty)_ | Bearer token for downstream auth; empty = no auth |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `LOG_FORMAT` | `auto` | `pretty\|text\|json\|auto` |
| `TLS_DOMAIN` | _(empty)_ | Set to enable Traefik + Let's Encrypt |
| `CLAUDE_PROXY_IMAGE` | `ghcr.io/p4u/claude-proxy:latest` | Override to use local build |

## Credential Management

```bash
make import FROM=path/to/.credentials.json LABEL=myaccount [WEIGHT=5]
make list                        # Show all credentials with status/counters
make usage                       # Fetch 5h/7d usage % from Anthropic for all creds
make usage ID=cred_xxx           # Usage % for one credential
make disable ID=...              # Exclude from round-robin
make rm ID=...                   # Delete credential
make refresh ID=...              # Force token refresh
make weight ID=... W=N           # Adjust round-robin weight
make export-credentials > f.jsonl  # Backup current tokens to file
cat f.jsonl | make import-credentials  # Restore from backup
```

Weights default: `max/team/enterprise=5`, `pro=1` (derived from subscription tier in the credential file).

## Admin API

All endpoints require `Authorization: Bearer <PROXY_AUTH_TOKEN>` if token is set.

- `GET /health` — liveness check
- `GET /admin/credentials` — credential list with status
- `GET /admin/conversations` — active conversation bindings
- `GET /admin/stats` — aggregate counters
- `POST /admin/credentials/:id/refresh` — force refresh
- `DELETE /admin/credentials/:id` — delete credential

## CI/CD

GitHub Actions (`.github/workflows/ci.yml`): lint → test → multi-arch Docker image (`linux/amd64` + `linux/arm64`) pushed to GHCR.

Image tags: `:latest` and `:sha-<short>` on `main`; semver tags on `v*.*.*` pushes.
