# Dependencies and licensing

Original project code: Apache-2.0. See LICENSE and NOTICE. Dependencies retain their upstream licenses; no dependency is relicensed by this repository.

## Direct choices

| Component | License | Reason |
| --- | --- | --- |
| Go standard library | BSD-3-Clause | HTTP, JSON, validation, logging, crypto primitives, shutdown |
| pgx v5 | MIT | Maintained native PostgreSQL driver, explicit transactions and pooling |
| PostgreSQL | PostgreSQL License | Authoritative transactional storage, JSONB, constraints, advisory locks |
| goose v3 | MIT | Versioned transactional SQL migrations and built-in PostgreSQL locking |
| sqlc (build tool only) | MIT | Generate typed pgx methods from explicit SQL; no runtime dependency |
| React / React DOM | MIT | Small conventional read-only operator interface |
| Vite | MIT | Local dev proxy and static frontend build; no runtime SaaS dependency |
| TypeScript | Apache-2.0 | Compile-time UI contract/type checks |
| Bun | MIT (plus bundled third-party notices) | Requested frontend package manager and tooling runtime |

Go migration dependencies also include interpolate, go-retry and multierr (MIT). Goose raises the minimum Go version to 1.26. SQLite-related checksums originate from goose's upstream tests; SQLite is not linked into or used by the API.

Go indirect dependencies: pgpassfile, pgservicefile and puddle (MIT); golang.org/x/sync and x/text (BSD-3-Clause). Test-only dependency licenses remain in their modules. Frontend type packages use MIT; transitive build dependencies are resolved in bun.lock and carry their own distribution notices. Container base images also bundle software with separate licenses. Distributors must preserve applicable upstream notices and verify the final shipped artifact's inventory.

The dependency set does not impose proprietary hosting or runtime registration. No job queue has been adopted. M3 targets Kafka and Temporal under [#64](https://github.com/buelbuel/customer-fabric/issues/64), which also owns license and self-hosting assessment. No Kratos, Kafka, Temporal, Hydra or ClickHouse is deployed in M0.

Sources: [pgx repository](https://github.com/jackc/pgx), [PostgreSQL license](https://www.postgresql.org/about/licence/), [React](https://github.com/facebook/react/blob/main/LICENSE), [Vite](https://github.com/vitejs/vite/blob/main/LICENSE), [Bun](https://github.com/oven-sh/bun/blob/main/LICENSE.md).

Maintenance: commit go.sum and bun.lock; use frozen frontend installs. Review upstream release notes and run vulnerability checks on changes. Image tags pin tool versions except the PostgreSQL 18 maintenance line; release deployments should lock reviewed image digests and automate security updates. Compatibility and package audits alone do not establish production security.
