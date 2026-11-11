# ADR 0013: Kafka event transport from the PostgreSQL outbox

Status: accepted for synthetic development (maintainer direction of 2026-11-10, #64). Implements the first half of the [event and workflow target](../architecture/eventing-workflows.md): PostgreSQL outbox → relay → Kafka. The Temporal half follows in its own decision.

## Decision

- **The outbox stays the source.** Profile, provenance and outbox rows still commit in one PostgreSQL transaction. Nothing writes to Kafka inside a business transaction; there is no dual write.
- **A relay publishes committed rows at least once.**
  - It runs inside the API process, only when `CF_KAFKA_BROKERS` is set; without it nothing changes.
  - It reads unrelayed rows in a short transaction and publishes them. Only after the brokers acknowledged every event does it record `outbox.relayed_at`.
  - A failed publish checkpoints nothing. A crash between the acknowledgement and the checkpoint republishes the same events, so consumers deduplicate by event ID.
  - Relayed rows are kept, as before.
  - **First enablement publishes history.** Rows written before the relay existed are unrelayed, so the
    first run publishes the whole existing outbox, in batches. That is acceptable for synthetic data.
    A deployment with real history decides on a backfill cut-off first.
  - **The relay never stops the API.** A broker that is down, including at startup, delays relaying;
    events wait in the outbox. Topic setup is retried in the background, and each batch has a one-minute
    deadline, so the relay's transaction and lock are never held indefinitely.
- **Order is per profile, never global.**
  - Records are keyed by profile ID, so a profile's events share a partition.
  - One relay runs at a time per database schema (a transaction advisory lock). A batch always includes every earlier unrelayed version of the profiles it contains, so a profile's events are published in version order.
  - A batch is capped at four times its configured size, in profile and version order. A cut can only leave a profile's later versions for the next batch.
  - A batch republished after a failure repeats its events. Order then holds once duplicates are removed by event ID.
  - The idempotent producer requires acknowledgement from all in-sync replicas and keeps order within a session.
- **Envelope `customer-fabric.event.v1`.** Each event carries only:
  - `id`, the mutation ID, which is stable across republishing;
  - `type`, `tenant`, `source`, `profile_id`, `profile_version` and `occurred_at`.

  It carries **no attribute values or other customer data**. A consumer reads current, authorized state from Customer Fabric. A consumer refuses a `schema` it does not know; a breaking change gets a new schema name and topic.
- **One topic per tenant deployment**: `CF_KAFKA_TOPIC`, default `customer-fabric.profile-events.v1`. `CF_TENANT_ID` names the tenant in every envelope and is required with Kafka.
- **Topic creation.** The relay creates the topic if it is missing: 3 partitions, broker-default replication, `cleanup.policy=delete`, 7-day retention. It never changes an existing topic's settings. Brokers run with automatic topic creation disabled.
- **Client: franz-go** (`github.com/twmb/franz-go` v1.22.2, BSD-3-Clause), a pure-Go client with no cgo. Its dependencies `pkg/kmsg` (BSD-3-Clause), `klauspost/compress` (BSD-3-Clause and Apache-2.0) and `pierrec/lz4` (BSD-3-Clause) are compatible with Customer Fabric's Apache-2.0 license.
- **Broker: Apache Kafka 4.3.2** (Apache-2.0), the official `apache/kafka` image pinned by digest, single node in KRaft mode. It is opt-in for local development through `deploy/compose.events.yaml`:
  - `make events-up` runs the API with the relay;
  - `make events-test` runs the relay tests against the broker.

  Neither `make up` nor `make test` starts Kafka, and the required CI checks do not depend on it.

## Not decided here

- **Production broker security** is not configured locally. That covers authentication, encryption, per-tenant ACLs, quotas and multi-broker replication; the local broker is plaintext on an internal network. It is tracked in #96 and must be in place before any non-synthetic use.
- **Temporal and the consumer side**: consumers that re-check tenant, lifecycle and permission, and durable workflows. They follow in a separate slice of #64.
- **Retention in the privacy register.** Kafka data, consumer offsets and dead-letter topics are data categories for the privacy retention rules (ADR 0003, #28). Envelopes hold identifiers and timestamps, which are pseudonymous personal data, so the 7-day default must be confirmed there.
- **Outbox pruning**: relayed rows are kept, unchanged from before this decision.

## Alternatives considered

- **Publishing from the request transaction:** a dual write that can publish events for rolled-back changes or lose them; rejected by AGENTS.md.
- **Debezium or another CDC connector:** an additional JVM service and connector configuration to operate, for a single table that the application already writes deliberately.
- **Sarama or confluent-kafka-go:** Sarama has a more complex API, and confluent-kafka-go requires cgo and librdkafka.
