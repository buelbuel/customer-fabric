# ADR 0004: Separate customer, operator and source identities

Status: accepted design, 2026-11-06; future implementation, no new authentication service in M0.

The first M1 development implementation is described in [ADR 0005](0005-customer-profile-linking.md).

Ory Kratos will authenticate customers and manage their identity/recovery/session lifecycle. A Kratos identity is not automatically a profile owner, operator, tenant administrator or source credential. Bind identities to profile subjects through an explicit verified association; do not merge by email. Customer-facing authorization must evaluate subject, tenant, action and resource independently of successful authentication.

Administrative authentication uses a separate operator trust boundary, issuer/audience or isolated identity configuration, MFA, session lifetime, revocation and recovery policy. Never accept a customer session on an administrative route simply because it is valid. Production console design must replace the pasted development source token. Prefer a same-origin backend session with Secure, HttpOnly cookies, CSRF defenses and strict origin validation; review session fixation, logout, idle/absolute expiry and step-up before implementing. No long-lived source secrets in localStorage, URLs or frontend bundles.

Authorization is an application policy boundary, separate from either login mechanism. Deny by default. Evaluate tenant, role/permission, resource and action on every request. Operator audit identity and authoritative data source are different fields: an operator must not freely impersonate an owning source. Any future override requires an explicit privileged action, purpose and audit record, not a client-supplied `source` property or automatic ownership transfer.

Source credentials authenticate ingestion services, not people. They need scoped namespaces/actions, tenant binding, expiry, rotation, revocation, rate limits and an auditable stable source ID. Rotation must not reset ownership or replay watermarks. Separate source-token audiences from customer and administrative sessions. A source's write authority does not imply unrestricted profile reads. Credential material remains server-side and is never returned in profiles or provenance.

M0 deliberately implements only local development bearer credentials: all configured sources can read all profiles, and accepted mutations derive source from the token mapping. Tokens do not expire and revocation requires API restart. This is neither Kratos integration nor production authorization. Authentication failures clear the console's memory and rendered data. Deployment still refuses modes other than development. No Hydra or other identity infrastructure is introduced by this decision.

Before real data, test cross-audience rejection, cross-tenant access, privilege escalation, source impersonation, session revocation/CSRF, operator recovery and subject unlink/erasure behavior. Record operator identity separately from data provenance, with retention governed by ADR 0003.

## Note (2026-11-12): M0 mode removed

The API always runs with the customer and operator issuers. Integration sources never read profiles and write only the profiles they created; operators read them with named roles. The separate M0 stack and its source reads are removed (#161).
