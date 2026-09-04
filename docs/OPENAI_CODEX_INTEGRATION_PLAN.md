# OpenAI Codex Subscription Integration Plan

_Research completed 4 September 2026. No runtime or authentication changes have been made._

## Recommendation

Use [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) as a private Docker sidecar, pinned initially to the tested `v7.2.149` release (preferably its image digest). It already exposes an Anthropic-compatible `/v1/messages` endpoint and performs the Codex request/response translation, streaming, tool-call handling, OAuth refresh, multi-account selection, and session affinity.

Do **not** make uploading `~/.codex/auth.json` the normal enrollment path. OpenAI describes cached credentials as sensitive, password-equivalent material, and CLIProxyAPI uses a different on-disk schema. Copying one refresh token into two independently refreshing clients can also make the original Codex login stale. Instead, the web UI should start a fresh OAuth authorization that is owned exclusively by the sidecar.

```text
Claude Code ──Anthropic API──> claude-proxy ──> Anthropic / GLM / MiMo
                                    │
                                    └──gpt-*──> CLIProxyAPI ──OAuth──> Codex

Browser UI ──authenticated API──> claude-proxy ──private management API──> CLIProxyAPI
```

This split preserves claude-proxy's downstream authentication, per-user limits, prompt capture, request logs, and model routing. CLIProxyAPI remains the sole owner of Codex tokens and account-level scheduling.

## Why This Architecture

The current data model pins every conversation to a local credential. Create one internal `codex-gateway` credential for the sidecar API key and let CLIProxyAPI manage the real OAuth accounts behind it. Existing non-Anthropic model aliasing can advertise `gpt-*` models as `claude-gpt-*` so they appear in Claude Code, then restore the wire model before forwarding.

A sidecar per account would preserve claude-proxy's account pool but makes dynamic containers, callback ports, and lifecycle management unnecessarily complex. Embedding CLIProxyAPI would tightly couple this project to a large, rapidly changing codebase. A single dedicated Codex-only sidecar also avoids upstream model/provider ambiguity when several protocols expose the same model name.

The tradeoff is that v1 statistics are aggregate for the Codex gateway. The UI can list sanitized account status from the sidecar, but exact request attribution per OpenAI account should be a later telemetry feature.

## Enrollment and UI Flow

Add an **OpenAI Codex subscription** option to Credentials:

1. The authenticated browser asks claude-proxy to begin login.
2. The backend calls CLIProxyAPI's typed `codex-auth-url` management endpoint and returns only the authorization URL and opaque state.
3. The UI opens the OpenAI login and polls claude-proxy for completion.
4. On success, the UI adds the sanitized account (email/label, status, plan metadata where available, never tokens) to the main Credentials table.
5. Operators can disable, reauthorize, or delete it from that unified list through narrow backend endpoints.

CLIProxyAPI registers `http://localhost:1455/auth/callback`; its Web UI forwarder then redirects to `http://127.0.0.1:8317/codex/callback`. Bind port 1455 to host loopback only and keep the main 8317 API private. The documented manual fallback accepts either exact loopback callback shape and submits `code`, `state`, and `error` to the backend. File import should be omitted from v1. If later added as an advanced migration tool, it must transform the native schema, validate ownership/permissions, redact every response, and warn that the source client must stop refreshing that login.

## Implementation Phases

### 1. Sidecar and secrets

- Add a pinned `cli-proxy-api` Compose service on an internal network. Do not publish port 8317.
- Persist only its auth directory under ignored `data/`; mount configuration read-only and the auth directory read-write.
- Generate separate high-entropy API and management keys in `.env`; never expose the management key to JavaScript or logs.
- Enable `routing.session-affinity`, add health checks, bounded retries/timeouts, and redact upstream errors.
- Bind OAuth callback port 1455 to `127.0.0.1` only. Validate non-root execution, dropped capabilities, and a read-only root filesystem before enabling those hardening settings.

### 2. Backend integration

- Add a small CLIProxy client with fixed base URLs and typed methods: health, models, begin/poll OAuth, list accounts, disable, and delete.
- Register provider ID `codex` (display name **OpenAI Codex**) with `gpt-` model prefixes and the existing `claude-` advertising alias.
- Reconcile the single internal gateway credential at startup without storing any OAuth material in the main database. Mark it unavailable when the sidecar has no usable account.
- Preserve proxy user authentication, usage parsing, quotas, prompt capture, and provider-scoped conversation IDs.

### 3. Web UI

- Add the OAuth modal, progress/error states, popup-blocker recovery, callback instructions, and a sanitized Codex account list.
- Require the existing UI session and CSRF protections for every operation; rate-limit OAuth starts and validate state.
- Clearly label the subscription as personal-account access rather than an OpenAI API key.

### 4. Verification and documentation

- Unit-test model detection/rewriting, gateway reconciliation, management response redaction, OAuth state handling, and failure mapping.
- Add mock-sidecar integration tests for streaming/non-streaming messages, tools, usage, 401 refresh, 429s, cancellation, and sidecar outages.
- Document backup/removal of the sidecar auth volume, key rotation, upgrades, remote callback forwarding, and rollback.

## Acceptance Test Matrix

A live release is not ready until it passes:

- model discovery and selection through Claude Code;
- streaming and non-streaming text;
- sequential and parallel tool calls plus tool-result follow-ups;
- multi-turn affinity, subagents, and failover after an account becomes limited;
- thinking/reasoning block preservation and cancellation;
- automatic OAuth refresh, restart persistence, disable/delete, and reauthorization;
- correct token usage in request logs and enforcement of downstream user limits;
- graceful behavior if `/v1/messages/count_tokens` is unsupported;
- no token, management key, authorization code, or auth-file content in browser responses or logs.

These cases deliberately cover previously reported upstream regressions around [tool/thinking translation](https://github.com/router-for-me/CLIProxyAPI/issues/3799), [tool-name casing](https://github.com/router-for-me/CLIProxyAPI/issues/3592), [provider selection](https://github.com/router-for-me/CLIProxyAPI/issues/4794), and [subagent affinity](https://github.com/router-for-me/CLIProxyAPI/issues/5364). Reports against older versions are test targets, not evidence that the pinned version still fails.

## Security and Policy Gate

The management API must never be public, and neither the native Codex auth file nor its token values should pass through the browser. [OpenAI's authentication guidance](https://learn.chatgpt.com/docs/auth) treats cached credentials as secrets. OpenAI also states that an account is intended for its creator and other people should use their own accounts ([account-sharing policy](https://help.openai.com/en/articles/10471989-openai-account-sharing-policy), [EU terms](https://openai.com/policies/eu-terms-of-use/)). Therefore, a personal Codex subscription should initially be owner-only, or every individual proxy user should connect their own account. Pooling one personal subscription for unrelated users is not an officially supported API deployment and may violate applicable terms. An OpenAI API key is the safer organizational alternative.

CLIProxyAPI is MIT-licensed, but its current container runs as root by default and the project changes frequently. Pin, harden, regression-test, and review each upgrade. See its [Codex provider guide](https://help.router-for.me/configuration/provider/codex), [management API](https://help.router-for.me/management/api), and [Docker guidance](https://help.router-for.me/docker/docker-compose).

## Current Validation and Required User Action

Read-only checks confirmed that this host is logged into Codex with ChatGPT, the local auth file has mode `0600`, Docker and Compose are available, and ports 8317/1455 were unused. No credential values were inspected. The repository's full `make test` suite and focused CLIProxyAPI Claude/Codex translator, handler, and management tests all pass.

Do not reuse the host's current auth file for the live test. Once the implementation is approved and built, the only required user action is to complete one fresh OpenAI browser authorization in the new UI. If the UI is accessed remotely, an SSH forward for local port 1455 (or the manual callback fallback) will also be needed.
