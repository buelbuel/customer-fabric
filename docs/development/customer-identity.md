# M1 customer identity development flow

The first M1 use case lets a customer register through native Kratos JSON flows, verify their email with a code and access a dedicated Customer Fabric profile. The live smoke creates an existing synthetic CRM profile with the same email first, then checks that the new customer profile is separate and the CRM record stays intact.

## Run

```sh
make m1-up
make m1-smoke
```

Use synthetic data in the loopback development stack. The API is at `127.0.0.1:8080`, the Vite operator console at `127.0.0.1:5173`, the Kratos public API at `127.0.0.1:4433`, and the captured mail inbox at `127.0.0.1:8025`. The Kratos admin listener is available only on loopback inside its container. PostgreSQL and SMTP have no host ports.

Setup creates ignored `.env.m1` with mode `0600` and preserves existing secret files. Use `CF_DEV_OPERATOR_TOKEN` from that file in the operator console. Integration credentials still come from `.env`. The M1 profile database and Kratos each have their own named volume. The M0 profile volume is preserved. Run `make down` before switching stacks or moving or removing a worktree. It stops the current project's services and keeps all volumes. Linked worktrees use separate projects and dynamically assigned loopback ports. Use `make m1-ports` to look them up. The native smoke reconstructs request URLs from those ports and reads the verification code without following the canonical URL printed in captured mail. Native clients in linked worktrees should use the origin from `make m1-ports` with returned action paths. Treat the captured mail links as local test data.

## Flow and authorization

| Step | Interface |
| --- | --- |
| Create a native registration flow | `GET /self-service/registration/api` on Kratos. Submit the password and email to `POST /self-service/registration?flow=…`. |
| Verify email | A password login before verification returns a Kratos verification continuation. Read the synthetic code from Mailpit and submit it to `POST /self-service/verification?flow=…`. |
| Get a session | Start `GET /self-service/login/api`, then submit verified credentials to `POST /self-service/login?flow=…`. |
| Provision and read | Send the native session as `X-Session-Token` on `PUT /v1/customer/profile`, then `GET /v1/customer/profile` on Customer Fabric. Neither accepts a target profile ID. |
| Revoke | Submit the native session token to `DELETE /self-service/logout/api` on Kratos. Customer Fabric rejects subsequent requests with that token. |

The smoke performs those steps with real containers, tests session rejection after logout and checks that the operator, source and customer credentials stay in their own scopes. A verified customer gets 404 before provisioning. Repeat provisioning returns the same profile. Unverified sessions are rejected and an unavailable verifier fails closed. The customer response includes the profile ID, version and attributes. Operator-only metadata such as identifiers and provenance stays on the operator routes.

| Credential | Allowed surface in M1 |
| --- | --- |
| Native Kratos customer session | Provision and read the session subject's own profile. |
| Development operator bearer | Read profiles through `GET /v1/operator/profiles` and `GET /v1/operator/profiles/{id}`. |
| Integration source bearer | Create through `POST /v1/profiles` and update profiles that the source originally created through `PATCH /v1/profiles/{id}`. |

Kratos supplies the native API, session and verification semantics. See [Kratos v26.2.0 configuration examples](https://github.com/ory/kratos/tree/v26.2.0/contrib/quickstart/kratos/email-password), [the verification hook](https://github.com/ory/kratos/blob/v26.2.0/selfservice/hook/require_verified_address.go) and [Mailpit's API](https://mailpit.axllent.org/docs/api-v1/).

## Readiness

M0 is the verified development baseline at `da16574`. The PostgreSQL gosu rebuild is a temporary security patch. Replace it with an official image once the candidate passes the same scanner, fresh startup, existing volume, migration and API checks. [M0 CI](https://github.com/buelbuel/customer-fabric/actions/runs/42248344176) · [PostgreSQL triage](../security/postgres-container-triage.md)

The M1 configuration uses loopback HTTP, local development operator credentials and captured email. Customer recovery and a named operator login with MFA remain open. Real-data use also requires a reviewed Kratos image and production transport, secrets and lifecycle controls.

On 2026-11-07, Trivy 0.76.1 on Linux ARM64 reported two critical and 59 high findings in the pinned official Kratos distroless image. This variant removes the two additional critical OS findings from the Alpine variant. The remaining critical findings affect server-side PostgreSQL protocol handlers in Kratos's pgx dependency. The local setup uses Kratos as a PostgreSQL client on a private network, and the affected handlers have no demonstrated path in this flow. The findings remain open and block production use. Mailpit reported zero critical or high findings. The existing Customer Fabric runtime image gate passed separately. [Go advisory for CVE-2026-33815](https://pkg.go.dev/vuln/GO-2026-4771) · [Go advisory for CVE-2026-33816](https://pkg.go.dev/vuln/GO-2026-4772)
