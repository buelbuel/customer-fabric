# Deterministic enterprise generator

Implementation: `backend/tests/enterprise`, CLI: `backend/tests/generator`. Standard-library Go; no new module, service, ORM, connector or matching engine. `config.json` is explicit JSON rather than YAML to avoid another parser dependency.

From the repository root (Colima/Docker, no host Go required):

```sh
make fixtures                              # 100 people -> .tools/enterprise-small
make fixtures FIXTURE_PRESET=medium         # 10,000 people
make fixtures FIXTURE_PRESET=large          # 100,000 people
make check-fixtures                        # validate the committed miniature
```

Output directories **must not exist**. To rerun, choose a new directory using `FIXTURE_OUT=../.tools/my-run`. There is no reset/overwrite flag and no database connection in the generator. Failed runs leave inspectable partial files; only a completed run has a final manifest and passes validation. Normal startup/migration does not delete them.

With the Compose Go tool:

```sh
docker compose --env-file .env -f deploy/compose.yaml run --rm go \
  go run ./tests/generator -config ../tests/generator/config.json -out ../.tools/custom-run

docker compose --env-file .env -f deploy/compose.yaml run --rm go \
  go run ./tests/generator -validate ../.tools/custom-run

docker compose --env-file .env -f deploy/compose.yaml run --rm go \
  go run ./tests/generator -replay ../.tools/custom-run/events/delivery.ndjson
```

Change seed/reference date/cohort probabilities/anomaly rates in a copied config. The initial reference-date contract accepts dates in 2026; history starts in 2011. Presets are 100/10,000/100,000; `-population` overrides a preset (maximum 1,000,000; sizes above 100,000 are not verified). Cohort probabilities sum to one. Unlisted anomaly keys default to zero when an explicit anomaly map is provided. Unknown config fields/cohorts/anomalies and invalid rates are rejected.

`-replay ... -shuffled` uses a fixed replay seed of 42; it prints events only and cannot submit mutations. Normal replay is streaming. Shuffled replay materializes events and is intended for small/medium tests. Source snapshots stream to disk; deterministic external event sorting uses 4,096-event persistent blocks and at most 64 merge readers. Validation retains reference/index digests, not full event bodies. Generation uses no system clock except test-directory isolation; artifacts depend on config and generator version.

Regenerate curated fixtures with `-curated ../.tools/curated-review`. Regenerate the committed miniature with `-population 12 -out ../.tools/sample-review`. Review diffs against `tests/fixtures/enterprise/{curated,sample}` before copying intentional updates. Do not blindly overwrite the checked-in library. No vendor calls or database changes occur.

Tests:

```sh
make test             # generator tests plus real PostgreSQL/API tests
make contracts        # OpenAPI/live API checks plus AJV fixture projection checks
make check-fixtures
```

Raw source projections and normalization are tested independently of API integration. Ground truth under `expected/` is assertion-only, not input to normalization or a future matcher. See the [fixture guide](../fixtures/enterprise/README.md) for exact source conventions and functional limits.
