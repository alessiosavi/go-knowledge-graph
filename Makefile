SHELL := /bin/bash
DOCKER ?= docker
ENV_FILE ?= .env

ifneq ($(words $(ENV_FILE)),1)
$(error ENV_FILE must be a single path without whitespace)
endif

COMPOSE_PROJECT :=
ifeq ($(origin PROJECT),command line)
COMPOSE_PROJECT = $(if $(strip $(PROJECT)),-p "$(PROJECT)")
endif
COMPOSE = GKG_ENV_FILE="$(abspath $(ENV_FILE))" $(DOCKER) compose $(COMPOSE_PROJECT) --env-file "$(ENV_FILE)" -f deploy/docker-compose.yml
ifeq ($(LIGHT),1)
COMPOSE += -f deploy/compose.light.yml
endif

.PHONY: help up down destroy ps logs build pull restart prune status smoke check-env

help:
	@printf '%s\n' \
	  'up       Start the stack; LIGHT=1 skips model services' \
	  'down     Stop the stack and keep graph data' \
	  'destroy  Delete the graph volume (requires CONFIRM=yes)' \
	  'ps       Show project services' \
	  'logs     Follow logs (optional S=service)' \
	  'build    Build images (optional S=service)' \
	  'pull     Pull images, skipping buildable services' \
	  'restart  Restart services (optional S=service)' \
	  'prune    Prune dangling images and host-wide builder cache older than 7 days' \
	  'status   Show project services and their resource usage' \
	  'smoke    Check the running project and delete the temporary document' \
	  'check-env Check that ENV_FILE exists'

check-env:
	@test -f "$(ENV_FILE)" || { printf '%s\n' 'stack: $(ENV_FILE) not found — cp .env.example .env and set MEMGRAPH_PASSWORD'; exit 1; }

up: check-env
	@$(COMPOSE) up -d --build --wait --remove-orphans

down:
	@$(COMPOSE) down --remove-orphans

destroy:
	@if [ "$(origin CONFIRM)" != 'command line' ] || [ "$(CONFIRM)" != yes ]; then printf '%s\n' 'stack: make destroy CONFIRM=yes deletes the graph volume'; exit 1; fi
	@$(COMPOSE) down -v --remove-orphans

ps:
	@$(COMPOSE) ps

logs:
	@$(COMPOSE) logs -f --tail=200 $(S)

build:
	@$(COMPOSE) build $(S)

pull:
	@$(COMPOSE) pull --ignore-buildable

restart:
	@$(COMPOSE) restart $(S)

prune:
	@$(DOCKER) image prune -f
	@$(DOCKER) builder prune -f --filter until=168h

status:
	@$(COMPOSE) ps
	@ids="$$($(COMPOSE) ps -q)" || exit 1; \
	  if [ -n "$$ids" ]; then $(DOCKER) stats --no-stream $$ids; fi

smoke:
	@binding="$$($(COMPOSE) port gkg 8080)" || exit 1; \
	  if [ -z "$$binding" ]; then printf '%s\n' 'stack: gkg is not running in this project'; exit 1; fi; \
	  umask 077; key="$$(mktemp)" || exit 1; \
	  trap 'rm -f "$$key"' EXIT; trap 'exit 1' HUP INT TERM; \
	  chmod 600 "$$key" || exit 1; \
	  $(COMPOSE) exec -T gkg printenv GKG_API_KEY >"$$key" 2>/dev/null || :; \
	  SMOKE_URL="http://$$binding" SMOKE_KEY_FILE="$$key" bash scripts/smoke.sh
