# Enterprise simulation fixtures

Fictional retailer **Muster & Marché**, operating in Europe from 2011 through 2026. Everything here is synthetic: reserved `example.com` addresses, invented test streets/postcodes, generated names and source IDs. No vendor credentials, customer exports or network access are needed. These are simulation assumptions, not industry population statistics.

## Layout and isolation

- `curated/`: readable standalone source examples, Salesforce CometD CDC lifecycle examples, a converted Lead, historical ERP Windows-1252 CSV, account-specific Emarsys mapping, and the scenario catalogue.
- `sample/`: a committed 12-person miniature generated with seed 42 and reference date 2026-11-01. This intentionally small example is not the 100-person regression preset.
- `schemas/`: schemas for the simulation envelope and selected source projections. These are **not complete vendor schemas**.
- Larger generated datasets belong in ignored `.tools/`; do not commit them.

Each dataset separates raw `sources/`, `events/`, and `expected/`. `expected/population.ndjson` is the hidden canonical population; `records.ndjson` maps file/line references to people, duplicates and unresolved records; `links.ndjson` distinguishes verified, ambiguous, unsafe, different-person and duplicate-source relationships; `outcomes.ndjson` records expected rejection and privacy scenarios. Ground truth is test-only assertion data. Never give `expected/` to a matcher, connector, source importer or API. Production code imports none of this package. Source payloads contain no canonical truth IDs.

The `identity` source is a fictional internal enrollment export, **not an Ory Kratos API response**. Kratos integration remains future work.

## Population and anomaly model

The default population is 10,000. Configurable weighted cohorts select CRM-only, marketing-only, commerce-only, dormant ERP, multichannel and fragmented populations. Multichannel people have ERP/CRM/commerce plus independently sampled marketing, loyalty and identity coverage. Not every source has the same people. Source launch assumptions are ERP 2011, CRM 2014, Emarsys 2015, Shopify 2017, loyalty 2018, Braze 2020 and identity 2022. These dates describe the fictional retailer, not vendor launch dates.

Each anomaly has its own deterministic hash draw. Rates are **person-level selections**, not promises of an exact percentage of source rows or events. Shared-email/reused-identifier draws use person pairs to model a collision between different people. Effects apply only where the corresponding source exists. Categories overlap; malformed or anonymous records can prevent otherwise observable effects. The manifest records actual selected counts; tests check exact determinism, 0/1 boundaries and distribution tolerance. `duplicate_source` creates extra source records, not extra canonical people. Anonymous Braze records deliberately withhold canonical association.

Examples include divergent emails/names/phones, same-email different people, conflicting source values, stale references, duplicate records, old country/date/encoding conventions, missing external IDs, malformed fields, deleted upstream profiles retained downstream, consent withdrawal lag, partial batches/migrations, duplicate deliveries, retry storms, late/backfill/out-of-order deliveries, owner-change proposals, identifier correction and recreate scenarios. An ownership-conflict expectation is conditional on explicitly directing the competing sources at one profile; generation alone never links them automatically.

Names, cities and languages cover AT/DE/FR/NL/ES/IT/PL/GB. Phones deliberately use the [Ofcom drama range](https://www.ofcom.org.uk/phones-and-broadband/phone-numbers/numbers-for-drama), formatted internationally or locally. They are reused and must never be treated as unique identifiers or dialed. Test addresses are intentionally non-deliverable. This initial library does not claim geographically valid postal or telephone datasets for every European country.

## Vendor fidelity

| Source | Selected convention and documentation | Deliberate limits/customization |
| --- | --- | --- |
| Salesforce | REST sObject projection `/services/data/v65.0/`; 15-character Contact/Lead/Account/User IDs; [CDC body](https://developer.salesforce.com/docs/platform/change-data-capture/guide/cdc-event-fields-body.html), [header](https://developer.salesforce.com/docs/platform/change-data-capture/guide/cdc-event-fields-header.html) | CometD JSON transport only. UPDATE includes changed values; explicit null means clear, absence means unchanged. No invented Pub/Sub Avro bitmap decoding or `nulledFields`/`diffFields` on CometD. Replay/schema/transaction identifiers are fictional examples, not usable subscriptions. Lead conversion is preserved as evidence, not automatic merging. |
| Braze | [`POST /users/export/ids`](https://braze.com/docs/api/endpoints/export/user_data/post_users_identifier) user-export envelope; [export purchase summaries](https://www.braze.com/docs/api/endpoints/export/user_data/post_users_segment) | Identified and alias/anonymous profiles; subscriptions retain vendor states. CRM references and loyalty attributes are fictional custom attributes. Purchase summaries use name/first/last/count, not purchase-ingestion objects. `subscribed` is not relabeled as explicit consent. |
| Shopify | [GraphQL Admin 2026-01 customers](https://shopify.dev/docs/api/admin-graphql/2026-01/queries/customers), [Customer](https://shopify.dev/docs/api/admin-graphql/2026-01/objects/Customer) | GraphQL `data.customers.nodes` projection, GIDs, string order counts/Money values, default email marketing state. Disabled accounts can represent guest purchasers. No live Shopify scope or API validation was performed. |
| SAP Emarsys | API v2 contact request family, `key_id` and numeric field keys; [SAP input guidance](https://learning.sap.com/courses/implementing-data-inputs-and-integrations-in-sap-engagement-cloud-emarsys-edition/handling-essential-data-input), [contact-data reference](https://dev.emarsys.com/docs/emarsys-api/1390c2e1ee412-get-contact-data) | Files represent bulk **request** bodies, not invented response envelopes. IDs 9001–9003 and their indexed-key/consent/import semantics are fictional account configuration. The detailed developer-hub page was inaccessible during review; no live endpoint conformance or error-code claim is made. |
| ERP / loyalty | Fictional source specifications defined by these fixtures | Historical source formats are intentionally retained; they do not require compatibility with old Customer Fabric database schemas. |

Vendor examples are selected-field projections, not exhaustive vendor records. Historical event envelopes are our own simulation format; they are not Salesforce CDC, Braze Currents or Shopify webhooks. Only explicitly named curated CDC files model the CometD transport.

## Time, replay and privacy

`events/occurrence.ndjson` is sorted by occurrence time; `delivery.ndjson` by delivery time. Both contain the same deliveries. `event_id` identifies logical intent, `delivery_id` a delivery attempt; retries preserve intent. `source_sequence` is a simulation sequence **within a source record**, not a vendor replay cursor. The original `by-record.ndjson` and bounded sorted `blocks/` are retained for inspection and reproducible merging. No host temporary directory is used. Default replay streams the delivery file; shuffled replay uses a deterministic ordering and materializes events in memory, so use default replay for large performance runs.

Snapshot references point at source export fixtures. This is a scenario/event-ordering simulator, not an event-sourced, field-complete reconstruction of every historical vendor record. Changed-value examples and richer CDC samples are retained separately. Event-time precedence, consent enforcement, lifecycle fences and source ownership transfer are not implemented in Customer Fabric M0.

Privacy expectations are purpose-specific: a withdrawn email-marketing permission means suppress that marketing use, not automatically deny order fulfillment or every processing purpose. Erasure/downstream suppression/retention-review expectations are design test inputs; M0 does not enforce them. See ADR 0003 before implementing lifecycle replay.

## Supported testing boundary

The test-only normalizer flattens supported string attributes and explicit source IDs into scalar M0 mutations. It rejects malformed/missing identifiers, anonymous Braze records and deleted Salesforce records, and refuses identity-provider normalization. It does not import consent as authorization, follow CRM pointers, select targets by email, transfer ownership or erase profiles. `ExplicitTarget` is a fixture-driver guard requiring an independently supplied verified target, not a production matcher or authorization service.

Real HTTP/PostgreSQL integration tests assert explicit identifier linking, source ownership rejection/provenance, duplicate delivery, replay after later writes, stale-version backfill rejection and partial imports. They use disposable test schemas, never the user's existing profile tables. Fixture consistency tests separately assert source relationships, configured rates, chronological/delivery ordering and duplicate-event identity. `Evaluate` reports false-positive merges separately from missed verified matches and unknown predictions; false positives must be a release-blocking metric for any future matching engine. No matching-quality claim is made today.

See [generator commands](../../generator/README.md) and the curated `scenarios.json` for supported versus future cases.
