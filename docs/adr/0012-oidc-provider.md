# ADR 0012: Ory Hydra as the OpenID Connect provider

Status: accepted for synthetic development (maintainer decision of 2026-11-10, provisional). Implements the OIDC requirement of #72.

## Decision

Customer applications sign customers in through **Ory Hydra** (OAuth 2.0 / OpenID Connect provider), alongside the existing customer Kratos issuer:

- **Authentication stays in Kratos.** Hydra delegates login and OAuth consent to Customer Fabric's reference gateway, which accepts a login only for a customer with a valid, verified Kratos session. Hydra's `sub` is the Kratos identity ID.
- **Authorization code with PKCE**, enforced for public clients; no implicit or password grants. Redirect URIs match exactly.
- **OAuth consent is not purpose consent.** Granting an app the `profile.read` scope lets it read the customer's own profile fields through the API on their behalf. It is never evidence for a processing purpose; marketing and other purposes stay in the consent registry (ADR 0009).
- **Access tokens are JWTs** signed by Hydra. The API verifies them against Hydra's JWKS (issuer, audience `customer-fabric-api`, expiry and scope) without calling Hydra per request.
- **Applications are registered by operators** with the `application_admin` role, through Customer Fabric's API, which writes to Hydra's admin API. Hydra's admin port is never published.
- Hydra keeps its own PostgreSQL database; Customer Fabric does not read Hydra tables.

## Known gaps

- The login bridge accepts any live, verified Kratos session. It does not yet honour `prompt=login` or `max_age` by forcing re-authentication; applications that need step-up must not rely on them yet.
- API access-token verification and Kratos-logout-to-token revocation follow in the next slice (#74).

## License verification (2026-11-10)

- `ory/hydra` and `ory/kratos` are licensed **Apache-2.0** (GitHub license metadata), actively maintained; the pinned images are `v26.2.0`.
- Ory also sells the **Ory Enterprise License (OEL)** for self-hosted deployments. It adds features Customer Fabric does not use (for example the resource-owner password grant and zero-downtime migrations) plus SLAs and CVE support. OEL builds are not public and need an agreement with Ory.
- Consequence: Customer Fabric can be distributed and self-hosted with Apache-2.0 Hydra. **Before any production commitment**, decide how security fixes are obtained: Apache-2.0 builds come without a patch SLA. This is a production gate, not a development blocker.

## Alternatives considered

- **Kratos sessions only:** no standard protocol for third-party applications; rejected by #72.
- **Keycloak:** a complete IdP that would duplicate Kratos identity, recovery and policy; two identity sources of truth.
- **Building an OAuth server:** high security risk without a reason to diverge from a maintained, certified implementation.
