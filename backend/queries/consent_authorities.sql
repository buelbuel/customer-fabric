-- name: InsertConsentAuthority :one
INSERT INTO consent_authorities (id, source, purpose_id, controller, collection_methods, approved_by)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListConsentAuthorities :many
SELECT * FROM consent_authorities ORDER BY approved_at DESC, id LIMIT 200;

-- name: GetActiveConsentAuthority :one
SELECT * FROM consent_authorities WHERE source = $1 AND purpose_id = $2 AND state = 'active';

-- name: RevokeConsentAuthority :one
UPDATE consent_authorities SET state = 'revoked', revoked_at = now() WHERE id = $1 AND state = 'active' RETURNING *;

-- name: GetConsentAuthority :one
SELECT * FROM consent_authorities WHERE id = $1;
