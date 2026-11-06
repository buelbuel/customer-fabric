# Initial increment verification

Verified locally on 2026-11-05 using Colima, Linux ARM64 containers, Go 1.27.3, PostgreSQL 18, and Bun 1.3.3. Host Go and Bun were not installed by this task.

## Passed

- Go formatting, `go test ./...`, `go vet ./...`, and `go build ./cmd/api` in the Go container.
- `go test -race -count=1 -v ./...` with a real Compose PostgreSQL database: five unit tests and nine integration tests passed; no integration skips in this run.
- Versioned migrations applied successfully, including repeat application in tests and the Compose migration service.
- Minimal non-root API Docker image built and started successfully.
- Live loopback HTTP smoke test: unauthenticated rejection, create, exact replay, PATCH, retrieval, listing, identifiers and provenance. One synthetic profile remains for exploration. Re-run with `python3 scripts/smoke.py`.
- Frontend frozen install, TypeScript check, and production build.
- `bun audit` after updating Vite from 7.2.2 to 8.3.6: no vulnerabilities reported. The first audit found advisories in the old version; it was replaced.
- OpenAPI YAML parsing and resolution of all local `$ref` targets; outbox schema JSON parsing. This is not a full external OpenAPI conformance test.
- Local console returns HTTP 200 and its connection screen renders correctly in the browser.
- `git diff --check`; no remaining zero-byte scaffold files outside dependencies/Git.

## Limits and unresolved checks

- Authenticated browser listing/detail interactions were not exercised. The live API data path was verified separately; the React UI uses that same API without mocks.
- The Go vulnerability checker could not complete: public-module downloads failed with DNS, connection and truncated-download errors. No clean Go vulnerability result is claimed.
- The optional standalone frontend Dockerfile build did not complete: fresh tarball downloads failed integrity checks, then connection errors on retry. Integrity verification was not disabled. The supported Compose development console uses a bind-mounted source tree, a frozen Bun install and the verified installed dependency set.
- A completely clean-cache installation under reliable networking and a full authenticated browser walkthrough are still required before declaring every M0 acceptance criterion verified.
- Container vulnerability scanning, backup restoration and production security validation were not performed. CI is configured but has not run remotely.

## Local state

The Compose `db`, `api` and `admin` services are running, with API and console published only on host loopback. An extra standalone database used during setup was stopped; its volume is preserved. Secrets are in ignored mode-0600 `.env` and were not printed. No Go/Bun toolchain was installed on the host. An initial Go release metadata download to `/tmp` happened before the user's instruction to avoid temporary tooling; it was not used, and no toolchain was downloaded there. Subsequent tooling ran in Docker.

## Next step

Complete the authenticated console walkthrough and clean-cache dependency/security checks, then establish production administrative authentication and an explicit retention/erasure design before using real customer data or starting broader identity work.
