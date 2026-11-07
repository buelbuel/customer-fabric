-- name: LockLinkRecord :exec
SELECT pg_advisory_xact_lock(hashtextextended('external-association:' || sqlc.arg(record_epoch)::uuid::text, 0));

-- name: GetLinkRecordEpoch :one
SELECT record_epoch FROM external_identifiers WHERE source=$1 AND namespace=$2 AND value=$3;

-- name: GetLinkRecordProfile :one
SELECT profile_id FROM external_identifiers WHERE record_epoch=$1 AND source=$2 FOR UPDATE;

-- name: HasActiveAssociation :one
SELECT EXISTS (SELECT 1 FROM verified_associations WHERE record_epoch=$1 AND revoked_at IS NULL);

-- name: CreateLinkingGrant :one
INSERT INTO linking_grants(id, secret_hash, source, record_profile_id, record_epoch,
 customer_subject, proof_method, proof_reference, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, 'synthetic_challenge', $7, statement_timestamp(), statement_timestamp() + interval '10 minutes')
ON CONFLICT (source, proof_reference) DO NOTHING
RETURNING expires_at;

-- name: GetLinkingGrantByHash :one
SELECT id, source, record_profile_id, record_epoch FROM linking_grants
WHERE secret_hash=$1 AND customer_subject=$2;

-- name: ConsumeLinkingGrant :execrows
UPDATE linking_grants SET consumed_at=clock_timestamp()
WHERE id=$1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > clock_timestamp();

-- name: GetLinkingGrantByIssuer :one
SELECT record_epoch FROM linking_grants WHERE id=$1 AND source=$2;

-- name: RevokeLinkingGrant :exec
UPDATE linking_grants SET revoked_at=clock_timestamp() WHERE id=$1 AND revoked_at IS NULL;

-- name: CancelPendingLinkingGrants :exec
UPDATE linking_grants SET revoked_at=clock_timestamp()
WHERE record_epoch=$1 AND consumed_at IS NULL AND revoked_at IS NULL;

-- name: CreateVerifiedAssociation :one
INSERT INTO verified_associations(id, grant_id, customer_subject, source, record_profile_id, record_epoch)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (record_epoch) WHERE revoked_at IS NULL DO NOTHING
RETURNING id, source, linked_at;

-- name: GetActiveVerifiedAssociation :one
SELECT a.id, a.source, a.linked_at
FROM verified_associations a
JOIN external_identifiers e ON (a.record_epoch, a.source, a.record_profile_id) =
 (e.record_epoch, e.source, e.profile_id)
WHERE a.id=$1 AND a.customer_subject=$2 AND a.revoked_at IS NULL;

-- name: ListActiveVerifiedAssociations :many
SELECT a.id, a.source, a.linked_at
FROM verified_associations a
JOIN external_identifiers e ON (a.record_epoch, a.source, a.record_profile_id) =
 (e.record_epoch, e.source, e.profile_id)
WHERE a.customer_subject=$1 AND a.revoked_at IS NULL AND a.id > $2
ORDER BY a.id LIMIT $3;

-- name: GetAssociationTarget :one
SELECT record_epoch FROM verified_associations WHERE id=$1 AND customer_subject=$2;

-- name: RevokeAssociationByCustomer :execrows
UPDATE verified_associations SET revoked_at=clock_timestamp(), revoked_by='customer'
WHERE id=$1 AND revoked_at IS NULL;

-- name: RevokeAssociationByIssuer :execrows
UPDATE verified_associations SET revoked_at=clock_timestamp(), revoked_by='issuer'
WHERE grant_id=$1 AND revoked_at IS NULL;
