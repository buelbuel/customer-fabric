# M0 stabilization verification — 2026-11-06

**Current status.** M0 functional verification and the local PostgreSQL image gate pass. M1 can proceed with synthetic data in the isolated development setup. Production-security readiness remains open. The [PostgreSQL triage follow-up](../security/postgres-container-triage.md) records the remediation and current CI evidence. The checks below describe the earlier stabilization checkpoint.

Executed on Colima, Linux ARM64 containers, Go 1.27.3, PostgreSQL 18, Bun 1.4.4. All customer records used were synthetic. Host Go/Bun were not installed. Tooling and reports used Docker volumes and ignored repository `.tools`, not host `/tmp`. Runtime nginx has an in-memory container scratch mount. No integrity verification or security gate was disabled.

## Executed checks

| Check | Actual result |
| --- | --- |
| `make test` | PASS: sqlc 1.31.1 generated-code diff, Go race tests, real PostgreSQL integration/migration suites, frozen frontend install, TypeScript check and production build. No database suites skipped. |
| Go formatting, `go vet ./...`, `go mod verify` | PASS. Fresh API builds also downloaded modules and verified checksums. |
| `make security` | PASS: govulncheck 1.9.0 reported no vulnerabilities; Bun audits reported none for frontend (47 packages) and verification tooling (16 packages). |
| Clean builds | PASS: fresh `docker build --no-cache` dependency layers for API, static frontend and contract-tool image. Frozen Bun installation retained integrity checking. |
| Reproducible application artifacts | PASS: two independent clean builds produced identical API binary and frontend HTML/JS/CSS bytes. OCI image IDs/timestamps are not asserted reproducible. Exact package revisions may eventually disappear from Alpine mirrors; archive approved artifacts for long-term rebuilds. |
| `make contracts` | PASS: OpenAPI 3.1 validation, resolved references, AJV component and outbox-schema compilation; real HTTP health, 401, create/replay, detail/list, ownership 409, missing precondition 428, invalid mutation 422, successful PATCH, stale 412 and missing-profile 404, with response schema/content-type/cache/ETag checks. Outbox schemas are compiled; no delivery worker exists. |
| `make smoke` | PASS: auth, create/replay, update, detail, exact identifiers and accepted provenance, list. Initially failed because the browser review added a legitimate rejected audit entry; fixed the assertion to verify the smoke receipt IDs, exact paths/outcomes/source/rule instead of assuming the entire shared audit history has three rows. |
| `bash scripts/scan-images.sh` | Executed completely, **FAIL (exit 1)** on PostgreSQL. API, frontend runtime and Bun image report zero findings after updates. |
| Git whitespace | `git diff --check` passed. |

Application artifact SHA-256 values from the independent builds:

- API: `2865d9b76b9c448666191ed12771400eb3292a214ab413cb35ed255af5e07fa5`
- JS: `e70329d044538f4863b124b6d4cc1dc6512f9a25e5e4e02e88abeed6aa871342`
- CSS: `71ddf317c0b35bb9615baf953531049579f2ebc7571223bc507fdcb611e6f3a6`
- HTML: `452fc85f03011b97e525bf86aed688a42a6680941590ed658c3558af8e02a6d7`

The local Compose classic-builder fallback unexpectedly reused frontend layers on one multi-service `--no-cache` invocation. A separate direct `docker build --no-cache` was therefore executed and confirmed fresh installation/build before comparing artifacts. Later musl package fixes changed the runtime layer only.

## Authenticated browser walkthrough

Executed against the static production-built console at `127.0.0.1:5174`, with the real API and PostgreSQL; no API responses mocked:

- Connected with the configured console credential; listed and opened a real synthetic profile.
- Verified attributes, source-scoped identifier and accepted provenance rendered in detail.
- Disconnect and page reload cleared the token and profile/list data.
- Invalid credential returned a readable 401 and cleared the credential.
- Stopped the real API: refresh cleared stale data and displayed a readable non-JSON 504/unavailable response. Restart recovered access.
- Temporarily restarted API with only the importer credential (without changing `.env`): refresh received 401, cleared the active session and displayed reconnect instructions. Restored the original configuration and successfully reconnected.
- Used the separate test-only browser request form for real mutation errors because the product console is read-only: importer ownership conflict 409, stale version 412, invalid scalar 422 and missing If-Match 428. The form forwards to the actual API and is not part of the product UI.

Screenshot evidence remains locally at `.tools/security/browser-profile.png`. The companion container was stopped after verification. The final patched static runtime was restarted and its HTTP/static/proxy behavior checked separately; the application assets are byte-identical to the browser-tested build.

## Image findings and remaining gate

Trivy 0.76.1 downloaded its vulnerability database and scanned image archives without Docker-socket access. No severity exclusions, ignored CVEs or unfixed-finding suppression were used. Initial scans found 56 frontend, 86 PostgreSQL and 291 old Bun-image findings. Updated pinned bases and exact zlib/musl fixes yielded:

| Image | Final findings |
| --- | --- |
| API scratch runtime | 0 |
| Static frontend runtime | 0 |
| Bun 1.4.4 Alpine | 0 |
| PostgreSQL 18 Alpine | 61: 1 critical, 25 high, 31 medium, 3 low, 1 unknown |

The high/critical PostgreSQL findings concern the old Go standard library embedded in `/usr/local/bin/gosu`. The image refresh fixed OS-library findings but not that binary. Reachability was not proven or waived. See the committed [scan inventory](../security/image-scan-2026-11-06.json) for IDs, package/fix versions and image identity; full raw reports and archives remain ignored under `.tools/security`. A newer reviewed upstream image or a reviewed rebuild plus revalidation is required. Scan results are point-in-time, ARM64-only, and not proof of absence of vulnerabilities.

## CI coverage and limits

CI now checks generated sqlc code, formatting, race-enabled Go and migration tests against PostgreSQL, vet, module checksums, govulncheck, frozen frontend install/type-check/build/audit, clean API/frontend/tool image builds, live OpenAPI contracts and all four image scans. Image reports upload even when the gate fails. Relevant commands were executed locally. At this checkpoint, remote GitHub CI and AMD64 scans were still open, and the documented PostgreSQL findings failed the local gate. The follow-up verified the [passing native AMD64 workflow](https://github.com/buelbuel/customer-fabric/actions/runs/42247751238).

The review and design outputs are [security/transaction review](../security/m0-review.md), [lifecycle ADR 0003](../adr/0003-data-lifecycle.md) and [authentication ADR 0004](../adr/0004-authentication-boundaries.md). No deletion feature, M1 feature or production authentication shortcut was introduced.

Remaining production limitations include development-only non-expiring tokens, unrestricted source reads, no customer/operator authorization, plaintext local transport, schema-owner database credentials, indefinite M0 retention and no tested backup restoration/erasure process. The lifecycle ADR is a design, not executed disaster recovery. Version ETags are mutation concurrency tokens, not full enriched-representation cache validators. See the security review for details.

At this checkpoint, the PostgreSQL image gate and remote CI were still open. See the triage follow-up for their current status. Real customer data requires the separate production controls above.

## Follow-up: pre-release cleanup and enterprise fixtures

The subsequent user-requested cleanup removed the original migration-ledger adoption adapter and compatibility-only tests. Goose remains the only migration system; the complete foundation SQL is unchanged. Fresh/repeated/concurrent migration, unexpected-schema non-destruction, Down/Up, rollback and all current API integration tests passed against PostgreSQL. No database reset or destructive command was needed or introduced. Existing local tables/volumes were preserved. The earlier artifact hashes above describe the stabilization checkpoint before this cleanup, not every later API build.

A standalone Go fixture generator and source-specific curated library were added without runtime features or connectors. Executed generation/validation: 100-person regression preset, 10,000-person medium dataset (112,514 deliveries), and 100,000-person large dataset (1,126,801 deliveries). An initial all-in-memory large run was killed by the container memory limit; bounded persistent sort blocks and digest-based validation replaced that approach, after which the large run passed. Failed/earlier generated runs were removed after the final datasets passed; final medium/large datasets remain under ignored `.tools/`.

`make test` passed with race detection, including deterministic generation, anomaly boundaries/distributions, fixture consistency, real HTTP/PostgreSQL fixture scenarios and current goose-only migration tests. The checked-in 12-person miniature validates successfully. The first AJV source-projection check found malformed schema JSON; corrected schema then passed. Live OpenAPI checks remained passing. No fixture data was bulk-imported into the user's database; fixture API tests use isolated schemas. Rich consent, erasure, matching and source-transfer cases remain future expectations, not working M0 features. See `tests/fixtures/enterprise/README.md` and `tests/generator/README.md`.

The fixture checks established functional coverage. The [PostgreSQL triage follow-up](../security/postgres-container-triage.md) records the subsequent security verification.
