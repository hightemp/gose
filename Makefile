SHELL := /bin/bash
COMPOSE_FILE := deploy/docker-compose.yml
ENV_FILE := .env
DC := docker compose --env-file $(ENV_FILE) -f $(COMPOSE_FILE)

.PHONY: help up up-all up-adminer down ps logs logs-% restart build pull stop start config clean db-shell

help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Compose file: $(COMPOSE_FILE)"
	@echo "Env file    : $(ENV_FILE)"
	@echo ""
	@echo "Targets:"
	@echo "  up           - Start all services in background"
	@echo "  up-adminer   - Start only Adminer"
	@echo "  down         - Stop and remove containers"
	@echo "  ps           - Show services status"
	@echo "  logs         - Follow all services logs"
	@echo "  logs-<name>  - Follow specific service logs (e.g., logs-search_ui)"
	@echo "  build        - Build images"
	@echo "  pull         - Pull images"
	@echo "  restart      - Restart all services"
	@echo "  stop/start   - Stop or start services"
	@echo "  config       - Validate and view merged config"
	@echo "  clean        - Down and remove volumes"
	@echo "  db-shell     - Open psql shell to Postgres"

# Default target
up: up-all

up-all:
	$(DC) up -d

up-adminer:
	$(DC) up -d adminer

down:
	$(DC) down

ps:
	$(DC) ps

logs:
	$(DC) logs -f --tail=100

logs-%:
	$(DC) logs -f --tail=100 $*

restart:
	$(DC) down
	$(DC) up -d

build:
	$(DC) build --pull

pull:
	$(DC) pull

stop:
	$(DC) stop

start:
	$(DC) start

config:
	$(DC) config

clean:
	$(DC) down -v

db-shell:
	docker exec -it search_pg psql -U "$${POSTGRES_USER:-search}" -d "$${POSTGRES_DB:-search}"