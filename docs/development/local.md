# Local development

The supported startup is `make up`. Install Compose separately when using Colima if `docker compose version` fails. Docker daemon access is required. All persistent project data stays in Docker named volumes; source and lockfiles stay in this checkout.

`make init` generates `.env` with a password and two random source tokens (`console`, `importer`). It uses restrictive file permissions and never replaces existing credentials. Sources are registered by API startup. Use an importer's credential to test ownership conflicts; both credentials intentionally have full read access for local administration.

## Individual container commands

Run from the repository root:

```sh
docker compose --env-file .env -f deploy/compose.yaml up -d --wait db
docker compose --env-file .env -f deploy/compose.yaml run --rm migrate
docker compose --env-file .env -f deploy/compose.yaml up -d api admin
```

Goose executes each migration transactionally under a PostgreSQL advisory lock and records versions in `goose_db_version`. Do not edit an applied migration: add the next numbered SQL file with goose Up/Down annotations. The embedded runner discovers new files without code changes. The initial Down migration is destructive and is tested only in isolated disposable schemas; the application command exposes Up only. Existing unmodified M0 databases are adopted without replaying schema creation or changing customer data. See [database development](database.md).

## Running host Go

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

The Compose console bind-mounts `web/admin`, runs a frozen Bun install, and supports source hot reload. Its optional Dockerfile is an alternative standalone image recipe; fresh network installation of that image was blocked during initial verification (see verification.md). Docker Buildx is optional for these simple Dockerfiles; the classic builder was used successfully for the API when Buildx was absent.
