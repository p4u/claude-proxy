# claude-proxy — docker-driven Makefile
#
# All targets run via `docker compose`. The proxy serves on
# http://${HOST_BIND}:${HOST_PORT} (see .env). Credential management talks to
# the same /data volume via `docker compose run --rm`, so it works whether
# `serve` is running or not.

DC      ?= docker compose
SERVICE ?= claude-proxy
DB      ?= /data/proxy.db
RUN     := $(DC) run --rm --no-deps -T $(SERVICE)

# .env must exist for compose env_file to resolve. `make env` bootstraps it.
ENV_FILE := .env

# Read .env values via grep (avoids quoting/sourcing surprises).
HOST_BIND  ?= $(shell grep -E '^HOST_BIND=..*'  $(ENV_FILE) 2>/dev/null | tail -n1 | cut -d= -f2- )
HOST_PORT  ?= $(shell grep -E '^HOST_PORT=..*'  $(ENV_FILE) 2>/dev/null | tail -n1 | cut -d= -f2- )
TLS_DOMAIN ?= $(shell grep -E '^TLS_DOMAIN=..*' $(ENV_FILE) 2>/dev/null | tail -n1 | cut -d= -f2- )
HOST_BIND  := $(if $(HOST_BIND),$(HOST_BIND),127.0.0.1)
HOST_PORT  := $(if $(HOST_PORT),$(HOST_PORT),8787)

# When TLS_DOMAIN is non-empty, activate the `tls` compose profile (traefik)
# AND have all `make` inspection commands hit https://$TLS_DOMAIN instead of
# the plain-HTTP loopback listener.
ifneq ($(strip $(TLS_DOMAIN)),)
COMPOSE_PROFILES := tls
export COMPOSE_PROFILES
BASE := https://$(TLS_DOMAIN)
else
BASE := http://$(HOST_BIND):$(HOST_PORT)
endif

.DEFAULT_GOAL := help

##@ Setup

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

env: ## Create or upgrade .env: sync UID/GID to host shell, add missing auth token.
	@mkdir -p data creds data/letsencrypt
	@if [ ! -f $(ENV_FILE) ]; then cp .env.example $(ENV_FILE); echo "wrote fresh $(ENV_FILE)"; fi
	@HOST_UID=$$(id -u); HOST_GID=$$(id -g); \
	 if grep -q '^PROXY_UID=' $(ENV_FILE); then \
		sed -i.bak "s|^PROXY_UID=.*|PROXY_UID=$$HOST_UID|" $(ENV_FILE); \
	 else echo "PROXY_UID=$$HOST_UID" >> $(ENV_FILE); fi; \
	 if grep -q '^PROXY_GID=' $(ENV_FILE); then \
		sed -i.bak "s|^PROXY_GID=.*|PROXY_GID=$$HOST_GID|" $(ENV_FILE); \
	 else echo "PROXY_GID=$$HOST_GID" >> $(ENV_FILE); fi; \
	 rm -f $(ENV_FILE).bak
	@if ! grep -q '^PROXY_AUTH_TOKEN=..*' $(ENV_FILE); then \
		TOKEN=$$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p -c 256); \
		if grep -q '^PROXY_AUTH_TOKEN=' $(ENV_FILE); then \
			sed -i.bak "s|^PROXY_AUTH_TOKEN=.*|PROXY_AUTH_TOKEN=$$TOKEN|" $(ENV_FILE) && rm -f $(ENV_FILE).bak; \
		else \
			echo "PROXY_AUTH_TOKEN=$$TOKEN" >> $(ENV_FILE); \
		fi; \
		echo "added PROXY_AUTH_TOKEN to $(ENV_FILE)"; \
	fi

token: ## Print the configured PROXY_AUTH_TOKEN (for setting ANTHROPIC_AUTH_TOKEN on clients).
	@if [ ! -f $(ENV_FILE) ]; then echo "no $(ENV_FILE) yet — run 'make env'" >&2; exit 1; fi
	@T=$$(grep -E '^PROXY_AUTH_TOKEN=' $(ENV_FILE) | tail -n1 | cut -d= -f2-); \
	 if [ -z "$$T" ]; then \
		echo "PROXY_AUTH_TOKEN is empty in $(ENV_FILE) — run 'make env' to generate one" >&2; exit 1; \
	 fi; \
	 echo "$$T"

fix-perms: ## chown ./data and ./creds to PROXY_UID:PROXY_GID (your host UID).
	@WANT="$$(id -u):$$(id -g)"; \
	 for d in data creds; do \
		[ -e "$$d" ] || mkdir -p "$$d"; \
		owner=$$(stat -c '%u:%g' "$$d" 2>/dev/null || stat -f '%u:%g' "$$d"); \
		if [ "$$owner" != "$$WANT" ]; then \
			if chown -R "$$WANT" "$$d" 2>/dev/null; then :; \
			else sudo chown -R "$$WANT" "$$d"; fi; \
			echo "chowned $$d -> $$WANT"; \
		fi; \
	 done

rotate-token: ## Generate a new PROXY_AUTH_TOKEN, write to .env, recreate the container.
	@TOKEN=$$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p -c 256); \
	 if grep -q '^PROXY_AUTH_TOKEN=' $(ENV_FILE); then \
		sed -i.bak "s|^PROXY_AUTH_TOKEN=.*|PROXY_AUTH_TOKEN=$$TOKEN|" $(ENV_FILE) && rm -f $(ENV_FILE).bak; \
	 else \
		echo "PROXY_AUTH_TOKEN=$$TOKEN" >> $(ENV_FILE); \
	 fi; \
	 echo "rotated to $$TOKEN"
	@$(DC) up -d --force-recreate $(SERVICE) 2>/dev/null || true

build: env ## Build the docker image locally (only needed for source-tree dev).
	$(DC) build

pull: env ## Pull the published image from GHCR.
	$(DC) pull $(SERVICE)

##@ Service lifecycle

up: env ## Start the proxy (and traefik, if TLS_DOMAIN is set).
	@if [ -n "$(TLS_DOMAIN)" ]; then echo "TLS enabled — domain=$(TLS_DOMAIN), starting traefik (profile=tls)"; fi
	$(DC) up -d
	@sleep 1
	@$(MAKE) -s health || true

down: ## Stop and remove the proxy container.
	$(DC) down

restart: ## Restart the proxy.
	$(DC) restart $(SERVICE)

logs: ## Tail proxy logs (Ctrl-C to stop).
	$(DC) logs -f --tail=200 $(SERVICE)

logs-traefik: ## Tail traefik logs (only when TLS is enabled).
	@if [ -z "$(TLS_DOMAIN)" ]; then echo "TLS_DOMAIN not set in .env — traefik is not running" >&2; exit 1; fi
	$(DC) logs -f --tail=200 traefik

tls-info: ## Show TLS / Traefik status.
	@if [ -z "$(TLS_DOMAIN)" ]; then \
		echo "TLS disabled. Set TLS_DOMAIN (and TLS_EMAIL) in .env, then 'make up'."; \
	else \
		echo "TLS_DOMAIN: $(TLS_DOMAIN)"; \
		echo "BASE URL : $(BASE)"; \
		echo "Profiles : $$COMPOSE_PROFILES"; \
		$(DC) ps traefik 2>/dev/null || true; \
		if [ -f data/letsencrypt/acme.json ]; then \
			echo "ACME storage: $$(stat -c '%s bytes (mode %a, owner %u:%g)' data/letsencrypt/acme.json)"; \
		else echo "ACME storage: not yet created (will appear after first cert request)"; fi; \
	fi

ps: ## Show container status.
	$(DC) ps

##@ Credentials

# usage: make import FROM=acct-A.json LABEL=acct-A [WEIGHT=5]
# FROM is a path RELATIVE to ./creds (which is bind-mounted as /creds:ro).
import: env ## Import a .credentials.json (FROM=acct-A.json LABEL=acct-A [WEIGHT=N]).
	@if [ -z "$(FROM)" ] || [ -z "$(LABEL)" ]; then \
		echo "usage: make import FROM=acct-A.json LABEL=acct-A [WEIGHT=N]"; exit 2; fi
	@if [ ! -f "creds/$(FROM)" ]; then \
		echo "creds/$(FROM) not found — drop the .credentials.json into ./creds first"; exit 1; fi
	$(RUN) creds import \
		--from /creds/$(FROM) --label "$(LABEL)" \
		$(if $(WEIGHT),--weight $(WEIGHT),) \
		--db $(DB)

list: ## List all credentials in the pool.
	$(RUN) creds list --db $(DB)

# usage: make usage [ID=cred_xxx]
usage: ## Show live 5h/7d usage % for all credentials (or ID=cred_xxx for one).
	$(RUN) creds usage $(if $(ID),"$(ID)",) --db $(DB)

# usage: make usage-history [PERIOD=24h] [ID=cred_xxx]
usage-history: ## Chart usage history (PERIOD=1h|6h|24h|7d|30d, optional ID=cred_xxx).
	$(RUN) creds usage-history $(if $(ID),"$(ID)",) $(if $(PERIOD),--period $(PERIOD),) --db $(DB)

# usage: make disable ID=cred_xxx
disable: ## Mark a credential disabled (ID=cred_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make disable ID=cred_xxx"; exit 2; fi
	$(RUN) creds disable "$(ID)" --db $(DB)

# usage: make rm ID=cred_xxx
rm: ## Remove a credential (ID=cred_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make rm ID=cred_xxx"; exit 2; fi
	$(RUN) creds rm "$(ID)" --db $(DB)

# usage: make refresh ID=cred_xxx
refresh: ## Force-refresh a credential's tokens (ID=cred_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make refresh ID=cred_xxx"; exit 2; fi
	$(RUN) creds refresh "$(ID)" --db $(DB)

# usage: make weight ID=cred_xxx W=5
weight: ## Set RR weight (ID=cred_xxx W=N).
	@if [ -z "$(ID)" ] || [ -z "$(W)" ]; then echo "usage: make weight ID=cred_xxx W=N"; exit 2; fi
	$(RUN) creds set-weight "$(ID)" "$(W)" --db $(DB)

##@ Inspection (curl the running service)

# Read PROXY_AUTH_TOKEN from .env; pass as Bearer header when non-empty.
_TOKEN := $(shell grep -E '^PROXY_AUTH_TOKEN=.+' $(ENV_FILE) 2>/dev/null | tail -n1 | cut -d= -f2-)
_AUTH  := $(if $(_TOKEN),-H "Authorization: Bearer $(_TOKEN)",)

health: ## GET /health.
	@curl -sf $(BASE)/health && echo

credentials: ## GET /admin/credentials (running service).
	@curl -s $(_AUTH) $(BASE)/admin/credentials

conversations: ## GET /admin/conversations.
	@curl -s $(_AUTH) $(BASE)/admin/conversations

stats: ## GET /admin/stats.
	@curl -s $(_AUTH) $(BASE)/admin/stats

##@ User tokens

# usage: make user-create NAME=alice
user-create: ## Create a user token (NAME=alice).
	@if [ -z "$(NAME)" ]; then echo "usage: make user-create NAME=alice"; exit 2; fi
	$(RUN) users create --name "$(NAME)" --db $(DB)

user-list: ## List all user tokens.
	$(RUN) users list --db $(DB)

# usage: make user-stats [ID=utok_xxx] [PERIOD=24h]
user-stats: ## Show per-user request stats (optional ID=utok_xxx PERIOD=1h|6h|24h|7d|30d).
	$(RUN) users stats $(if $(ID),"$(ID)",) $(if $(PERIOD),--period $(PERIOD),) --db $(DB)

# usage: make user-token ID=utok_xxx
user-token: ## Print the bearer token for a user (ID=utok_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make user-token ID=utok_xxx"; exit 2; fi
	$(RUN) users token "$(ID)" --db $(DB)

# usage: make user-disable ID=utok_xxx
user-disable: ## Disable a user token (ID=utok_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make user-disable ID=utok_xxx"; exit 2; fi
	$(RUN) users disable "$(ID)" --db $(DB)

# usage: make user-enable ID=utok_xxx
user-enable: ## Re-enable a user token (ID=utok_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make user-enable ID=utok_xxx"; exit 2; fi
	$(RUN) users enable "$(ID)" --db $(DB)

# usage: make user-rm ID=utok_xxx
user-rm: ## Delete a user token (ID=utok_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make user-rm ID=utok_xxx"; exit 2; fi
	$(RUN) users rm "$(ID)" --db $(DB)

# usage: make user-refresh ID=utok_xxx
user-refresh: ## Rotate a user's bearer token (ID=utok_xxx).
	@if [ -z "$(ID)" ]; then echo "usage: make user-refresh ID=utok_xxx"; exit 2; fi
	$(RUN) users refresh "$(ID)" --db $(DB)

##@ Credential backup / restore

export-credentials: ## Dump all credentials as JSONL to stdout.  Usage: make export-credentials > backup.jsonl
	$(RUN) creds export --db $(DB)

import-credentials: ## Import credentials from JSONL on stdin.  Usage: cat backup.jsonl | make import-credentials
	$(DC) run --rm --no-deps -i $(SERVICE) creds import-bulk --db $(DB)

##@ Maintenance

test: ## Run the Go test suite (locally, not in docker).
	go test -race ./...

lint: ## Run gofmt, go vet, and golangci-lint locally (matches CI).
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt would change:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m; \
	elif [ -x "$$(go env GOPATH)/bin/golangci-lint" ]; then \
		"$$(go env GOPATH)/bin/golangci-lint" run --timeout=5m; \
	else \
		echo "golangci-lint not installed. Run 'make lint-install'." >&2; exit 1; \
	fi

lint-install: ## go-install golangci-lint v2.12.2 into $GOPATH/bin.
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
	@echo "Installed to $$(go env GOPATH)/bin/golangci-lint"

clean: ## Stop service and delete the SQLite DB. Keeps creds/.
	-$(DC) down
	rm -f data/proxy.db data/proxy.db-shm data/proxy.db-wal

distclean: clean ## clean + remove built image and .env.
	-docker rmi claude-proxy:latest
	rm -f $(ENV_FILE)

.PHONY: help env token rotate-token fix-perms build pull up down restart logs logs-traefik tls-info ps lint lint-install \
        import list usage usage-history disable rm refresh weight \
        export-credentials import-credentials \
        user-create user-list user-stats user-token user-disable user-enable user-rm user-refresh \
        health credentials conversations stats \
        test clean distclean
