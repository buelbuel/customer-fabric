# M0 stabilization security review — 2026-11-06

Scope: local development modular monolith, synthetic data, current Go/pgx/sqlc/goose persistence, React console and Compose images. This is a focused review, not a penetration test or production certification.

## Authentication, browser and exposure

Development mode fails closed for other modes, missing/duplicate credentials and invalid source names. Credentials now accept only 32–256 URL-safe ASCII characters; generated values are random. SHA-256 digests are compared in constant time. Source attribution comes exclusively from server configuration. Body aliases such as `SET`, identifier `Source`, client-selected source fields, repeated authorization/mutation headers and ambiguous query values are rejected. Token format validation is not an entropy guarantee for manually chosen values.

Browser credentials live only in React memory, never cookies/localStorage/URLs. Reload, disconnect and 401 clear credentials and rendered data. Requests omit cookies, reject redirects, disable caching and time out. Errors distinguish unavailable/non-JSON backend responses from empty results. React renders values as text. Static assets have a restrictive same-origin CSP, no-store, nosniff, anti-framing and referrer headers. These reduce exposure; XSS, malicious extensions and a compromised host can still read an in-memory token.

Only loopback API and console ports are published; PostgreSQL is private to the Compose network. No permissive CORS headers are emitted. Browser preflight does not authorize cross-origin credentialed requests. Static nginx proxies only `/v1/` to the fixed API service, removes cookies, disables access logs and runs without root/capabilities on a read-only filesystem. Container scratch space is in-memory. Vite remains a local development tool. A production-built UI does not make the API production-ready.

No request bodies, tokens or customer values are logged by application request handlers; internal failures return generic problem responses. Nginx access logging is disabled and error logging restricted to critical failures. Health is unauthenticated but returns only status. Global request/body/field bounds limit abuse; the single-process global rate limiter is not fair per source or a production distributed control.

Remaining risks: static non-expiring bearer secrets, all-source read access, no RBAC/tenant isolation, revocation by restart, local plaintext HTTP/database transport, schema-owner DB role, no production secret manager, no tested backup/erasure workflow. Do not expose this deployment or ingest real data. ADR 0004 defines the future identity boundaries.

## Persistence and mutation review

Application SQL is parameterized and generated from reviewed SQL; domain validation remains outside generated code. Queries are bound to native pgx transactions. Mutation-key advisory locking precedes profile row locking. Hash collisions can serialize unrelated keys but cannot bypass the `(source,key)` receipt lookup. Replay checks precede optimistic version checks, preserving original receipts even after later changes. Request fingerprints include method, path, expected version and normalized mutation. Different intent with the same source/key rejects; another source has a separate key namespace.

A profile row lock serializes concurrent updates; version mismatch rejects without consuming a receipt. Unique source/namespace/value constraints serialize identifier claims across profiles. Accepted state, ownership, identifiers, provenance, receipt and outbox commit atomically. Ownership rejection deliberately commits only rejected provenance and a stable 409 receipt. Other failures roll back all changes. A failed response after commit is recoverable by replaying the unchanged key. Multi-identifier claims can deadlock in opposing order; PostgreSQL aborts one transaction safely and callers may retry the same key. No claim of deadlock-free execution is made.

Profile detail uses a repeatable-read snapshot, pagination is bounded UUID keyset pagination, provenance is capped at the latest 100 decisions, and removed attributes retain ownership tombstones. Added real-PostgreSQL tests cover replay after later updates/source scoping, concurrent identifier claims, retry after an outbox-trigger failure, and strict HTTP contract boundaries. Existing tests cover concurrency, rejection atomicity, version checks, identifier conflicts, receipts and transactional migration rollback.

The current quoted version ETag is an optimistic mutation token. Rejected provenance can change the enriched detail response without changing that version. It must not be treated as a byte-for-byte cache validator; responses are no-store and conditional GET is unsupported. A future representation-validator revision is required before enabling HTTP caching. This limitation does not weaken the row-locked mutation version check.

## Supply chain gate

Fresh frozen Bun installs and Go checksum verification remain enabled. Runtime/tool bases are pinned by digest; nginx zlib and musl are pinned to the fixed package revision. sqlc and govulncheck versions are explicit. CI adds live OpenAPI response validation, dependency audits and image scanning; remote CI has not been observed. GitHub Actions still use major-version tags, a remaining supply-chain hardening opportunity.

The follow-up PostgreSQL triage replaced the bundled gosu binary with a pinned, patched build. The local image gate passes with one documented Windows-only advisory still visible in the Linux scan. See the [PostgreSQL triage and verification](postgres-container-triage.md).
