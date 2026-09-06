# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Is

`claude-proxy` is a sticky multi-subscription credential proxy for Claude Code. It sits between Claude Code clients and its upstream providers, managing OAuth subscriptions, API keys, custom-host model declarations, and a sidecar-backed Codex gateway with **usage-aware weighted-random selection** and stable per-conversation credential pinning. The main service is a single static Go binary backed by SQLite; OpenAI Codex account OAuth is delegated to a private sidecar.

It serves `/v1/*` as a transparent pass-through for Anthropic-compatible upstreams, swapping in a managed credential's `Authorization` header. Custom OpenAI-compatible hosts are translated at the boundary while clients still speak the Anthropic Messages API. This is what Claude Code clients connect to.

Six provider modes are supported (`internal/provider`): **Anthropic** OAuth subscriptions, **Z.AI GLM** coding plans, **Xiaomi MiMo** token plans, **OpenAI Codex** subscriptions through a private CLIProxyAPI sidecar, and dynamic **custom Anthropic** or **custom OpenAI-compatible** hosts. The provider that serves a request is decided by the model the client asked for, so a user picks a GLM, GPT, or custom-host model in Claude Code's `/model` picker and it just works. See [Providers](#providers-multi-upstream).

## Build & Test Commands

```bash
# Run tests (host Go toolchain required)
make test           # go test -race ./...
go test -race -run TestName ./internal/pool/   # single test in one package
make lint           # gofmt + go vet + golangci-lint

# Install golangci-lint v2 (CI version)
make lint-install

# Verify/build all Go packages without Docker
go build ./...
# Run locally in the foreground (Ctrl-C to stop)
go run ./cmd/claude-proxy serve --addr 127.0.0.1:8787 --db ./data/proxy.db

# Build Docker image from source
make build

# Pull latest published image
make pull
```

The module and Docker build use Go 1.26.2 (`go.mod` / `Dockerfile`).

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
      → pool.AcquireScoped(): get/create the sticky binding + request lease;
        emergency rebind for unusable or saturated pins; when enabled, announce
        and later revalidate a proactive rebalance of long-lived Anthropic pins
      → forward to the provider's base URL with swapped Authorization header
      → GET /v1/models: union of every provider that has a usable credential,
        Anthropic entries augmented with "[1m]" variants, cached 5min
        (internal/proxy/models1m.go) for Claude Code gateway model discovery;
        it strips an incoming Anthropic-Version while querying providers and
        reinjects it only for Anthropic, because the Codex sidecar's translated
        catalogue becomes obfuscated when that header is forwarded
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

| | Anthropic | Z.AI GLM | Xiaomi MiMo |
|---|---|---|---|
| Base URL | `api.anthropic.com` | `api.z.ai/api/anthropic` | `token-plan-<region>.xiaomimimo.com/anthropic` |
| Model prefix | `claude-` | `glm-` | `mimo-` |
| Credential | OAuth (access + refresh) | static API key | static API key |
| Refreshable | yes | no — a 401 means a bad key | no |
| Usage API | `/api/oauth/usage` | none | none |
| `GET /v1/models` | yes | yes | **no** — nginx 404; static catalogue |
| `count_tokens` | yes | yes | **no** — nginx 404 |
| `[1m]` variants | yes | no — 400s on the suffixed form | no — 400s on the suffixed form |
| Advertised as | native ID | `claude-`-prefixed alias | `claude-`-prefixed alias |
| Endpoints | one | global / cn | sgp / ams / cn / payg |

The provider registry also contains these non-default modes:

| Mode | Base URL / model source | Boundary behavior | Picker alias |
|---|---|---|---|
| OpenAI Codex | private CLIProxyAPI sidecar; accounts live there | sidecar translates the Anthropic request to OpenAI | `gpt-*` → `claude-gpt-*` |
| Custom Anthropic | per-credential URL and model catalogue | Anthropic Messages pass-through | native ID → `claude-<id>` |
| Custom OpenAI | per-credential URL and model catalogue | `internal/proxy/openai.go` translates Chat Completions | native ID → `claude-openai-<id>` |

Codex uses the private sidecar described below; `custom` and `custom_openai` get
their base URL and model catalogue from each credential rather than from the
fixed registry defaults.

**Endpoints are per credential, not per provider** (`credentials.base_url`,
empty = the provider default). A MiMo Token Plan key is bound to one regional
cluster and answers every other with a bare `Invalid API Key` that says nothing
about the region, so the operator picks the endpoint when adding the key —
`creds add-key --endpoint sgp`, the TUI's endpoint prompt, or the web UI's
selector. `provider.ResolveEndpoint` accepts a short name or a full URL, so a
cluster added after this build is still reachable without a registry edit.
`ResolveBaseURL` then resolves the stored override at forward time.

**Presets are a convenience, not a constraint.** Every surface accepts a custom
`https://` URL as well as a preset name, and an existing credential can be moved
afterwards — `creds set-endpoint <id> <name|url>`, the TUI's `e`, or the web UI's
per-row *Endpoint* button (`POST /api/credentials/{id}/endpoint`). The UI fetches
the preset list from `GET /api/credentials/endpoints` rather than keeping its own
copy, so it cannot drift from the registry.

`ingest.UpdateKeyEndpoint` **re-verifies the key against the new endpoint before
committing**, and leaves the credential untouched if it fails — swapping one
broken endpoint for another would be worse than refusing. On success it also
heals a `revoked`/`expired`/`limited` status back to `active`, because the usual
reason to move a credential is that it was added against the wrong cluster and
was marked revoked by that cluster's rejections. Only the stored override is
written, and only when it differs from the provider default, so a
default-endpoint credential keeps following the registry.

**Key verification uses `POST /v1/messages`, not `GET /v1/models`.**
`api.z.ai/api/anthropic/v1/models` returns **200 for a garbage bearer token**,
so verifying against it admitted any string and defeated the whole point of the
check; MiMo has no `/v1/models` at all. A `max_tokens:1` completion is the
smallest request both providers accept and both reject with 401, and it is what
catches a right-key/wrong-cluster mistake at add time rather than in production.

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

**Neither needs a translation layer.** Both surfaces were verified live against
`/v1/messages`, SSE streaming, tool use, `cache_control` and Claude Code's
`anthropic-beta` headers (GLM additionally serves `/v1/models` and
`count_tokens`; MiMo serves neither, and returns `thinking` content blocks).
Only the base URL and the credential differ, so `usagecapture.go`,
`promptcapture.go`, the 429 handling and the SSE relay all work unmodified.

> MiMo's missing `count_tokens` is forwarded and 404s back to the client.
> Claude Code tolerates that (it falls back to estimating), so it is passed
> through honestly rather than synthesized.

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

**Their tokens are still counted.** `usagecapture.go` has no provider gating, so
every GLM and MiMo response is parsed for the four token counts and written to
`request_log` exactly like Anthropic traffic — the dashboard's per-credential
token stats already include them. What is missing is only a *percentage*, since
neither upstream publishes an allowance. `/api/usage/current` therefore carries a
`metered` block for these providers (rolling 5h/7d sums straight from
`request_log`, one grouped query per window), and the Subscriptions card renders
it in the same slot the utilization meters occupy — same label, same value
position, **no bar**, because there is no published cap to fill against and a
fabricated denominator would read as authoritative. Credentials that do have a
real usage API omit `metered` entirely rather than carry a worse second number.

> Note the metered figure only counts traffic through this proxy; a key also
> used elsewhere has consumed more than it shows.

> Possible future refinement, deliberately not implemented: Z.AI publishes a
> credit formula (`(in×Min + cached×Mcached + out×Mout)/10000`), per-model
> multipliers, and per-plan 5h/weekly caps, which would turn the metered totals
> into a genuine utilization % feeding `pool.Score`. MiMo publishes per-MTok
> prices but no quota formula, so it could only ever show estimated spend.

### Custom hosts (`provider.Custom`, `provider.CustomOpenAI`, `internal/proxy/custom.go`)

An endpoint may speak Anthropic Messages (`custom`) or OpenAI Chat Completions
(`custom_openai`). Unlike registry providers, a custom host has **no fixed base
URL and no fixed model list** — both live on the credential, because each host
*is* its own upstream.

**Routing is dynamic, not prefix-based.** `provider.ForModel` places `glm-4.7`
from a compile-time prefix; a custom host serves whatever its operator declared
(`Qwen3.6-fable`, `my-llama`, …), so `matchCustomModel` asks which credential
declares the requested name. That lookup runs against the credential list the
request path already loaded. Anthropic hosts advertise `claude-<model>`;
OpenAI hosts advertise `claude-openai-<model>` so their names cannot collide
with Codex or an Anthropic host. The declared native name goes upstream.

**The mapping is model → credential IDs, not just model → provider.** Multiple
hosts of one protocol share a provider while serving disjoint models, so
`pool.BindScoped` takes an explicit candidate allowlist and additionally scopes
the sticky key by model. A conversation switching custom models cannot reuse a
pin to a host that cannot serve the new one.

**Adding a host is a probe, not a form.** `ingest.ProbeCustomHost` interrogates
the endpoint and fills in everything it can discover, because a translation shim
commonly ignores the requested model and answers as whatever it fronts — a name
the operator would have to guess otherwise. It checks, in order:

| Probe | Why |
|---|---|
| `GET /v1/models` | The only source of **context windows**; taken wholesale when present |
| `POST /v1/messages` (`max_tokens:1`) | Liveness, key validity, and the model name the host reports |
| same, **without** the key | Whether auth is actually enforced — flags an accidentally open endpoint |
| `POST /v1/messages/count_tokens` | Presence only |

Context window is **omitted rather than guessed** when the host publishes no
`/v1/models`: nothing short of binary-searching the input size could find it,
and a wrong window misconfigures the client's context management. Discovered
values are editable — the probe is a starting point, not a verdict.

`ProbeCustomOpenAIHost` performs the parallel OpenAI checks at `GET /models`
and `POST /chat/completions`. `openai.go` translates normal and streaming text,
images, tools/results, stop reasons, errors, and usage. Chat Completions has no
token-count operation, so the Anthropic count endpoint returns a local context
preflight estimate, never a billing figure.

Surfaces: `creds add-custom --url … [--model ID]…` for Anthropic hosts, and the
web UI's two custom-host credential types (`POST /api/credentials/probe` then
`/custom`).

### OpenAI Codex subscriptions (`internal/codexgateway/`)

Codex is a separate provider backed by a private, pinned CLIProxyAPI Docker
sidecar. The sidecar owns the OpenAI OAuth files under `data/cliproxy-auth/`;
the main database stores only one synthetic `gateway_codex` credential with the
sidecar URL and internal API key. `codexgateway.ReconcileCredential` creates or
disables that row at startup, so the existing proxy authentication, sticky
bindings, request logs, usage parsing, and per-user limits remain in the main
request path without copying Codex tokens into `proxy.db` or the browser.

`gpt-*` models route to the Codex provider and are advertised as
`claude-gpt-*` so Claude Code's gateway model filter accepts them. The alias is
removed before the request reaches CLIProxyAPI. Codex weights compete only with
other Codex accounts inside the sidecar; the proxy sees the gateway as one
credential.

**Per-Codex-account balancing reuses the Anthropic pool math.** The sidecar
captures per-account quota from OpenAI's `X-Codex-*` response headers
(primary/secondary used-percent, reset-at, window-minutes). CLIProxyAPI
≥ 7.2.149 records them under `model_quotas.<model>.signals` — one block per
model that served a request — and leaves the top-level `quota.signals` empty,
so `latestQuota` takes the top-level block when it has signals and otherwise
the most recently observed per-model block (the account-level windows are the
same in every model's block). A background loop
(`codexgateway.RebalanceLoop`, every 90s) reads accounts, computes each
effective score with `pool.EffectiveScore(base_weight, fh%, sd%,
sd_resets_at, now)` — the same function `weightedRandPick` uses — normalizes
to an integer in `[1, 1000]`, and pushes it via `SetWeight`. The sidecar's
routing strategy must therefore be `weighted-round-robin`
(`config/cliproxy.yaml.tmpl`); with `round-robin` the computed weights are
ignored. Windows are picked by their published length (300 min → 5h, 10080
min → 7d), not by primary/secondary name, because the mapping is
plan-dependent (`prolite` puts the weekly window under `primary`).

The operator's base weight lives in a new table `codex_account_weight(name PK,
weight INT)` — not in the sidecar. The web UI's *Weight* input for Codex
accounts writes only to that table (`SetBaseWeight`); the rebalance loop is
the sole writer to the sidecar's weight field, so the previous tick's score
can never become the next tick's input. `GET /api/codex/accounts` is
decorated with `base_weight` (from DB) and `effective_weight` (what the loop
will push next), so the browser view matches the sidecar's next state. `internal/codexgateway` exposes only the typed, sanitized management
operations used by the web UI (start/poll/cancel OAuth, callback submission,
list/disable/weight/delete accounts).

The web UI starts a fresh sidecar-owned OAuth flow; it must not upload
`~/.codex/auth.json`. The OAuth callback listener is bound to host loopback on
`127.0.0.1:1455` because OpenAI's registered redirect is fixed to localhost.
For a remote browser, forward that port over SSH or use the UI's manual
loopback-callback handoff. `CLIPROXY_MANAGEMENT_KEY` is server-side only, and
port 8317 is kept off the host network.

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
| `internal/codexgateway/` | Private CLIProxyAPI client, sanitized Codex OAuth/account management, and synthetic gateway-credential reconciliation |
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

`pickActiveLocked` → `weightedRandPick`: each candidate gets `score = weight × room_5h × room_7d^1.5 × (1 + urgency)`, where `room_X = max(0, 1 − utilization/100)`. The 5h and 7d windows are **independent ceilings** (a request 429s on whichever it hits first), so their remaining room is **multiplied**, not averaged — saturation on either window drives the score toward zero. The `^1.5` on the 7d term protects the slow-resetting weekly quota harder than the cheap 5h window (`sevenDayExp` constant). `seven_day_sonnet_pct` is intentionally ignored. The most recent `usage_history` snapshot is used regardless of age — stale data beats assuming 0% usage; `headroom=1.0` when no snapshot exists (newly imported creds).

**Urgency — use it or lose it** (`pool.Urgency`): `urgency = max(0, room_7d / remaining_fraction_7d − 1)`, i.e. how much faster than even pace the weekly allowance would have to be burned to be used up before it resets. On or ahead of pace → 0; 0% used with 2.5 days left → ~1.8 (×2.8); 0% used with 1h45m left → ~95 (×96). It is deliberately hyperbolic in time-to-reset: quota in a window about to reset is *free* to spend, while a window with days left will still be there tomorrow, so an about-to-reset unspent account should take nearly all new bindings. The remaining fraction is floored at one hour of the week so the term stays finite. **Only the 7-day window counts** — spending inside an about-to-reset 5-hour window still charges the weekly allowance, so it is not free; the 5h window is a ceiling (`room_5h`), not an expiring budget. Zero when the reset time is unknown or already past (the snapshot describes the previous window). `pool.EffectiveScore` bundles `Score × (1 + Urgency)` and is the **single function** used by `weightedRandPick`, the web UI's `/api/usage/current` selection view (which therefore shows the real routing share, not a baseline), and the Codex rebalance loop. An earlier version used a bounded "behind linear pace" boost capped at 3×; it gave a fresh account with days left nearly the same boost as one resetting within the hour, which is exactly the case that matters.

**Hard saturation cutoff:** a credential whose latest snapshot reports either window at **≥100%** is excluded from the active candidate set *before* scoring (`NOT EXISTS` clause in the active query) — a maxed-out subscription is never selected, regardless of weight. If every active credential is saturated, the pool falls through to `limited` credentials (to obtain a real 429 + Retry-After), and returns `ErrNoCredentials` only if there are none.

**Sticky rebinding:** the ≥100% cutoff also applies to *existing* bindings. On each `Bind()`, if the pinned credential's latest snapshot is saturated, the conversation is migrated onto a fresh non-saturated credential (the `conversations` row's `credential_id` is updated and the caller sees `isNew=true`). When no healthy alternative exists the saturated pin is kept, so the request still reaches Anthropic for a real 429. The scoring math (`Score`, `Room`, `Saturated`, `SevenDayExp`) is exported from `internal/pool` and reused verbatim by the web UI's `/api/usage/current` selection view, so the two can never drift.

**Proactive long-session rebalancing** (`internal/pool/rebalance.go`): long-lived
pins may move to a substantially better-provisioned Anthropic subscription after
an API header notice — on by default (`REBALANCE_SESSIONS`, `--rebalance-sessions`);
the Codex sidecar and emergency failover paths are untouched (exhaustion of a
pinned credential is still handled by the pre-existing emergency rebind). Dwell/
cooldown both key off the durable `bound_at` timestamp: pins must be at least
an hour old, with at most one proactive switch per conversation per hour; this
survives restart. A separate per-process gate allows at most one elective move
per destination per 5 min and resets on restart. These are request-time checks,
not a fixed-time scheduler. A destination qualifies when its token expiry is
> 5 min in the future, its 5h/7d usage is ≤ 60%, its effective score is
≥ 4× the source's **and** (unweighted room score ≥ 2× **or** weekly urgency
factor ≥ 4×), required for the two latest usage samples (≥ 5 min apart,
latest ≤ 15 min old, older ≤ 30 min old, resets consistent/future when
known). The first successful response on the old account announces the
move (`X-Router-Rebalance: pending` + a generic `X-Router-Message`); a later
eligible request revalidates and switches after this conversation binding's
prior in-flight responses drain, and the header reads `switched`. `count_tokens` and
background Haiku calls join the lease but never drive a switch. The pending
announcement expires after 15 min and restarts discard it (re-announced);
if the destination stops qualifying the pending switch is cancelled. The
cancellable wait on in-flight responses can add latency, in-flight streams are
never interrupted, and the new account's prompt cache may need a cold rebuild. Requests
with account-scoped files/containers/server tools are excluded (body guard).
In-flight tracking is local to one pool/process — a single serving instance
is recommended (not cross-process safe). The headers are not necessarily
displayed by Claude Code, and no account IDs/names/usage are exposed.

> Selection stays **weighted-random** rather than greedy-best on purpose: bindings are sticky and usage is only polled every 10 min, so greedy would dump every new conversation onto one cred between polls (thundering herd) and overshoot. Weighted-random spreads load and self-corrects each poll cycle.

### Storage Schema (SQLite)

Eight tables (`internal/store/schema.go`): `credentials` (incl. `provider`:
`anthropic` | `glm` | `mimo` | `codex` | `custom` | `custom_openai`, added via
the swallow-duplicate `ALTER TABLE` mechanism — its `DEFAULT 'anthropic'` is
what migrates existing rows, no backfill needed),
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

The Docker/Compose workflow reads runtime configuration from `.env` (copy from
`.env.example`; `make env` also generates missing auth and Codex keys). A direct
`go run`/binary invocation reads flags and `CLAUDE_PROXY_*` environment variables
instead; it does not load `.env` automatically. Key Compose variables:

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
| `REBALANCE_SESSIONS` | `1` | Proactive long-session rebalance for direct Anthropic subscriptions, announced via an API header notice (`CLAUDE_PROXY_REBALANCE_SESSIONS`; `0` disables it). Emergency failover is unchanged. |
| `UI_SECURE_COOKIES` | _(empty)_ | Force `Secure` UI session cookies (`CLAUDE_PROXY_UI_SECURE_COOKIES`); auto-detected behind Traefik via `X-Forwarded-Proto` |
| `PROMPT_RETENTION_DAYS` | `7` | Retain prompt/full-capture rows; `0` disables capture (`CLAUDE_PROXY_PROMPT_RETENTION_DAYS`) |
| `CLIPROXY_API_KEY` | _(generated)_ | Private proxy-to-CLIProxyAPI API key; never sent to OpenAI |
| `CLIPROXY_MANAGEMENT_KEY` | _(generated)_ | Server-side key for Codex OAuth/account management; never expose to browsers |
| `CLIPROXY_IMAGE` | pinned digest in `.env.example` | CLIProxyAPI sidecar image; upgrade deliberately and rerun Codex translation/tool/streaming tests |
| `TLS_DOMAIN` | _(empty)_ | Set (with `TLS_EMAIL`) to enable Traefik + Let's Encrypt |
| `TLS_EMAIL` | _(empty)_ | Required contact address when `TLS_DOMAIN` is set |
| `TLS_CASERVER` | Let's Encrypt production | Use the staging directory while diagnosing certificate issuance |
| `TRAEFIK_LOG_LEVEL` | `INFO` | Traefik log level |
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

TUI keys also include `k` — add a static API key (GLM or MiMo).

### Adding a GLM (Z.AI) or MiMo API key

```bash
# Reads the key from stdin, which keeps it out of shell history and out of
# `ps` output (where a --key flag would be visible to any local user).
echo "$ZAI_KEY" | claude-proxy creds add-key --provider glm --label zai-main --plan pro
make add-key PROVIDER=glm LABEL=zai-main            # prompts for the key
make add-key PROVIDER=mimo LABEL=mimo-main ENDPOINT=sgp  # regional Token Plan key
```

The key is verified with a live `POST /v1/messages` using `max_tokens:1` before
it is stored. `GET /v1/models` is not an authentication check: some providers
return 200 for a garbage bearer token, and MiMo does not expose that endpoint at
all. A credential that cannot authenticate must never enter the pool, because the
selector would then hand live conversations to it. Duplicates are rejected by
access token (`creds.HasAccessToken`) — `HasRefreshToken` cannot work here, since
every API-key row stores an empty refresh token and would collide with every
other.

Key credentials store `expires_at` ~100 years out: the column is `NOT NULL` and
the refresher compares against it, so a date the refresh window can never reach
is simpler than threading a nullable expiry through every caller. The refresher
also skips non-refreshable providers outright. CLI, TUI and web UI all render it
as "never" rather than a date in 2126.

```bash
make import FROM=path/to/.credentials.json LABEL=myaccount [WEIGHT=5]
make update ID=cred_xxx FROM=path/to/new.credentials.json  # replace tokens from a fresh login
make list                        # Show all credentials with status/counters
make usage                       # Fetch live Anthropic 5h/7d usage for credentials
                                # (non-Anthropic providers have no usage API)
make usage ID=cred_xxx           # Fetch Anthropic usage for one credential
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
