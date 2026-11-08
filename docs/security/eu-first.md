# EU-first security and privacy boundary

**Status:** M0 and M1 are synthetic-data development milestones. EU-first is a design goal and deployment responsibility, not a data-residency or GDPR-compliance guarantee.

Customer Fabric runs independently, without cloud registration or mandatory telemetry. Runtime data stays in PostgreSQL, the isolated Kratos databases, the local mail sink and the operator's browser. Dependency installation and container pulls contact public registries at build time. Operators choose where machines, volumes and backups are located; software alone cannot establish residency or GDPR compliance.

## Current threat model

| Threat | Implemented control | Remaining boundary |
| --- | --- | --- |
| Unauthenticated profile access | Credentials required on all `/v1` routes; non-development modes refuse startup | In the M0 stack all configured sources read all profiles; no production RBAC |
| Audience confusion | Customer, source, linking-issuer and operator credentials are distinct and route-scoped; customer sessions are rejected elsewhere | Static development bearers do not expire |
| Customer account takeover | Kratos verification, recovery and online session checks; profile access requires the current email to be verified | Loopback HTTP and captured mail. A concurrent settings race can [retain a stale recovery address](../development/customer-identity.md#concurrent-settings) |
| False account linking | Issuer-attested, single-use, hashed, expiring grants bound to subject and record lifetime; no email matching | Synthetic proof authority only; real proof adapters and revocation channels are future work |
| Operator misuse | Separate Kratos issuer, AAL2 TOTP and an admin-managed read role in the operator overlay | Reads through the overlay produce a log line, not a retained record, and the development bearer path logs none; Docker access is the bootstrap and break-glass boundary |
| Source impersonation | Credential-to-source mapping at startup; payload source rejected | Compromise of a credential grants that source's privileges |
| Lost updates and retry duplicates | Row locking, version precondition, transactional idempotency | Clients must retain keys and reconcile uncertain outcomes |
| Cross-source overwrite or resurrection | First-writer ownership and persistent tombstones | Owner's fresh requests can restore values; no event-time semantics |
| SQL injection | Parameterized application SQL | DB administrator remains trusted |
| Ambiguous/unbounded requests | Duplicate JSON key rejection, strict fields, limits and timeouts | Global development limiter is not a production DoS defense |
| Unnecessary PII copies | No request/body/token logs; receipts and outbox exclude values | Attribute names, IDs, sources, hashes and timestamps can still be personal metadata |
| Public local services | Loopback-only published ports; no database host port | Local host users and Docker access are trusted |
| Browser credential persistence | Console token in memory; browser references use Kratos cookies, CSRF nodes and exact-origin checks; no CORS enabled | Browser extensions and local XSS can access in-memory data; cookies are not `Secure` on loopback HTTP |

Use synthetic data only. No automated erasure, retention, data export, audit expiry, backup deletion, or downstream deletion exists yet. PostgreSQL volume deletion is not a claim of secure physical erasure. Removing current values does not remove backup copies. Define purpose, retention periods, access policies, backup lifecycle, incident handling, and legal responsibilities before processing real customer data.

The outbox currently grows without a consumer. Rejected attribute attempts record source, field, mutation ID, timestamp and rule, without attempted values. Other errors do not persist failure audit records. Request hashes are unkeyed digests; they are not anonymization and may permit guessing low-entropy inputs with database access. Credentials reside in an ignored mode-0600 local `.env`, and Docker administrators can inspect container environments.

Compose is a local development configuration, not a production deployment. It uses one DB owner role and unencrypted connections inside the Docker network. Production requires real admin authorization, credential lifecycle, TLS, database role separation, dependency vulnerability review, retention/deletion controls, tested restore, and operational access auditing. Do not expose Vite, PostgreSQL or development tokens publicly. Kratos administrative ports must remain private. The pinned Kratos image has open critical and high findings, and both Kratos services run with `--dev`; neither is acceptable for production.
