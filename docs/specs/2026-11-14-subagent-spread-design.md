# Subagents on their own subscriptions: design

Status: draft   Class: architectural (schema column, admin API, CLI)
Owner decisions so far:
- 2026-11-14: implement it in the proxy, with an option to disable it per user in the web UI,
  enabled by default.
- 2026-11-14: Anthropic subscriptions only, and only for requests for Anthropic models.

## Problem

When Claude Code launches subagents (the Agent tool, workflows), every subagent request carries
the parent session's `metadata.user_id`. `router.Derive` (internal/router/router.go:31-41) keys
all of them as `u_<hash>`, the same key as the main conversation. So `pool.AcquireScoped`
(internal/pool/rebalance.go:143) finds the main conversation's binding, and the subagents and
the main thread all run on one subscription. Five parallel subagents burn one account's
5-hour window while the others sit idle.

Claude Code 2.1.323 sends `x-claude-code-agent-id` on every request a subagent makes. The
binary sets it from the request's agent context whenever that context is not the main thread
(`...ie?.agentId&&{"x-claude-code-agent-id":…}` in `z1`, with no env opt-in), along with
`x-claude-code-parent-agent-id` for nested subagents. The proxy reads neither header today.

## Goals

1. A subagent request for an Anthropic model gets its own sticky binding, separate from the
   main conversation's.
2. A subagent's first binding prefers an Anthropic subscription that the same session is not
   already using, whether for the main conversation or for another live subagent. With more
   subagents than eligible subscriptions, the binding still succeeds and shares one.
3. Each user token has a switch to turn this off. The switch is on by default, for new and
   existing users. It can be changed from the web UI's Users table, the admin API and the CLI.
4. Everything else routes exactly as today. That covers main-thread requests, requests without
   the header, non-Anthropic and custom-host models, users with the switch off, and
   account-scoped requests.

## Non-goals

- No spreading for GLM, MiMo, Codex, Gemini, Privatemode or custom providers, even when the
  request carries the header.
- No change to scoring, weights, saturation cutoffs, rebalancing or compaction handling. A
  subagent binding is an ordinary conversation for all of them.
- The anti-affinity applies only when a binding is first created. A later saturation re-pin or
  rebalance does not consider siblings.
- No global (proxy-wide) switch, and no behaviour keyed on `x-claude-code-agent-type` or
  `x-claude-code-request-class`. Claude Code sends those only with the client-side
  `CLAUDE_CODE_GATEWAY_HINT_HEADERS` opt-in.
- No change to `X-Router-Conversation-ID` semantics: an explicit key is the client's choice.

## Current state

- The request path is in internal/proxy/proxy.go:
  - `providerFor` (routing.go:37) picks the provider from the model.
  - A custom-host match may then replace it (:139-146).
  - `router.Derive` runs at :176. Then `pool.KeyScoped(dr.ConvID, prov, custom.Model)` at :187
    gives the qualified key. Anthropic keys are bare, other providers are prefixed
    (pool.go:120-130).
  - `RequestOptions` is built at :188-196.
  - `h.pool.AcquireScoped(ctx, dr.ConvID, prov, custom.Model, custom.CredIDs, opts)` is called
    at :202.
  - `h.Sessions.RecordRequest(...)` at :219-222 records the session's binding for
    `/v1/claudio/session` and `/v1/claudio/session/switch`, unless the request is observe-only.
- Account affinity is `accountBound := directSubscription && !portableRebalanceRequest(body)`
  (proxy.go:~192; rebalance.go:80). A body that references `file_id`, `container`,
  `server_tool_use` or `code_execution` is account-bound, and `bindOnce` latches the flag on the
  binding (pool.go:178-187).
- New bindings are created in `bindOnce` (pool.go:137-165): `pickActiveLocked(ctx, tx, prov,
  allowed)` (:307), then an `INSERT INTO conversations`, in one transaction under `p.mu`.
  - `activeCandidates(ctx, q, prov, allowed, exclude string, now)` (:357) is filtered by
    provider. Its `exclude` removes a single id and is used only by `RequestSwitch` and
    `userSwitchOnce` (switch.go:42, :148).
  - `pickActiveLocked` passes `""`.
- `conversations.id` is the TEXT primary key. Rows are never pruned except when a credential is
  deleted (creds.go:458).
- `sessionbind.Registry` (internal/sessionbind/sessionbind.go) keeps one binding per
  `X-Claude-Code-Session-Id`, and the latest request wins. Subagent requests carry the parent's
  session id.
- Per-user flags follow the `block_suggestions` pattern:
  - the `user_tokens` column (store/schema.go:53-69) plus an `ALTER` entry (store.go:~76);
  - `usertoken.columns`/`scan`, the `UserToken` and `Identity` fields, and
    `SetBlockSuggestions` (usertoken.go);
  - a copy into `Identity` in `AuthMiddleware` (proxy/auth.go:76-84), which runs a DB lookup on
    every request;
  - the CLI `users suggestions <id> on|off` (cmd/claude-proxy/main.go:1023, :1322) and the SUGG
    column in `users list`;
  - the admin route `POST /api/users/{id}/suggestions` (webui/users.go:94, :191);
  - the web UI `suggestionsToggle` with `switchToggle` (static/js/pages/users.js:762, :826),
    `api.js:150` and the `mock.js` route;
  - docs: WEBUI.md "v5 additions" and the CLAUDE.md user-token section.

## Design

### When a request is spread

A request is a **spread subagent request** when all of these hold:

1. it is sticky (`needsSticky`);
2. `prov == provider.Anthropic` after the custom-host match, and `custom.Model == ""`. The
   candidate set is therefore Anthropic subscriptions only, since `activeCandidates` filters by
   provider;
3. `dr.Source == router.SourceMetadataUID`;
4. `X-Claude-Code-Agent-Id` is present and valid, meaning 1-64 characters of `[0-9A-Za-z_-]`.
   Anything else is ignored as if absent;
5. the request is portable (`portableRebalanceRequest(body)` is true). An account-scoped
   subagent request stays on the session's key, where its resources live;
6. the caller's spreading flag is on. `Identity.SpreadSubagents` is true for a user token.
   Admin and anonymous (no-auth deployments) callers count as on.

Otherwise the request routes exactly as today.

### Keys

`router.SubagentKey(parent, agentID string) string` returns `parent + "/a_" + shorten(agentID)`.
The parent is the `u_…` key Derive returned. `/` does not occur in `u_` keys, because `shorten`
yields the raw value or hex. A parent key containing `/` is not spread. The child key is the
raw conversation key passed to `AcquireScoped`. Because the provider is Anthropic, the
qualified key is the same string.

A nested subagent (with a parent-agent id) is keyed by its own agent id under the session's
`u_` key. Every subagent of a session is a sibling, whatever its depth.

### Anti-affinity on the first binding

`pool.RequestOptions` gains `Family string`. It holds the parent's raw key, and is empty for
every other request. In `bindOnce`, only when the key has no row and `Family != ""`, the picker
excludes the **family's credentials**:

- the credential of the conversation whose key is `Family` (the main thread), if it is bound;
- the credentials of conversations whose key starts with `Family + "/"` and whose
  `last_seen_at` is within the last 15 minutes (live siblings).

The lookup is a primary-key range scan (`id >= Family+"/" AND id < Family+"0"`, since `0`
follows `/`), never `LIKE` and never a table scan.

`activeCandidates` takes `exclude []string` instead of a single string. The two switch callers
pass their one id. If excluding the family leaves no active candidate, the pick runs again
without the exclusion. That is the ordinary pick, including the `limited` fallback.

Concurrent first requests from parallel subagents are serialized by `p.mu`, and each insert
commits before the next pick. So siblings launched together see each other.

### Session record

A spread subagent request does not call `h.Sessions.RecordRequest`. `/v1/claudio/session` and
`/v1/claudio/session/switch` keep describing and moving the main conversation's binding, which
claudio's usage-limit auto-switch relies on.

### The per-user switch

| Surface | Change |
|---|---|
| Schema | `user_tokens.spread_subagents INTEGER NOT NULL DEFAULT 1` in `CREATE TABLE` and the `ALTER` list. Existing rows get 1. |
| usertoken | `UserToken.SpreadSubagents`, `Identity.SpreadSubagents`, `columns`/`scan`, and `SetSpreadSubagents(ctx, db, id string, on bool) error`, which returns `ErrNotFound` on 0 rows. |
| Auth | `AuthMiddleware` copies the field into `Identity`. |
| Admin API | `POST /api/users/{id}/subagents` with body `{"spread": bool}` replies `{"ok":true,"spread_subagents":bool}`, or 404 for an unknown id. The users list JSON gains `spread_subagents`. |
| Web UI | A "Subagents" column in the Users table with a switch built from `switchToggle` and a help text. `api.setUserSubagents` and a stateful `mock.js` route. |
| CLI | `claude-proxy users subagents <id> on\|off` (with `-db`). `users list` gains a column showing `spread`/`share`. |
| Docs | A docs/WEBUI.md section, plus CLAUDE.md (user tokens, conversation key derivation, request flow) and the README line beside prompt suggestions. |

### Logging

The `bind` log line for a spread request adds `"agent", <child suffix>` and `"family", <parent
key>`, both hashed or shortened forms only. Raw header values are never logged.

### Alternatives considered

| Option | Why not |
|---|---|
| Spread by `x-claude-code-request-class: subagent` | Only sent with the client-side gateway opt-in, and it carries no per-subagent identity. |
| In-memory family map in the pool | It is lost on restart and duplicates what `conversations` already holds. The key prefix gives the family for free. |
| Hard exclusion (fail when no other subscription) | It would turn "five subagents, three subscriptions" into errors. The goal is load spreading, not isolation. |
| Global switch in the server flags | The owner asked for a per-user switch in the UI. |

## Compatibility and rollout

- **Schema.** An additive column with a default, applied by the existing ALTER loop. An older
  binary ignores it. Rollback is the old binary, after which subagent requests share the parent
  binding again and leftover `…/a_…` rows are inert.
- **Clients.** Claude Code before the agent-id header behaves as today. Clients sending the
  header for a non-Anthropic model behave as today.
- **API.** The new endpoint and JSON field are additive. `mock.js` gains the route.
- **Prompt cache.** A subagent on another subscription builds its cache there. The parent's
  cache is untouched. This is an accepted cost of spreading load.
- **Stats.** `request_log.conv_id` records the child key, so a subagent shows as its own
  conversation. Per-user totals are unchanged.

## Failure modes and security

- **Untrusted header.** The agent id is validated, then hashed into the key. A client sending
  random ids creates bindings at the rate it could already create them with random
  `metadata.user_id` values. No new trust is granted: the switch only narrows routing within
  the caller's own provider pool.
- **Header absent or renamed** by a future Claude Code: requests route as today (fail closed to
  the current behaviour).
- **All subscriptions in use or saturated:** the fallback to the ordinary pick applies. No new
  error path.
- **Restart:** families are rebuilt from `conversations`.
- **Concurrency:** covered by `p.mu` and the bind transaction. The range lookup runs inside the
  same transaction as the insert.
- **Account-scoped resources:** non-portable subagent requests keep the session key (condition
  5). The proxy cannot know which account created a resource a subagent references, so it
  keeps the session's account.

## Acceptance criteria

- [ ] AC1: `router` unit tests cover `SubagentKey` and the agent-id validation: valid,
  over-long, bad characters and empty.
- [ ] AC2: proxy test with three Anthropic subscriptions, one main request and two subagent
  requests with distinct agent ids. The three bind to three distinct credentials, and a second
  request per subagent sticks to its first credential.
- [ ] AC3: proxy test with two subscriptions and three subagents. Every request succeeds, and
  the subagents prefer the credential the main thread is not using while it has room.
- [ ] AC4: proxy test: a user with `spread_subagents` off, and a non-portable subagent request,
  each bind to the main conversation's credential.
- [ ] AC5: proxy test: a GLM-model request with the header, and a custom-host request with the
  header, bind exactly as without it (the same key as the parent).
- [ ] AC6: proxy test: after main and subagent requests with the same `X-Claude-Code-Session-Id`,
  the session registry holds the main conversation's credential.
- [ ] AC7: pool test: a family credential last seen over 15 minutes ago is not excluded, and the
  exclusion falls back to the ordinary pick when it empties the set.
- [ ] AC8: webui test: the flag defaults to true for a new user, `POST …/subagents` toggles and
  persists it, the list reflects it, and an unknown id gives 404.
- [ ] AC9: store test: a DB created by the previous schema (without the column) opens with
  every existing user at `spread_subagents = 1`.
- [ ] AC10: `make test` and `make lint` pass, and `go build ./...` is clean.
- [ ] AC11 (owner, after deploy): a Claude Code session that launches two parallel subagents
  shows three `bind` lines with distinct credentials and the new `family` field in the proxy
  log.

## Constraints

- Go 1.26.2. Follow the repo's CLAUDE.md conventions. Every new `/api` endpoint goes in
  `mock.js`.
- No change to selection scoring, rebalance thresholds, compaction or Codex/Gemini balancing.
- Raw header values, user ids and tokens are never logged.
- Commits use conventional commits, with no AI attribution and no `Co-Authored-By` trailers.
  Implementers commit with explicit paths and never push.
- Do not touch the owner's uncommitted `CLAUDE.md` edit. Doc edits to CLAUDE.md wait for the
  controller (see the plan).

## Open questions

None.
