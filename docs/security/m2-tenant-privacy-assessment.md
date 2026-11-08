# M2 tenant and privacy boundary assessment

Dated 2026-11-08. Compares [ADR 0009](../adr/0009-m2-consent-subjects-and-lifecycle.md) with the target architecture in [PR #102](https://github.com/buelbuel/customer-fabric/pull/102), which is a draft. Cloud, Kafka, Temporal and OIDC are **target architecture and are not implemented**. This is a design assessment, not a security certification or legal opinion.

## Discrepancies found

**Confirmed defects, corrected in ADR 0009**

| Finding | Why it failed | Correction |
| --- | --- | --- |
| "One controller per deployment" treated a deployment as an installation | Cloud has one controller per tenant data plane | Section 1 |
| Journal "in a separate database" | A second logical database on a shared cluster can be rewound with the tenant by cluster recovery | Section 9: outside the recovery domain |
| One HMAC key assumed | A shared key would correlate one identifier across tenants | Section 10: per data plane |
| Controller check claimed to catch a wrong restore | It only compares configuration | Section 1: deployment binding, with no restore claim |
| Restore relied on replay protection surviving | Consumer dedupe rolls back with the database, so retained events arrive as new | Section 13: fences, epoch advance, recovery generation |
| Recovery generation had no enforcer | A restored snapshot could serve erased data | Section 13: recovery check, dedicated restore command, stamped identities |
| Journal covered erasure only | A restore revived withdrawals, restrictions and unlinks | Sections 9, 13: lifecycle journal |
| Crash reconciliation could void a committed erasure | Restored state looks as if nothing committed | Section 9: intent ID in the transaction; in-doubt intents apply on restore |
| Withdrawal could be refused while restricted or pending | An old grant stayed effective | Sections 5, 6 |
| Erasure lacked the natural-person check that export had | One customer could destroy a corporate record | Section 9 |
| Erasure could be read as complete after the database transaction | Asynchronous copies would remain uncounted | Sections 7, 17: register and per-store status |
| Receipt-expiry horizon ignored the event platform | Expiry shorter than Kafka or Temporal retention would re-enable replay | Section 12 |

**Incomplete requirements, added**

Staff authority per tenant; grant and source-name scoping; host-only cookies; tenant identifiers never reused; termination order; OAuth and consent separation; customer-owned attributes on the anchor; anonymous subject kinds need an amendment; read models and token lifetimes as register entries.

**Intentional future work, unchanged**

Kafka topic design, Temporal namespace choice, OIDC product selection, full event ordering in #35, receipt expiry in #86, and production recovery in #49.

## Preserved security decisions

Reviewed against the target and retained: stable customer anchors; distinct source-record subjects; associations without merging; withdrawal surviving unlink; conservative shared-profile handling; profile and subject lifecycle states; minimal epochs; receipt and identifier fences; mutation-key-first locking; concurrent erasure safety; catalog-tested inventory; explicit foreign-key purge order; restore from pre-erasure snapshots; and the M2 versus receipt-expiry split. Advisory locks are per database, so tenants cannot contend on them, although the lock table is shared cluster memory and the association limit keeps erasure lock counts bounded.

## What applies where

| Applies to | Scope |
| --- | --- |
| **Both products** | Sections 1–14, 16 and 17. One data plane, one controller, one customer issuer, deployment binding, register, fences, erasure status. |
| **Self-hosted only** | Local operator issuer. No control plane, billing or provisioning dependency. |
| **Cloud only** | Section 15 and the Cloud release gate in section 18. |

## Cloud boundary assessment

| # | Question | Assessment |
| --- | --- | --- |
| 1 | Consent-subject identifiers across tenants | Meaningful only inside one database. Never a cross-tenant key. |
| 2 | Controller and policies per tenant | One controller row per data plane. Policies are not shared. Templates are copied. |
| 3 | Cloud administrators versus customer subjects | Separate issuers and separate identities, even for one person. Staff are never consent subjects. |
| 4 | One operator across two tenants | Possible. Two separate tenant-audience authorities, no shared credential, and no customer permission unless the tenant role grants it. |
| 5 | Identity collision and grant confusion | Issuer, grants, proof references and sources are per data plane. Shared connectors hold separate credentials. |
| 6 | Inventory and erasure scope | The schema inventory is shared and tested. The store register is per deployment. Erasure never crosses a data plane. |
| 7 | Restoring one tenant | Side instance and logical copy on shared clusters. Quarantine, journal, epoch advance. Must be proven in #93 and #100. |
| 8 | Staff accounts and control-plane retention | Separate register and policy. Staff removal drops the mapping, not tenant audit history. |
| 9 | Shared Kafka and Temporal holding personal data | Register entries with opaque-by-default payloads, per-tenant keys and credentials. See below. |
| 10 | Tenant termination | Ordered, forward-only procedure in section 15. Journal outlives the last backup. Identifiers never reused. |

## Kafka

| Concern | Control | Stage |
| --- | --- | --- |
| Event identity and ordering | Stable event IDs and per-source policy; missing trust is quarantined | M3 (#35, #36) |
| Tenant topics, principals, quotas | Per-tenant ACLs, no wildcard; envelope tenant checked, never authority | Cloud (#96) |
| Lifecycle and source epochs | Carried from the outbox and compared on consumption | M2 fields, M3 enforcement |
| Withdrawal or erasure while queued | Authority is the database; recheck at consumption and dispatch | M3 |
| Replay after erasure or receipt expiry | Fences first; expiry horizon exceeds retention | M3 (#86) |
| Historical import after withdrawal | Source grants cannot override a withdrawal | M2 rule, M3 path |
| Retention, compaction, tombstones, backups | Not erasure. `bounded` until retention elapses | M3 contract, M5 proof |
| Physical deletion limits | Stated in status; key destruction only with all copies controlled | M5 |
| Cross-tenant publish or consume | Denied by ACL; on a mismatch the receiver drops the value and raises an incident on the owning side | Cloud (#96, #100) |
| Consent before activation | Final check immediately before the external call | M3 (#37), M4 (#44) |

## Temporal

| Concern | Control | Stage |
| --- | --- | --- |
| Tenant workflows and workers | Credential-enforced boundary per tenant; one tenant per worker | Cloud (#97) |
| Arguments, results, failures, search attributes | Opaque only; schema tests | M3 |
| Propagation and acknowledgments | PostgreSQL ledger is authority | M2 ledger, M3 execution |
| Cancellation versus deletion | Cancelling leaves obligations open | M3 |
| Activity retry after withdrawal or erasure | Each attempt rechecks | M3 |
| Deterministic replay | Fence lives in activities | M3 |
| History and visibility retention | Registered, bounded | M3 contract, M5 proof |
| Suspension, deletion, provisioning | No external effects while suspended; idempotent forward-only steps | Cloud (#98) |
| Partially completed privacy workflow | Forward only, never compensated by restoring | M3 |
| Restore predating erasure | Workers off until the journal is reapplied | M5 (#49), Cloud (#100) |

## Dependency map

| Stage | Order |
| --- | --- |
| Before M2 code | Maintainer review of ADR 0009; PR #102 decision; journal storage decision; #16 fixed by PR #89. Legal validation is required before real data. |
| M2 | #22 → #23 → #24; #84 (columns) → #27 (journal, enforcement) → #85 → #31; #27 → #28; #87 and #88 before #25 |
| M3 | #64 (envelope) → #34, #35 → #36 → #37; #35 and #36 → #86 |
| M5 | #46–#53; #70 closed or formally accepted; encrypted recovery in #49 |
| Cloud | #91–#95 → #96, #97 → #98 → #100, on top of M5. M3 and self-hosted do not depend on these. |
| Product core | #72 → #73, #74; #75; #76 depends on #22–#24 and #75 |

## Open decisions

These need maintainer approval and are not resolved by this document.

1. Whether raw personal payloads may enter Kafka at all, for example during source backfill (#64).
2. Provider support access to tenant data: none, or a break-glass process with approval and expiry (#92).
3. Staff authorization at the data plane: short-lived tenant assertions or online checks, and the maximum staleness (#92, #95).
4. Temporal namespace-per-tenant confirmation, and the retention values for Kafka and Temporal (#64, #96, #97).
5. Journal storage for Cloud and self-hosted, given the recovery-domain requirement. **This blocks #27, #84 and #85.**
6. Whether a self-hosted deployment must run Kafka and Temporal for full CDP function, and which minimal profile is supported (#54, #64).
7. Controller and processor roles for Cloud, and retention periods. These need legal review.
8. OIDC product choice (#72), pairwise subject identifiers (#74) and behavioral event scope (#77, #78).
9. Mitigation path for the Kratos stale-address defect (#70).
10. Whether a `deny` materializes at link time as a withdrawal does, and how objections work for public-task purposes.
