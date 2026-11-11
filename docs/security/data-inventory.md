# Data inventory: events, workflows and external notifications

This register lists where the event and workflow path ([ADR 0013](../adr/0013-event-transport.md), [ADR 0014](../adr/0014-temporal-workflows.md)) keeps or sends personal data. It is input to the retention and erasure work (#28, #27), the backup and recovery scope (#31) and a data-flow assessment. It must be kept current as stores are added. Identifiers in it (event, record, profile and mutation IDs) are pseudonymous personal data, not anonymous.

| Store or flow | Holds | Personal data | Retention today | Erasure today | Backup and recovery |
|---|---|---|---|---|---|
| `outbox` (PostgreSQL) | One row per committed profile change: event, profile and mutation IDs, source, version, time, `relayed_at` | Pseudonymous identifiers | Indefinite; never pruned (#28) | Not erased with a profile yet (#27) | In the tenant database's backups |
| Kafka topic `customer-fabric.profile-events.v1` | The event envelope, keyed by profile ID | Pseudonymous identifiers | 7 days (`retention.ms`), set when the relay creates the topic | Only by retention; compaction or tombstones are no erasure guarantee | Broker log directories; not covered by Customer Fabric backups |
| Kafka consumer-group offsets | Positions only | None | Broker default | — | Broker |
| Temporal workflow histories and visibility | Workflow input (the envelope), activity results (outcome codes, status codes), workflow IDs that contain the event ID | Pseudonymous identifiers | The namespace's retention; the local development server keeps everything in memory | Only by retention | Temporal's persistence; production persistence is not decided (#97) |
| `notification_subscriptions` (PostgreSQL) | Destination, purpose, recipient role, event types, data categories, sources, state, the creating operator | Operator identifier | Indefinite (configuration and authorization evidence) | — | Tenant database backups |
| `notification_deliveries` (PostgreSQL) | One outcome per event and subscription: event and profile IDs, version, outcome, reason code, attempts, status code | Pseudonymous identifiers | Indefinite (#28) | Not erased with a profile yet (#27) | Tenant database backups |
| `consent_authorities` (PostgreSQL) | Which source may report consent for which purpose, the controller, approved methods, the approving and revoking operators, state | Operator identifier | Indefinite (authorization evidence) | — | Tenant database backups |
| Source consent decisions (`consent_evidence`, ADR 0015) | Decision, policy version, collection time, method or channel, the source's receipt reference and, for grants, the notice reference, a SHA-256 digest of the proof and the source's epoch | Pseudonymous identifiers and receipt references; the proof itself stays with the source | Indefinite, append-only (#28) | Not erased with a record yet (#27) | Tenant database backups |
| External recipients (subscribed destinations) | The envelope, signed, with the event ID as `Idempotency-Key` | Pseudonymous identifiers | Decided by the recipient's contract | No deletion from recipients is claimed; no correction or withdrawal protocol exists yet | Outside Customer Fabric |
| Application logs | Event IDs, partition and offset numbers, reason codes and error types | Pseudonymous identifiers | Deployment's log retention | Only by log retention | Deployment's logs |

Never in any of these: attribute values, names, contact details, tokens or payloads of source writes.

**Before real customer data:** the recipient contracts (controller or processor terms, retention, deduplication of at-least-once delivery, handling of withdrawn authorization), the Kafka and Temporal retention values and the outbox and delivery pruning must be decided and enforced (#28), together with the other release gates.
