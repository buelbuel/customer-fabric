-- name: LockProfileForRestriction :one
SELECT lifecycle_state, restricted FROM profiles WHERE id = $1 FOR NO KEY UPDATE;

-- name: SetProfileRestricted :exec
UPDATE profiles SET restricted = $2 WHERE id = $1;

-- name: InsertRestrictionDecision :one
INSERT INTO processing_restriction_decisions (id, profile_id, action, reason, review_by, actor)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING decided_at;

-- name: ListRestrictionDecisions :many
SELECT * FROM processing_restriction_decisions WHERE profile_id = $1 ORDER BY decided_at DESC, id LIMIT 100;

-- name: ListRestrictionsDueForReview :many
-- Restricted profiles whose latest restriction's review date has passed. A restriction is never lifted
-- by its date passing; it stays in force until an operator lifts it.
SELECT d.profile_id, d.reason, d.review_by, d.decided_at FROM (
  SELECT DISTINCT ON (profile_id) * FROM processing_restriction_decisions ORDER BY profile_id, decided_at DESC, id) d
JOIN profiles p ON p.id = d.profile_id
WHERE d.action = 'restrict' AND p.restricted AND d.review_by <= sqlc.arg(today)::date
ORDER BY d.review_by, d.profile_id LIMIT 500;
