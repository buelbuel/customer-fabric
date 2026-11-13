-- name: HoldScopeTarget :one
-- What a hold may be placed on: a source record (created by a source) or a customer's profile anchor.
SELECT p.id, (p.creator_source IS NOT NULL)::boolean AS source_record,
  EXISTS (SELECT 1 FROM customer_identity_links l WHERE l.profile_id = p.id)::boolean AS customer_anchor
FROM profiles p WHERE p.id = $1;

-- name: InsertLegalHold :one
INSERT INTO legal_holds (id, scope, profile_id, categories, reason, legal_reference, review_by, requested_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: LockLegalHold :one
SELECT * FROM legal_holds WHERE id = $1 FOR UPDATE;

-- name: ApproveLegalHold :one
UPDATE legal_holds SET state = 'active', approved_by = sqlc.arg(actor)::text, approved_at = now()
WHERE id = sqlc.arg(id)::uuid RETURNING *;

-- name: ReleaseLegalHold :one
UPDATE legal_holds SET state = 'released', released_by = sqlc.arg(actor)::text, released_at = now(), release_reason = sqlc.arg(reason)::text
WHERE id = sqlc.arg(id)::uuid RETURNING *;

-- name: InsertLegalHoldDecision :exec
INSERT INTO legal_hold_decisions (id, hold_id, action, reason, actor) VALUES ($1, $2, $3, $4, $5);

-- name: GetLegalHold :one
SELECT * FROM legal_holds WHERE id = $1;

-- name: ListLegalHolds :many
-- Holds that are not released, newest first, or a profile's holds of any state.
SELECT * FROM legal_holds
WHERE (sqlc.arg(profile_id)::text = '' AND state <> 'released') OR profile_id::text = sqlc.arg(profile_id)::text
ORDER BY requested_at DESC, id LIMIT 200;

-- name: ListLegalHoldDecisions :many
SELECT * FROM legal_hold_decisions WHERE hold_id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY hold_id, decided_at, id;

-- name: ListLegalHoldsDue :many
-- Approved holds past their review date, and requests that lapsed unapproved.
SELECT * FROM legal_holds
WHERE state = 'active' AND review_by <= sqlc.arg(today)::date
   OR state = 'requested' AND requested_at <= now() - interval '14 days'
ORDER BY requested_at, id LIMIT 500;
