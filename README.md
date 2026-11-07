# Customer Fabric

**Identity. Consent. Customer Data. One coherent platform.**

An Apache-2.0, EU-first customer infrastructure platform. M0 provides the development profile API, PostgreSQL persistence and React operations console. The first M1 slice adds native Kratos registration, email verification, login and access to a dedicated customer profile. Consent, background delivery and cloud services are future milestones.

## Run locally

Requirements: Docker Engine with Docker Compose v2 (Docker Desktop or Colima plus the Compose plugin), Python 3 and Make. Go and Bun can run in the provided tool containers; host installations are optional.

```sh
make init   # creates ignored .env with random secrets, never overwrites it
make up     # PostgreSQL, migration, API, React development console
```

Open the console address printed by `make up` or `make ports` (normally http://127.0.0.1:5173). Copy the `console` token from your local `.env` into the connection form. Tokens stay in browser memory. **Use synthetic data only.** The primary checkout API defaults to http://127.0.0.1:8080. Only these loopback ports are published; PostgreSQL has no host port. No cloud registration or outbound runtime telemetry is required.

`make down` stops services and preserves database data. `make logs` shows sanitized API lifecycle messages. Never use `down -v` unless you intend to delete local database volumes. If `.env` is lost while a volume remains, restore its database password rather than generating a new incompatible one.

The API requires `CF_MODE=development` and configured source credentials. Keep the Vite console and development API on loopback and use synthetic data.

For the customer identity slice, run `make m1-up` and `make m1-smoke`. This uses separate M1 profile and Kratos database volumes. The operator console uses the operator credential from `.env.m1`. Open the printed customer identity URL for the browser reference. For named MFA operators, use the separate `make operator-up` overlay. See the [browser and native flows](docs/development/customer-identity.md) and [operator setup](docs/development/operator-identity.md).

## Use the API

The following example uses the M0 compatibility stack. In M1, integration credentials may create profiles and update profiles that they created. Operator credentials provide read access.

Use the API address from `make ports` for this checkout and load its generated configuration without printing credentials:

```sh
export CF_API_URL='http://127.0.0.1:8080'  # replace with this checkout's API address
set -a
. ./.env
set +a
export CF_TOKEN="$(python3 -c 'import json,os; print(json.loads(os.environ["CF_DEV_TOKENS"])["console"])')"

curl -i "$CF_API_URL/v1/profiles" \
  -H "Authorization: Bearer $CF_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: synthetic-create-0001' \
  --data '{"set":{"display_name":"Synthetic Ada","active":true},"identifiers":[{"namespace":"crm","value":"synthetic-001"}]}'
```

Creation returns a receipt `{id, version, mutation_id}`, a `Location`, and an ETag such as `"1"`. Set `PROFILE_ID` to the returned ID:

```sh
export PROFILE_ID='replace-with-returned-id'
curl -i "$CF_API_URL/v1/profiles/$PROFILE_ID" -H "Authorization: Bearer $CF_TOKEN"
curl "$CF_API_URL/v1/profiles?limit=20" -H "Authorization: Bearer $CF_TOKEN"
curl -i -X PATCH "$CF_API_URL/v1/profiles/$PROFILE_ID" \
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

For host Go 1.26+ and Bun 1.4.4+:

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

See the [macOS and Codex workflow](docs/development/workflow.md) for isolated worktrees and PR preparation. See [local development](docs/development/local.md) for running the API with host Go and a private local database, and [testing](docs/development/testing.md) for test isolation and verification limits.

## Scope and documentation

- [Architecture](docs/architecture/overview.md) and [initial ADR](docs/adr/0001-architecture.md)
- [REST OpenAPI 3.1 contract](contracts/openapi/customer-fabric.yaml)
- [Outbox schemas](contracts/events): durable records only; **no delivery worker**
- [Security, privacy and EU-first limitations](docs/security/eu-first.md)
- [Roadmap](docs/architecture/roadmap.md), [dependencies](docs/development/dependencies.md)
- [Contribution policy](CONTRIBUTING.md), [security reporting](SECURITY.md)

Missing production controls include operational hardening for named operator access, TLS, credential lifecycle, least-privilege DB roles, automated retention, export/erasure, backup lifecycle and tested disaster recovery. No GDPR compliance or production-readiness claim is made.

See [initial verification results](docs/development/verification.md) for executed checks and remaining security gates.


Stabilization commands: `make production-ui` serves the static build at http://127.0.0.1:5174;
`make contracts` validates schemas and real API responses; `make security` runs dependency checks.
After building images, `bash scripts/scan-images.sh` saves full reports and fails on high/critical findings.
See the [M0 security review](docs/security/m0-review.md),
[data lifecycle ADR](docs/adr/0003-data-lifecycle.md) and
[authentication boundaries ADR](docs/adr/0004-authentication-boundaries.md).

## Synthetic enterprise fixtures

`make fixtures` generates a deterministic 100-person regression dataset; use
`FIXTURE_PRESET=medium` (10,000) or `FIXTURE_PRESET=large` (100,000) for larger runs.
Outputs stay in ignored `.tools/` and existing output directories are never overwritten.
The generator cannot connect to or reset a database. See the
[fixture library](tests/fixtures/enterprise/README.md) and
[generation/replay guide](tests/generator/README.md).
