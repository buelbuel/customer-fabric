-- name: InsertImport :one
INSERT INTO source_imports (id, source, max_errors, format, mapping) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetImport :one
-- Scoped to the credential's source, so another source's import does not exist for it.
SELECT * FROM source_imports WHERE id = $1 AND source = $2;

-- name: LockImport :one
-- Serializes batches of one import: each row's write and the checkpoint commit together under it.
SELECT * FROM source_imports WHERE id = $1 AND source = $2 FOR UPDATE;

-- name: AdvanceImport :exec
UPDATE source_imports SET checkpoint = sqlc.arg(checkpoint), applied = applied + sqlc.arg(applied), created = created + sqlc.arg(created),
  quarantined = quarantined + sqlc.arg(quarantined), state = sqlc.arg(state), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: QuarantineImportRow :exec
INSERT INTO source_import_quarantine (import_id, line, reason) VALUES ($1, $2, $3);

-- name: ListImportQuarantine :many
SELECT line, reason FROM source_import_quarantine WHERE import_id = $1 ORDER BY line LIMIT sqlc.arg(row_limit)::int;

-- name: ListImportReconciliation :many
-- Operator reconciliation across sources. Mapping never leaves the source-facing import resource.
SELECT i.id, i.source, i.format, i.state, i.checkpoint, i.applied, i.created, i.quarantined, i.max_errors, i.created_at, i.updated_at
FROM source_imports i ORDER BY i.updated_at DESC, i.id LIMIT 100;

-- name: GetImportReconciliation :one
SELECT i.id, i.source, i.format, i.state, i.checkpoint, i.applied, i.created, i.quarantined, i.max_errors, i.created_at, i.updated_at,
  coalesce(q.reasons, '[]'::json) AS quarantine_reasons
FROM source_imports i
LEFT JOIN LATERAL (
  SELECT json_agg(qs ORDER BY qs.count DESC, qs.reason) AS reasons FROM (
    SELECT reason, count(*)::bigint AS count FROM source_import_quarantine WHERE import_id = i.id GROUP BY reason
  ) qs
) q ON true
WHERE i.id = $1;
