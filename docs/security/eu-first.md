# EU-first security and privacy boundary

Customer Fabric runs independently, without cloud registration or mandatory telemetry. Runtime data stays in PostgreSQL and the operator's browser. Dependency installation and container pulls contact public registries at build time. Operators choose where machines, volumes and backups are located; software alone cannot establish residency or GDPR compliance.

## Initial threat model

| Threat | Implemented control | Remaining boundary |
| --- | --- | --- |
| Unauthenticated profile access | Bearer authentication on all `/v1` routes; production mode refuses startup | All configured sources have full read access; no production RBAC |
| Source impersonation | Credential-to-source mapping at startup; payload source rejected | Compromise of a credential grants that source's privileges |
| Lost updates and retry duplicates | Row locking, version precondition, transactional idempotency | Clients must retain keys and reconcile uncertain outcomes |
| Cross-source overwrite or resurrection | First-writer ownership and persistent tombstones | Owner's fresh requests can restore values; no event-time semantics |
| SQL injection | Parameterized application SQL | DB administrator remains trusted |
| Ambiguous/unbounded requests | Duplicate JSON key rejection, strict fields, limits and timeouts | Global development limiter is not a production DoS defense |
| Unnecessary PII copies | No request/body/token logs; receipts and outbox exclude values | Attribute names, IDs, sources, hashes and timestamps can still be personal metadata |
| Public local services | Loopback-only published ports; no database host port | Local host users and Docker access are trusted |
| Browser credential persistence | Token in memory; no storage or cookies; no CORS enabled | Browser extensions and local XSS can access in-memory data |

Use synthetic data only. No automated erasure, retention, data export, audit expiry, backup deletion, or downstream deletion exists yet. PostgreSQL volume deletion is not a claim of secure physical erasure. Removing current values does not remove backup copies. Define purpose, retention periods, access policies, backup lifecycle, incident handling, and legal responsibilities before processing real customer data.

The outbox currently grows without a consumer. Rejected attribute attempts record source, field, mutation ID, timestamp and rule, without attempted values. Other errors do not persist failure audit records. Request hashes are unkeyed digests; they are not anonymization and may permit guessing low-entropy inputs with database access. Credentials reside in an ignored mode-0600 local `.env`, and Docker administrators can inspect container environments.

Compose is a local development configuration, not a production deployment. It uses one DB owner role and unencrypted connections inside the Docker network. Production requires real admin authorization, credential lifecycle, TLS, database role separation, dependency vulnerability review, retention/deletion controls, tested restore, and operational access auditing. Do not expose Vite, PostgreSQL or development tokens publicly. Kratos administrative ports must remain private when introduced.
