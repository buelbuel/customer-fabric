# Customer Fabric

**Identity. Consent. Customer Data. One coherent platform.**

An Apache-2.0, EU-first customer infrastructure platform. This repository implements the first **development-only profile vertical slice**: a Go API, PostgreSQL persistence and a React operations console. Identity, consent, background delivery and cloud services are future milestones, not working integrations.

## Run locally

Requirements: Docker Engine with Docker Compose v2 (Docker Desktop or Colima plus the Compose plugin), Python 3 and Make. Go and Bun can run in the provided tool containers; host installations are optional.

```sh
make init   # creates ignored .env with random secrets, never overwrites it
make up     # PostgreSQL, migration, API, React development console
```

Open http://127.0.0.1:5173. Copy the `console` token from your local `.env` into the connection form. Tokens stay in browser memory. **Use synthetic data only.** API: http://127.0.0.1:8080. Only these loopback ports are published; PostgreSQL has no host port. No cloud registration or outbound runtime telemetry is required.

`make down` stops services and preserves database data. `make logs` shows sanitized API lifecycle messages. Never use `down -v` unless you intend to delete local database volumes. If `.env` is lost while a volume remains, restore its database password rather than generating a new incompatible one.

The API refuses startup unless `CF_MODE=development` and valid source credentials are configured. There is no production mode in this increment. The Vite console and development bearer credentials must not be exposed publicly.

## Use the API

Load the generated configuration without printing credentials:

```sh
set -a
. ./.env
set +a
export CF_TOKEN="$(python3 -c 'import json,os; print(json.loads(os.environ["CF_DEV_TOKENS"])["console"])')"

curl -i http://127.0.0.1:8080/v1/profiles \
  -H "Authorization: Bearer $CF_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: synthetic-create-0001' \
  --data '{"set":{"display_name":"Synthetic Ada","active":true},"identifiers":[{"namespace":"crm","value":"synthetic-001"}]}'
```

Creation returns a receipt `{id, version, mutation_id}`, a `Location`, and an ETag such as `"1"`. Set `PROFILE_ID` to the returned ID:

```sh
export PROFILE_ID='replace-with-returned-id'
curl -i "http://127.0.0.1:8080/v1/profiles/$PROFILE_ID" -H "Authorization: Bearer $CF_TOKEN"
curl 'http://127.0.0.1:8080/v1/profiles?limit=20' -H "Authorization: Bearer $CF_TOKEN"
curl -i -X PATCH "http://127.0.0.1:8080/v1/profiles/$PROFILE_ID" \
  -H "Authorization: Bearer $CF_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: synthetic-update-0001' \
  -H 'If-Match: "1"' \
  --data '{"set":{"display_name":"Synthetic Grace"},"remove":["active"],"identifiers":[{"namespace":"support","value":"synthetic-002"}]}'
```

Retry uncertain mutations unchanged with the same key. New intent requires a new key. A stale version returns 412; conflicting source ownership or identifier association returns 409. The server derives source attribution from credentials. Clients cannot nominate an authoritative source in the request.

Attributes support **top-level non-null scalars only**. Arrays, nested objects and full replacement fail explicitly. First writer owns a field; its removal retains ownership. The same owner can explicitly restore it. Identifier association is add-only, scoped by credential source and namespace. No automatic matching or merging by email occurs.

## Develop and verify

```sh
make test  # real PostgreSQL integration tests + Go race detector + console checks/build
make fmt
make generate         # regenerate typed pgx queries after SQL changes
make check-generated  # detect stale generated code
make migrate          # apply reviewed goose SQL migrations
make build
```

For host Go 1.26+ and Bun 1.3.3+:

```sh
cd backend
go test ./...      # integration tests skip unless TEST_DATABASE_URL is set
go vet ./...
go build ./cmd/api
cd ../web/admin
bun install --frozen-lockfile
bun run check
bun run build
bun run dev --host 127.0.0.1
```

See [local development](docs/development/local.md) for running the API with host Go and a private local database, and [testing](docs/development/testing.md) for test isolation and verification limits.

## Scope and documentation

- [Architecture](docs/architecture/overview.md) and [initial ADR](docs/adr/0001-architecture.md)
- [REST OpenAPI 3.1 contract](contracts/openapi/customer-fabric.yaml)
- [Outbox schemas](contracts/events): durable records only; **no delivery worker**
- [Security, privacy and EU-first limitations](docs/security/eu-first.md)
- [Roadmap](docs/architecture/roadmap.md), [dependencies](docs/development/dependencies.md)
- [Contribution policy](CONTRIBUTING.md), [security reporting](SECURITY.md)

Missing production controls include real administrative authentication/authorization, TLS, credential lifecycle, least-privilege DB roles, automated retention, export/erasure, backup lifecycle and tested disaster recovery. No GDPR compliance or production-readiness claim is made.

See [initial verification results](docs/development/verification.md) for executed checks and remaining browser/network-related verification gaps.
