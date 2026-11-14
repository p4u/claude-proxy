# Subagents on their own subscriptions: design

Status: approved (2026-11-14)   Class: architectural (schema column, admin API, CLI)
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
(`...ie?.agentId&&{"x-claude-code-agent-id":…}`, where `ie` is unset for `agentType==="main"`;
no env opt-in), along with
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
- No change to the scoring formula, weights, saturation cutoffs, rebalancing or compaction
  handling. The only changes to selection are the candidate set of a subagent's first binding
  and the limited re-pin of a child binding (both below). A subagent binding is otherwise an
  ordinary conversation.
- The anti-affinity applies only when a binding is first created. A later re-pin or rebalance
  does not consider siblings.
- Forks (subagents that start with the parent's history) and subagents already running when
  this ships are not spread: their binding is created on the session's subscription, so they
  keep its prompt cache.
- No global (proxy-wide) switch, and no behaviour keyed on `x-claude-code-agent-type` or
  `x-claude-code-request-class`. Claude Code sends those only with the client-side
  `CLAUDE_CODE_GATEWAY_HINT_HEADERS` opt-in.
- No change to `X-Router-Conversation-ID` semantics: an explicit key is the client's choice.

## Current state

- The request path is in internal/proxy/proxy.go:
  - `providerFor` (routing.go:37) picks the provider from the model.
  - A custom-host match may then replace it (:141-147).
  - `router.Derive` runs at :176. Then `pool.KeyScoped(dr.ConvID, prov, custom.Model)` at :188
    gives the qualified key. Anthropic keys are bare, other providers are prefixed
    (pool.go:123-135).
  - `RequestOptions` is built at :193-197.
  - `h.pool.AcquireScoped(ctx, dr.ConvID, prov, custom.Model, custom.CredIDs, opts)` is called
    at :202.
  - `h.Sessions.RecordRequest(...)` at :221-222 records the session's binding for
    `/v1/claudio/session` and `/v1/claudio/session/switch`, unless the request is observe-only.
- Account affinity is `accountBound := directSubscription && !portableRebalanceRequest(body)`
  (proxy.go:~192; rebalance.go:80). A body that references `file_id`, `container`,
  `server_tool_use` or `code_execution` is account-bound, and `bindOnce` latches the flag on the
  binding (pool.go:178-187).
- New bindings are created in `bindOnce` (pool.go:137-272, new-row branch :153-164, the
  account-bound latch :177-187): `pickActiveLocked(ctx, tx, prov, allowed)` (:307), then an `INSERT INTO conversations`, in one transaction under `p.mu`.
  - `activeCandidates(ctx, q, prov, allowed, exclude string, now)` (:357) is filtered by
    provider. Its `exclude` removes a single id and is used only by `RequestSwitch` and
    `userSwitchOnce` (switch.go:87, :165).
  - `pickActiveLocked` passes `""`.
- `conversations.id` is the TEXT primary key. Rows are never pruned except when a credential is
  deleted (creds.go:458).
- `sessionbind.Registry` (internal/sessionbind/sessionbind.go) keeps one binding per
  `X-Claude-Code-Session-Id`, and the latest request wins. Subagent requests carry the parent's
  session id.
- Per-user flags follow the `block_suggestions` pattern:
  - the `user_tokens` column (store/schema.go:69) plus an `ALTER` entry (store.go:81);
  - `usertoken.columns`/`scan`, the `UserToken` and `Identity` fields, and
    `SetBlockSuggestions` (usertoken.go);
  - a copy into `Identity` in `AuthMiddleware` (proxy/auth.go:76-84), which runs a DB lookup on
    every request;
  - the CLI `users suggestions <id> on|off` (cmd/claude-proxy/main.go:1023-1024, :1322) and the
    SUGG column in `users list` (:1134);
  - the admin route `POST /api/users/{id}/suggestions` (webui/users.go:94-96, :191-204);
  - the web UI `suggestionsToggle` with `switchToggle` (static/js/pages/users.js:762, :826),
    `api.js:152` and the `mock.js` route (:1071);
  - docs: WEBUI.md "v5 additions" and the CLAUDE.md user-token section.

## Design

### When a request is spread

A request is a **spread subagent request** when all of these hold:

1. it is sticky (`needsSticky`);
2. `prov == provider.Anthropic` after the custom-host match, and `custom.Model == ""`. The
   candidate set is therefore Anthropic subscriptions only, since `activeCandidates` filters by
   provider;
3. `dr.Source == router.SourceMetadataUID`;
4. `X-Claude-Code-Agent-Id` is present and valid, meaning 1-128 characters of `[0-9A-Za-z_-]`.
   Anything else is ignored as if absent;
5. the key rule below selects the child key;
6. spreading is on for the caller: `usertoken.SpreadSubagentsFor(id *Identity) bool`, the only
   reader of the flag, returns true for a nil identity (no-auth deployment) and for `IsAdmin`,
   and `id.SpreadSubagents` for a user token.

Conditions 1-4 and 6 make a request a **subagent candidate**. Anything else routes exactly as
today.

### Keys

`router.SubagentKey(parent, agentID string) string` returns `parent + "/a_" + shorten(agentID)`.
The parent is the `u_…` key Derive returned. `shorten` returns the raw value up to 32 bytes and
16 hex characters otherwise, so a `u_` key from a short `metadata.user_id` can contain `/`; such
a parent is not spread. Claude Code's own `user_id` is longer and always hashed. Because the
provider is Anthropic, the qualified key of a child is the same string.

A nested subagent (with a parent-agent id) is keyed by its own agent id under the session's
`u_` key. Every subagent of a session is a sibling, whatever its depth.

### Which key a subagent request uses

Every request of a subagent candidate uses its child key (ruling R9, after the plan audit).
The first request of the subagent decides how the child binding is created:

- a request that may start a child (`StartChild`) gets the anti-affinity pick;
- any other request binds the child to the session's current credential: that of the `Family`
  row, when the row exists and its credential is `active` or `limited`. Otherwise it gets the
  ordinary pick.

Once the child row exists, the subagent stays on it like any conversation. `AccountBound` is
latched on the child binding exactly as on any other, so a subagent that becomes
account-scoped stays on the account it started on.

`StartChild` holds only when the request is portable (`portableRebalanceRequest(body)`) and no
element of its `messages` array has `role` `"assistant"`, i.e. it is the first turn of a fresh
subagent. A fork, and a subagent that was already running, carry assistant turns, so their
child binding is created on the session's subscription. Their later requests use the same
binding. That includes side requests sent with the subagent's context (WebFetch's haiku call
passes `agentContext`) and the first request after a compaction, so neither moves them.

`pool.RequestOptions` gains `Family` (the parent's raw key), `Child` (the child's raw key) and
`StartChild bool`; all are empty for every request that is not a subagent candidate. The proxy
always passes the parent key as `convID`; `AcquireScoped` swaps it for `Child` when the options
are valid (`Family == convID`, `Child` under `Family`, an unscoped Anthropic binding) and reports
the raw key it used in `Lease.ConvID`. The proxy derives the logged and
recorded `convID` from `Lease.ConvID` (`pool.KeyScoped`) and uses it for `request_log`,
`prompt_log` and `conversation_message`. A spread request is one whose `Lease.ConvID` is the
child key.

### Anti-affinity on the first binding

In `bindOnce`, only when a child row is created, the picker excludes the **family's
credentials**:

- the credential of the conversation whose key is `Family` (the main thread), if it is bound;
- the credentials of conversations whose key starts with `Family + "/"` and whose
  `last_seen_at` is within the last 15 minutes (live siblings).

The lookup is a primary-key range scan (`id >= Family+"/" AND id < Family+"0"`, since `0`
follows `/`), never `LIKE` and never a table scan.

`activeCandidates` takes `exclude []string` instead of a single string. The two switch callers
pass their one id. The family exclusion is kept only if at least one remaining candidate has
headroom ≥ 0.25 (a constant), where headroom is `Room(5h) × Room(7d)^SevenDayExp` from its
latest snapshot (1 with no snapshot); remaining candidates below 0.25 are dropped too.
Otherwise the pick runs again without the exclusion: the ordinary pick, including the `limited`
fallback. Among the kept candidates `weightedRandPick` decides as today, so weights still apply.

`bindLocked` and `bindOnce` take the family as a parameter; `BindScoped` passes none. The
liveness cut-off compares `last_seen_at` with `time.Now()`, like the writes. The main thread's
credential is excluded whatever its `last_seen_at`. Range bounds are bound as Go strings (a
`[]byte` bind compares as BLOB and matches nothing).

### Limited child bindings move

A child binding has no user switch, so it does not wait on a limited subscription. In
`bindOnce`, when a child row exists, the binding is not account-bound and its credential is
`limited` with `retry_after` in the future, the pin is re-picked with the ordinary picker (no
family exclusion) and moved if the pick is a different, active credential; with no active
candidate it stays, so the client still gets the real 429. This is the existing saturation
re-pin with one more trigger, for child keys only.

Concurrent first requests from parallel subagents are serialized by `p.mu`, and each insert
commits before the next pick. So siblings launched together see each other.

### Session record

A spread subagent request does not call `h.Sessions.RecordRequest`. `/v1/claudio/session` and
`/v1/claudio/session/switch` keep describing and moving the main conversation's binding, which
claudio's usage-limit auto-switch relies on. A usage limit hit by a subagent is handled by the
limited re-pin above, not by `/v1/claudio/session/switch`.

### The per-user switch

| Surface | Change |
|---|---|
| Schema | `user_tokens.spread_subagents INTEGER NOT NULL DEFAULT 1` in `CREATE TABLE` and the `ALTER` list. Existing rows get 1. |
| usertoken | `UserToken.SpreadSubagents`, `Identity.SpreadSubagents`, `columns`/`scan`, `SetSpreadSubagents(ctx, db, id string, on bool) error` (returns `ErrNotFound` on 0 rows), and `SpreadSubagentsFor(id *Identity) bool` (condition 6). |
| Auth | `AuthMiddleware` copies the field into `Identity`. |
| Admin API | `POST /api/users/{id}/subagents` with body `{"spread": bool}` replies `{"ok":true,"spread_subagents":bool}`, or 404 for an unknown id. The users list JSON gains `spread_subagents`. |
| Web UI | A "Subagents" column in the Users table with a switch built from `switchToggle` and a help text. `api.setUserSubagents` and a stateful `mock.js` route; `mock.js` seed users and the list mapper carry `spread_subagents`. |
| CLI | `claude-proxy users subagents <id> on\|off` (with `-db`). `users list` gains a column showing `spread`/`share`. |
| Docs | A docs/WEBUI.md section, plus CLAUDE.md (user tokens, conversation key derivation, request flow) and the README line beside prompt suggestions. |

### Logging

The `bind` log line for a spread request adds `"agent", <the child key's suffix after "/a_">`
and `"family", <parent key>`. These are the forms already stored in `conversations.id`; the
validated agent id (at most 32 characters raw, else hashed) is not a secret. No other header
value is logged, and never the raw `metadata.user_id`.

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
- **Prompt cache.** Only a fresh subagent's first turn can place it on another subscription,
  so the cost is that subagent's own system prompt and tools, written once there. Forks and
  subagents already running at deploy time are bound to the session's subscription and keep
  its cache. If the main thread later moves (a switch or rebalance), a fork stays where it
  was bound.
- **Rollback and credential deletion.** On rollback, child rows are inert and subagents share
  the parent binding again. Deleting a credential removes its child rows; a running subagent
  then has assistant turns and is re-bound on the session's credential.
- **Stats.** `request_log.conv_id`, `prompt_log` and `conversation_message` record the child
  key, so a subagent shows as its own conversation in the user's conversation list and in
  captured transcripts. Token and request totals per user are unchanged; conversation counts
  (per user, per credential, "active conversations") rise by one per subagent. The expiry
  shadow forecast sees each child binding as an arrival. `conversations` grows by one row per
  subagent and is still pruned only with its credential; this is accepted.

## Failure modes and security

- **Untrusted header.** The agent id is validated, then shortened into the key. An
  `X-Router-Conversation-ID` shaped like a child key only reaches its caller's own family,
  whose parent key is unguessable. A client sending
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
- **Account-scoped resources:** a subagent whose first request is account-scoped is bound to
  the session's credential (the resources can only be the parent's). Once the child
  binding exists the subagent never leaves it; a later account-scoped request latches
  `account_bound` on the child row.

## Acceptance criteria

- [ ] AC1: `router` unit tests cover `SubagentKey` and the agent-id validation: valid,
  129 characters, bad characters (including `/`) and empty.
- [ ] AC2: proxy test with three Anthropic subscriptions, one main request and two subagent
  requests with distinct agent ids. The three bind to three distinct credentials, and a second
  request per subagent sticks to its first credential.
- [ ] AC3: proxy test with two subscriptions and three subagents. Every request succeeds, and
  the first subagent binds the credential the main thread is not using when that credential's
  headroom is ≥ 0.25, and may bind either when it is below.
- [ ] AC4: proxy test: a user with `spread_subagents` off, and a first-turn subagent request
  that is not portable, each bind to the main conversation's credential.
- [ ] AC5: proxy test: a GLM-model request with the header, and a custom-host request with the
  header, bind exactly as without it (the same key as the parent).
- [ ] AC6: proxy test: after main and subagent requests with the same `X-Claude-Code-Session-Id`,
  the session registry holds the main conversation's credential.
- [ ] AC7: pool test: a sibling credential last seen over 15 minutes ago (set in the database
  directly) is not excluded, and the exclusion falls back to the ordinary pick when it empties
  the set.
- [ ] AC8: webui test: the flag defaults to true for a new user, `POST …/subagents` toggles and
  persists it, the list reflects it, and an unknown id gives 404.
- [ ] AC9: store test: a DB created by the previous schema (without the column) opens with
  every existing user at `spread_subagents = 1`.
- [ ] AC10: `make test` and `make lint` pass, and `go build ./...` is clean.
- [ ] AC12: proxy test: a subagent binds a child key with a portable request, then sends a
  request containing a `"container"` value; it is served by the same credential, the child row
  has `account_bound=1`, and the parent row's `account_bound` is unchanged.
- [ ] AC13: pool test: a child binding on a credential marked limited moves to an active
  credential on its next request; a main binding in the same state keeps its pin.
- [ ] AC14: proxy test: a fork's request (header, assistant turn in its history, no child row)
  creates its child row on the main thread's credential. The same agent's haiku request,
  `count_tokens` request and single-user-message request (as after a compaction) stay on that
  credential under the child key. A fresh haiku subagent (no assistant turn) binds a credential
  other than the main thread's.
- [ ] AC15: proxy test: a subagent request under the admin token, and one with no identity (no
  auth configured), each bind a child key.
- [ ] AC16: proxy test: with the agent header, a request keyed by `X-Router-Conversation-ID`,
  one with no `metadata.user_id`, and one whose agent id is invalid (129 chars, or a `/`) each
  bind the same key and credential as the same request without the header.
- [ ] AC17: pool test: a new `Pool` on the same database (restart) excludes the main thread's
  and a live sibling's credentials for a new child of that family.
- [ ] AC18: proxy test: the `bind` log line of a child binding has `agent` and `family`, and no
  log line contains the raw `metadata.user_id`.
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
