-- name: InsertPurpose :exec
INSERT INTO consent_purposes(id, name) VALUES($1, $2);

-- name: GetPurpose :one
SELECT id, name FROM consent_purposes WHERE id=$1;

-- name: InsertPolicyVersion :exec
INSERT INTO consent_policy_versions(id, purpose_id, version, state, lawful_basis, notice_reference, explanation, jurisdiction, locale, channel, retention_reference, carry_forward)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);

-- name: NextPolicyVersion :one
SELECT coalesce(max(version), 0) + 1 AS version FROM consent_policy_versions WHERE purpose_id=$1;

-- name: GetPolicyVersion :one
SELECT id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, carry_forward, created_at, explanation
FROM consent_policy_versions WHERE id=$1;

-- name: ListPolicyVersions :many
SELECT id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, carry_forward, created_at, explanation
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

-- name: GetCustomerLinkForProfile :one
SELECT kratos_identity_id FROM customer_identity_links WHERE profile_id=$1;

-- name: GetEvidenceByIdempotencyKey :one
SELECT id, policy_version_id, decision, actor_kind, source, occurred_at, proof_channel, proof_reference, notice_reference, proof_digest
FROM consent_evidence WHERE subject_id=$1 AND idempotency_key=$2;

-- name: InsertEvidence :exec
INSERT INTO consent_evidence(id, subject_id, policy_version_id, decision, actor_kind, source, source_epoch, occurred_at, proof_channel, proof_reference, idempotency_key, quarantined,
  authority_id, notice_reference, proof_digest, source_generation)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16);



-- name: GetRecordEpochProfile :one
SELECT profile_id FROM external_identifiers WHERE record_epoch=$1;

-- name: GetActivePolicyForPurpose :one
SELECT id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, carry_forward, created_at, explanation
FROM consent_policy_versions WHERE purpose_id=$1 AND state='active';

-- name: ListPolicyVersionsForPurpose :many
SELECT version, lawful_basis, carry_forward FROM consent_policy_versions WHERE purpose_id=$1;

-- name: ListCustomerConsentPolicies :many
-- One row per purpose: its active consent version, else the newest consent version this subject
-- decided under, else its active version. A consent decision therefore stays withdrawable after its
-- version is retired, even when the purpose has moved to another lawful basis.
SELECT DISTINCT ON (p.id) p.name AS purpose_name, pv.id, pv.purpose_id, pv.version, pv.state, pv.lawful_basis,
  pv.explanation, pv.notice_reference, pv.jurisdiction, pv.locale, pv.channel, pv.retention_reference
FROM consent_purposes p
JOIN consent_policy_versions pv ON pv.purpose_id = p.id
WHERE p.id > sqlc.arg(cursor)::uuid AND (pv.state = 'active' OR (pv.state = 'retired' AND EXISTS (
  SELECT 1 FROM consent_evidence e WHERE e.subject_id = sqlc.arg(subject_id)::uuid AND e.policy_version_id = pv.id)))
ORDER BY p.id, (pv.state = 'active' AND pv.lawful_basis = 'consent') DESC, (pv.lawful_basis = 'consent') DESC, pv.version DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListSubjectEvidenceHistory :many
-- Newest first. A cursor from another subject matches nothing, so the page is empty.
SELECT e.id, p.id AS purpose_id, p.name AS purpose_name, pv.id AS policy_version_id, pv.version AS policy_version,
  e.decision, e.actor_kind, e.received_at
FROM consent_evidence e
JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
JOIN consent_purposes p ON p.id = pv.purpose_id
WHERE e.subject_id = sqlc.arg(subject_id)::uuid AND (sqlc.arg(cursor)::text = '' OR (e.received_at, e.id) < (
  SELECT c.received_at, c.id FROM consent_evidence c WHERE c.id::text = sqlc.arg(cursor)::text AND c.subject_id = sqlc.arg(subject_id)::uuid))
ORDER BY e.received_at DESC, e.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCustomerRecordEpochs :many
SELECT DISTINCT record_epoch FROM verified_associations WHERE customer_subject = $1 ORDER BY record_epoch;
