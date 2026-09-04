# Repository Guidelines

## Project Structure & Module Organization

The entry point is `cmd/claude-proxy/main.go`. Focused packages live under `internal/`: `proxy` handles forwarding, `provider` defines upstream behavior, `pool` selects and pins credentials, `store` owns SQLite persistence, and `router` derives conversation keys. Tests sit beside implementation files as `*_test.go`. The embedded web UI is in `internal/webui/static/`; update `static/js/mock.js` when adding UI API calls. Documentation belongs in `docs/`; `data/` and `creds/` are runtime mounts.

## Build, Test, and Development Commands

- `make env` creates `.env`, runtime directories, and authentication secrets.
- `make build` builds the Docker image; `make up`, `make health`, and `make logs` start and inspect it.
- `go build ./...` verifies all Go packages without Docker.
- `make test` runs `go test -race ./...`.
- `make lint` checks `gofmt`, `go vet`, and `golangci-lint`; run `make lint-install` to install the pinned linter.
- `go test -race -run TestName ./internal/pool/` runs one targeted test.

The UI uses plain ES modules and CSS with no Node build step. Open it with `?mock=1` for mock data.

## Coding Style & Naming Conventions

Use standard Go formatting (tabs from `gofmt`) and keep packages domain-oriented. Exported identifiers use `PascalCase`; unexported names use `camelCase`; package names are short lowercase nouns. Wrap errors with useful context. JavaScript uses two-space indentation, semicolons, `camelCase`, and uppercase module constants. Follow existing BEM-like CSS class patterns.

## Testing Guidelines

Use Go's `testing` package and name tests `TestBehavior`; prefer table-driven cases and `httptest` for HTTP paths. Add regression tests in the changed package. CI runs race-enabled tests with atomic coverage; there is no fixed threshold.

## Commit & Pull Request Guidelines

Recent commits use concise, lowercase, imperative subjects such as `add per-user usage limits`. Keep commits focused. Pull requests should explain behavior and motivation, identify configuration or schema effects, link relevant issues, and report `make test` and `make lint` results. Include screenshots for UI changes and update documentation when operator workflows change.

## Security & Configuration

Never commit `.env`, credential JSON, databases, tokens, or generated TLS state. Start from `.env.example`, bind to loopback by default, and require `PROXY_AUTH_TOKEN` whenever exposing the proxy beyond localhost.
