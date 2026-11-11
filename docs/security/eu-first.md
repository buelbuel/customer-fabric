# EU-first security and privacy boundary

**Status:** M0 and M1 are synthetic-data development milestones. EU-first is a design goal and deployment responsibility, not a data-residency or GDPR-compliance guarantee.

Customer Fabric runs independently, without cloud registration or mandatory telemetry. Runtime data stays in PostgreSQL, the isolated Kratos databases, the local mail sink and the operator's browser. Dependency installation and container pulls contact public registries at build time. Operators choose where machines, volumes and backups are located; software alone cannot establish residency or GDPR compliance.

## Current threat model

| Threat | Implemented control | Remaining boundary |
| --- | --- | --- |
| Unauthenticated profile access | Credentials required on all `/v1` routes; non-development modes refuse startup | In the M0 stack all configured sources read all profiles; no production RBAC |
| Audience confusion | Customer, source, linking-issuer and operator credentials are distinct and route-scoped; customer sessions are rejected elsewhere | Static development bearers do not expire |
| Customer account takeover | Kratos verification, recovery and online session checks; profile access requires the current email to be verified | Loopback HTTP and captured mail. A concurrent settings race can [retain a stale recovery address](../development/identity.md#concurrent-settings) usable for account takeover by a holder of a privileged session |
| False account linking | Issuer-attested, single-use, hashed, expiring grants bound to subject and record lifetime; no email matching | Synthetic proof authority only; real proof adapters and revocation channels are future work |
| Operator misuse | Separate Kratos issuer, AAL2 TOTP and an admin-managed read role in the operator overlay | Reads through the overlay produce a log line, not a retained record, and the development bearer path logs none; Docker access is the bootstrap and break-glass boundary |
| Source impersonation | Credential-to-source mapping at startup; payload source rejected | Compromise of a credential grants that source's privileges |
| Lost updates and retry duplicates | Row locking, version precondition, transactional idempotency | Clients must retain keys and reconcile uncertain outcomes |
| Cross-source overwrite or resurrection | First-writer ownership and persistent tombstones | Owner's fresh requests can restore values; no event-time semantics |
| SQL injection | Parameterized application SQL | DB administrator remains trusted |
| Ambiguous/unbounded requests | Duplicate JSON key rejection, strict fields, limits and timeouts | Global development limiter is not a production DoS defense |
| Unnecessary PII copies | No request/body/token logs; receipts and outbox exclude values | Attribute names, IDs, sources, hashes and timestamps can still be personal metadata |
| Public local services | Loopback-only published ports; no database host port | Local host users and Docker access are trusted |
| Production-built console exposure | Static nginx proxies only `/v1/` to the fixed API service, removes cookies, disables access logs and runs without root or capabilities on a read-only filesystem | A production-built UI does not make the API production-ready |
| Browser credential persistence | Console token in memory; browser references use Kratos cookies, CSRF nodes and exact-origin checks; no CORS enabled | Browser extensions and local XSS can access in-memory data; cookies are not `Secure` on loopback HTTP |

Use synthetic data only. No automated erasure, retention, data export, audit expiry, backup deletion, or downstream deletion exists yet. PostgreSQL volume deletion is not a claim of secure physical erasure. Removing current values does not remove backup copies. Define purpose, retention periods, access policies, backup lifecycle, incident handling, and legal responsibilities before processing real customer data.

The outbox currently grows without a consumer. Rejected attribute attempts record source, field, mutation ID, timestamp and rule, without attempted values. Other errors do not persist failure audit records. Request hashes are unkeyed digests; they are not anonymization and may permit guessing low-entropy inputs with database access. Credentials reside in an ignored mode-0600 local `.env`, and Docker administrators can inspect container environments. Credential format validation is not an entropy guarantee for manually chosen values.

Compose is a local development configuration, not a production deployment. It uses one DB owner role and unencrypted connections inside the Docker network. Production requires real admin authorization, credential lifecycle, TLS, database role separation, dependency vulnerability review, retention/deletion controls, tested restore, and operational access auditing. Do not expose Vite, PostgreSQL or development tokens publicly. Kratos administrative ports must remain private. The pinned Kratos image has open critical and high findings, and both Kratos services run with `--dev`; neither is acceptable for production.

## Open findings and production blockers

Scan results are point-in-time and not proof of absence of vulnerabilities. CI uploads the current full reports as artifacts on every run, and `bash scripts/scan-images.sh` writes them to ignored `.tools/security/` locally. No severity exclusions or CVE suppressions are used.

### Before real data

- Reviewed upstream Kratos and PostgreSQL images, with Kratos run without `--dev`.
- Verified real source authorities. The synthetic source authority and local operator bootstrap are development mechanisms.
- TLS, proper secrets and database role separation. Development bearers do not expire, revocation requires a restart, and the database role owns its schema.
- Scoped source credentials with expiry, immediate revocation and audited rotation. In the M0 stack all sources can read all profiles, and there is no RBAC or tenant isolation.
- Monitored backups, restore drills, tested erasure and approved retention and audit policy. [ADR 0003](../adr/0003-data-lifecycle.md) is a design, not executed disaster recovery.
- Issue [#70](https://github.com/buelbuel/customer-fabric/issues/70) resolved: concurrent email changes can [retain a stale recovery address](../development/identity.md#concurrent-settings).

### Kratos image

On 2026-11-07, Trivy 0.76.1 on Linux ARM64 reported two critical and 59 high findings in the pinned official Kratos distroless image. This variant removes the two additional critical OS findings from the Alpine variant. The remaining critical findings affect server-side PostgreSQL protocol handlers in Kratos's pgx dependency. The local setup uses Kratos as a PostgreSQL client on a private network, and the affected handlers have no demonstrated path in this flow. The findings remain open and block production use. Mailpit reported zero critical or high findings. The existing Customer Fabric runtime image gate passed separately. [Go advisory for CVE-2026-33815](https://pkg.go.dev/vuln/GO-2026-4771) · [Go advisory for CVE-2026-33816](https://pkg.go.dev/vuln/GO-2026-4772)

The 2026-11-07 review found v26.2.0 is still the latest published upstream release. Its tagged Go module declares pgx/v5 v5.7.5. No newer released image was identified for a supported upgrade. The pinned distroless image and its findings stay visible in the identity CI artifact. Re-evaluate on a new upstream release, changes to database reachability or any proposal to accept real identities. Review more than the two critical findings before approving a production deployment. Re-derive the `make m1-settings-race` outcome classifier whenever the Kratos pin changes.
[Upstream releases](https://github.com/ory/kratos/releases) · [Tagged dependencies](https://github.com/ory/kratos/blob/v26.2.0/go.mod)

### PostgreSQL image

Compose builds `customer-fabric-db` from `postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395` and replaces only the bundled `gosu` binary. This is a temporary security patch. Return to a maintained official image once a candidate passes the same scan, fresh startup, existing volume, migration and API checks.

The official image's gosu 1.19, built with Go 1.24.6, produced 61 findings including 1 critical and 25 high. The Alpine package scan found zero, and PostgreSQL handles SQL traffic only after gosu has dropped privileges and replaced itself. Reachability was assessed but the gate was not waived, so [`deploy/postgres/Dockerfile`](../../deploy/postgres/Dockerfile) rebuilds gosu from pinned source with Go 1.27.4. The published alternatives did not clear the findings: all four official PostgreSQL 18.6 variants bundled the same helper, the gosu 1.20 release binary still used Go 1.24.6, and Alpine's `gosu-1.19-r6` used Go 1.26.9 with the older user parser. Debian variants also added OS findings, and switching would require verifying collation, extension ABI and volume ownership. The official image builds PostgreSQL from source, so APK inventory cannot verify the engine; check its version against [PostgreSQL's advisories](https://www.postgresql.org/support/security/18/).

The rebuilt image reports one finding, which remains visible in scans: CVE-2026-39824 ([GO-2026-5024](https://pkg.go.dev/vuln/GO-2026-5024)) in `golang.org/x/sys` v0.1.0, severity UNKNOWN, fixed in v0.44.0. The affected `windows.NewNTUnicodeString` function is Windows-only and both the deployment and the gosu build target Linux. Re-evaluate it at the next PostgreSQL or gosu refresh, if the advisory expands to Linux, or if build targets change. Review the image pins and advisories on each PostgreSQL 18 security release, on upstream gosu or Go releases, and whenever a new high or critical finding appears.

### Supply chain

GitHub Actions still use major-version tags rather than pinned commits, which is a remaining hardening opportunity. Dependency maintenance is described in [dependencies](../development/dependencies.md).
