-- name: GetDeploymentBinding :one
SELECT deployment_id, controller_id FROM deployment_binding WHERE singleton;

-- name: InsertPurpose :exec
INSERT INTO consent_purposes(id, name) VALUES($1, $2);

-- name: GetPurposeByName :one
SELECT id, name FROM consent_purposes WHERE name=$1;

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

-- name: ActivatePolicyVersion :exec
UPDATE consent_policy_versions SET state='active' WHERE id=$1 AND state='draft';

-- name: RetirePolicyVersion :exec
UPDATE consent_policy_versions SET state='retired' WHERE id=$1 AND state='active';

-- name: LockPurposePolicies :exec
SELECT pg_advisory_xact_lock(hashtextextended('consent-purpose:' || sqlc.arg(purpose_id)::uuid::text, 0));
