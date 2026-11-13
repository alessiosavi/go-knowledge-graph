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
# Only explicit make arguments may select edge state or extra compose files.
EDGE_AUTH_PATH := deploy/edge/htpasswd
ifeq ($(origin EDGE_HTPASSWD),command line)
EDGE_AUTH_PATH := $(EDGE_HTPASSWD)
endif
EDGE_WAF_PATH := deploy/edge/waf.env
ifeq ($(origin EDGE_WAF_ENV),command line)
EDGE_WAF_PATH := $(EDGE_WAF_ENV)
endif
PASSWD_LOGIN :=
ifeq ($(origin LOGIN),command line)
PASSWD_LOGIN := $(LOGIN)
endif
PASSWD_GENERATE := 0
ifeq ($(origin GENERATE),command line)
PASSWD_GENERATE := $(GENERATE)
endif
WAF_MODE :=
ifeq ($(origin MODE),command line)
WAF_MODE := $(MODE)
endif
# Quote literal values before passing them to the shell (Make 3.81 compatible).
quote = '$(subst ','"'"',$(1))'
# The edge renders these bind-mounted files only when its container starts, and
# compose never compares mounted file contents. Their digest labels the edge
# service, so `compose up` recreates the edge exactly when one of them changes.
# Keep the list equal to the edge's bind mounts (TestComposeEdgeConfigDigest).
EDGE_CONFIG_FILES := deploy/edge/default.conf.template deploy/edge/setup.conf.template deploy/edge/15-validate-env.sh deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf deploy/edge/modsec/RESPONSE-999-EXCLUSION-RULES-AFTER-CRS.conf
EDGE_CONFIG_DIGEST := $(shell cat $(wildcard $(EDGE_CONFIG_FILES)) /dev/null | cksum | tr -s ' ' '-')
COMPOSE_ENV = GKG_ENV_FILE=$(call quote,$(abspath $(ENV_FILE))) EDGE_HTPASSWD=$(call quote,$(abspath $(EDGE_AUTH_PATH))) EDGE_WAF_ENV=$(call quote,$(abspath $(EDGE_WAF_PATH))) EDGE_CONFIG_DIGEST=$(call quote,$(EDGE_CONFIG_DIGEST))
COMPOSE_CMD = $(DOCKER) compose $(COMPOSE_PROJECT) --env-file "$(ENV_FILE)" -f deploy/docker-compose.yml
ifeq ($(LIGHT),1)
COMPOSE_CMD += -f deploy/compose.light.yml
endif
ifeq ($(origin COMPOSE_EXTRA),command line)
COMPOSE_CMD += $(if $(strip $(COMPOSE_EXTRA)),-f $(call quote,$(COMPOSE_EXTRA)))
endif

COMPOSE = $(COMPOSE_ENV) $(COMPOSE_CMD)
CERT_STAGING :=
ifeq ($(origin STAGING),command line)
ifeq ($(STAGING),1)
CERT_STAGING := --test-cert
endif
endif

.PHONY: help up down destroy ps logs build pull restart prune status smoke check-env check-edge repo-modes passwd waf-mode edge-check cert renew

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
	  'passwd   Create/update a login: LOGIN=<name> [GENERATE=1]' \
	  'waf-mode Set MODE=On|DetectionOnly and recreate the edge' \
	  'cert     Issue and install for the running edge [STAGING=1]' \
	  'renew    Renew, install, reload and verify served certificate' \
	  'edge-check Check an isolated edge stack (Docker required)' \
	  'check-edge Check that the password file exists' \
	  'check-env Check that ENV_FILE exists' \
	  'box-*    Provision, configure and manage the AWS box (docs/deploy-aws.md)' \
	  'deploy   Deploy REF=<commit> through SSM and check the public edge'

check-env:
	@test -f "$(ENV_FILE)" || { printf '%s\n' 'stack: $(ENV_FILE) not found — cp .env.example .env and set MEMGRAPH_PASSWORD'; exit 1; }

check-edge:
	@test -s $(call quote,$(abspath $(EDGE_AUTH_PATH))) || { printf '%s\n' 'stack: password file missing or empty — run make passwd LOGIN=<name>'; exit 1; }

repo-modes:
	@bash scripts/repo-modes.sh

passwd:
	@bash scripts/edge-passwd.sh $(call quote,$(DOCKER)) $(call quote,$(abspath $(EDGE_AUTH_PATH))) $(call quote,$(PASSWD_LOGIN)) $(call quote,$(PASSWD_GENERATE))

waf-mode: repo-modes
	@bash scripts/edge-waf-mode.sh $(call quote,$(WAF_MODE)) $(call quote,$(abspath $(EDGE_WAF_PATH)))
	@$(COMPOSE) up -d --force-recreate --no-deps edge

cert: repo-modes
	@$(COMPOSE_ENV) bash scripts/edge-renew.sh --issue $(CERT_STAGING) $(COMPOSE_CMD)

renew: repo-modes
	@$(COMPOSE) --profile cert run --rm certbot renew
	@$(COMPOSE_ENV) bash scripts/edge-renew.sh $(COMPOSE_CMD)

edge-check:
	@bash scripts/edge-check.sh

up: check-env check-edge repo-modes
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

restart: repo-modes
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

# AWS targets take non-secret switches only from explicit make arguments.
AWS_REF :=
ifeq ($(origin REF),command line)
AWS_REF := $(REF)
endif
AWS_YES := 0
ifeq ($(origin YES),command line)
AWS_YES := $(YES)
endif
AWS_PURGE := 0
ifeq ($(origin PURGE),command line)
AWS_PURGE := $(PURGE)
endif
AWS_PURGE_SNAPSHOTS := 0
ifeq ($(origin PURGE_SNAPSHOTS),command line)
AWS_PURGE_SNAPSHOTS := $(PURGE_SNAPSHOTS)
endif
AWS_REPLACE_A := 0
ifeq ($(origin REPLACE_A),command line)
AWS_REPLACE_A := $(REPLACE_A)
endif
.PHONY: up-cached box-provision box-dns box-dns-check box-docker box-env box-login box-secrets box-bootstrap deploy box-update box-teardown

up-cached: check-env check-edge repo-modes
	@$(COMPOSE) up -d --wait --no-build --pull never --remove-orphans

box-provision:
	@bash deploy/aws/provision.sh

box-dns:
	@REPLACE_A=$(call quote,$(AWS_REPLACE_A)) bash deploy/aws/dns.sh

box-dns-check:
	@bash deploy/aws/dns.sh --check

box-docker:
	@bash deploy/aws/remote.sh --docker

box-env:
	@SOURCE_ENV=$(call quote,$(abspath $(ENV_FILE))) bash deploy/aws/make-box-env.sh

box-login:
	@bash deploy/aws/deploy-login.sh

box-secrets:
	@bash deploy/aws/upload-secrets.sh

box-bootstrap:
	@test -n $(call quote,$(AWS_REF)) || { printf '%s\n' 'gkg-box: REF required on command line'; exit 1; }
	@bash deploy/aws/remote.sh bootstrap.sh $(call quote,$(AWS_REF))

deploy:
	@test -n $(call quote,$(AWS_REF)) || { printf '%s\n' 'gkg-box: REF required on command line'; exit 1; }
	@bash deploy/aws/remote.sh deploy.sh $(call quote,$(AWS_REF))

box-update:
	@bash deploy/aws/remote.sh --update

box-teardown:
	@YES=$(call quote,$(AWS_YES)) PURGE=$(call quote,$(AWS_PURGE)) PURGE_SNAPSHOTS=$(call quote,$(AWS_PURGE_SNAPSHOTS)) bash deploy/aws/teardown.sh
