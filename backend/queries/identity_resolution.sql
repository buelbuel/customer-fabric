-- name: ResolutionKeysForSource :many
-- The trusted identity-key namespaces this source may present.
SELECT namespace, sources FROM identity_resolution_keys WHERE sqlc.arg(source)::text = ANY(sources);

-- name: LockIdentityKey :exec
-- Serializes resolution of one key value, so concurrent records with the same key resolve in order.
SELECT pg_advisory_xact_lock(hashtextextended('identity-key:' || sqlc.arg(namespace)::text || ':' || sqlc.arg(value)::text, 0));

-- name: ProfilesWithIdentityKey :many
-- Active source profiles that a trusted source identifies by this key value. A customer's profile anchor
-- never carries source identifiers.
SELECT DISTINCT e.profile_id FROM external_identifiers e JOIN profiles p ON p.id = e.profile_id
WHERE e.namespace = sqlc.arg(namespace) AND e.value = sqlc.arg(value) AND e.source = ANY(sqlc.arg(sources)::text[])
  AND p.creator_source IS NOT NULL AND p.lifecycle_state = 'active'
ORDER BY e.profile_id LIMIT 2;

-- name: InsertIdentityResolution :exec
INSERT INTO identity_resolutions (id, mutation_id, source, profile_id, outcome, key_namespace)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListIdentityResolutions :many
SELECT * FROM identity_resolutions WHERE profile_id = $1 ORDER BY decided_at, id;

-- name: ListResolutionKeys :many
SELECT * FROM identity_resolution_keys ORDER BY namespace;

-- name: SetResolutionKey :one
INSERT INTO identity_resolution_keys (namespace, sources, changed_by) VALUES ($1, $2, $3)
ON CONFLICT (namespace) DO UPDATE SET sources = EXCLUDED.sources, changed_by = EXCLUDED.changed_by, changed_at = now()
RETURNING *;

-- name: DeleteResolutionKey :execrows
DELETE FROM identity_resolution_keys WHERE namespace = $1;

-- name: UnknownSources :many
SELECT s FROM unnest(sqlc.arg(sources)::text[]) s WHERE NOT EXISTS (SELECT 1 FROM sources WHERE name = s);
