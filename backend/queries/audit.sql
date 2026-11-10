-- name: InsertOperatorAudit :exec
INSERT INTO operator_audit (id, actor, method, resource, status) VALUES ($1, $2, $3, $4, $5);

-- name: ListOperatorAudit :many
SELECT * FROM operator_audit ORDER BY at DESC, id LIMIT 200;
