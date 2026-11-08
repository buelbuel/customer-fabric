# M1 customer identity development flow

The first M1 use case lets a customer register through native Kratos JSON flows, verify their email with a code and access a dedicated Customer Fabric profile. The live smoke creates an existing synthetic CRM profile with the same email first, then checks that the new customer profile is separate and the CRM record stays intact.

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

M0 is the verified development baseline at `da16574`. The PostgreSQL gosu rebuild is a temporary security patch. Replace it with an official image once the candidate passes the same scanner, fresh startup, existing volume, migration and API checks. [M0 CI](https://github.com/buelbuel/customer-fabric/actions/runs/42248344176) · [PostgreSQL triage](../security/postgres-container-triage.md)

The M1 configuration uses loopback HTTP, local development credentials and captured email. Named MFA operators are available through the separate [operator overlay](operator-identity.md). Real-data use also requires a reviewed Kratos image and production transport, secrets and lifecycle controls.

On 2026-11-07, Trivy 0.76.1 on Linux ARM64 reported two critical and 59 high findings in the pinned official Kratos distroless image. This variant removes the two additional critical OS findings from the Alpine variant. The remaining critical findings affect server-side PostgreSQL protocol handlers in Kratos's pgx dependency. The local setup uses Kratos as a PostgreSQL client on a private network, and the affected handlers have no demonstrated path in this flow. The findings remain open and block production use. Mailpit reported zero critical or high findings. The existing Customer Fabric runtime image gate passed separately. [Go advisory for CVE-2026-33815](https://pkg.go.dev/vuln/GO-2026-4771) · [Go advisory for CVE-2026-33816](https://pkg.go.dev/vuln/GO-2026-4772)

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

Across 420 parallel races on 2026-11-08 (Linux ARM64): 397 returned 200/200, 13 returned 200/404, 10 returned 200/400, and no final email came from a failed request. **390 retained a stale address.** The check confirms that Customer Fabric stays closed when only a stale address is verified, because access requires the current email. A one-off manual probe that day, not repeated by the check, showed that a retained address accepted a recovery code, that the recovery revoked the original session, and that the next single email change removed the stale rows. This is an open production blocker, tracked in [issue #70](https://github.com/buelbuel/customer-fabric/issues/70). One earlier run stopped on a 30-second synthetic mail timeout that did not recur in those 420 races; every queued message had been sent.

### Image review

The 2026-11-07 review found v26.2.0 is still the latest published upstream release.
Its tagged Go module declares pgx/v5 v5.7.5. No newer released image was identified
for a supported upgrade. The pinned distroless image and its findings stay visible
in the identity CI artifact. Re-evaluate on a new upstream release, changes to
database reachability or any proposal to accept real identities. Review more than
the two critical findings before approving a production deployment.
[Upstream releases](https://github.com/ory/kratos/releases) · [Tagged dependencies](https://github.com/ory/kratos/blob/v26.2.0/go.mod)
