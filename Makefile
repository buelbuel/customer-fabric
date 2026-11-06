COMPOSE = docker compose --env-file .env -f deploy/compose.yaml
.PHONY: init up down test fmt build logs
init:
	python3 scripts/init-dev.py
up: init
	$(COMPOSE) up --build -d db migrate api admin
down:
	$(COMPOSE) down
test: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm go go test -race -count=1 ./...
	$(COMPOSE) run --rm bun bun install --frozen-lockfile
	$(COMPOSE) run --rm bun bun run check
	$(COMPOSE) run --rm bun bun run build
fmt:
	$(COMPOSE) run --rm go go fmt ./...
build:
	$(COMPOSE) build
logs:
	$(COMPOSE) logs --tail=100 api

.PHONY: smoke
smoke:
	python3 scripts/smoke.py
