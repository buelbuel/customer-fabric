-- Queueing touches an existing entry, so a review claiming it at the same time conflicts and leaves it
-- queued instead of deciding on state from before this change.
-- name: QueueReviewForProfile :exec
-- A profile whose own state changed, and every source record verifiably associated with the customer it
-- anchors, if it is a customer's profile anchor.
INSERT INTO notification_review_queue (profile_id)
SELECT sqlc.arg(profile_id)::uuid
UNION
SELECT e.profile_id FROM customer_identity_links l
JOIN verified_associations a ON a.customer_subject = l.kratos_identity_id AND a.revoked_at IS NULL
JOIN external_identifiers e ON e.record_epoch = a.record_epoch
WHERE l.profile_id = sqlc.arg(profile_id)::uuid
ON CONFLICT (profile_id) DO UPDATE SET enqueued_at = now();

-- name: QueueReviewForSubject :exec
-- The source records a consent decision on this subject decides: the record it is anchored on, or every
-- record verifiably associated with the customer.
INSERT INTO notification_review_queue (profile_id)
SELECT e.profile_id FROM consent_subjects s JOIN external_identifiers e ON e.record_epoch = s.anchor_record_epoch
WHERE s.id = sqlc.arg(subject_id)::uuid AND s.kind = 'source_record'
UNION
SELECT e.profile_id FROM consent_subjects s
JOIN customer_identity_links l ON l.profile_id = s.anchor_profile_id
JOIN verified_associations a ON a.customer_subject = l.kratos_identity_id AND a.revoked_at IS NULL
JOIN external_identifiers e ON e.record_epoch = a.record_epoch
WHERE s.id = sqlc.arg(subject_id)::uuid AND s.kind = 'customer'
ON CONFLICT (profile_id) DO UPDATE SET enqueued_at = now();

-- name: PostponeReview :exec
-- Moves a record whose review failed behind the others.
UPDATE notification_review_queue SET enqueued_at = now() WHERE profile_id = $1;

-- name: ClaimReview :one
DELETE FROM notification_review_queue
WHERE profile_id = (SELECT profile_id FROM notification_review_queue ORDER BY enqueued_at, profile_id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING profile_id;

-- name: HeldRecords :many
-- The subscriptions holding a record: those that delivered a version of it newer than any notice that
-- was not cancelled, with that version.
SELECT d.subscription_id, max(d.profile_version)::bigint AS version
FROM notification_deliveries d
WHERE d.profile_id = $1 AND d.outcome = 'delivered'
GROUP BY d.subscription_id
HAVING max(d.profile_version) > coalesce((SELECT max(w.held_version) FROM notification_withdrawals w
  WHERE w.subscription_id = d.subscription_id AND w.profile_id = $1 AND w.state <> 'cancelled'), 0)
ORDER BY d.subscription_id;

-- name: NextHeldProfiles :many
-- The sweep's next batch: profiles some subscription delivered, after the cursor.
SELECT DISTINCT profile_id FROM notification_deliveries
WHERE outcome = 'delivered' AND profile_id > sqlc.arg(after)::uuid
ORDER BY profile_id LIMIT sqlc.arg(row_limit)::int;

-- name: InsertWithdrawal :exec
INSERT INTO notification_withdrawals (id, subscription_id, profile_id, held_version, reason)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (subscription_id, profile_id, held_version) WHERE state <> 'cancelled' DO NOTHING;

-- name: ClaimDueWithdrawal :one
-- One due notice, leased for one attempt.
UPDATE notification_withdrawals SET attempts = attempts + 1, lease_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::int),
  next_attempt_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int)
WHERE id = (SELECT id FROM notification_withdrawals WHERE state = 'pending' AND next_attempt_at <= now()
  ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: SettleWithdrawal :execrows
-- Settles an attempt only while its claim holds.
UPDATE notification_withdrawals SET state = sqlc.arg(state)::text, next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz, lease_until = NULL,
  settled_at = CASE WHEN sqlc.arg(state)::text = 'pending' THEN NULL ELSE now() END
WHERE id = sqlc.arg(id)::uuid AND state = 'pending' AND attempts = sqlc.arg(attempt)::int;

-- name: InsertWithdrawalAttempt :exec
INSERT INTO notification_withdrawal_attempts (withdrawal_id, attempt, outcome, reason, status_code)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING;

-- name: ListWithdrawals :many
SELECT * FROM notification_withdrawals WHERE subscription_id = $1 ORDER BY created_at DESC, id LIMIT 100;

-- name: ListWithdrawalAttempts :many
SELECT * FROM notification_withdrawal_attempts WHERE withdrawal_id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY withdrawal_id, attempt, recorded_at;

-- name: RedriveFailedWithdrawals :execrows
-- Failed notices get a fresh attempt budget; each is decided again before it is sent.
UPDATE notification_withdrawals SET state = 'pending', attempt_base = attempts, next_attempt_at = now(), settled_at = NULL
WHERE subscription_id = $1 AND state = 'failed';

-- name: QueueReviewForRecordEpoch :exec
-- The source record of a record lifetime whose verified association changed.
INSERT INTO notification_review_queue (profile_id)
SELECT DISTINCT profile_id FROM external_identifiers WHERE record_epoch = $1
ON CONFLICT (profile_id) DO UPDATE SET enqueued_at = now();
