-- name: InsertConsentAuthority :one
INSERT INTO consent_authorities (id, source, purpose_id, controller, collection_methods, approved_by)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListConsentAuthorities :many
SELECT * FROM consent_authorities ORDER BY approved_at DESC, id LIMIT 200;

-- name: GetActiveConsentAuthority :one
SELECT * FROM consent_authorities WHERE source = $1 AND purpose_id = $2 AND state = 'active';

-- name: RevokeConsentAuthority :one
UPDATE consent_authorities SET state = 'revoked', revoked_at = now(), revoked_by = $2 WHERE id = $1 AND state = 'active' RETURNING *;

-- name: GetConsentAuthority :one
SELECT * FROM consent_authorities WHERE id = $1;

-- name: LatestProtectiveDecision :one
-- When the subject last withdrew, denied or objected for the purpose, under any version; the Unix epoch if never.
SELECT coalesce(max(e.received_at), 'epoch'::timestamptz)::timestamptz AS received_at
FROM consent_evidence e JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
WHERE e.subject_id = $1 AND pv.purpose_id = $2 AND e.decision <> 'grant';
