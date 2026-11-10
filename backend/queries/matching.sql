-- name: ListMatchableRecords :many
-- Active, unrestricted source-owned records in a stable order. Customer anchors have no creator source and
-- are never compared.
SELECT id, creator_source::text AS source, attributes FROM profiles
WHERE creator_source IS NOT NULL AND lifecycle_state = 'active' AND NOT restricted
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: InsertMatchReview :one
INSERT INTO match_reviews (id, profile_id, candidate_profile_id, decision, class, agree, disagree)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING reviewed_at;

-- name: ListMatchReviews :many
-- Reviews involving a record from either side, newest first.
SELECT * FROM match_reviews WHERE profile_id = sqlc.arg(profile_id)::uuid OR candidate_profile_id = sqlc.arg(profile_id)::uuid
ORDER BY reviewed_at DESC, id LIMIT 200;

-- name: LatestMatchReviews :many
-- The latest decision for every pair involving a record, uncapped: at most one row per other record.
SELECT DISTINCT ON (other) other::uuid AS other, decision FROM (
  SELECT CASE WHEN profile_id = sqlc.arg(profile_id)::uuid THEN candidate_profile_id ELSE profile_id END AS other, decision, reviewed_at, id
  FROM match_reviews WHERE profile_id = sqlc.arg(profile_id)::uuid OR candidate_profile_id = sqlc.arg(profile_id)::uuid
) r ORDER BY other, reviewed_at DESC, id DESC;
