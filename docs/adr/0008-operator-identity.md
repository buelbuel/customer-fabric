# ADR 0008: Isolate named operator identity

Status: accepted for the synthetic development environment. This implements the operator boundary from [ADR 0004](0004-authentication-boundaries.md).

Deploy the same pinned Kratos build with a separate operator configuration, database volume, network, schema and cookie/cipher secrets. Customer sessions are checked against the customer issuer. Operator sessions are checked against the operator issuer on each request. Source tokens still map directly to their configured source and have their own audience. The operator deployment disables the previous development operator bearer credential.

The operator issuer has no public registration or email recovery. A local host maintainer with Docker access provisions a named operator via its private loopback admin API. The operator enrolls TOTP in Kratos settings. Docker access is the development break-glass boundary. Revoke lost or compromised sessions and replace the account through the same boundary after disabling its old permission. The service requires AAL2 with password and TOTP, an active subject and session, and the admin-managed `profile_reader` role. The role is carried in metadata set through the private admin API and cannot be set by Kratos self-service. It permits GET on the operator profile endpoints only. Audit logs record the opaque operator subject and authorization action.

The extra issuer adds migrations, another database volume, and separate secrets, account administration and backup obligations. It avoids shared issuer and account-recovery policy between customers and operators. Current deployment uses loopback HTTP and synthetic accounts. A production deployment must protect the issuer and all credentials with TLS, protected bootstrap, approved operator recovery and stronger operational audit controls.

## Amendment (2026-11-10): scoped write roles and audit

Operator roles live in the operator identity's admin-managed `metadata_public.customer_fabric_roles`. The earlier single `customer_fabric_role` is no longer read, because no release used it; re-assign roles with `make operator-roles`. They are checked on every request, so a role changed with `make operator-roles SUBJECT=… ROLES="…"` applies to the operator's next request. Unknown role names are ignored, and an operator with no known role is refused. Password plus TOTP at AAL2 is still required.

| Role | Allows |
|---|---|
| `profile_reader` | every operator read: profiles, views, attributes, consent registry, segments, previews, snapshots, activations, metrics, match candidates |
| `governance_admin` | define attributes and access policies |
| `consent_admin` | purposes, policy versions and operator consent evidence |
| `audience_manager` | segments and snapshots |
| `activation_operator` | destinations, activations and redrive |
| `identity_reviewer` | match-candidate review decisions |
| `source_admin` | advance source epochs |
| `audit_reader` | `GET /v1/operator/audit` |

Every successful operator write is recorded in an append-only audit: the actor's subject, the method, the request path and the status, never the body. The audit is best effort: the record is written after the operation commits, so a failed insert is logged as an error and does not undo the operation. Request paths can contain pseudonymous profile IDs and segment names; their retention and erasure belong to the lifecycle work in #28. The role tool accepts only known role names and replaces only the role list. The development bearer holds every role and is audited as `development-bearer`; it is never production authorization.
