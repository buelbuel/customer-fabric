# PostgreSQL container security triage

Reviewed 2026-11-06. The rebuilt PostgreSQL image clears all 26 critical and high findings from the M0 scan. The local integration and image gates pass. The remaining finding affects a Windows-only function and stays in the scan report.

## Baseline and source of findings

The deployment used `postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395`. The explicit `18.6-alpine3.24` tag resolves to the same index. The tested Linux ARM64 manifest was `sha256:c935aff069a7bbe5c286411a08e9678850d79be7848b190a0e4637b266478626`. It contains PostgreSQL 18.6 on Alpine 3.24.4 with gosu 1.19 built using Go 1.24.6.

Trivy `0.76.1` at image digest `sha256:790fd66d1697395d8693d1acaa1ea702406a20f109b6fc7bac8db623eca6c06c` ran all local comparisons against vulnerability database schema 2, updated 2026-11-06 at 09:50 UTC. The scan found 59 Go standard library findings, one Moby user parser finding and one x/sys finding. All were in `/usr/local/bin/gosu`. The Alpine package scan found zero. See the [per-CVE appendix](postgres-baseline-findings.md) for installed and fixed versions, sources, severity and exploit prerequisites.

PostgreSQL handles SQL traffic after gosu has switched to the fixed `postgres` account and replaced itself. The critical TLS finding requires session resumption, which is absent from the helper's runtime path. The Moby parser is called at startup. Exploiting that denial of service requires control over the container's account files, which the deployment leaves root-owned and outside the database mount. See the [Go TLS advisory](https://pkg.go.dev/vuln/GO-2026-4337) and [Moby user parser advisory](https://github.com/moby/sys/security/advisories/GHSA-mjcv-p78q-w5fw).

The Alpine image builds PostgreSQL from source. APK inventory alone cannot verify the engine, so its version was checked against PostgreSQL's own server and client advisories. As of the review date, 18.6 is the current 18.x minor and covers the published PostgreSQL 18 fixes. [PostgreSQL version policy](https://www.postgresql.org/support/versioning/) · [PostgreSQL 18 advisories](https://www.postgresql.org/support/security/18/)

## Official image comparison

All four Docker Official Image variants were pulled and scanned as Linux ARM64 archives. Each bundled gosu 1.19 with Go 1.24.6 and produced the same 61 helper findings. The table separates the findings in OS packages from those in the bundled helper. Vendor databases and backport policies differ across distributions, so counts alone do not establish relative security. [Docker Official Image tags and source](https://hub.docker.com/_/postgres)

| PostgreSQL 18.6 tag | Image index SHA-256 | OS | OS findings (critical / high / total) | Compatibility consideration |
| --- | --- | --- | ---: | --- |
| `18.6-alpine3.24` | `123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395` | Alpine 3.24.4 | 0 / 0 / 0 | Exact existing base, postgres UID 70 |
| `18.6-alpine3.23` | `a0cbdbbcc57f7317ca183ac9d10eb3af72b5b05b2f25acad58e765bd2b139099` | Alpine 3.23.8 | 0 / 0 / 0 | Older musl and OS package set, same helper |
| `18.6-trixie` | `bd5dfa494a5a259704365813dbfa5fa6b3e69be7348d43b547f540c730478583` | Debian 13.7 | 1 / 66 / 336 | glibc and locale change, postgres UID 999 |
| `18.6-bookworm` | `f1ea2a1a4fb7b68d4836be1fdcb8a6466a89b072158bf9d97663600538c294c5` | Debian 12.15 | 3 / 50 / 397 | glibc and locale change, postgres UID 999 |

All use the PostgreSQL 18 data directory under `/var/lib/postgresql/18/docker`. A Debian switch would additionally require verifying collation behavior, extension ABI and ownership on existing volumes. The published gosu 1.20 binary fixes the Moby parser with `v0.4.1` but still uses Go 1.24.6. The Alpine 3.24 community package `gosu-1.19-r6` uses Go 1.26.9 and the older Moby parser. Go 1.26.9 predates the X.509 fix in 1.26.10 and 1.27.3. [Gosu 1.20 release](https://github.com/tianon/gosu/releases/tag/1.20) · [Alpine package recipe](https://raw.githubusercontent.com/alpinelinux/aports/3.24-stable/community/gosu/APKBUILD) · [Go release history](https://go.dev/doc/devel/release)

## Remediation and residual finding

Compose builds `customer-fabric-db` from the exact PostgreSQL base above. The only runtime binary replaced is gosu. Its release source is pinned at Go module version `v0.0.0-20261025094355-29b939aa5bd4` and fetched with Go checksum database verification. The build uses the digest-pinned Go 1.27.3 image and disables automatic toolchain switching. It retains gosu module metadata for scanning and tests privilege drop at image build time. The runtime inherits the official entrypoint and PostgreSQL binaries, packages, locale libraries and volume behavior. The upstream helper bytes remain in the lower base layer. The replacement is the only gosu executable at that path in the merged runtime filesystem. [Pinned gosu source](https://github.com/tianon/gosu/tree/29b939aa5bd4dc588af765b8248f6e39150d2565) · [Dockerfile](../../deploy/postgres/Dockerfile)

The local ARM64 image scanned under Trivy image ID `sha256:74b916b7531ac3cf3961a17c3a26752d6616b38136b994822968be107b7925f9`. PostgreSQL yielded 0 critical, 0 high, 0 medium, 0 low and 1 unknown. The scan gate checked all severities and failed on any high or critical finding, including unfixed findings. The API, admin production and Bun images each yielded zero findings. No CVEs were ignored. Raw local comparison reports are under `.tools/security/postgres-triage/` and gate reports under `.tools/security/ci-*.json`.

| Remaining CVE | Package and versions | Severity and source | Applicability, mitigation and review |
| --- | --- | --- | --- |
| CVE-2026-39824 | `golang.org/x/sys` v0.1.0. Fix available in v0.44.0. | UNKNOWN in Trivy, Go Vulnerability Database. [GO-2026-5024](https://pkg.go.dev/vuln/GO-2026-5024) | The affected `windows.NewNTUnicodeString` function is Windows-only. Both the deployment and the gosu build are Linux and use the Unix implementation. Retain Linux targets and the upstream dependency graph. Reviewed 2026-11-06. Re-evaluate at the next PostgreSQL or gosu refresh, if the advisory expands to Linux, or if build targets change. |

Review the image pins and advisories on the next PostgreSQL 18 security release, on upstream gosu or Go releases, and whenever a new high or critical finding appears. Return to a maintained official image when it provides equivalent coverage and passes the same migration, startup and scan checks.

## Verification and readiness

| Check | Result |
| --- | --- |
| Fresh ARM64 build and image scan | Passed with the pinned inputs. Two clean builds produced gosu SHA-256 `a3a9a1543402754ed4214e0f672f62646d0d0e5f5504a285bca7831f9e6291eb`. Startup reports gosu 1.20 and Go 1.27.3. |
| Existing volume | Same named volume and cluster system identifier after replacement. PostgreSQL 18.6, UTF8, `en_US.utf8` collation and ctype, `plpgsql` 1.0, PGDATA and pre-smoke profile count matched. |
| Fresh database | Isolated memory-backed initialization and SQL query passed. PostgreSQL ran as the `postgres` user at PID 1. |
| `make test` | Passed sqlc generated-code diff, Go race tests, real PostgreSQL integration and migration tests, fixture tests and frontend checks. |
| `make migrate` | Passed against the existing database. |
| `make contracts` and `make smoke` | Passed live API contract and synthetic-data smoke checks. |
| Local AMD64 build | The Colima classic builder rejected the cross-platform stage copy. The native GitHub Actions AMD64 build and scan passed with the same residual finding. The artifact identifies image `sha256:45a6f85d0252b5569787328df7584a7ab33fc0eaec9354783c67961177cfd679`. [Native AMD64 scan artifact](https://github.com/buelbuel/customer-fabric/actions/runs/42247384626/artifacts/13517987906) |
| GitHub Actions | Backend, admin and contracts-and-images all passed on `f08a8f5`. The first push surfaced an existing Go formatting failure. The follow-up formatted the integration test and guarded Compose cleanup when initialization had not run. [Passing workflow](https://github.com/buelbuel/customer-fabric/actions/runs/42247751238) |

M0 meets its local functional criteria and the PostgreSQL scan gate. M1 development can proceed in the existing isolated development setup with synthetic data. Production-security readiness remains open. The current API uses development credentials and permissions, and production authentication, database least privilege, encrypted transport, and tested backup and data lifecycle controls still require verification. [M0 security review](m0-review.md)
