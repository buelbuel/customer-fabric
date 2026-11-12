# ADR 0005 Customer profile linking

Accepted 2026-11-07 for the first M1 development slice.

Kratos owns customer credentials, verification and sessions. Customer Fabric owns business profiles. Each deployment trusts one configured Kratos public API and treats its identity UUID as a subject within that deployment. Changing the trusted Kratos instance requires reviewing the identity links and preserving the correct identity database.

Customer Fabric validates native sessions online through `/sessions/whoami`. It checks the active identity and session, session expiry and verification of the identity's current email address. Email verification establishes control of that mailbox. Profile lookup and authorization use the Kratos subject UUID. Customer Fabric receives credentials only through `X-Session-Token` on the customer route and discards the email after checking verification.

A verified customer explicitly provisions their profile through `PUT /v1/customer/profile`. One transaction creates a blank profile and its unique subject link. Retries and concurrent requests resolve to the same profile. The customer supplies neither a profile selector nor a subject identifier. `GET /v1/customer/profile` reads through the link and returns a limited profile view. Provisioning has its own link timestamp and does not create a source mutation, provenance decision or integration outbox event.

A CRM record can exist before registration with the same email address, including an integration identifier named for Kratos. Neither establishes a trusted identity link. The CRM record keeps its existing identifier, ownership and history. Any future reassociation of an existing profile requires an independently designed proof and audit flow.

Operator authentication is a separate local development credential with profile read permissions. Operators have no source identity and customer session tokens confer no operator permission. M1 integration credentials can create profiles and update profiles that carry their source as the creator. They cannot enumerate or read profiles. Creator metadata is stored on the profile in the same transaction as source creation and stays independent of event retention. Migration 003 backfills successful source creation receipts. Customer-linked and unproven profiles have no source creator.

The M1 app and Kratos each use a separate database and volume. M0-compatible source reads remain available against the separate M0 database. The M1 database name and role differ so a mixed Compose startup fails closed. The M1 slice runs on loopback with synthetic data and a local SMTP sink. Named operator login, MFA, customer recovery validation, and production deployment controls remain separate work.

## Note (2026-11-12): unified profiles

Profiles have no owning source. A source writes any profile it is associated with, and joins an existing profile only through identity resolution on a trusted identity key ([ADR 0018](0018-unified-profiles-identity-resolution.md)). No source ever writes a customer's profile anchor.
