-- name: TryRelayLock :one
-- One relay at a time per database schema, so each profile's events reach Kafka in version order.
SELECT pg_try_advisory_xact_lock(hashtext('outbox-relay:' || current_schema()));

-- name: PendingOutbox :many
-- The oldest unrelayed rows, widened to every earlier unrelayed version of the same profiles: a later
-- version can commit with an earlier occurred_at (its transaction started first and waited on the
-- profile lock), and must never be published ahead of the version it followed.
WITH picked AS (
  SELECT c.profile_id, max(c.version) AS version FROM (
    SELECT profile_id, version FROM outbox WHERE relayed_at IS NULL ORDER BY occurred_at, id LIMIT sqlc.arg(row_limit)::int) c
  GROUP BY c.profile_id)
SELECT o.id, o.type, o.profile_id, o.version, o.source, o.occurred_at FROM outbox o
JOIN picked p ON p.profile_id = o.profile_id AND o.version <= p.version
WHERE o.relayed_at IS NULL ORDER BY o.profile_id, o.version;

-- name: MarkOutboxRelayed :execrows
UPDATE outbox SET relayed_at = now() WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND relayed_at IS NULL;

