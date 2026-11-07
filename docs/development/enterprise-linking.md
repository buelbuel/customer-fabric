# Verify enterprise linking locally

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
