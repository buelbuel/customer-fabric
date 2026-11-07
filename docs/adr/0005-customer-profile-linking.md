# ADR 0005 Customer profile linking

Accepted 2026-11-07 for the first M1 development slice.

Kratos owns customer credentials, verification and sessions. Customer Fabric owns business profiles. Each deployment trusts one configured Kratos public API and treats its identity UUID as a subject within that deployment. Changing the trusted Kratos instance requires reviewing the identity links and preserving the correct identity database.

Customer Fabric validates native sessions online through `/sessions/whoami`. It checks the active identity and session, session expiry and verification of the identity's current email address. Email verification establishes control of that mailbox. Profile lookup and authorization use the Kratos subject UUID. Customer Fabric receives credentials only through `X-Session-Token` on the customer route and discards the email after checking verification.

A verified customer explicitly provisions their profile through `PUT /v1/customer/profile`. One transaction creates a blank profile and its unique subject link. Retries and concurrent requests resolve to the same profile. The customer supplies neither a profile selector nor a subject identifier. `GET /v1/customer/profile` reads through the link and returns a limited profile view. Provisioning has its own link timestamp and does not create a source mutation, provenance decision or integration outbox event.

A CRM record can exist before registration with the same email address, including an integration identifier named for Kratos. Neither establishes a trusted identity link. The CRM record keeps its existing identifier, ownership and history. Any future reassociation of an existing profile requires an independently designed proof and audit flow.

Operator authentication is a separate local development credential with profile read permissions. Operators have no source identity and customer session tokens confer no operator permission. M1 integration credentials can create profiles and update profiles whose immutable creation event names their source. They cannot enumerate or read profiles. The current design depends on retaining source creation events for this authorization check. Before adding outbox retention, move creator authorization data into durable profile metadata or an explicit access mapping.

The M1 app and Kratos each use a separate database and volume. M0-compatible source reads remain available against the separate M0 database. The M1 database name and role differ so a mixed Compose startup fails closed. The M1 slice runs on loopback with synthetic data and a local SMTP sink. Named operator login, MFA, customer recovery validation, and production deployment controls remain separate work.
