PROJECT := $(shell python3 scripts/compose-project.py)
ifeq ($(PROJECT),)
$(error Could not identify checkout; run Make from the repository root)
endif
ifneq ($(PROJECT),customer-fabric)
# Let Docker allocate loopback ports atomically when each service starts.
PORTS = CF_API_PORT=0 CF_ADMIN_PORT=0 CF_PRODUCTION_UI_PORT=0 CF_KRATOS_PORT=0 CF_MAILPIT_PORT=0
endif
COMPOSE = env -u POSTGRES_PASSWORD -u CF_DEV_TOKENS -u CF_DEV_OPERATOR_TOKEN -u CF_KRATOS_POSTGRES_PASSWORD -u CF_KRATOS_COOKIE_SECRET -u CF_KRATOS_CIPHER_SECRET $(PORTS) docker compose --project-name $(PROJECT) --env-file .env -f deploy/compose.yaml
COMPOSE_M1 = $(COMPOSE) --env-file .env.m1 -f deploy/compose.m1.yaml
.PHONY: init up down test fmt build logs ports
init:
	python3 scripts/init-dev.py
up: init
	$(COMPOSE) up --build -d --wait api admin
	$(MAKE) ports
ports:
	@address=$$($(COMPOSE) port api 8080) && printf "API:     http://%s\n" "$$address"
	@address=$$($(COMPOSE) port admin 5173) && printf "Console: http://%s\n" "$$address"
down:
	@if [ -f .env.operator ]; then $(COMPOSE_OPERATOR) --profile '*' down; elif [ -f .env.m1 ]; then $(COMPOSE_M1) --profile '*' down; else $(COMPOSE) --profile '*' down; fi
test: init check-generated
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
	@address=$$($(COMPOSE) port api 8080) && python3 scripts/smoke.py "$${address#127.0.0.1:}"

.PHONY: m1-up m1-smoke m1-ports
M1_ENDPOINTS = api=$$($(COMPOSE_M1) port api 8080) && kratos=$$($(COMPOSE_M1) port customer-reference 8081) && mail=$$($(COMPOSE_M1) port mailpit 8025)
M1_SMOKE = $(M1_ENDPOINTS) && python3 scripts/m1-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
m1-up: init
	python3 scripts/init-identity-dev.py
	$(COMPOSE_M1) up --build -d --no-deps customer-reference
	@address=$$($(COMPOSE_M1) port customer-reference 8081) && \
		CF_CUSTOMER_BROWSER_ORIGIN="http://$$address" CF_CUSTOMER_BROWSER_PORT="$${address##*:}" $(COMPOSE_M1) up --build -d --wait api admin
	@$(M1_SMOKE) --ready
	$(MAKE) m1-ports
m1-smoke:
	@$(M1_SMOKE)
.PHONY: m1-session-smoke
m1-session-smoke:
	@$(M1_ENDPOINTS) && $(PORTS) python3 scripts/m1-session-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail" -- docker compose --project-name $(PROJECT) --env-file .env -f deploy/compose.yaml --env-file .env.m1 -f deploy/compose.m1.yaml
m1-ports: ports
	@address=$$($(COMPOSE_M1) port customer-reference 8081) && printf "Customer identity: http://%s\n" "$$address"
	@address=$$($(COMPOSE_M1) port mailpit 8025) && printf "Mailpit: http://%s\n" "$$address"

.PHONY: m1-linking-init m1-linking-smoke
m1-linking-init:
	python3 scripts/init-linking-dev.py
m1-linking-smoke:
	@api=$$($(COMPOSE_M1) port api 8080) && kratos=$$($(COMPOSE_M1) port customer-reference 8081) && mail=$$($(COMPOSE_M1) port mailpit 8025) && python3 -B scripts/m1-linking-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"

.PHONY: generate check-generated migrate
generate: init
	$(COMPOSE) run --rm sqlc generate
check-generated: init
	$(COMPOSE) run --rm sqlc diff
migrate: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm --build migrate

.PHONY: check-recovery-protocol
check-recovery-protocol:
	python3 -B tools/verification/recovery_protocol_model.py
	python3 -B tools/verification/recovery_protocol_scopes_model.py

.PHONY: contracts security production-ui
contracts: init
	$(COMPOSE) up --build -d --wait api
	$(COMPOSE) run --rm contracts sh -c 'bun install --frozen-lockfile && bun contracts.ts && bun fixtures.ts'
security: init
	$(COMPOSE) run --rm go go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
	$(COMPOSE) run --rm bun sh -c 'bun install --frozen-lockfile && bun audit'
	$(COMPOSE) run --rm contracts sh -c 'bun install --frozen-lockfile && bun audit'
production-ui: init
	$(COMPOSE) up --build -d admin-production

FIXTURE_PRESET ?= small
FIXTURE_OUT ?= ../.tools/enterprise-$(FIXTURE_PRESET)
.PHONY: fixtures check-fixtures
fixtures: init
	$(COMPOSE) run --rm go go run ./tests/generator -preset $(FIXTURE_PRESET) -out $(FIXTURE_OUT)
check-fixtures: init
	$(COMPOSE) run --rm go go run ./tests/generator -validate ../tests/fixtures/enterprise/sample

.PHONY: m1-browser
BROWSER_PYTHON ?= python3
m1-browser:
	@kratos=$$($(COMPOSE_M1) port customer-reference 8081) && mail=$$($(COMPOSE_M1) port mailpit 8025) && $(BROWSER_PYTHON) scripts/m1-browser.py --kratos "$$kratos" --mailpit "$$mail"

COMPOSE_OPERATOR = env -u CF_OPERATOR_POSTGRES_PASSWORD -u CF_OPERATOR_COOKIE_SECRET -u CF_OPERATOR_CIPHER_SECRET $(COMPOSE_M1) --env-file .env.operator -f deploy/compose.operator.yaml
.PHONY: operator-up operator-admin operator-bootstrap operator-smoke operator-browser
operator-up: init
	python3 scripts/init-identity-dev.py
	python3 scripts/init-operator-dev.py
	$(COMPOSE_OPERATOR) up --build -d --no-deps customer-reference operator-reference
	@customer=$$($(COMPOSE_OPERATOR) port customer-reference 8081) && operator=$$($(COMPOSE_OPERATOR) port operator-reference 8081) && \
		CF_CUSTOMER_BROWSER_ORIGIN="http://$$customer" CF_CUSTOMER_BROWSER_PORT="$${customer##*:}" \
		CF_OPERATOR_BROWSER_ORIGIN="http://$$operator" CF_OPERATOR_BROWSER_PORT="$${operator##*:}" \
		$(COMPOSE_OPERATOR) up --build -d --wait api && \
		printf "Customer identity: http://%s\nOperator identity: http://%s\n" "$$customer" "$$operator"
operator-admin:
	@$(COMPOSE_OPERATOR) run --rm --no-deps -T operator-admin-tool
operator-bootstrap:
	@python3 scripts/operator-dev.py bootstrap
operator-smoke:
	@api=$$($(COMPOSE_OPERATOR) port api 8080) && kratos=$$($(COMPOSE_OPERATOR) port operator-reference 8081) && customer=$$($(COMPOSE_OPERATOR) port customer-reference 8081) && mail=$$($(COMPOSE_OPERATOR) port mailpit 8025) && python3 scripts/operator-dev.py smoke "$$api" "$$kratos" "$$customer" "$$mail"
operator-browser:
	@address=$$($(COMPOSE_OPERATOR) port operator-reference 8081) && $(BROWSER_PYTHON) scripts/operator-browser.py "$$address"
