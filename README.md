# claude-proxy

A sticky multi-subscription HTTP proxy for [Claude Code](https://docs.claude.com/en/docs/agents-and-tools/claude-code/overview).
It pools multiple Claude subscription OAuth credentials, assigns each new
conversation to one of them via weighted round-robin, and pins that
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
                          ┌─────────────────────────────────┐
  Claude Code agent A ───►│                                 │
  Claude Code agent B ───►│         claude-proxy            │──► api.anthropic.com
  Claude Code agent C ───►│                                 │
                          │  • bearer auth (clients)        │
                          │  • sticky conv → cred binding   │
                          │  • weighted round-robin         │
                          │  • 401 → refresh + retry        │
                          │  • 429 → mark cred limited      │
                          │  • SSE pass-through             │
                          └─────────────────────────────────┘
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
- **Weighted round-robin** for new conversations, with smooth interleaved expansion. Default weights: `max`/`team`/`enterprise` = 5, `pro` = 1.
- **Automatic refresh** — proactive (every 60 s if `expires_at < now+5min`) and reactive (on `401` retry once with a fresh token).
- **Automatic reroute** — when a pinned credential becomes permanently invalid (`expired`/`revoked`), the conversation auto-rebinds to a healthy credential.
- **Rate-limit awareness** — `429` from Anthropic flips the credential to `limited` and excludes it from new-conversation selection until `Retry-After` elapses (heals automatically).
- **Downstream auth** — single shared bearer token enforced on `/v1/*` and `/admin/*`, configurable via `.env`.
- **Pretty logging** with per-credential color so multiple parallel conversations are visually distinguishable, plus `text` and `json` formats.
- **Admin API** for inspection (credentials, conversations, distribution stats).
- **Single static binary** (~22 MB), pure Go, no CGO, SQLite for state. Distroless Docker image with `make`-driven workflow.

## Quick start (Docker, recommended)

Requires `docker` (with the `compose` plugin) and `make`.

```bash
git clone https://github.com/p4u/claude-proxy.git
cd claude-proxy
make env                         # generates .env with current UID/GID + auth token
make build                       # builds the image
make up                          # starts the proxy
make health                      # → {"ok":true}
```

Now import a credential. For each Claude subscription you want in the pool:

```bash
# 1. Log in under a DEDICATED config dir (so a local `claude` won't reuse and
#    rotate the same refresh token, which would invalidate the proxy's copy):
CLAUDE_CONFIG_DIR=~/cp-creds/acct-A claude /login

# 2. Move the resulting credentials into ./creds (the proxy's mount):
mv ~/cp-creds/acct-A/.credentials.json ./creds/acct-A.json
chmod 600 ./creds/acct-A.json

# 3. Import into the pool:
make import FROM=acct-A.json LABEL=acct-A
```

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
| `TLS_DOMAIN` | (empty) | FQDN that resolves to this host. Setting it activates Traefik + Let's Encrypt on `:80`/`:443`. |
| `TLS_EMAIL` | (empty) | contact email for Let's Encrypt expiry notices (required when `TLS_DOMAIN` is set). |
| `TLS_CASERVER` | LE production | switch to LE staging URL while debugging to avoid rate limits. |
| `TRAEFIK_LOG_LEVEL` | `INFO` | `DEBUG` while diagnosing cert issuance. |

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

## Weighted round-robin

Each credential carries an integer `weight`. Defaults derive from the
subscription tier:

| tier | default weight |
|---|---|
| `max`, `team`, `enterprise` | 5 |
| `pro` | 1 |
| (unknown) | 1 |

The selector expands the active set into a slot list using **interleaved
expansion** (one slot per credential per round, draining weights one at a
time):

| weights | rotation |
|---|---|
| `[A:5, B:5]` | `A B A B A B A B A B` |
| `[A:5, B:1]` | `A B A A A A` |
| `[A:5, B:1, C:2]` | `A B C A C A A A` |

Override at import time or at runtime:

```bash
./bin/claude-proxy creds import --from ... --weight 3
make weight ID=cred_xxx W=8
```

## Operator workflow with `make`

```
Setup
  make help                 Show this help
  make env                  Create/upgrade .env (UID/GID/auth token)
  make token                Print the configured PROXY_AUTH_TOKEN
  make rotate-token         Generate a new token and recreate the container
  make fix-perms            chown ./data and ./creds to the host UID:GID
  make build                Build the docker image

Service lifecycle
  make up                   Start the proxy (and traefik, if TLS_DOMAIN is set)
  make down                 Stop and remove the container
  make restart              Restart the proxy
  make logs                 Tail logs (Ctrl-C to stop)
  make logs-traefik         Tail traefik logs (TLS only)
  make tls-info             Show TLS / Traefik status + ACME storage info
  make ps                   Container status

Credentials
  make import FROM=foo.json LABEL=acct-A [WEIGHT=N]
                            Import a credential from ./creds/foo.json
  make list                 List credentials with status, weight, counters
  make disable ID=cred_xxx  Mark a credential disabled (excluded from RR)
  make rm ID=cred_xxx       Remove a credential row
  make refresh ID=cred_xxx  Force-refresh a credential's tokens
  make weight ID=... W=...  Set the round-robin weight

Inspection
  make health               GET /health
  make credentials          GET /admin/credentials (running service)
  make conversations        GET /admin/conversations
  make stats                GET /admin/stats

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

claude-proxy creds import     --from FILE [--label NAME] [--weight N]
claude-proxy creds list
claude-proxy creds disable    <id>
claude-proxy creds rm         <id>
claude-proxy creds refresh    <id>
claude-proxy creds set-weight <id> <weight>
```

`--auth-token` falls back to `CLAUDE_PROXY_AUTH_TOKEN` env var. All `creds`
subcommands accept `--db` (defaults to `./proxy.db`).

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
internal/pool/              weighted RR + sticky binding + janitor
internal/proxy/             forwarder, header rewrites, retries, downstream auth
internal/admin/             /admin/* JSON endpoints
internal/prettylog/         tty-friendly slog handler with per-credential color
```

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

- **Don't reuse a `.credentials.json` after import.** Once imported, the
  proxy owns that refresh-token chain. If a local `claude` later uses the
  same file, it will rotate the refresh token and the proxy's copy
  silently goes stale (`401` → `refresh failed: invalid_grant` →
  credential `expired`). Always log in under a dedicated `CLAUDE_CONFIG_DIR`
  per credential.
- **`HOST_BIND=127.0.0.1`** binds loopback only. If Claude Code runs on a
  different host, set `HOST_BIND=0.0.0.0` *and* set `PROXY_AUTH_TOKEN`.
- **Permission errors on `/data/proxy.db`** mean `PROXY_UID`/`PROXY_GID` in
  `.env` don't match the host owner of `./data`. `make env` resyncs them;
  `make fix-perms` fixes the directory ownership.
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
- Web UI
- Anthropic API-key (`x-api-key`) credentials — only OAuth subscription tokens
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
