# ADR 0006 Customer browser sessions and account lifecycle

Accepted 2026-11-07. This extends ADR 0005 with a browser credential transport.
The native flow and subject-to-profile link remain supported.

Kratos owns browser CSRF, credentials, recovery, verification and sessions. The local
reference serves minimal HTML on the same origin as an allowlist of public Kratos
routes. Its origin is derived from the Compose-assigned loopback port at startup.
The Kratos admin listener stays private. The profile API sends only this listener's
session cookie to the configured Kratos API and checks the exact browser origin on
cookie-authenticated provisioning. Successful session checks are never cached.

Mailbox verification follows the current identity trait. A pending changed email
temporarily blocks profile access. Re-verification restores access to the same
subject link. Profile attributes and source identifiers do not move. Recovery uses
Kratos code flows and the revoke-active-sessions hook. Identity deactivation denies
access without erasing profile data. Re-registration with a different Kratos subject
does not inherit a prior profile based on email. ADR 0003 governs future coordinated
erasure and recovery from backups.
