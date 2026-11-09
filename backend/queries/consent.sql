-- name: InsertPurpose :exec
INSERT INTO consent_purposes(id, name) VALUES($1, $2);

-- name: GetPurpose :one
SELECT id, name FROM consent_purposes WHERE id=$1;

-- name: InsertPolicyVersion :exec
INSERT INTO consent_policy_versions(id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, carry_forward)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11);

-- name: NextPolicyVersion :one
SELECT coalesce(max(version), 0) + 1 AS version FROM consent_policy_versions WHERE purpose_id=$1;

-- name: GetPolicyVersion :one
SELECT id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, carry_forward, created_at
FROM consent_policy_versions WHERE id=$1;

-- name: ListPolicyVersions :many
SELECT id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, carry_forward, created_at
FROM consent_policy_versions WHERE purpose_id=$1 ORDER BY version;

-- name: ActivatePolicyVersion :execrows
UPDATE consent_policy_versions SET state='active' WHERE id=$1 AND state='draft';

-- name: RetirePolicyVersion :execrows
UPDATE consent_policy_versions SET state='retired' WHERE id=$1 AND state='active';

-- name: LockPurposePolicies :exec
SELECT pg_advisory_xact_lock(hashtextextended('consent-purpose:' || sqlc.arg(purpose_id)::uuid::text, 0));
-- name: GetCustomerSubjectByAnchor :one
SELECT id, lifecycle_state FROM consent_subjects WHERE kind='customer' AND anchor_profile_id=$1;

-- name: GetRecordSubjectByAnchor :one
SELECT id, lifecycle_state FROM consent_subjects WHERE kind='source_record' AND anchor_record_epoch=$1;

-- name: InsertCustomerSubject :exec
INSERT INTO consent_subjects(id, kind, anchor_profile_id) VALUES($1,'customer',$2);

-- name: InsertRecordSubject :exec
INSERT INTO consent_subjects(id, kind, anchor_record_epoch) VALUES($1,'source_record',$2);

-- name: LockConsentSubject :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: GetSubjectLifecycle :one
SELECT lifecycle_state FROM consent_subjects WHERE id=$1;

-- name: GetEvidenceByIdempotencyKey :one
SELECT id, decision, received_at FROM consent_evidence WHERE subject_id=$1 AND idempotency_key=$2;

-- name: InsertEvidence :exec
INSERT INTO consent_evidence(id, subject_id, policy_version_id, decision, actor_kind, source, source_epoch, occurred_at, proof_channel, proof_reference, idempotency_key, quarantined)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);

-- name: ListEvidenceForSubjectPurpose :many
SELECT e.id, e.decision, e.received_at, e.occurred_at, e.actor_kind, e.source, e.quarantined,
       pv.id AS policy_version_id, pv.version AS policy_version, pv.state AS policy_state, pv.lawful_basis, pv.carry_forward
FROM consent_evidence e
JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
WHERE e.subject_id=$1 AND pv.purpose_id=$2
ORDER BY e.received_at DESC, e.id DESC LIMIT 500;

-- name: InsertMaterialization :exec
INSERT INTO consent_evidence_materializations(id, evidence_id, subject_id, association_id)
VALUES($1,$2,$3,$4) ON CONFLICT (evidence_id, subject_id) DO NOTHING;

-- name: ListActiveAssociationsForCustomer :many
SELECT id, record_profile_id, record_epoch FROM verified_associations
WHERE customer_subject=sqlc.arg(customer_subject)::uuid AND revoked_at IS NULL;

-- name: ProfileForIdentity :one
SELECT profile_id FROM customer_identity_links WHERE kratos_identity_id=$1;

-- name: ListActiveAssociationsForRecord :many
SELECT id, customer_subject FROM verified_associations
WHERE record_epoch=sqlc.arg(record_epoch)::uuid AND revoked_at IS NULL;

-- name: GetRecordEpochProfile :one
SELECT profile_id FROM external_identifiers WHERE record_epoch=$1;
