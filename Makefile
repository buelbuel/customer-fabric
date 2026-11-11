PROJECT := $(shell python3 scripts/compose-project.py)
ifeq ($(PROJECT),)
$(error Could not identify checkout; run Make from the repository root)
endif
ifneq ($(PROJECT),customer-fabric)
# Let Docker allocate loopback ports atomically when each service starts.
PORTS = CF_API_PORT=0 CF_ADMIN_PORT=0 CF_PRODUCTION_UI_PORT=0 CF_KRATOS_PORT=0 CF_MAILPIT_PORT=0 CF_WEBHOOK_SINK_PORT=0
endif
COMPOSE = env -u POSTGRES_PASSWORD -u CF_DEV_TOKENS -u CF_DEV_OPERATOR_TOKEN -u CF_KRATOS_POSTGRES_PASSWORD -u CF_KRATOS_COOKIE_SECRET -u CF_KRATOS_CIPHER_SECRET $(PORTS) docker compose --project-name $(PROJECT) --env-file .env -f deploy/compose.yaml
COMPOSE_M1 = $(COMPOSE) --env-file .env.m1 --env-file .env.oidc -f deploy/compose.m1.yaml
.PHONY: init up down test fmt build logs ports import-benchmark audience-benchmark
init:
	python3 scripts/init-env.py core
up: init
	$(COMPOSE) up --build -d --wait api admin
	$(MAKE) ports
ports:
	@address=$$($(COMPOSE) port api 8080) && printf "API:     http://%s\n" "$$address"
	@address=$$($(COMPOSE) port admin 5173) && printf "Console: http://%s\n" "$$address"
down:
	@if [ -f .env.m1 ] && [ ! -f .env.oidc ]; then python3 scripts/init-env.py oidc >/dev/null; fi
	@if [ -f .env.operator ]; then $(COMPOSE_OPERATOR) --profile '*' down; elif [ -f .env.m1 ]; then $(COMPOSE_M1) --profile '*' down; else $(COMPOSE) --profile '*' down; fi
test: init check-generated
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm go go test -race -count=1 ./...
	$(COMPOSE) run --rm bun bun install --frozen-lockfile
	$(COMPOSE) run --rm bun bun run check
	$(COMPOSE) run --rm bun bun run build
import-benchmark: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm -e CF_IMPORT_BENCHMARK=1 go go test -count=1 -timeout 30m -v -run TestImportBenchmark ./tests/integration/
audience-benchmark: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm -e CF_AUDIENCE_BENCHMARK=1 go go test -count=1 -timeout 60m -v -run 'TestAudienceBenchmark|TestMatchBenchmark' ./tests/integration/
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
	python3 scripts/init-env.py identity
	python3 scripts/init-env.py oidc
	$(COMPOSE_M1) up --build -d --no-deps customer-reference
	@address=$$($(COMPOSE_M1) port customer-reference 8081) && \
		CF_CUSTOMER_BROWSER_ORIGIN="http://$$address" CF_CUSTOMER_BROWSER_PORT="$${address##*:}" $(COMPOSE_M1) up --build -d --wait api admin webhook-sink hydra
	@$(M1_SMOKE) --ready
	$(MAKE) m1-ports
m1-smoke:
	@$(M1_SMOKE)
.PHONY: m1-session-smoke m1-settings-race
M1_SESSION_SMOKE = $(M1_ENDPOINTS) && $(PORTS) python3 scripts/m1-session-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
M1_SESSION_COMPOSE = -- docker compose --project-name $(PROJECT) --env-file .env -f deploy/compose.yaml --env-file .env.m1 --env-file .env.oidc -f deploy/compose.m1.yaml
SETTINGS_RACE_ITERATIONS ?= 40
SETTINGS_RACE_WORKERS ?= 4
m1-session-smoke:
	@$(M1_SESSION_SMOKE) $(M1_SESSION_COMPOSE)
m1-settings-race:
	@$(M1_SESSION_SMOKE) --settings-race-iterations $(SETTINGS_RACE_ITERATIONS) --settings-race-workers $(SETTINGS_RACE_WORKERS) --settings-race-report .tools/reports/settings-race.json $(M1_SESSION_COMPOSE)
m1-ports: ports
	@address=$$($(COMPOSE_M1) port customer-reference 8081) && printf "Customer identity: http://%s\n" "$$address"
	@address=$$($(COMPOSE_M1) port mailpit 8025) && printf "Mailpit: http://%s\n" "$$address"

.PHONY: m1-contracts
m1-contracts:
	@$(M1_ENDPOINTS) && CF_CONTRACT_BROWSER_ORIGIN="http://$$kratos" $(COMPOSE_M1) run --rm contracts sh -c 'bun install --frozen-lockfile && bun contracts-m1.ts'

.PHONY: source-events-demo
source-events-demo:
	@api=$$($(COMPOSE_M1) port api 8080) && bash scripts/source-events-demo.sh "$$api"
	@$(COMPOSE_M1) exec -T db psql -U fabric_m1 -d fabric_m1 -c "SELECT type, version, source, occurred_at FROM outbox ORDER BY occurred_at DESC LIMIT 3"

.PHONY: m1-linking-init m1-linking-smoke customer-view-demo segment-demo activation-demo import-demo
m1-linking-init:
	python3 scripts/init-env.py linking
m1-linking-smoke:
	@$(M1_ENDPOINTS) && python3 -B scripts/m1-linking-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
customer-view-demo:
	@$(M1_ENDPOINTS) && python3 -B scripts/customer-view-demo.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
segment-demo:
	@$(M1_ENDPOINTS) && python3 -B scripts/segment-demo.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
import-demo:
	@api=$$($(COMPOSE_M1) port api 8080) && python3 -B scripts/import-demo.py --api "$$api"
activation-demo:
	@$(M1_ENDPOINTS) && sink=$$($(COMPOSE_M1) port webhook-sink 8090) && python3 -B scripts/activation-demo.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail" --sink "$$sink"

.PHONY: generate check-generated migrate
generate: init
	$(COMPOSE) run --rm sqlc generate
check-generated: init
	$(COMPOSE) run --rm sqlc diff
migrate: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm --build migrate

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
	@$(M1_ENDPOINTS) && $(BROWSER_PYTHON) scripts/m1-browser.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"

COMPOSE_OPERATOR = env -u CF_OPERATOR_POSTGRES_PASSWORD -u CF_OPERATOR_COOKIE_SECRET -u CF_OPERATOR_CIPHER_SECRET $(COMPOSE_M1) --env-file .env.operator -f deploy/compose.operator.yaml
.PHONY: operator-up operator-admin operator-bootstrap operator-roles operator-smoke operator-browser
operator-up: init
	python3 scripts/init-env.py identity
	python3 scripts/init-env.py oidc
	python3 scripts/init-env.py operator
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
operator-roles:
	@python3 scripts/operator-dev.py roles $(SUBJECT) $(ROLES)
operator-smoke:
	@api=$$($(COMPOSE_OPERATOR) port api 8080) && kratos=$$($(COMPOSE_OPERATOR) port operator-reference 8081) && customer=$$($(COMPOSE_OPERATOR) port customer-reference 8081) && mail=$$($(COMPOSE_OPERATOR) port mailpit 8025) && python3 scripts/operator-dev.py smoke "$$api" "$$kratos" "$$customer" "$$mail"
operator-browser:
	@address=$$($(COMPOSE_OPERATOR) port operator-reference 8081) && $(BROWSER_PYTHON) scripts/operator-browser.py "$$address"
