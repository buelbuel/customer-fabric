-- name: InsertDestination :one
INSERT INTO destinations (name, url, signing_secret) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING RETURNING created_at;

-- name: ListDestinations :many
SELECT name, url, created_at FROM destinations ORDER BY name;

-- name: GetDestination :one
SELECT * FROM destinations WHERE name = $1;

-- name: GetSegmentRevision :one
SELECT * FROM segment_definitions WHERE name = $1 AND revision = $2;

-- name: InsertActivation :one
INSERT INTO activations (id, segment_name, segment_revision, destination, purpose_id) VALUES ($1, $2, $3, $4, $5) RETURNING created_at;

-- name: InsertDelivery :exec
INSERT INTO activation_deliveries (id, activation_id, anchor_profile_id) VALUES ($1, $2, $3);

-- name: GetActivation :one
SELECT * FROM activations WHERE id = $1;

-- name: ListActivationDeliveries :many
SELECT id, anchor_profile_id, state, attempts, reason, updated_at FROM activation_deliveries
WHERE activation_id = $1 ORDER BY anchor_profile_id LIMIT sqlc.arg(row_limit)::int;

-- name: ListActivationReceipts :many
SELECT r.* FROM delivery_receipts r JOIN activation_deliveries d ON d.id = r.delivery_id
WHERE d.activation_id = $1 ORDER BY r.delivery_id, r.attempt;

-- name: ClaimDueDeliveries :many
-- Leases due deliveries to one dispatcher. A crashed dispatcher's lease expires and the delivery is
-- retried with the same ID, which the receiver uses as its idempotency key.
UPDATE activation_deliveries d SET attempts = d.attempts + 1, next_attempt_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int), updated_at = now()
FROM activations a
WHERE d.activation_id = a.id AND d.id IN (
  SELECT id FROM activation_deliveries WHERE state = 'pending' AND next_attempt_at <= now()
  ORDER BY next_attempt_at, id LIMIT sqlc.arg(row_limit)::int FOR UPDATE SKIP LOCKED)
RETURNING d.id, d.activation_id, d.anchor_profile_id, d.attempts, a.segment_name, a.segment_revision, a.destination, a.purpose_id;

-- name: SettleDelivery :execrows
-- Applies only while the dispatcher still holds the claim it made at this attempt.
UPDATE activation_deliveries SET state = sqlc.arg(state), reason = sqlc.arg(reason), next_attempt_at = sqlc.arg(next_attempt_at), updated_at = now()
WHERE id = sqlc.arg(id) AND attempts = sqlc.arg(attempt) AND state = 'pending';

-- name: InsertReceipt :exec
INSERT INTO delivery_receipts (delivery_id, attempt, outcome, reason, status_code) VALUES ($1, $2, $3, $4, $5);
