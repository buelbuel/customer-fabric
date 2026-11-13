PROJECT := $(shell python3 scripts/compose-project.py)
ifeq ($(PROJECT),)
$(error Could not identify checkout; run Make from the repository root)
endif
ifneq ($(PROJECT),customer-fabric)
# Let Docker allocate loopback ports atomically when each service starts.
PORTS = CF_API_PORT=0 CF_ADMIN_PORT=0 CF_PRODUCTION_UI_PORT=0 CF_KRATOS_PORT=0 CF_MAILPIT_PORT=0 CF_WEBHOOK_SINK_PORT=0 CF_OPERATOR_PORT=0
endif
COMPOSE = env -u POSTGRES_PASSWORD -u CF_DEV_TOKENS -u CF_KRATOS_POSTGRES_PASSWORD -u CF_KRATOS_COOKIE_SECRET -u CF_KRATOS_CIPHER_SECRET \
	-u CF_OPERATOR_POSTGRES_PASSWORD -u CF_OPERATOR_COOKIE_SECRET -u CF_OPERATOR_CIPHER_SECRET $(PORTS) \
	docker compose --project-name $(PROJECT) --env-file .env --env-file .env.m1 --env-file .env.oidc --env-file .env.operator -f deploy/compose.yaml
.PHONY: init up down test fmt build logs ports import-benchmark audience-benchmark
# Creates each missing ignored env file with random local credentials; never overwrites one.
init:
	@for stack in core identity oidc operator webhooks linking; do python3 scripts/init-env.py $$stack; done
ENDPOINTS = api=$$($(COMPOSE) port api 8080) && kratos=$$($(COMPOSE) port customer-reference 8081) && mail=$$($(COMPOSE) port mailpit 8025)
# A session of a new synthetic operator holding every role, signed in with password and TOTP at the
# operator issuer, for the scripts and contracts that exercise operator surfaces.
OPERATOR_SESSION = operator=$$($(COMPOSE) port operator-reference 8081) && CF_OPERATOR_SESSION=$$(python3 -B scripts/operator-dev.py session "$$operator") && export CF_OPERATOR_SESSION
SMOKE = $(ENDPOINTS) && python3 scripts/m1-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
up: init
	$(COMPOSE) up --build -d --no-deps customer-reference operator-reference
	@customer=$$($(COMPOSE) port customer-reference 8081) && operator=$$($(COMPOSE) port operator-reference 8081) && \
		CF_CUSTOMER_BROWSER_ORIGIN="http://$$customer" CF_CUSTOMER_BROWSER_PORT="$${customer##*:}" \
		CF_OPERATOR_BROWSER_ORIGIN="http://$$operator" CF_OPERATOR_BROWSER_PORT="$${operator##*:}" \
		$(COMPOSE) up --build -d --wait api admin webhook-sink hydra
	@$(SMOKE) --ready
	$(MAKE) ports
ports:
	@address=$$($(COMPOSE) port api 8080) && printf "API:               http://%s\n" "$$address"
	@address=$$($(COMPOSE) port admin 5173) && printf "Console:           http://%s\n" "$$address"
	@address=$$($(COMPOSE) port customer-reference 8081) && printf "Customer identity: http://%s\n" "$$address"
	@address=$$($(COMPOSE) port operator-reference 8081) && printf "Operator identity: http://%s\n" "$$address"
	@address=$$($(COMPOSE) port mailpit 8025) && printf "Mailpit:           http://%s\n" "$$address"
down: init
	$(COMPOSE) --profile '*' down
test: init check-generated
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm go go test -race -count=1 ./...
	$(COMPOSE) run --rm bun bun install --frozen-lockfile
	$(COMPOSE) run --rm bun bun run check
	$(COMPOSE) run --rm bun bun run build
import-benchmark: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm -e CF_IMPORT_BENCHMARK=1 go go test -count=1 -timeout 30m -v -run TestImportBenchmark ./tests/integration/
# Opt-in event transport (ADR 0013). Kafka never starts with `make up` or `make test`.
COMPOSE_EVENTS = $(COMPOSE) -f deploy/compose.events.yaml
.PHONY: events-up events-test events-scan events-down
events-up: init
	$(COMPOSE_EVENTS) up --build -d --wait api
events-test: init
	@# Created by the caller first: a directory Docker creates for the mount would be root-owned and
	@# unwritable to events-scan afterwards.
	mkdir -p .tools/security
	$(COMPOSE_EVENTS) up -d --wait db kafka temporal
	$(COMPOSE_EVENTS) run --rm go go test -race -count=1 -v -run 'TestOutboxRelay|TestProfileChangeWorkflowEndToEnd' ./tests/integration/
events-scan: init
	$(COMPOSE_EVENTS) build kafka
	bash scripts/scan-images.sh --events
events-down:
	$(COMPOSE_EVENTS) down
audience-benchmark: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm -e CF_AUDIENCE_BENCHMARK=1 go go test -count=1 -timeout 60m -v -run 'TestAudienceBenchmark|TestMatchBenchmark|TestAudienceJobBenchmark' ./tests/integration/
fmt:
	$(COMPOSE) run --rm go go fmt ./...
build:
	$(COMPOSE) build
logs:
	$(COMPOSE) logs --tail=100 api


.PHONY: smoke session-smoke settings-race
smoke:
	@$(OPERATOR_SESSION) && $(SMOKE)
SESSION_SMOKE = $(ENDPOINTS) && $(PORTS) python3 scripts/m1-session-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
SESSION_COMPOSE = -- docker compose --project-name $(PROJECT) --env-file .env --env-file .env.m1 --env-file .env.oidc --env-file .env.operator -f deploy/compose.yaml
SETTINGS_RACE_ITERATIONS ?= 40
SETTINGS_RACE_WORKERS ?= 4
session-smoke:
	@$(SESSION_SMOKE) $(SESSION_COMPOSE)
settings-race:
	@$(SESSION_SMOKE) --settings-race-iterations $(SETTINGS_RACE_ITERATIONS) --settings-race-workers $(SETTINGS_RACE_WORKERS) --settings-race-report .tools/reports/settings-race.json $(SESSION_COMPOSE)

# Live contracts against the running stack: the source integration API (contracts.ts), every customer,
# linking and operator operation (contracts-identity.ts), and the committed enterprise fixtures.
.PHONY: contracts
contracts:
	@$(ENDPOINTS) && $(OPERATOR_SESSION) && CF_CONTRACT_OPERATOR_SESSION="$$CF_OPERATOR_SESSION" CF_CONTRACT_BROWSER_ORIGIN="http://$$kratos" \
		$(COMPOSE) run --rm contracts sh -c 'bun install --frozen-lockfile && bun contracts.ts && bun contracts-identity.ts && bun fixtures.ts'

.PHONY: source-events-demo linking-smoke customer-view-demo segment-demo activation-demo import-demo
source-events-demo:
	@api=$$($(COMPOSE) port api 8080) && $(OPERATOR_SESSION) && bash scripts/source-events-demo.sh "$$api"
	@$(COMPOSE) exec -T db psql -U fabric_m1 -d fabric_m1 -c "SELECT type, version, source, occurred_at FROM outbox ORDER BY occurred_at DESC LIMIT 3"
linking-smoke:
	@$(ENDPOINTS) && python3 -B scripts/m1-linking-smoke.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
customer-view-demo:
	@$(ENDPOINTS) && $(OPERATOR_SESSION) && python3 -B scripts/customer-view-demo.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
segment-demo:
	@$(ENDPOINTS) && $(OPERATOR_SESSION) && python3 -B scripts/segment-demo.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"
import-demo:
	@api=$$($(COMPOSE) port api 8080) && python3 -B scripts/import-demo.py --api "$$api"
activation-demo:
	@$(ENDPOINTS) && $(OPERATOR_SESSION) && sink=$$($(COMPOSE) port webhook-sink 8090) && python3 -B scripts/activation-demo.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail" --sink "$$sink"

.PHONY: generate check-generated migrate
generate: init
	$(COMPOSE) run --rm sqlc generate
check-generated: init
	$(COMPOSE) run --rm sqlc diff
migrate: init
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm --build migrate

.PHONY: security production-ui
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

.PHONY: browser
BROWSER_PYTHON ?= python3
browser:
	@$(ENDPOINTS) && $(OPERATOR_SESSION) && $(BROWSER_PYTHON) scripts/m1-browser.py --api "$$api" --kratos "$$kratos" --mailpit "$$mail"

.PHONY: operator-admin operator-bootstrap operator-roles operator-session operator-smoke operator-browser
operator-admin:
	@$(COMPOSE) run --rm --no-deps -T operator-admin-tool
operator-bootstrap:
	@python3 scripts/operator-dev.py bootstrap
operator-roles:
	@python3 scripts/operator-dev.py roles $(SUBJECT) $(ROLES)
# Prints a session token of a new synthetic operator holding every role, for the console. Local only.
operator-session:
	@operator=$$($(COMPOSE) port operator-reference 8081) && python3 -B scripts/operator-dev.py session "$$operator"
operator-smoke:
	@api=$$($(COMPOSE) port api 8080) && kratos=$$($(COMPOSE) port operator-reference 8081) && customer=$$($(COMPOSE) port customer-reference 8081) && mail=$$($(COMPOSE) port mailpit 8025) && python3 scripts/operator-dev.py smoke "$$api" "$$kratos" "$$customer" "$$mail"
operator-browser:
	@address=$$($(COMPOSE) port operator-reference 8081) && $(BROWSER_PYTHON) scripts/operator-browser.py "$$address"
