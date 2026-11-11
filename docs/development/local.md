# Local development

Setup, daily commands, database changes, testing and pull request preparation. Use synthetic data only. Identity stacks have their own [guide](identity.md).

The supported startup is `make up`. Install Compose separately when using Colima if `docker compose version` fails. Docker daemon access is required. All persistent project data stays in Docker named volumes; source and lockfiles stay in this checkout.

`make init` generates `.env` with a password and two random source tokens (`console`, `importer`). It uses restrictive file permissions and never replaces existing credentials. Sources are registered by API startup. Use an importer's credential to test ownership conflicts; both credentials intentionally have full read access for local administration.

## Set up macOS

1. Install Apple's command line tools (`xcode-select --install`) for Git and Make. Install Python 3, Colima, Docker CLI, and the Compose v2 plugin. With Homebrew: `brew install python colima docker docker-compose`. If Docker cannot find the plugin, follow `brew info docker-compose` to configure plugin discovery while preserving existing Docker settings. Buildx is optional.
2. Start Colima with `colima start`. Verify `docker context show`, `docker info`, and `docker compose version`; select the intended local context before running a target. Put the repository and linked worktrees under a Colima mount (normally your macOS home directory). A checkout under host `/tmp` may appear empty to containers.
3. Install the ChatGPT desktop app for Codex Desktop, or install the [Codex CLI](https://learn.chatgpt.com/docs/codex/cli). Sign in to the client you use. Install GitHub CLI (`brew install gh`) and authenticate with `gh auth login`, or use GitHub in the browser with authenticated Git for pushes. Go, Bun and sqlc come from the tool containers; host installs are optional. No application API key or MCP server is required.

## Commands

Run these from the checkout root in the CLI, or choose the matching action in Codex Desktop. `scripts/` holds the implementations; use the Make targets rather than calling them directly.

| Action | Command | What it verifies or starts |
| --- | --- | --- |
| Tests | `make test` | SQL generation, real PostgreSQL Go race tests, admin type check and build |
| Contracts | `make contracts` | Starts the local API and validates live OpenAPI behavior and enterprise fixtures |
| Smoke | `make smoke` | Exercises the running local API with synthetic writes; run Local development or Contracts first |
| Local development | `make up` | Starts PostgreSQL, migrations, API and console, then prints their HTTP addresses |
| M1 local development | `make m1-up` | Starts the isolated customer identity stack |
| M1 smoke | `make m1-smoke` | Verifies real native flows using the checkout ports |
| Live M1 contracts | `make m1-linking-init`, `make m1-up`, `make m1-contracts` | Validates every customer, linking and operator operation against OpenAPI with real Kratos sessions, and fails on an undocumented status |
| Source events | `make m1-up`, `make source-events-demo` | Sends source events with curl: applied, retried, stale, reused ID, wrong source and old epoch; then typed attributes on two records, including refused invalid values |
| Customer browser | `make m1-browser` | Runs real Kratos self-service in Chromium |
| Session failure checks | `make m1-session-smoke` | Exercises expiry, issuer outage and one settings race in the current project's synthetic stack. Run it on an isolated, running M1 project: it adjusts timestamps and state only for newly created synthetic test rows and briefly stops that project's customer Kratos service |
| Settings race | `make m1-settings-race` | Repeats the concurrent email-change race in parallel and writes aggregate outcomes to `.tools/reports/settings-race.json`. `SETTINGS_RACE_ITERATIONS` and `SETTINGS_RACE_WORKERS` default to 40 and 4. Each race makes about seven API calls against the 600-per-minute development limit |
| Enterprise linking | `make m1-linking-init`, `make m1-up`, `make m1-linking-smoke` | Exercises separately credentialed issuer and customer flows |
| Customer view demo | `make m1-linking-init`, `make m1-up`, `make customer-view-demo` | Shows the governed operator unified view: attribute access policies, provenance, conflicts, restricted and undefined attributes, ambiguous records, a narrowed policy and revocation |
| Import demo | `make m1-up`, `make import-demo` | Imports 2,500 synthetic rows in batches; an oversized line stops a batch, the client resumes from the checkpoint, and malformed rows are quarantined by line and reason |
| Import benchmark | `make import-benchmark` | Imports 100,000 synthetic rows in 1,000-row batches and reports throughput, batch latency percentiles and early-versus-late batch times (about 3.5 minutes) |
| Segment demo | `make m1-linking-init`, `make m1-up`, `make segment-demo` | Ingests three synthetic records, links verified customers, records their own consent, defines a segment and previews it; a withdrawal removes a member |
| Activation demo | `make m1-linking-init`, `make m1-up`, `make activation-demo` | Runs Kratos customer → verified association → consent → source event → governed view → segment → signed test webhook (the `webhook-sink` service); a withdrawal before dispatch suppresses delivery and a replay no longer selects the customer |
| Event transport and workflows (opt-in) | `make events-test`; `make events-up` | Starts a local single-node Kafka broker and a Temporal development server on an internal network. `events-test` runs the relay tests and the end-to-end workflow test against them; `events-up` runs the API with the relay and the profile-change workflow; notifications go only to destinations named by an active notification subscription (`/v1/operator/notification-subscriptions`) on an allowlisted host, under the webhook egress policy. `make events-scan` scans both images; `make events-down` stops them. Never part of `make up` or `make test` |
| Named operators | `make operator-up`, `make operator-smoke`, `make operator-browser` | Starts the separate issuer and verifies MFA and read access |
| Security | `make security`, `bash scripts/scan-images.sh` | Dependency checks, then an image scan after building images. Reports stay in `.tools/security`; the scan fails on high or critical findings, including unfixed ones |
| Stop | `make down` | Stops the current checkout's services and preserves named volumes |

## Worktrees

Check `git status --short --branch`, `git worktree list`, and `gh pr status`. Confirm the base and head; extend an existing PR only when the task belongs to it. Leave unrelated edits in their original checkout.

In Desktop, choose a Worktree chat based on the intended branch. In the CLI, create a linked checkout and launch Codex there:

```sh
git fetch origin
# Set BASE to a ref containing the workflow integration.
git worktree add -b codex/short-task ../customer-fabric-short-task "$BASE"
cd ../customer-fabric-short-task
make init
codex
```

Select the checked-in **Customer Fabric** [local environment](https://learn.chatgpt.com/docs/environments/local-environment) in Desktop. Its setup script runs `make init` in the new worktree. It creates fresh, ignored development credentials with mode 0600 and leaves an existing file alone. Keep the setup script small; run the checks as actions. Never copy `.env`, tokens, or data from another checkout.

For linked worktrees, Make scopes the Compose project and its PostgreSQL volume to the checkout path. Docker assigns available loopback ports for the API and both admin services; run `make ports` with the development stack up to see its addresses. The primary checkout keeps its usual ports and project. PostgreSQL has no host port. Use Make targets from worktrees: raw Compose commands in CI use the primary project defaults. Run `make down` before moving, removing, or archiving a checkout; the project name is path based. A retained database volume requires the original local database password.

## Run the API with host Go

Start PostgreSQL with a local override exposing **only** `127.0.0.1:5432` if a host process needs access. The default deliberately exposes no DB port. Alternatively use an independently managed local PostgreSQL database.

```sh
set -a
. ./.env
set +a
export CF_MODE=development
export DATABASE_URL="postgres://fabric:${POSTGRES_PASSWORD}@127.0.0.1:5432/fabric?sslmode=disable"
cd backend
go run ./cmd/api migrate
go run ./cmd/api
```

Default API bind address is `127.0.0.1:8080`. Compose explicitly uses `0.0.0.0:8080` inside its container while publishing only host loopback. `CF_LISTEN` overrides the process address. Never use this to expose development administration on a shared network.

In `web/admin`, `bun run dev --host 127.0.0.1` starts the host frontend. Its same-origin `/v1` proxy targets `http://127.0.0.1:8080`; the container uses `API_URL=http://api:8080`. There are no external fonts or analytics. The frontend is a read-only technical console; curl performs mutations.

## Configuration and operations

- `CF_MODE`: must be exactly `development`; unset/production values fail startup.
- `CF_DEV_TOKENS`: JSON object from source name to distinct token, 32–256 characters each. Never put it in Vite's client environment.
- `DATABASE_URL`: pgx PostgreSQL connection string. Local transport uses no TLS; secure deployments need TLS and a new production-auth design.
- `CF_LISTEN`: API bind address.
- `TEST_DATABASE_URL`: integration-test DB; tests create and remove only uniquely named test schemas.

Database errors and tokens are not logged. Startup errors are intentionally generic to avoid printing connection strings. Check database health, migration completion, and config presence without printing secrets. `/healthz` is unauthenticated liveness only, not database readiness.

Runtime credentials are environment-based for local simplicity and visible to Docker administrators. Production secret stores, role separation and rotation are deferred. Build dependency downloads contact Go/npm/container registries; running the application does not require them after images are built.

The Compose console bind-mounts `web/admin`, runs a frozen Bun install, and supports source hot reload. `web/admin/Dockerfile` builds the static `make production-ui` image. Docker Buildx is optional for these simple Dockerfiles. The classic builder can reuse layers on a multi-service `--no-cache` build, so use a direct `docker build --no-cache` to prove a fresh install. It also rejects the cross-platform stage copy in `deploy/postgres/Dockerfile`; CI covers native AMD64.

## Database changes

Use PostgreSQL, pgx/v5, pgxpool, sqlc and goose. Domain rules stay in `internal/profiles`; SQL belongs in `backend/queries`. Generated code in `backend/internal/platform/db` is an adapter detail, never a domain dependency.

### Change a query

1. Edit a named SQL query in `backend/queries/profiles.sql` (or add a focused query file).
2. Run `make generate` using pinned sqlc 1.31.1.
3. Update the platform adapter to use the generated parameter/result types.
4. Run `make test` against real PostgreSQL and commit SQL plus generated code together.

`make check-generated` runs `sqlc diff`, which fails when generated output is stale. CI repeats this check. Hand-written copies of generated methods are not accepted. String UUID and time.Time overrides keep conversion at the persistence boundary small; database constraints still enforce UUID validity. JSONB remains raw JSON at the query boundary and is decoded by the adapter.

### Change the schema

Add the next numbered migration, for example `backend/migrations/002_profile_example.sql`, with explicit `-- +goose Up` and `-- +goose Down` sections. Follow goose's `StatementBegin`/`StatementEnd` syntax for procedural SQL. Do not modify applied migrations or use runtime schema auto-sync. sqlc reads these same migration files and ignores Down sections.

Run `make generate`, review both SQL and generated changes, and add tests for the new constraints or behavior. `make test` creates isolated PostgreSQL schemas and exercises real migrations. `make migrate` builds and runs the application migration command against the local database. It applies only pending Up migrations; existing migrations are not replayed.

Goose runs migrations in transactions by default, serializes runners with a PostgreSQL advisory lock and records versions in `goose_db_version`. The embedded runner discovers new files without code changes. Do not add `NO TRANSACTION` without a documented requirement and a failure/recovery plan. Native application pgx transactions must wrap all related profile, provenance and outbox writes, with generated queries bound using `WithTx(tx)`.

### Clean pre-release baseline

`make migrate` initializes new databases with goose alone. Obsolete development schemas are unsupported and fail if tables conflict with the baseline. No migration command resets data or Docker volumes. Databases already using the current goose baseline require no reset. Manual schema drift is never hidden with `CREATE TABLE IF NOT EXISTS`.

The initial Down migration deletes all profile tables. It is exercised only in isolated test schemas and is not exposed as an application command. Do not use it against retained data.

See [ADR 0002](../adr/0002-sql-first-persistence.md) for locking and runtime boundaries. Official references: [sqlc pgx support](https://docs.sqlc.dev/en/latest/guides/using-go-and-pgx.html), [goose provider](https://pressly.github.io/goose/documentation/provider/).

## Testing

`make test` first checks that sqlc output matches the committed SQL, then starts PostgreSQL, runs Go tests with the race detector, then installs the locked frontend dependencies, type-checks and builds the console. A C toolchain is available in the Go tool image (Alpine with gcc and musl-dev) for the race detector. `go test ./...` without `TEST_DATABASE_URL` skips integration tests; this is not database verification.

Integration tests create a random isolated PostgreSQL schema, apply migrations twice to check repeatability, and drop that schema during cleanup. They never truncate the application schema. The test role needs schema creation permission. Use a disposable development database, never production.

Covered invariants: creation/retrieval/update, invalid payloads, unauthorized access, immutable source attribution, identifier uniqueness and source scoping, optimistic concurrency, sequential and concurrent idempotency, mutation key conflicts, ownership rejection and tombstones, provenance consistency, keyset pagination, and rollback when an injected outbox trigger fails. Accepted state and outbox must commit together. Rejected ownership attempts are audited once and do not emit outbox records.

Unit tests exercise scalar validation, ownership removal checks, fail-closed configuration, duplicate JSON keys, and authorization before storage access. TypeScript checking and a production frontend build verify the console source; they do not replace browser testing.

Dependency review: `bun audit` for frontend packages and `govulncheck ./...` for Go when that tool is available. Container CVE review and production hardening are separate release gates. The CI workflow repeats core checks against PostgreSQL.

Migration integration tests additionally cover fresh initialization and repeated application, rejecting unexpected existing schemas without data destruction, concurrent migration runners, Down/Up in a disposable schema, and transactional rollback of a failing migration. These run against PostgreSQL, not a substitute database.

## Keep core documents current

Review this table for every PR and change the document in the same PR as the behavior. Accepted ADRs are historical records: add a new one rather than rewriting them. Verification evidence belongs in the pull request and its GitHub Actions run.

| When you change | Update |
| --- | --- |
| Routes, credentials or audiences | `contracts/openapi/customer-fabric.yaml`, `docs/architecture/overview.md` trust boundaries, `docs/security/eu-first.md` |
| Tables or retained data categories | `docs/architecture/overview.md` data model; ADR 0003 category policy through a new ADR if the policy changes |
| Compose services, images or identity configuration | `docs/development/identity.md` and the open findings in `docs/security/eu-first.md` |
| Milestone scope or sequencing | GitHub issue #8 and `docs/architecture/roadmap.md` |

## Commit and prepare a PR

Run `make test` and the checks relevant to the change. Run `make generate` for SQL edits and include the generated output; use the repository skills for database, security, fixtures and feature delivery. Inspect the staged diff, stage only relevant paths or hunks, and use `git commit -s` with one logical change per commit. Get an independent review against the intended base, such as a separate Codex review or a teammate.

When ready to publish, push the named branch and open a draft against the agreed base. Explain the behavior, checks actually run, skipped checks and any migration impact. GitHub Actions is authoritative: inspect the backend formatting, sqlc, PostgreSQL race tests, fixtures, vet, module verification, vulnerability and build checks; admin type checking, build and audit; the fresh image builds, live contracts, audits and image scans; and the customer and operator identity jobs. Use the run for the PR's current head (for example, `gh pr checks --watch`). Leave merging to the maintainer.

## Webhook egress

Every webhook (activation deliveries and change notifications) needs an `https` URL on a `CF_WEBHOOK_ALLOWED_HOSTS` host:port. Each connection's actual address must be public unicast, checked when the socket opens. The M1 stack's local sink is a container on a private network reached over http. Only that stack therefore sets `CF_WEBHOOK_ALLOW_PRIVATE_NETWORKS=true`, which adds loopback, private and shared address space but never link-local or metadata addresses, and `CF_WEBHOOK_ALLOW_PLAINTEXT=true`. Never set either outside local development.
