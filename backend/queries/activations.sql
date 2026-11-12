-- name: InsertDestination :one
-- A new destination's secret is sealed in destination_secrets; it never has a plaintext secret.
INSERT INTO destinations (name, url) VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING created_at;

-- name: LockDestination :one
SELECT * FROM destinations WHERE name = $1 FOR UPDATE;

-- name: RegisterSealingKey :exec
INSERT INTO sealing_keys (id, check_value) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: SealingKeyCheck :one
SELECT check_value FROM sealing_keys WHERE id = $1;

-- name: InsertDestinationSecret :exec
INSERT INTO destination_secrets (destination, version, key_id, sealed) VALUES ($1, $2, $3, $4);

-- name: LatestDestinationSecretVersion :one
SELECT COALESCE(max(version), 0)::integer FROM destination_secrets WHERE destination = $1;

-- name: RetireDestinationSecrets :exec
-- Rotation: the newest secret signs until retires_at; every older one is erased now.
UPDATE destination_secrets SET
  retires_at = CASE WHEN version = sqlc.arg(newest) THEN least(retires_at, sqlc.arg(retires_at)::timestamptz)
                    ELSE least(retires_at, now()) END,
  sealed = CASE WHEN version = sqlc.arg(newest) THEN sealed END,
  key_id = CASE WHEN version = sqlc.arg(newest) THEN key_id END,
  erased_at = CASE WHEN version = sqlc.arg(newest) THEN erased_at ELSE coalesce(erased_at, now()) END
WHERE destination = sqlc.arg(destination) AND (sealed IS NOT NULL OR retires_at IS NULL);

-- name: LiveDestinationSecrets :many
-- Newest first. Only these may sign a send once a destination has any sealed secret.
SELECT version, key_id, sealed FROM destination_secrets
WHERE destination = $1 AND sealed IS NOT NULL AND (retires_at IS NULL OR retires_at > now())
ORDER BY version DESC;

-- name: DestinationHasSealedSecret :one
SELECT EXISTS (SELECT 1 FROM destination_secrets WHERE destination = $1);

-- name: EraseRetiredDestinationSecrets :execrows
UPDATE destination_secrets SET sealed = NULL, key_id = NULL, erased_at = now()
WHERE sealed IS NOT NULL AND retires_at <= now();

-- name: ClaimSecretsToReseal :many
SELECT destination, version, key_id, sealed FROM destination_secrets
WHERE sealed IS NOT NULL AND key_id <> sqlc.arg(key_id) AND NOT (destination = ANY(sqlc.arg(skip)::text[]))
ORDER BY destination, version LIMIT sqlc.arg(row_limit) FOR UPDATE SKIP LOCKED;

-- name: ResealDestinationSecret :exec
UPDATE destination_secrets SET key_id = $3, sealed = $4 WHERE destination = $1 AND version = $2 AND sealed IS NOT NULL;

-- name: ClaimPlaintextDestinations :many
-- Destinations whose plaintext secret has not been sealed yet.
SELECT d.name, d.signing_secret FROM destinations d
WHERE d.signing_secret IS NOT NULL AND NOT EXISTS (SELECT 1 FROM destination_secrets s WHERE s.destination = d.name)
ORDER BY d.name LIMIT $1 FOR UPDATE OF d SKIP LOCKED;

-- name: ClearPlaintextSecret :exec
UPDATE destinations SET signing_secret = NULL WHERE name = $1;

-- name: RestorePlaintextSecret :exec
UPDATE destinations SET signing_secret = $2 WHERE name = $1 AND signing_secret IS NULL;

-- name: DestinationSecretStates :many
SELECT d.name, (d.signing_secret IS NOT NULL)::boolean AS has_plaintext,
  EXISTS (SELECT 1 FROM destination_secrets s WHERE s.destination = d.name)::boolean AS has_sealed
FROM destinations d ORDER BY d.name;

-- name: ListDestinations :many
SELECT name, url, created_at FROM destinations ORDER BY name;

-- name: GetDestination :one
SELECT * FROM destinations WHERE name = $1;

-- name: GetSegmentRevision :one
SELECT * FROM segment_definitions WHERE name = $1 AND revision = $2;

-- name: InsertActivation :one
INSERT INTO activations (id, segment_name, segment_revision, destination, purpose_id) VALUES ($1, $2, $3, $4, $5) RETURNING created_at;


-- name: GetActivation :one
SELECT * FROM activations WHERE id = $1;

-- name: ListActivationDeliveries :many
-- One page of an activation's deliveries, by anchor keyset.
SELECT id, anchor_profile_id, state, attempts, redrives, reason, updated_at FROM activation_deliveries
WHERE activation_id = sqlc.arg(activation_id)::uuid AND anchor_profile_id > sqlc.arg(after)::uuid
ORDER BY anchor_profile_id LIMIT sqlc.arg(row_limit)::int;

-- name: ListActivationReceipts :many
SELECT r.* FROM delivery_receipts r WHERE r.delivery_id = ANY(sqlc.arg(delivery_ids)::uuid[]) ORDER BY r.delivery_id, r.attempt;

-- name: ClaimDueDeliveries :many
-- Leases due deliveries to one dispatcher. A crashed dispatcher's lease expires and the delivery is
-- retried with the same ID, which the receiver uses as its idempotency key.
UPDATE activation_deliveries d SET attempts = d.attempts + 1, next_attempt_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int), updated_at = now()
FROM activations a
WHERE d.activation_id = a.id AND d.id IN (
  SELECT id FROM activation_deliveries WHERE state = 'pending' AND next_attempt_at <= now()
  ORDER BY next_attempt_at, id LIMIT sqlc.arg(row_limit)::int FOR UPDATE SKIP LOCKED)
RETURNING d.id, d.activation_id, d.anchor_profile_id, d.attempts, d.attempt_base, a.segment_name, a.segment_revision, a.destination, a.purpose_id;

-- name: SettleDelivery :execrows
-- Applies only while the dispatcher still holds the claim it made at this attempt.
UPDATE activation_deliveries SET state = sqlc.arg(state), reason = sqlc.arg(reason), next_attempt_at = sqlc.arg(next_attempt_at), updated_at = now()
WHERE id = sqlc.arg(id) AND attempts = sqlc.arg(attempt) AND state = 'pending';

-- name: InsertReceipt :exec
INSERT INTO delivery_receipts (delivery_id, attempt, outcome, reason, status_code) VALUES ($1, $2, $3, $4, $5);

-- name: RedriveFailedDeliveries :execrows
-- Requeues failed deliveries of one activation for another attempt budget. Suppressed deliveries are not
-- failures and are never requeued; a delivery is redriven at most 10 times.
UPDATE activation_deliveries SET state = 'pending', reason = NULL, redrives = redrives + 1, attempt_base = attempts, next_attempt_at = now(), updated_at = now()
WHERE activation_id = $1 AND state = 'failed' AND redrives < 10;

-- name: CountActivationStates :many
SELECT state, count(*)::int AS deliveries FROM activation_deliveries WHERE activation_id = $1 GROUP BY state;

-- name: ListRecentActivations :many
-- The 50 most recent activations with deliveries per state.
SELECT a.id, a.segment_name, a.segment_revision, a.destination, a.created_at,
  count(d.id) FILTER (WHERE d.state = 'pending')::int AS pending,
  count(d.id) FILTER (WHERE d.state = 'delivered')::int AS delivered,
  count(d.id) FILTER (WHERE d.state = 'suppressed')::int AS suppressed,
  count(d.id) FILTER (WHERE d.state = 'failed')::int AS failed
FROM activations a LEFT JOIN activation_deliveries d ON d.activation_id = a.id
GROUP BY a.id ORDER BY a.created_at DESC, a.id LIMIT 50;

-- name: AudienceMetrics :one
-- Aggregate counts only. Dispatch lag is how long the oldest pending delivery has waited since it was
-- queued, last attempted or redriven.
SELECT
  (SELECT count(DISTINCT name) FROM segment_definitions)::int AS segments,
  (SELECT count(*) FROM segment_snapshots)::int AS snapshots,
  (SELECT count(*) FROM activations)::int AS activations,
  count(d.id) FILTER (WHERE d.state = 'pending')::int AS pending,
  count(d.id) FILTER (WHERE d.state = 'delivered')::int AS delivered,
  count(d.id) FILTER (WHERE d.state = 'suppressed')::int AS suppressed,
  count(d.id) FILTER (WHERE d.state = 'failed')::int AS failed,
  coalesce(extract(epoch FROM now() - min(d.updated_at) FILTER (WHERE d.state = 'pending')), 0)::bigint AS oldest_pending_seconds
FROM activation_deliveries d;

-- name: DeliveryReasonCounts :many
SELECT state, reason, count(*)::int AS deliveries FROM activation_deliveries
WHERE state IN ('suppressed', 'failed') AND reason IS NOT NULL GROUP BY state, reason ORDER BY state, reason;
