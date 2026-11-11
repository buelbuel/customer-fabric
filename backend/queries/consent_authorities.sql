-- name: InsertConsentAuthority :one
INSERT INTO consent_authorities (id, source, purpose_id, controller, collection_methods, approved_by, arrangement_reference,
  vetting_reference, review_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: ListConsentAuthorities :many
SELECT * FROM consent_authorities ORDER BY approved_at DESC, id LIMIT 200;

-- name: GetActiveConsentAuthority :one
-- The source's authority for the purpose that is active and not past its review date.
SELECT * FROM consent_authorities WHERE source = $1 AND purpose_id = $2 AND state = 'active' AND review_by >= current_date;

-- name: RevokeConsentAuthority :one
UPDATE consent_authorities SET state = 'revoked', revoked_at = now(), revoked_by = $2 WHERE id = $1 AND state <> 'revoked' RETURNING *;

-- name: SuspendConsentAuthority :one
UPDATE consent_authorities SET state = 'suspended' WHERE id = $1 AND state = 'active' RETURNING *;

-- name: RenewConsentAuthority :one
-- Records a new review of the source and resumes the authority.
UPDATE consent_authorities SET state = 'active', vetting_reference = $2, arrangement_reference = $3, review_by = $4
WHERE id = $1 AND state <> 'revoked' RETURNING *;

-- name: LockConsentAuthority :one
SELECT * FROM consent_authorities WHERE id = $1 FOR UPDATE;

-- name: InsertAuthorityKey :one
INSERT INTO consent_authority_keys (id, authority_id, key_id, public_key, added_by) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (authority_id, key_id) DO NOTHING RETURNING *;

-- name: ListAuthorityKeys :many
SELECT * FROM consent_authority_keys WHERE authority_id = $1 ORDER BY added_at, key_id;

-- name: GetAuthorityKey :one
SELECT * FROM consent_authority_keys WHERE authority_id = $1 AND key_id = $2;

-- name: SetAuthorityKeyState :one
-- Retires an active key, or marks an active or retired key compromised; compromise is final.
UPDATE consent_authority_keys SET state = sqlc.arg(state)::text, changed_at = now()
WHERE authority_id = sqlc.arg(authority_id)::uuid AND key_id = sqlc.arg(key_id)::text
  AND (state = 'active' OR state = 'retired' AND sqlc.arg(state)::text = 'compromised')
RETURNING *;

-- name: InsertAuthorityAudit :exec
INSERT INTO consent_authority_audit (id, authority_id, action, key_id, reference, actor) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListAuthorityAudit :many
SELECT * FROM consent_authority_audit WHERE authority_id = $1 ORDER BY recorded_at, id;

-- name: GetConsentAuthority :one
SELECT * FROM consent_authorities WHERE id = $1;

-- name: LatestProtectiveDecision :one
-- When the subject last withdrew, denied or objected for the purpose, under any version; the Unix epoch if never.
SELECT coalesce(max(e.received_at), 'epoch'::timestamptz)::timestamptz AS received_at
FROM consent_evidence e JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
WHERE e.subject_id = $1 AND pv.purpose_id = $2 AND e.decision <> 'grant';
