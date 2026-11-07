# M1 development verification and M2 handoff

The milestone is delivered as three dependent changes based on the merged M0 and
initial M1 baseline at `98d0e16`. Review customer self-service first, verified
enterprise linking second, and named operator authentication third. Use GitHub
Actions on each draft's current head before merging. Synthetic data and isolated
loopback services are required throughout.

| Increment | Local evidence |
| --- | --- |
| Customer self-service | Real PostgreSQL tests and migrations, native Kratos smoke, and real Chrome browser registration, CSRF rejection, verification resend and replay rejection, login, profile provisioning, email re-verification, password change, recovery, two prior sessions revoked, unknown-account recovery and logout. A live Kratos smoke also verified expired flows, expired sessions, deactivation, fail-closed behavior during an identity-service outage and concurrent email changes that preserved the profile anchor. The isolated reference image scan reported no findings. |
| Verified enterprise linking | Real PostgreSQL transaction, exclusivity, rollback, expiry and replay tests. Live Kratos with two customers and a separate synthetic source challenge verified linking, subject mismatch rejection and authorized unlinking. SQL generation, native and browser smoke, and live contracts passed. |
| Named operator authentication | Separate Kratos PostgreSQL database and issuer, named bootstrap, TOTP enrollment and AAL2 step-up, explicit read role, cross-audience rejection, logout and revocation. Native and real Chrome checks passed. A local test with a shortened Kratos TTL confirmed that an expired operator session loses access. A real database test confirms that source credential rotation preserves creator authorization after outbox cleanup. |

Local Go vulnerability and Bun dependency audits passed on the combined
feature code. The identity CI report preserves all findings. The pinned Kratos
image reported 2 critical and 59 high findings on Linux ARM64. The two critical
entries affect pgx server-side protocol handling and have no demonstrated path
through Kratos as a database client in this setup. The findings remain open.
See the [image review](customer-identity.md#image-review) and
[PostgreSQL image triage](../security/postgres-container-triage.md) for the
upstream review and temporary gosu adjustment.

Run `make m1-session-smoke` on an isolated, running M1 project. It adjusts
timestamps and state only for newly created synthetic test rows and briefly
stops that project's customer Kratos service to verify denial.

The core development paths are implemented. The milestone remains pending
maintainer integration and GitHub Actions verification of the integrated tree.
Existing synthetic identities created before
recovery enrollment may need to change and re-verify their address. Named
operator authentication requires the explicit operator overlay. The compatibility
stack retains the scoped development operator credential.

## Invariants available to M2

- One verified Kratos subject resolves to one stable Customer Fabric profile.
  Email changes, re-verification and password recovery preserve the subject link.
  A new subject does not inherit a profile or relationship by email.
- A verified association has its own ID, source and external record lifetime.
  A matching attribute conveys no authorization. Source data and attribute
  ownership remain separate, and linkage exposes association metadata only.
- One active association can reference a given source-record lifetime.
  Revocation and source record deletion suppress access. Recreating the
  identifier does not restore the old relationship. The trusted issuer must
  report loss of upstream authority.
- Operator and source credentials use separate trust and permission checks.
  Customer sessions confer access to their own resources only. Source rotation
  preserves stable source identity, ownership and replay scope.

M2 should use explicit customer, profile, source-record and association references
when modeling consent purpose, legal basis, evidence, versioning, withdrawal and
downstream propagation. Consent changes cannot be inferred from profile linking
or mailbox verification. Erasure needs coordinated fences for Kratos identities
and sessions, profile links, outstanding grants and associations. It must minimize
association audit history and handle provenance, outbox, idempotency receipts
and backups under [ADR 0003](../adr/0003-data-lifecycle.md). Backup restore must
reapply revocation and erasure fences before reopening access.

Production requires reviewed upstream Kratos and PostgreSQL images, verified real
source authorities, TLS, proper secrets and role separation, credential expiration
and revocation, monitored backups and restore drills, and approved retention and
audit policy. The current synthetic source authority and local operator bootstrap
are development mechanisms.
