-- name: AllowRetentionPurge :exec
-- Declares this transaction a retention purge, for the append-only guards that admit one.
SELECT set_config('customer_fabric.retention_purge', 'on', true);

-- name: PruneOutbox :execrows
-- Outbox rows older than the horizon: relayed ones, or any when no relay is configured, since those are
-- never published. Unrelayed rows wait for a configured relay however old they are.
DELETE FROM outbox WHERE id IN (
  SELECT id FROM outbox WHERE occurred_at < sqlc.arg(horizon)::timestamptz AND (relayed_at IS NOT NULL OR NOT sqlc.arg(relay)::boolean)
  ORDER BY occurred_at LIMIT sqlc.arg(row_limit)::int);

-- name: PruneNotificationOutcomes :execrows
-- Notification outcomes older than the horizon, except a subscription's newest delivered version of a
-- record that no live withdrawal notice covers yet: that row is what holds the record (ADR 0014).
DELETE FROM notification_deliveries WHERE (event_id, subscription_id) IN (
  SELECT d.event_id, d.subscription_id FROM notification_deliveries d
  WHERE d.recorded_at < sqlc.arg(horizon)::timestamptz
    AND NOT (d.outcome = 'delivered'
      AND d.profile_version = (SELECT max(n.profile_version) FROM notification_deliveries n
        WHERE n.subscription_id = d.subscription_id AND n.profile_id = d.profile_id AND n.outcome = 'delivered')
      AND NOT EXISTS (SELECT 1 FROM notification_withdrawals w WHERE w.subscription_id = d.subscription_id
        AND w.profile_id = d.profile_id AND w.state <> 'cancelled' AND w.held_version >= d.profile_version))
  ORDER BY d.recorded_at LIMIT sqlc.arg(row_limit)::int);

-- name: PrunableWithdrawals :many
-- Settled notices older than the horizon. A delivered one goes only once no delivered outcome it covers
-- remains, so the record it closed is never held again; pending and failed notices always stay.
SELECT w.id FROM notification_withdrawals w
WHERE w.state IN ('delivered','cancelled') AND w.settled_at < sqlc.arg(horizon)::timestamptz
  AND (w.state = 'cancelled' OR NOT EXISTS (SELECT 1 FROM notification_deliveries d WHERE d.subscription_id = w.subscription_id
    AND d.profile_id = w.profile_id AND d.outcome = 'delivered' AND d.profile_version <= w.held_version))
ORDER BY w.settled_at LIMIT sqlc.arg(row_limit)::int;

-- name: DeleteWithdrawalAttempts :exec
DELETE FROM notification_withdrawal_attempts WHERE withdrawal_id = ANY(sqlc.arg(ids)::uuid[]);

-- name: DeleteWithdrawals :execrows
DELETE FROM notification_withdrawals WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: PrunableRecoveries :many
-- Recovery requests older than the horizon with nothing still queued. Their items, which keep an event
-- from being recovered twice, concern events long past the recovery window.
SELECT r.id FROM notification_recoveries r
WHERE r.requested_at < sqlc.arg(horizon)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM notification_recovery_items i WHERE i.recovery_id = r.id AND i.state = 'queued')
ORDER BY r.requested_at LIMIT sqlc.arg(row_limit)::int;

-- name: DeleteRecoveryItems :execrows
DELETE FROM notification_recovery_items WHERE recovery_id = ANY(sqlc.arg(ids)::uuid[]);

-- name: DeleteRecoveries :execrows
DELETE FROM notification_recoveries WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: InsertRetentionPurge :exec
INSERT INTO retention_purges (id, category, horizon, purged) VALUES ($1, $2, $3, $4);

-- name: RetentionOldest :many
-- The oldest row each category still holds; an empty category has no row.
SELECT 'outbox'::text AS category, min(occurred_at)::timestamptz AS oldest FROM outbox HAVING count(*) > 0
UNION ALL SELECT 'notification_outcomes', min(recorded_at) FROM notification_deliveries HAVING count(*) > 0
UNION ALL SELECT 'withdrawal_notices', min(created_at) FROM notification_withdrawals HAVING count(*) > 0
UNION ALL SELECT 'notification_recoveries', min(requested_at) FROM notification_recoveries HAVING count(*) > 0;

-- name: LatestRetentionPurges :many
SELECT DISTINCT ON (category) * FROM retention_purges ORDER BY category, ran_at DESC;

-- name: ListRetentionPurges :many
SELECT * FROM retention_purges ORDER BY ran_at DESC, id LIMIT 100;
