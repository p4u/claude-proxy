# claude-proxy — docker-driven Makefile
#
# All targets run via `docker compose`. The proxy serves on
# http://${HOST_BIND}:${HOST_PORT} (see .env). Credential management talks to
# the same /data volume via `docker compose run --rm`, so it works whether
# `serve` is running or not.

DC      ?= docker compose
SERVICE ?= claude-proxy
DB      ?= /data/proxy.db
RUN     := $(DC) run --rm --no-deps $(SERVICE)

# .env must exist for compose env_file to resolve. `make env` bootstraps it.
ENV_FILE := .env

# Where on the host imports/admin curls hit the running service. Re-read from
# .env on every invocation so the user can edit it freely.
HOST_BIND ?= $(shell . ./$(ENV_FILE) 2>/dev/null; echo $${HOST_BIND:-127.0.0.1})
HOST_PORT ?= $(shell . ./$(ENV_FILE) 2>/dev/null; echo $${HOST_PORT:-8787})
BASE      := http://$(HOST_BIND):$(HOST_PORT)

.DEFAULT_GOAL := help

##@ Setup

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

env: ## Create or upgrade .env: sync UID/GID to host shell, add missing auth token.
	@mkdir -p data creds
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

fix-perms: ## chown ./data and ./creds to the current host UID:GID (uses sudo if needed).
	@HOST_UID=$$(id -u); HOST_GID=$$(id -g); \
	 NEED_SUDO=""; \
	 for d in data creds; do \
		[ -e "$$d" ] || mkdir -p "$$d"; \
		owner=$$(stat -c '%u:%g' "$$d" 2>/dev/null || stat -f '%u:%g' "$$d"); \
		want="$$HOST_UID:$$HOST_GID"; \
		if [ "$$owner" != "$$want" ]; then \
			if [ -w "$$d" ] && chown -R "$$want" "$$d" 2>/dev/null; then :; \
			else NEED_SUDO=1; sudo chown -R "$$want" "$$d"; fi; \
			echo "chowned $$d -> $$want"; \
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

build: env ## Build the docker image.
	$(DC) build

##@ Service lifecycle

up: env ## Start the proxy in the background.
	$(DC) up -d
	@sleep 1
	@$(MAKE) -s health || true

down: ## Stop and remove the proxy container.
	$(DC) down

restart: ## Restart the proxy.
	$(DC) restart $(SERVICE)

logs: ## Tail proxy logs (Ctrl-C to stop).
	$(DC) logs -f --tail=200 $(SERVICE)

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

health: ## GET /health.
	@curl -sf $(BASE)/health && echo

credentials: ## GET /admin/credentials (running service).
	@curl -s $(BASE)/admin/credentials

conversations: ## GET /admin/conversations.
	@curl -s $(BASE)/admin/conversations

stats: ## GET /admin/stats.
	@curl -s $(BASE)/admin/stats

##@ Maintenance

test: ## Run the Go test suite (locally, not in docker).
	go test ./...

clean: ## Stop service and delete the SQLite DB. Keeps creds/.
	-$(DC) down
	rm -f data/proxy.db data/proxy.db-shm data/proxy.db-wal

distclean: clean ## clean + remove built image and .env.
	-docker rmi claude-proxy:latest
	rm -f $(ENV_FILE)

.PHONY: help env token rotate-token fix-perms build up down restart logs ps \
        import list disable rm refresh weight \
        health credentials conversations stats \
        test clean distclean
