# claude-proxy

[![CI](https://github.com/p4u/claude-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/p4u/claude-proxy/actions/workflows/ci.yml)
[![GHCR](https://img.shields.io/badge/ghcr.io-claude--proxy-blue?logo=docker)](https://github.com/p4u/claude-proxy/pkgs/container/claude-proxy)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL%20v3-blue.svg)](./LICENSE)

A sticky multi-subscription HTTP proxy for [Claude Code](https://docs.claude.com/en/docs/agents-and-tools/claude-code/overview).
It pools multiple Claude subscription OAuth credentials, assigns each new
conversation to one of them via usage-aware weighted selection, and pins that
conversation to the chosen credential for the rest of its lifetime.

> [!WARNING]
> **Research and educational proof-of-concept.** This project exists
> exclusively to study and document the security and authentication model
> of Claude Code's subscription tokens. It is **not** a product, **not**
> intended for production use, and **not** intended to enable circumvention
> of any service's terms of use. See the [Disclaimer](#disclaimer) below
> before running any of this code.

---

## Why

Claude Code authenticates against your Pro / Max / Team / Enterprise
subscription with an OAuth Bearer token stored in `~/.claude/.credentials.json`.
A single token has a per-account usage cap. If you legitimately own multiple
subscriptions (CI, separate work/personal accounts, …) and want to use them
from one machine without juggling environment variables, you need a router
that:

- holds the OAuth credentials for you,
- keeps refreshing them so they don't expire,
- transparently swaps the `Authorization` header on every outbound request to Anthropic,
- decides which credential a *new* conversation should bind to,
- pins each conversation to one credential so prompt caching and conversation continuity stay intact,
- automatically reroutes around credentials that hit a rate limit (`429`) or break (`401`).

`claude-proxy` is exactly that.

## Architecture

```
                          ┌─────────────────────────────────────┐
  Claude Code agent A ───►│  /v1/*                              │
  Claude Code agent B ───►│  • sticky conv → cred binding       │──► api.anthropic.com
  Claude Code agent C ───►│  • usage-aware weighted selection   │
                          │  • 401 → refresh + retry            │
                          │  • 429 → mark limited + reroute     │
                          └─────────────────────────────────────┘
```

Each Claude Code instance is configured with `ANTHROPIC_BASE_URL` pointing at
the proxy. The proxy receives the request, picks/keeps a credential for the
conversation, replaces only the `Authorization` header (everything else passes
through unmodified — including `anthropic-beta`, `user-agent: claude-cli/...`,
`x-app: cli`, and the `"You are Claude Code"` system block that Anthropic's
OAuth backend requires), and streams the SSE response back unchanged.

The proxy itself **never performs OAuth.** Operators run `claude /login` once
per subscription, then import the resulting `.credentials.json` into the
proxy. From then on, the proxy refreshes access tokens on its own
(`grant_type=refresh_token` against `https://platform.claude.com/v1/oauth/token`)
to keep them alive.

## Features

- **Sticky binding** — each Claude Code conversation pins to a single credential for its lifetime.
- **Usage-aware weighted selection** for new conversations: each credential's score combines its configured weight with live 5h/7d usage headroom, and a credential that has hit either limit is excluded entirely. Default weights: `max`/`team`/`enterprise` = 5, `pro` = 1.
- **Automatic refresh** — proactive (every 60 s if `expires_at < now+5min`) and reactive (on `401` retry once with a fresh token).
- **Automatic reroute** — when a pinned credential becomes permanently invalid (`expired`/`revoked`), the conversation auto-rebinds to a healthy credential.
- **Rate-limit awareness** — `429` from Anthropic flips the credential to `limited` and excludes it from new-conversation selection until `Retry-After` elapses (heals automatically).
- **Downstream auth** — single shared bearer token enforced on `/v1/*` and `/admin/*`, configurable via `.env`.
- **Pretty logging** with per-credential color so multiple parallel conversations are visually distinguishable, plus `text` and `json` formats.
- **Admin API** for inspection (credentials, conversations, distribution stats).
- **Single static binary** (~22 MB), pure Go, no CGO, SQLite for state. Distroless Docker image with `make`-driven workflow.

## Quick start (Docker, recommended)

Requires `docker` (with the `compose` plugin) and `make`. **No Go toolchain
needed** — the published image at
[`ghcr.io/p4u/claude-proxy`](https://github.com/p4u/claude-proxy/pkgs/container/claude-proxy)
is built and pushed by CI on every commit to `main`.

```bash
git clone https://github.com/p4u/claude-proxy.git
cd claude-proxy
make env                         # generates .env with current UID/GID + auth token
make pull                        # pulls the latest GHCR image
make up                          # starts the proxy
make health                      # → {"ok":true}
```

Pin a specific version in `.env` for production:

```dotenv
CLAUDE_PROXY_IMAGE=ghcr.io/p4u/claude-proxy:v0.2.0
```

Or build from source if you've changed the code:

```bash
make build                       # builds locally; sets the same image tag
make up
```

Now import a credential. For each Claude subscription you want in the pool:

### Generating credentials (do this once per account)

> [!IMPORTANT]
> Each subscription account needs its **own isolated login** in a dedicated
> directory. Never share a `credentials.json` between the proxy and a local
> `claude` installation. Anthropic issues a new refresh token on every renewal
> and immediately invalidates the old one — two consumers of the same token
> chain will fight, and whichever loses ends up with a permanently revoked
> credential.

```bash
# 1. Log in using a dedicated config directory — one per account.
#    This keeps the proxy's token chain completely separate from your
#    local claude installation.
CLAUDE_CONFIG_DIR=~/cp-creds/acct-A claude /login
#    Follow the browser prompt to authenticate with the Claude account
#    you want to add to the pool.

# 2. Copy the resulting credentials into ./creds (the proxy's bind mount).
#    Use cp, not mv — keep the original as a fallback until import succeeds.
cp ~/cp-creds/acct-A/.credentials.json ./creds/acct-A.json
chmod 600 ./creds/acct-A.json

# 3. Import into the pool. The proxy verifies the credential is alive and
#    performs an immediate token refresh before storing it.
make import FROM=acct-A.json LABEL=acct-A

# 4. Once import succeeds, delete the original file. The proxy now owns
#    the token chain; any other user of the same file will cause revocation.
rm -f ~/cp-creds/acct-A/.credentials.json
```

> [!WARNING]
> After a successful import the proxy **owns** that refresh token chain.
> Delete the source `credentials.json` and never use `CLAUDE_CONFIG_DIR=~/cp-creds/acct-A`
> again for that account — logging in again there or running any `claude`
> command with it will rotate the refresh token and silently invalidate the
> proxy's copy, causing `401 → refresh failed: invalid_grant → revoked`.
>
> **Backup instead of re-importing:** Before wiping the database, always run
> `make export-credentials > backup.jsonl` to capture the current (rotated)
> tokens. Re-importing the original `credentials.json` after the proxy has
> been running will not work — the original refresh token is long since
> superseded.

Repeat for each account. Then point Claude Code at the proxy:

```bash
ANTHROPIC_BASE_URL=http://<proxy-host>:8787 \
ANTHROPIC_AUTH_TOKEN=$(make token) \
claude
```

Open a second `claude` in another terminal — it will be assigned a different
credential. Watch what's happening live:

```bash
make logs            # pretty, color-coded per credential
make conversations   # JSON dump of bindings
make stats
```

## Quick start (from source)

Requires Go 1.22+ (the repo currently builds with the toolchain in `go.mod`).

```bash
git clone https://github.com/p4u/claude-proxy.git
cd claude-proxy
go build -o ./bin/claude-proxy ./cmd/claude-proxy
./bin/claude-proxy creds import --from ~/cp-creds/acct-A/.credentials.json --label acct-A
./bin/claude-proxy serve --addr 127.0.0.1:8787 --db ./proxy.db --log-level debug
```

## Configuration (`.env`)

`.env.example` is the source of truth. Key fields:

| variable | default | meaning |
|---|---|---|
| `HOST_BIND` | `127.0.0.1` | host interface to bind. Use `0.0.0.0` to expose to a LAN/tailnet — only with `PROXY_AUTH_TOKEN` set. |
| `HOST_PORT` | `8787` | host port mapped to the container's `:8787`. |
| `PROXY_UID` / `PROXY_GID` | host's `id -u`/`id -g` | UID/GID the container runs as. Must own `./data` and `./creds`. `make env` syncs these to your shell. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `LOG_FORMAT` | `auto` | `auto` (pretty on tty, json otherwise) / `pretty` / `text` / `json`. |
| `LOG_COLOR` | `auto` | `auto` (on whenever format is `pretty`) / `always` / `never`. |
| `PROXY_AUTH_TOKEN` | (generated by `make env`) | shared bearer token clients must send. Empty = no downstream auth (loopback only). |
| `UI_PASSWORD` | (generated by `make env`) | password for the web UI served at the root URL. Empty = UI disabled. |
| `UI_SECURE_COOKIES` | (empty) | force `Secure` UI session cookies. Usually leave empty — auto-detected behind Traefik. |
| `TLS_DOMAIN` | (empty) | FQDN that resolves to this host. Setting it activates Traefik + Let's Encrypt on `:80`/`:443`. |
| `TLS_EMAIL` | (empty) | contact email for Let's Encrypt expiry notices (required when `TLS_DOMAIN` is set). |
| `TLS_CASERVER` | LE production | switch to LE staging URL while debugging to avoid rate limits. |
| `TRAEFIK_LOG_LEVEL` | `INFO` | `DEBUG` while diagnosing cert issuance. |
| `CLAUDE_PROXY_IMAGE` | `ghcr.io/p4u/claude-proxy:latest` | which container image to use. Pin a tag for production; switch to `claude-proxy:dev` if you `make build` from source. |

## Downstream auth

When `PROXY_AUTH_TOKEN` is set, all requests except `/health` must include
`Authorization: Bearer <token>`. Claude Code uses whatever is in
`ANTHROPIC_AUTH_TOKEN`, so:

```bash
ANTHROPIC_AUTH_TOKEN=$(make token) ANTHROPIC_BASE_URL=http://<proxy>:8787 claude
```

Comparison is constant-time (`crypto/subtle.ConstantTimeCompare`); `Bearer ` prefix matching is case-insensitive.

To rotate:

```bash
make rotate-token        # generates a new token in .env and recreates the container
make token               # prints the new value
```

> [!IMPORTANT]
> Always set `PROXY_AUTH_TOKEN` when `HOST_BIND=0.0.0.0` or the proxy is
> reachable beyond loopback. Without it, anyone who can reach the listener
> can spend your subscription quota.

## HTTPS via Traefik + Let's Encrypt (optional)

`docker-compose.yml` ships a Traefik service that terminates HTTPS for the
proxy. It activates automatically whenever `TLS_DOMAIN` is set in `.env`
(via the compose `tls` profile, which the Makefile turns on for you).

Requirements:

- A DNS A/AAAA record for `TLS_DOMAIN` pointing at this host's public IP.
- TCP `:80` and `:443` reachable from the public internet (Let's Encrypt's
  TLS-ALPN-01 challenge needs to hit `:443`).
- A real address in `TLS_EMAIL` — Let's Encrypt sends expiry notices there.

Setup:

```bash
make env          # ensures .env has TLS_DOMAIN / TLS_EMAIL placeholders
$EDITOR .env      # set TLS_DOMAIN=proxy.example.com and TLS_EMAIL=ops@…
make up           # auto-detects TLS, starts traefik on :80/:443

make tls-info     # shows status: domain, profiles, ACME storage
make logs-traefik # tail traefik logs while the cert is issued
```

While debugging DNS / firewall, switch to the Let's Encrypt **staging**
endpoint to avoid hitting production rate limits:

```bash
TLS_CASERVER=https://acme-staging-v02.api.letsencrypt.org/directory
```

Once `curl -v https://$TLS_DOMAIN/health` succeeds, switch back to the
production CA URL (default in `.env.example`) and either delete
`./data/letsencrypt/acme.json` or rename it; Traefik will then re-issue a
trusted production certificate.

When TLS is enabled, all `make` inspection commands (`make health`,
`make credentials`, …) automatically hit `https://$TLS_DOMAIN` instead of
the loopback HTTP listener. You almost certainly want to keep
`HOST_BIND=127.0.0.1` so the plain-HTTP port is not exposed alongside HTTPS.

> [!IMPORTANT]
> Always combine TLS with `PROXY_AUTH_TOKEN`. HTTPS protects the *transport*
> but anyone who learns your domain name can still reach the listener — the
> bearer token is what stops them from spending your subscription quota.

## Sticky binding & conversation detection

The proxy derives a stable "conversation key" per request, in priority order:

1. **`X-Router-Conversation-ID` header** — explicit override (for tests and bespoke launchers; Claude Code does not send this).
2. **`$.metadata.user_id`** from the JSON body — what Claude Code currently emits per session.
3. **`sha256(system_prompt + first_user_message)[:8]`** — stable across turns of one session.
4. **Fallback**: `sha256(remote_addr + body[:4096])[:8]`.

The first time a key is seen, it's bound to a credential via weighted RR. All
subsequent requests with the same key reuse that credential. Bindings persist
in SQLite across restarts.

| pinned credential state | next request behavior |
|---|---|
| `active` | normal forward (sticky) |
| `limited` (rate-limited) | sticky: pass `429` through to client; the credential heals when `Retry-After` elapses |
| `expired` / `revoked` / `disabled` | **auto-rebind** to a healthy active credential and continue |
| no active credential exists | `503` with `X-Router-Reason: credential-orphaned` |

## Usage-aware weighted selection

Each credential carries an integer `weight`. Defaults derive from the
subscription tier:

| tier | default weight |
|---|---|
| `max`, `team`, `enterprise` | 5 |
| `pro` | 1 |
| (unknown) | 1 |

When a *new* conversation needs a credential, the pool scores every healthy
candidate and picks one by weighted-random draw:

```
score = weight × room_5h × room_7d^1.5        room_X = max(0, 1 − utilization/100)
```

The 5h and 7d usage windows (polled from Anthropic every 10 min) are treated as
independent ceilings, so their remaining room is multiplied — a credential
near-saturated on *either* window scores close to zero. The `^1.5` on the 7-day
term protects the slow-resetting weekly quota harder than the 5-hour window. A
credential whose latest snapshot shows **either window at ≥100%** is excluded
from selection entirely. Selection stays weighted-random (not greedy) so load
spreads smoothly across credentials and self-corrects each poll cycle.

Override the weight at import time or at runtime:

```bash
./bin/claude-proxy creds import --from ... --weight 3
make weight ID=cred_xxx W=8
```

## Interactive management (TUI)

Everything below can be driven one command at a time, but the easiest way to
manage the pool is the full-screen TUI. It runs **entirely inside the Docker
image** — the host only needs `docker`, `make`, and `bash`; no Go toolchain.

```bash
make tui
```

`make tui` is smart about where it runs:

- If the proxy container is **already running** (`make up`), it attaches with
  `docker compose exec` — the TUI runs as a second process inside the live
  container, against the same `/data/proxy.db`.
- If the proxy is **stopped**, it starts a throwaway one-off container
  (`docker compose run --rm`) that mounts the same `./data` volume, so you can
  manage credentials without the proxy being up.

It needs an interactive terminal (a TTY); it won't start from a pipe or CI job.

**Keys**

| Context | Keys |
|---|---|
| Global | `tab` switch Credentials/Users · `↑`/`↓` move · `q` quit |
| Credentials | `r` refresh token · `u` update token from a file · `w` weight · `d` disable/enable · `x` delete · `i` import from file · `p` **paste** a `.credentials.json` |
| Users | `c` create · `R` rotate token · `d` disable/enable · `x` delete |

The `p` (paste) action opens a multi-line box — paste the raw contents of a
`.credentials.json` and press `ctrl+s` to import. This avoids having to copy the
file into the `./creds` bind mount first.

Prefer to run it without Docker (e.g. a dev box with Go)? `go run
./cmd/claude-proxy tui --db ./data/proxy.db` works too.

## Web UI

For operators who'd rather click than type, the same binary also serves a
browser dashboard, embedded via `go:embed` — no separate service, no Node
toolchain on the host.

Enable it by setting `UI_PASSWORD` in `.env` (`make env` generates one for
you if it's missing) and restarting (`make up` / `make restart`). The UI is
then reachable at:

```
http://<HOST_BIND>:<HOST_PORT>/      # plain HTTP
https://<TLS_DOMAIN>/                # when Traefik/TLS is enabled — same port, no extra config
```

`make ui` prints the URL and whether `UI_PASSWORD` is currently set. Leaving
`UI_PASSWORD` empty disables the UI entirely (nothing is
mounted).

**Features:**

- **Dashboard** — request/token/error/latency stats over a selectable window
  (1h–30d), including per-user token usage.
- **Subscriptions** — live 5h/7d utilization per credential with reset-time
  countdowns, plus historical utilization charts (the same data driving the
  usage-aware selection algorithm above).
- **Credentials** — the same actions as the TUI (import, enable/disable,
  refresh, re-weight, update tokens, delete), from a browser.
- **Users** — create/rotate/disable/delete per-user bearer tokens, with
  per-user stats.

**Session model:** the UI has a single password (no per-user UI logins) —
logging in sets an `HttpOnly` session cookie good for 24h. The cookie is
`Secure` automatically when Traefik/TLS is in front of the proxy (detected via
`X-Forwarded-Proto`), or force it with `UI_SECURE_COOKIES=1`. Failed login
attempts are rate-limited per IP. See [`docs/WEBUI.md`](./docs/WEBUI.md) for
the full auth model and REST API contract.

Note: to power the per-request token figures shown in the dashboard, the
proxy now parses `model`/`usage` out of every forwarded response (SSE and
non-streaming JSON alike) and stores input/output/cache token counts in
`request_log` — this happens on the tee'd response stream and never affects
what the client actually receives.

## Operator workflow with `make`

```
Setup
  make help                 Show this help
  make env                  Create/upgrade .env (UID/GID/auth token)
  make token                Print the configured PROXY_AUTH_TOKEN
  make rotate-token         Generate a new token and recreate the container
  make fix-perms            chown ./data and ./creds to the host UID:GID
  make build                Build the docker image locally (source-tree dev)
  make pull                 Pull the latest published image from GHCR

Service lifecycle
  make up                   Start the proxy (and traefik, if TLS_DOMAIN is set)
  make down                 Stop and remove the container
  make restart              Restart the proxy
  make logs                 Tail logs (Ctrl-C to stop)
  make logs-traefik         Tail traefik logs (TLS only)
  make tls-info             Show TLS / Traefik status + ACME storage info
  make ps                   Container status

Interactive management (recommended)
  make tui                  Full-screen TUI to manage credentials + users.
                              Attaches to the running proxy container if one is
                              up (docker compose exec), otherwise starts a
                              one-off container sharing the same /data volume.
                              No Go on the host — runs entirely in the image.

Credentials
  make import FROM=foo.json LABEL=acct-A [WEIGHT=N]
                            Import a credential from ./creds/foo.json
  make update ID=cred_xxx FROM=new.json
                            Replace a credential's tokens from a fresh login
  make list                 List credentials with status, weight, counters
  make usage                Fetch 5h/7d usage % for all credentials from Anthropic
  make usage ID=cred_xxx    Fetch usage % for a single credential
  make disable ID=cred_xxx  Mark a credential disabled (excluded from selection)
  make rm ID=cred_xxx       Remove a credential row
  make refresh ID=cred_xxx  Force-refresh a credential's tokens
  make weight ID=... W=...  Set the selection weight

User tokens (per-user bearer auth)
  make user-create NAME=alice   Create a user token (prints the bearer token)
  make user-list                List all user tokens
  make user-stats [ID=...] [PERIOD=24h]   Per-user request stats
  make user-token ID=utok_xxx   Print a user's bearer token
  make user-disable / user-enable / user-rm / user-refresh ID=utok_xxx

Credential backup / restore
  make export-credentials   Dump all credentials (with current tokens) to stdout
                              e.g. make export-credentials > backup.jsonl
  make import-credentials   Import credentials from JSONL on stdin
                              e.g. cat backup.jsonl | make import-credentials

Inspection
  make health               GET /health
  make credentials          GET /admin/credentials (running service)
  make conversations        GET /admin/conversations
  make stats                GET /admin/stats
  make ui                   Print the web UI URL and whether UI_PASSWORD is set

Maintenance
  make test                 go test ./... (host, not docker)
  make clean                down + delete proxy.db
  make distclean            clean + remove image and .env
```

## CLI reference

```
claude-proxy serve [--addr :8787] [--db ./proxy.db]
                   [--auth-token TOKEN]
                   [--on-limited passthrough]
                   [--log-level debug|info|warn|error]
                   [--log-format auto|pretty|text|json]
                   [--log-color  auto|always|never]

claude-proxy tui                 [--db PATH]   # interactive management UI

claude-proxy creds import        --from FILE [--label NAME] [--weight N]
claude-proxy creds update        <id> --from FILE   # replace tokens from a fresh login
claude-proxy creds export        [--db PATH]   # JSONL to stdout
claude-proxy creds import-bulk   [--db PATH]   # JSONL from stdin
claude-proxy creds list
claude-proxy creds usage         [<id>]
claude-proxy creds usage-history [--period 1h|6h|24h|7d|30d] [<id>]
claude-proxy creds disable       <id>
claude-proxy creds rm            <id>
claude-proxy creds refresh       <id>
claude-proxy creds set-weight    <id> <weight>

claude-proxy users create        --name NAME
claude-proxy users list
claude-proxy users stats         [<id>] [--period 1h|6h|24h|7d|30d]
claude-proxy users token         <id>
claude-proxy users disable|enable|rm|refresh  <id>
```

`--auth-token` falls back to `CLAUDE_PROXY_AUTH_TOKEN` env var. All `creds`
subcommands accept `--db` (defaults to `./proxy.db`).

### Backup and restore

`creds export` outputs JSONL with current tokens (not the original import file —
tokens rotate on every refresh, so only the DB copy is valid after initial import):

```bash
# Backup
make export-credentials > backup.jsonl

# Restore after a wipe
cat backup.jsonl | make import-credentials

# Migrate to a new host
make export-credentials | ssh newhost 'cd claude-proxy && cat | make import-credentials'
```

## Admin API

Loopback by default. Honors `PROXY_AUTH_TOKEN` (the same bearer token clients
use). `/health` is always reachable so the docker healthcheck keeps working.

```
GET    /health                        → {"ok":true}
GET    /admin/credentials             list with status, weight, counters
GET    /admin/conversations           last 200 conversations
GET    /admin/stats                   totals + RR distribution
POST   /admin/credentials/:id/disable
DELETE /admin/credentials/:id
```

Sample `/admin/credentials` row:

```json
{
  "id": "cred_4b016e0489fc0ef34972e9f9",
  "label": "acct-A",
  "subscription_type": "max",
  "status": "active",
  "expires_at": "2026-05-07T05:01:55Z",
  "last_success_at": "2026-05-06T23:06:01Z",
  "last_request_at": "2026-05-06T23:05:57Z",
  "request_count": 6,
  "success_count": 6,
  "error_count": 0,
  "weight": 5,
  "active_conversations": 2
}
```

## Logging

Default `--log-format=auto` picks `pretty` on a tty and `json` when stderr is
a pipe. Pretty output renders one line per event:

```
22:48:00.449 INF [e1d300…eaaf acct-A] bind conv=u_e2e-alpha src=metadata.user_id new=true sub=max weight=1
22:48:00.699 DBG [e1d300…eaaf acct-A] upstream resp status=401 req_id=req_011…
22:48:00.920 INF [e1d300…eaaf acct-A] forwarded conv=u_e2e-alpha status=401 latency_ms=471
```

The bracketed `[<credShort> <label>]` prefix is **colorized per credential**
(stable hash of the credential ID into a fixed palette), so log lines from
the same credential are visually grouped. Levels are color-coded: dim DBG,
cyan INF, yellow WRN, red ERR.

For docker, `LOG_FORMAT=pretty` keeps colors visible through `make logs`.
Switch to `LOG_FORMAT=json` for log shippers (Loki, etc).

## Storage schema

SQLite (WAL mode, pure-Go via `modernc.org/sqlite` — no CGO):

```sql
credentials(
  id, label, subscription_type, access_token, refresh_token,
  expires_at, status, retry_after, last_success_at, last_429_at,
  last_request_at, request_count, success_count, error_count,
  weight, created_at
)

conversations(
  id, credential_id, created_at, last_seen_at, request_count, status
)

rr_cursor(k, idx)
```

Tokens are stored in plaintext (file mode 0600). KMS / vault integration is
intentionally out of scope for this PoC.

## Forwarding semantics

The proxy modifies **only**:

- `Authorization` — replaced with `Bearer <bound_credential.access_token>`
- `X-Api-Key` — stripped (never mixed with OAuth)
- `X-Router-*` — stripped (router-internal headers)

Everything else is forwarded verbatim. SSE responses are passed byte-for-byte
with per-chunk `http.Flusher.Flush()` so streaming feels native. The request
body is buffered once (16 MiB cap) so the proxy can replay it on a 401-retry
with a refreshed token.

## Layout

```
cmd/claude-proxy/main.go    subcommand dispatch
internal/store/             sqlite open + schema + idempotent migrations
internal/creds/             credential model + refresh-token client
internal/ingest/            .credentials.json importer
internal/router/            conversation key derivation
internal/pool/              usage-aware weighted selection + sticky binding + janitor
internal/proxy/             forwarder, header rewrites, retries, downstream auth
internal/admin/             /admin/* JSON endpoints
internal/usertoken/         named per-user bearer tokens + request identity
internal/usage/             Anthropic usage API client, poller, history chart
internal/prettylog/         tty-friendly slog handler with per-credential color
```

## Continuous integration & releases

GitHub Actions runs on every push and pull request:

| job | runs |
|---|---|
| `lint` | `gofmt -l`, `go vet`, `golangci-lint run` |
| `test` | `go build ./...`, `go test -race -covermode=atomic ./...` |
| `image` | multi-arch (`linux/amd64` + `linux/arm64`) `docker buildx`, push to `ghcr.io/p4u/claude-proxy` — only on push to `main` and on `v*.*.*` tags |

Image tags published:

| event | tags pushed |
|---|---|
| commit on `main` | `:latest`, `:main`, `:sha-<short>` |
| tag `v1.2.3` | `:v1.2.3`, `:1.2.3`, `:1.2`, `:1`, `:latest`, `:sha-<short>` |

Pull a specific commit's build from GHCR by short sha:

```bash
docker pull ghcr.io/p4u/claude-proxy:sha-abc1234
```

Workflow file: [`.github/workflows/ci.yml`](./.github/workflows/ci.yml).
Lint config: [`.golangci.yml`](./.golangci.yml).

## Testing

```bash
make test                 # or: go test ./...
```

Covers conversation key derivation, weighted-RR distribution (asserts exact
500/100 split with weights 5/1 over 600 binds), interleaved expansion shape,
sticky binding, limited-skip on new conversations, sticky-on-limited for
existing ones, auto-rebind on permanent failure, end-to-end forwarding
through a fake upstream, 429 → limited, 401 → refresh → retry → 200 with
token rotation, and downstream auth behavior on `/v1/*`, `/admin/*`, `/health`.

## Operational gotchas

- **Don't reuse a `credentials.json` after import** — see [Generating credentials](#generating-credentials-do-this-once-per-account) for the full explanation. Short version: the proxy owns the token chain after import; any other consumer of the same file causes immediate revocation.
- **Back up before wiping the DB.** The original `credentials.json` will not work after the proxy has been running — the refresh token has rotated. Always run `make export-credentials > backup.jsonl` before `make clean`.
- **`HOST_BIND=127.0.0.1`** binds loopback only. If Claude Code runs on a
  different host, set `HOST_BIND=0.0.0.0` *and* set `PROXY_AUTH_TOKEN`.
- **Permission errors on `/data/proxy.db`** mean the container user (uid 65532) doesn't own `./data`. Run `make fix-perms` to fix it.
- **Refresh-token rotation is one-shot.** Anthropic invalidates old refresh
  tokens immediately on use. Two consumers of the same token chain will
  fight; whichever loses ends up `expired`.
- **Multiple Claude Code sessions on the same host with the same login**
  may emit the same `metadata.user_id` and therefore collapse onto the
  same credential. Use distinct logins / `CLAUDE_CONFIG_DIR`s, or a
  launcher that injects `X-Router-Conversation-ID` if you need
  guaranteed split routing.

## Out of scope

- KMS-backed token storage
- Multi-tenant isolation (the proxy assumes a single trusting operator)
- Anthropic API-key (`x-api-key`) credentials in the pool — only OAuth subscription tokens are pooled; `x-api-key` on incoming requests is accepted as the downstream auth token only
- Bedrock / Vertex / Foundry pass-through
- Metrics export (Prometheus / OTel)

## Disclaimer

This software is published **for research and educational purposes only**.

- It exists to document and study how Claude Code's subscription authentication
  works, as a learning resource for security researchers, students, and
  developers interested in OAuth proxy patterns and Anthropic's CLI internals.
- It is **not** a hosted service, **not** a commercial product, and **not**
  endorsed by, affiliated with, or sponsored by Anthropic.
- The author makes **no representation** that running this code is permitted
  by your Claude subscription's terms of service or by any law in your
  jurisdiction. **It is entirely your responsibility** to read Anthropic's
  [Acceptable Use Policy](https://www.anthropic.com/legal/aup) and
  [Consumer Terms](https://www.anthropic.com/legal/consumer-terms) (or the
  Commercial / Enterprise terms that apply to you) and to determine whether
  your intended use is allowed.
- You must only use credentials that you personally own. You must not use
  this software to access any account that you are not authorized to access,
  to share access to a paid subscription with people not entitled to it, to
  bypass usage limits in violation of any agreement, or to facilitate any
  unauthorized resale of model capacity.
- The software is provided **"as is", without warranty of any kind**, as
  stated in the [LICENSE](./LICENSE). The author and any contributors
  expressly **disclaim all liability** for any direct, indirect, incidental,
  consequential, or other damages arising from the use, misuse, or inability
  to use this software, including (without limitation) account suspension,
  data loss, financial loss, or legal consequences.
- By cloning, building, running, or otherwise using this code, **you accept
  full and sole responsibility** for the consequences. If you do not accept
  these terms, do not use this software.

## Credits & references

- [Claude Code documentation](https://docs.claude.com/en/docs/agents-and-tools/claude-code/overview) for `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `claude /login`, `claude setup-token`.
- [`@mariozechner/pi-mono`](https://github.com/badlogic/pi-mono) — reference TypeScript implementation of the Anthropic OAuth flow, used to confirm the OAuth client_id, scopes, and refresh-token endpoint shape.

## License

[GNU Affero General Public License v3.0](./LICENSE).
