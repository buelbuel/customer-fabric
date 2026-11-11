# Customer Fabric

**Identity. Consent. Customer Data. One coherent platform.**

An Apache-2.0, EU-first, headless customer identity (CDC) and customer data (CDP) platform. **It runs today as a synthetic-data development build. It is not production-ready.**

**Works today:** the profile API with first-writer attribute ownership, provenance, idempotent writes and an outbox record (M0); Kratos customer registration, login and recovery with a stable customer profile; verified links between a customer and source records; and a separate operator login with TOTP (M1).

**Works today, for consent (M2):** operators manage purposes and policy versions, and verified customers view, grant and withdraw their own consent through the API or the local reference page. Decisions are append-only evidence that drives effective-permission evaluation. A source system can report consent it collected about its own records, including unlinked ones, only under a vetted consent authority a privacy admin approved, with proof of each grant and a receipt signed by a registered source key ([ADR 0015](docs/adr/0015-source-consent.md), [ADR 0017](docs/adr/0017-source-consent-trust.md)); opt-in flags in source data and operator entries never count as consent. Such a grant is the source's attestation: independent evidence verification is not implemented yet and blocks any real-data use.

**Works today, for source events:** an authenticated source sends a change to its own record through `POST /v1/source-events`, with idempotent retries and deterministic stale-event handling. `make source-events-demo` shows it with curl.

**Works today, for imports:** a source imports its own historical records as ordered NDJSON or CSV batches (`/v1/imports`; CSV through an explicit column mapping) through the same typed, ownership-checked write path. Each row commits with a checkpoint, so a client resumes exactly after a failure. Invalid rows are quarantined by line and reason code without storing content, and rows never overwrite a record that already received newer source events. `make import-demo` shows it.

**Works today, for segments:** operators define typed, versioned segment rules over governed attributes (`/v1/operator/segments`). Every segment names a consent purpose, and a live preview lists only customers who permit it, explaining each match by provenance and never by value. Rules are data, never SQL. A preview evaluates at most 1,000 customers in one snapshot. Operators can also store immutable membership snapshots (anchor IDs only) and compare two of them to see who joined and who left. `make segment-demo` shows it.

**Works today, for activation:** an operator registers a test webhook on an allowlisted host (`CF_WEBHOOK_ALLOWED_HOSTS`) and activates a segment. Members are queued in PostgreSQL, and immediately before each send the dispatcher re-checks consent, lifecycle, membership, segment governance and the destination. It sends only attributes granted to the `activation` reader, signs each delivery with HMAC, retries transient failures a bounded number of times with a stable idempotency key, and records payload-free receipts. Operators can redrive failed deliveries, and each redrive passes every gate again. `make activation-demo` runs the full chain: a verified customer withdrawing before dispatch receives nothing. For populations too large to evaluate inside a request, snapshots and activations can be submitted as durable audience jobs (`/v1/operator/segments/{name}/snapshot-jobs` and `/activation-jobs`): a worker in the API process decides every customer from one database state and publishes the result atomically, a failed or interrupted attempt publishes nothing and is retried from the start, and deliveries are still re-checked before every send.

**Opt-in, synthetic only:** an outbox relay that publishes every committed profile change to Apache Kafka as an envelope of opaque references, at least once and in order per profile, and a consumer that turns each event into a Temporal workflow. The workflow re-checks the record and notifies a registered destination, once per event (`make events-up`, `make events-test`; [ADR 0013](docs/adr/0013-event-transport.md), [ADR 0014](docs/adr/0014-temporal-workflows.md)). Notifications are at least once where authorized, not a lossless change stream: retiring a policy version interrupts its subscriptions after an acknowledged impact preview, and what they missed is sent only after explicit reapproval and a recovery request, each change authorized again ([ADR 0016](docs/adr/0016-policy-change-notification-recovery.md)). Kafka and Temporal never start with `make up` or `make test`.

**Planned, not running:** connector-specific receivers, event deletes, vendor-specific destinations, further Temporal workflows and production Kafka/Temporal deployment (target architecture, see below), and hosted Cloud tenancy.

## Run locally

Requirements: Docker Engine with Docker Compose v2 (Docker Desktop or Colima plus the Compose plugin), Python 3 and Make. Go and Bun can run in the provided tool containers; host installations are optional.

```sh
make init   # creates ignored .env with random secrets, never overwrites it
make up     # PostgreSQL, migration, API, React development console
```

Open the console address printed by `make up` or `make ports` (normally http://127.0.0.1:5173). Copy the `console` token from your local `.env` into the connection form. Tokens stay in browser memory. **Use synthetic data only.** The primary checkout API defaults to http://127.0.0.1:8080. Only these loopback ports are published; PostgreSQL has no host port. No cloud registration or outbound runtime telemetry is required.

`make down` stops services and preserves database data. `make logs` shows sanitized API lifecycle messages. Never use `down -v` unless you intend to delete local database volumes. If `.env` is lost while a volume remains, restore its database password rather than generating a new incompatible one.

The API requires `CF_MODE=development` and configured source credentials. Keep the Vite console and development API on loopback and use synthetic data.

For the customer identity slice, run `make m1-up` and `make m1-smoke`. This uses separate M1 profile and Kratos database volumes. The operator console uses the operator credential from `.env.m1`. Open the printed customer identity URL for the browser reference. For named MFA operators, use the separate `make operator-up` overlay. See the [identity guide](docs/development/identity.md).

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

Attributes support **top-level non-null scalars only**. Arrays, nested objects and full replacement fail explicitly. An operator can give an attribute a type through `/v1/operator/attributes` (string, integer, decimal, boolean, date, timestamp or enum); every later write of it is validated and its provenance records the definition revision. An access policy on a defined attribute decides which readers (operator view, segmentation, activation) may see it and from which sources; undefined or unpolicied attributes are never disclosed. First writer owns a field; its removal retains ownership. The same owner can explicitly restore it. Identifier association is add-only, scoped by credential source and namespace. No automatic matching or merging by email occurs.

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

See [local development](docs/development/local.md) for setup, worktrees, host Go, database changes, testing and PR preparation.

## Scope and documentation

- [Architecture](docs/architecture/overview.md) and [initial ADR](docs/adr/0001-architecture.md)
- [Cloud architecture — shared management and tenant-isolated data](docs/architecture/cloud-control-plane.md)
- [Kafka event backbone and Temporal workflows — target design](docs/architecture/eventing-workflows.md)
- [GitHub product roadmap](https://github.com/buelbuel/customer-fabric/issues/8), [Cloud epic](https://github.com/buelbuel/customer-fabric/issues/90)
- [REST OpenAPI 3.1 contract](contracts/openapi/customer-fabric.yaml)
- [Outbox schemas](contracts/events): durable records only; **no delivery worker**
- [Security, privacy and EU-first limitations](docs/security/eu-first.md)
- [Roadmap](docs/architecture/roadmap.md), [local development](docs/development/local.md), [identity flows](docs/development/identity.md), [dependencies](docs/development/dependencies.md)
- [Contribution policy](CONTRIBUTING.md), [security reporting](SECURITY.md)

Missing production controls include operational hardening for named operator access, TLS, credential lifecycle, least-privilege DB roles, automated retention, export/erasure, backup lifecycle and tested disaster recovery. No GDPR compliance or production-readiness claim is made.

See the [open findings](docs/security/eu-first.md#open-findings-and-production-blockers) for remaining security gates.

Additional commands: `make production-ui` serves the static build at http://127.0.0.1:5174;
`make contracts` validates schemas and real API responses; `make security` runs dependency checks.
After building images, `bash scripts/scan-images.sh` saves full reports and fails on high/critical findings.
See the [data lifecycle ADR](docs/adr/0003-data-lifecycle.md) and
[authentication boundaries ADR](docs/adr/0004-authentication-boundaries.md).

## Synthetic enterprise fixtures

`make fixtures` generates a deterministic 100-person regression dataset; use
`FIXTURE_PRESET=medium` (10,000) or `FIXTURE_PRESET=large` (100,000) for larger runs.
Outputs stay in ignored `.tools/` and existing output directories are never overwritten.
The generator cannot connect to or reset a database. See the
[fixture library](tests/fixtures/enterprise/README.md) and
[generation/replay guide](tests/generator/README.md).
