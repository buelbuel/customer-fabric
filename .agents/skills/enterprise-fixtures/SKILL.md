---
name: enterprise-fixtures
description: Generate, update, or validate deterministic enterprise test fixtures for Customer Fabric.
---

Read the [fixture library](../../../tests/fixtures/enterprise/README.md) and [generator guide](../../../tests/generator/README.md). Extend the generator for reusable scenarios; use a stable seed and explicit expected outcomes. Never derive records from real customer exports.

Run `make fixtures FIXTURE_PRESET=small` for an ignored local dataset, then `make check-fixtures` for the committed sample. Keep large generated sets in `.tools/`. Run `make test` for persistence behavior and `make contracts` for live fixture assertions after changing scenario coverage. Review fixture diffs for determinism and scope before staging.
