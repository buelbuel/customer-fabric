-- name: LockAttributeSchemaShared :exec
-- Writes that set attributes hold this shared, so a definition is never added between a write's
-- validation and its commit.
SELECT pg_advisory_xact_lock_shared(hashtextextended('attribute-schema', 0));

-- name: LockAttributeSchemaExclusive :exec
SELECT pg_advisory_xact_lock(hashtextextended('attribute-schema', 0));

-- name: ListCurrentAttributeDefinitions :many
SELECT DISTINCT ON (path) path, revision, type, max_length, enum_values, created_at
FROM attribute_definitions ORDER BY path, revision DESC;

-- name: GetCurrentAttributeDefinitions :many
SELECT DISTINCT ON (path) path, revision, type, max_length, enum_values, created_at
FROM attribute_definitions WHERE path = ANY(sqlc.arg(paths)::text[]) ORDER BY path, revision DESC;

-- name: ListAttributeRevisions :many
SELECT path, revision, type, max_length, enum_values, created_at
FROM attribute_definitions WHERE path=$1 ORDER BY revision;

-- name: InsertAttributeDefinition :one
INSERT INTO attribute_definitions(path, revision, type, max_length, enum_values) VALUES($1,$2,$3,$4,$5)
RETURNING created_at;

-- name: ListStoredAttributeValues :many
-- Live values under a name, so a new definition can check them. Bounded by the caller.
SELECT (p.attributes -> o.path)::text AS value
FROM attribute_ownership o JOIN profiles p ON p.id = o.profile_id
WHERE o.path = sqlc.arg(path)::text AND NOT o.tombstone AND p.attributes ? o.path
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCurrentAccessPolicies :many
SELECT DISTINCT ON (path) path, revision, readers, sources, created_at
FROM attribute_access_policies ORDER BY path, revision DESC;

-- name: ListAccessPolicyRevisions :many
SELECT path, revision, readers, sources, created_at
FROM attribute_access_policies WHERE path=$1 ORDER BY revision;

-- name: InsertAccessPolicy :one
INSERT INTO attribute_access_policies(path, revision, readers, sources) VALUES($1,$2,$3,$4)
RETURNING created_at;

-- name: CountKnownSources :one
SELECT count(*) FROM sources WHERE name = ANY(sqlc.arg(names)::text[]);
