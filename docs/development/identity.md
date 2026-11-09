# Identity development flows

Customer self-service, verified enterprise linking and named operators on the local Kratos stacks. Everything here is synthetic-data development only.

The first use case lets a customer register through native Kratos JSON flows, verify their email with a code and access a dedicated Customer Fabric profile. The live smoke creates an existing synthetic CRM profile with the same email first, then checks that the new customer profile is separate and the CRM record stays intact.

## Run

```sh
make m1-up
make m1-smoke
```

Use synthetic data in the loopback development stack. The API is at `127.0.0.1:8080`, the Vite operator console at `127.0.0.1:5173`, the Kratos public API at `127.0.0.1:4433`, and the captured mail inbox at `127.0.0.1:8025`. The Kratos admin listener is available only on loopback inside its container. PostgreSQL and SMTP have no host ports.

Setup creates ignored `.env.m1` with mode `0600` and preserves existing secret files. Use `CF_DEV_OPERATOR_TOKEN` from that file in the operator console. Integration credentials still come from `.env`. The M1 profile database and Kratos each have their own named volume. The M0 profile volume is preserved. Run `make down` before switching stacks or moving or removing a worktree. It stops the current project's services and keeps all volumes. Linked worktrees use separate projects and dynamically assigned loopback ports. Use `make m1-ports` to look them up. The native smoke reconstructs request URLs from those ports and reads the verification code without following the URL printed in captured mail. Native clients in linked worktrees should use the origin from `make m1-ports` with returned action paths. Treat the captured mail links as local test data.

## Flow and authorization

| Step | Interface |
| --- | --- |
| Create a native registration flow | `GET /self-service/registration/api` on Kratos. Submit the password and email to `POST /self-service/registration?flow=…`. |
| Verify email | A password login before verification returns a Kratos verification continuation. Read the synthetic code from Mailpit and submit it to `POST /self-service/verification?flow=…`. |
| Get a session | Start `GET /self-service/login/api`, then submit verified credentials to `POST /self-service/login?flow=…`. |
| Provision and read | Send the native session as `X-Session-Token` on `PUT /v1/customer/profile`, then `GET /v1/customer/profile` on Customer Fabric. Neither accepts a target profile ID. |
| Recover | Start `GET /self-service/recovery/api`. Submit `method: code` and the address, then submit the captured code to that flow. Follow the returned settings continuation with the recovery session and submit a new password. |
| View identity and change settings | Read `GET /sessions/whoami`. Start `GET /self-service/settings/api` with `X-Session-Token`, then submit the Kratos settings action with the same header and either `method: profile` with traits or `method: password` with the new password. Restart an expired flow and refresh the login when Kratos requires a fresh session. |
| Revoke | Submit the native session token to `DELETE /self-service/logout/api` on Kratos. Customer Fabric rejects subsequent requests with that token. |

The smoke performs those steps with real containers, tests session rejection after logout and checks that the operator, source and customer credentials stay in their own scopes. A verified customer gets 404 before provisioning. Repeat provisioning returns the same profile. Unverified sessions are rejected and an unavailable verifier fails closed. The customer response includes the profile ID, version and attributes. Operator-only metadata such as identifiers and provenance stays on the operator routes.

| Credential | Allowed surface in M1 |
| --- | --- |
| Native Kratos customer session | Provision and read the session subject's own profile; redeem a linking grant and list, read or unlink its own associations. |
| Development operator bearer | Read profiles through `GET /v1/operator/profiles` and `GET /v1/operator/profiles/{id}`. |
| Integration source bearer | Create through `POST /v1/profiles` and update profiles that the source originally created through `PATCH /v1/profiles/{id}`. |
| Linking issuer bearer | Issue and revoke grants for its own source through `/v1/linking/grants`. |

Kratos supplies the native API, session and verification semantics. See [Kratos v26.2.0 configuration examples](https://github.com/ory/kratos/tree/v26.2.0/contrib/quickstart/kratos/email-password), [the verification hook](https://github.com/ory/kratos/blob/v26.2.0/selfservice/hook/require_verified_address.go) and [Mailpit's API](https://mailpit.axllent.org/docs/api-v1/).

## Readiness

The M1 configuration uses loopback HTTP, local development credentials and captured email. Named MFA operators are available through the separate [operator overlay](#named-operators). Real-data use also requires a reviewed Kratos image and production transport, secrets and lifecycle controls. Open image findings and production gates are tracked in the [security guide](../security/eu-first.md#open-findings-and-production-blockers).

## Browser self-service reference

If upgrading from the initial M1 stack, run `make down` first to free its direct
Kratos port while preserving the volumes. Run `make m1-up` and open the customer
identity URL printed by `make m1-ports`.
The same loopback origin serves the minimal HTML and the allowlisted Kratos public
endpoints. It uses Kratos browser flow IDs, cookies, CSRF nodes, and form actions.
The reference checks action origins and renders escaped HTML without client scripts.
The gateway never publishes the Kratos admin listener. Use a fresh browser context
when running more than one worktree. Cookies on the same loopback hostname are
shared across ports by browsers. The gateway filters other worktrees' named session
cookies before proxying. Loopback HTTP is for synthetic development only.

Use Register, Verify email, then Log in. Codes arrive in the worktree's Mailpit inbox. Resend requests a fresh code and invalidates the previous one.
Start a new flow for an expired one. Current profile lets a verified customer provision
and view their own stable profile. Account settings delegates email and password
changes to Kratos. A changed email temporarily blocks profile access until the current
email is verified. The change neither writes source-owned attributes nor selects any
external profile. Sensitive settings use Kratos's five-minute privileged session window. An ordinary password change can leave existing sessions active. Recovery uses the separate session-revocation hook below.

Recover account starts Kratos code recovery and sends mail through the local sink.
After a correct code, Kratos revokes earlier sessions and opens privileged settings for
password replacement. The recovery subject keeps its existing profile association.
Identities registered before the recovery address was added may lack a recovery
address. While still signed in or able to log in, change to a different controlled
email and verify it to enroll that address without replacing the profile link.
A locked-out legacy synthetic identity needs private Kratos administrative handling.
Log out confirms Kratos's browser logout URL and invalidates the current session.
Customer Fabric checks Kratos online for every authorized profile request and denies
expired, inactive, unverified or uncheckable sessions. There is no positive auth cache.

Native clients still use the JSON `/api` flow initiators and `X-Session-Token`. Browser
clients use the `/browser` initiators and must preserve Kratos's same-origin CSRF cookie
and hidden form token. Never move native session tokens into browser URLs or storage.
The cookie-authenticated profile mutation also requires the configured Origin.

To run the browser acceptance check, install Playwright 1.60.0 in a local virtual
environment and run `python -m playwright install chromium`. Run
`make m1-browser BROWSER_PYTHON=path/to/venv/bin/python` while the worktree's M1
stack is up. A preinstalled Chromium-compatible executable can be selected with
`CF_BROWSER_EXECUTABLE`. CI installs Chromium and runs the same test.

### Concurrent settings

Two concurrent email changes on one identity race inside Kratos. `Manager.Update` diffs against an identity read outside the write transaction, so a writer does not see an address its competitor inserted. On pinned `v26.2.0` the race produces these outcomes:

| Observation | Meaning |
| --- | --- |
| Both requests return 200 | The usual result. Usually both addresses remain on the identity. |
| One 200 and one 404 with `Unable to locate the resource` | The loser hit a row the winner replaced. This is not a missing flow; that error has its own reason and fails the check. |
| One 200 and one 400 with an inner 500 naming `identity_verification_codes_identity_verifiable_addresses_id_fk` | The loser's post-write verification step lost its address row. |

A failed response does not mean nothing was written. The loser's identity update may already have committed, leaving its address behind. `make m1-settings-race` accepts only the outcomes above, requires one success, and checks the subject, enrolled addresses, unverified state, profile anchor and a bystander customer. Any other status fails. It never retries.

Across 420 parallel races on 2026-11-08 (Linux ARM64): 397 returned 200/200, 13 returned 200/404, 10 returned 200/400, and no final email came from a failed request. **390 retained a stale address.** The check confirms that Customer Fabric stays closed when only a stale address is verified, because access requires the current email. A one-off manual probe that day, not repeated by the check, showed that a retained address accepted a recovery code, that the recovery revoked the original session, and that the next single email change removed the stale rows. This is an open production blocker, tracked in [issue #70](https://github.com/buelbuel/customer-fabric/issues/70). The check does not cover recovery. After recovering through a retained address, the holder has a session and could change and verify the current email, which would pass the access check. Creating the stale address requires a live session inside the five-minute privileged window. Separately, account setup in the check occasionally stops on a 30-second mail timeout, about once per 250 races under load, tracked in [issue #104](https://github.com/buelbuel/customer-fabric/issues/104).

## Enterprise linking

Use synthetic records only. Run these commands from the intended worktree.

```sh
make m1-linking-init
make m1-up
make m1-linking-smoke
make down
```

The first command creates a separate ignored `.env.linking` file with mode 0600. It does not overwrite existing credentials. If the stack was already started, run `make m1-up` again so Compose loads the issuer credential. Use `make m1-ports` for that checkout. Keep the issuer credential private to the synthetic proof adapter and stop the stack before removing the checkout. Stop preserves volumes.

The smoke imports historical ERP and duplicate Salesforce records from the deterministic fixture library. A verified Kratos subject gets a separate profile. The in-process synthetic authority verifies a random out-of-band source challenge against a specific record and resolves the customer's subject through Kratos. The adapter issues a grant through the real REST API, and the customer redeems it through their own native session. The smoke checks ambiguous records stay unlinked, two verified relationships can coexist, and neither source ownership nor the customer anchor changes. It checks denial of invalid proof, credential crossover, replay and access after logout. Database tests exercise expiry, concurrency, revocation, rollback, and deleted or recycled source records in isolated schemas.

Use the [OpenAPI contract](../../contracts/openapi/customer-fabric.yaml) for request and response details. The issuer registers an attestation with `POST /v1/linking/grants` after proof. The verified customer sends only the grant to `POST /v1/customer/associations` with `X-Session-Token`. A customer lists or reads their association metadata and can unlink it. The issuer can revoke the grant, including its resulting association. No source attributes or identifiers are returned by these endpoints. A consumed grant returns a conflict when replayed. On an uncertain redemption response, check the association list.

See [ADR 0007](../adr/0007-verified-enterprise-associations.md) for the proof, privacy and concurrency model. This is a synthetic provider. A real source integration needs a reviewed account verification adapter, scoped credentials, private delivery and a revocation channel before real data.

## Named operators

Run `make operator-up` to start the M1 overlay with a separate Kratos issuer for operators. It creates `.env.operator` with mode 0600 on the first run. Use synthetic data. The operator and customer reference origins are printed with their separate Docker-allocated ports. The admin endpoint binds only to the operator container's loopback and has no host port. `make down` stops both issuers and retains their separate volumes.

Run `make operator-bootstrap` in a private interactive terminal. Supply a distinct synthetic username and password and enroll TOTP through the operator issuer’s Kratos settings flow. The password travels to the admin service through stdin, and the service prints only the opaque subject UUID on success. Operator bootstrap requires Docker access to this checkout's Compose project.

For native TOTP enrollment, sign in with the bootstrap password and use the session to initialize `GET /self-service/settings/api`. Enroll the returned Kratos TOTP secret in an authenticator and submit `method=totp` and `totp_code` to the settings flow. Log out after enrollment.

Native clients initialize `GET /self-service/login/api`, then POST `method=password`, `identifier` and `password` to the returned flow's login endpoint. Use the returned token in `X-Session-Token` when creating a second native login flow with `aal=aal2`. Submit `method=totp` and `totp_code` to that flow with the same header. Use the resulting AAL2 session as `Authorization: Bearer …` on `GET /v1/operator/profiles` or its detail endpoint. Password-only sessions and MFA sessions without the assigned role receive 403. Invalid, expired and revoked sessions receive 401. Issuer failures fail closed. Sessions have a 20-minute lifespan and the API validates them online every time.

Log out with Kratos `DELETE /self-service/logout/api` and its native `session_token` body. A maintainer can invalidate every operator session with `python3 scripts/operator-dev.py revoke SUBJECT_UUID`. For a lost factor or suspected compromise, run `python3 scripts/operator-dev.py disable SUBJECT_UUID` to deactivate the old operator and revoke its sessions before bootstrapping a replacement under a new username. Preserve opaque subject IDs in audit records. No email recovery or automatic reassignment is available.

The operator browser reference uses native Kratos browser flows for login, account settings, TOTP enrollment, second factor verification and logout. Open the printed operator origin and sign in, then use **Account settings** to enroll TOTP. Log out after enrollment. On future logins, use **Verify second factor** and then **Read profiles**. The reference sends the per-port HttpOnly cookie through the same-origin read route. Kratos validates form CSRF. Separate browser contexts are recommended for concurrent worktrees because browsers share cookies across ports on the same host.

Run `make operator-browser` with the same Playwright setup as `make m1-browser` to verify CSRF enforcement, the per-worktree cookie, MFA, protected reads and logout. Run `make operator-smoke` to check real password login, TOTP, the read role, cross-audience rejection, logout and administrative revocation. The previous native M1 smoke can still run in the compatibility stack started by `make m1-up`. The operator overlay rejects the old pasted bearer credential.

Source authorization continues to use stable source IDs. The current development rotation changes a token in local configuration and restarts the API while keeping the same source ID and its ownership/replay state. Production requires separate scoped credentials per integration, bounded expiration, immediate revocation, a controlled rollover window, and audited issuance and rotation. Existing development tokens have no expiry and their replacement requires restart.
