# EU-first security and privacy boundaries

**Status:** M0/M1 are synthetic-data development milestones. EU-first is a design goal and a deployment responsibility, not an automatic data-residency or GDPR-compliance guarantee. See [product roadmap #8](https://github.com/buelbuel/customer-fabric/issues/8) and [production-readiness epic #13](https://github.com/buelbuel/customer-fabric/issues/13).

Customer Fabric needs no mandatory hosted SaaS registration or runtime telemetry. The local Go API, PostgreSQL, Kratos customer and optional operator instances, Mailpit, React console and minimal browser reference run on isolated loopback Compose services. Image pulls and developer tooling contact external registries at build/install time; the operating organization controls where runtime, backups, logs, support access and keys reside.

## Current development controls and gaps

| Boundary | Current M1 development behavior | Production blocker / further work |
| --- | --- | --- |
| Customer identity | Separate Ory Kratos customer issuer; live session verification, email proof, recovery and browser CSRF. Subject links to stable profile. | Production TLS, reliable credentials/recovery, abuse controls, lifecycle policy and audited Kratos image CVEs. |
| Named operators | Optional isolated operator Kratos issuer with TOTP/AAL2 and read-only role. Compatibility development stack retains separate local bearer. | Production operator lifecycle, MFA recovery, authorization audit, privilege escalation tests and stronger secret management. |
| Integration sources | Server-configured source credentials; creator-source authority stored on profile; source A cannot modify B. No unrestricted profile read in M1. | Expiring/rotatable scoped service credentials, tenant binding, rate limits, compromise response. |
| Verified associations | Dedicated issuer attests independent **synthetic** proof; short-lived, one-use hashed grant; no email auto-link or source attribute transfer. | Real trustworthy source proof adapters, issuer revocation/rotation policy and proof/evidence retention. |
| Persistence | PostgreSQL with transactional idempotency, provenance, ownership tombstones and metadata-only outbox; SQL is parameterized. | Least-privilege DB roles, DB TLS, backup encryption, data lifecycle and audit retention. |
| Browser boundary | Customer and operator reference browsers use same-origin cookie/CSRF protections; native clients use session token headers. | HTTPS/Secure cookies, trusted proxy configuration, session hardening and security review. |
| Data rights and consent | **Not implemented:** there is no purpose-based consent engine, subject export/erasure, retention worker or downstream propagation. | M2 lifecycle design and implementation, legal holds, replay fences and tested restoration. |
| Event delivery | Outbox records are stored only; no durable dispatcher, destination connectors, event replay ordering or consent-aware activation. | M3 integration reliability and M4 activation controls. |
| Software supply chain | CI runs Go/Bun audits, contract tests and image reports. Kratos findings are visible, not silently filtered. | Resolve or independently assess material vulnerabilities, signed supported releases, artifact provenance and response SLAs. |

## Threat model priorities

- Identity/session confusion across customer, operator, linking issuer and source audiences; authorization must deny by default.
- Account takeover, stolen verification/linking grants, replay, source impersonation and cross-subject association claims; email equality is not ownership proof.
- Late source events or restored backups resurrecting erased or withdrawn data; M2/M3 must enforce durable epochs, suppress in-flight delivery and test recovery.
- PII leakage via caches, source adapters, logs, receipts, traces, proofs and backups. Identifiers and hashes can still be personal data.
- Denial of service from oversized imports, expensive audiences, insufficient source rate limits and unbounded workers.
- Local plaintext HTTP, database owner role and Docker host access are development assumptions, not production security measures.

## Release rule

Use synthetic data only until [production security #13](https://github.com/buelbuel/customer-fabric/issues/13) and [v1 release #14](https://github.com/buelbuel/customer-fabric/issues/14) meet their evidence-based acceptance criteria. Controller/legal assessment is required per deployment. Do not claim that deleting a PostgreSQL volume is complete physical erasure. See [ADR 0003](../adr/0003-data-lifecycle.md), [M1 verification](../development/m1-completion.md) and [Kratos review](../development/customer-identity.md#image-review).
