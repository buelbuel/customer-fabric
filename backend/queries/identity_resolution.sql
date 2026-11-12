-- name: CanonicalOf :one
-- The unified customer a profile belongs to: its canonical profile, or itself.
SELECT coalesce((SELECT canonical_id FROM profile_aliases WHERE profile_id = sqlc.arg(profile_id)::uuid), sqlc.arg(profile_id)::uuid)::uuid;

-- name: CustomerMembers :many
-- Every source profile of a unified customer, oldest first.
SELECT p.id, p.creator_source, p.version, p.attributes, p.created_at, p.updated_at FROM profiles p
WHERE p.id = sqlc.arg(canonical_id)::uuid OR p.id IN (SELECT profile_id FROM profile_aliases WHERE canonical_id = sqlc.arg(canonical_id)::uuid)
ORDER BY p.created_at, p.id;

-- name: SourceRecordsOfCustomer :many
-- A source's own profiles in the unified customer that profile_id belongs to.
SELECT p.id FROM profiles p
WHERE p.creator_source = sqlc.arg(source)::text AND (p.id = sqlc.arg(canonical_id)::uuid
  OR p.id IN (SELECT profile_id FROM profile_aliases WHERE canonical_id = sqlc.arg(canonical_id)::uuid))
ORDER BY p.id LIMIT 2;

-- name: RecordCreator :one
SELECT creator_source FROM profiles WHERE id = $1;

-- name: EnqueueReconciliation :exec
INSERT INTO identity_reconciliation_queue (profile_id) VALUES ($1) ON CONFLICT DO NOTHING;

-- name: EnqueueNamespace :execrows
-- Every source profile with an identifier in the namespace, or a link by it.
INSERT INTO identity_reconciliation_queue (profile_id)
SELECT DISTINCT e.profile_id FROM external_identifiers e JOIN profiles p ON p.id = e.profile_id
WHERE e.namespace = sqlc.arg(namespace)::text AND p.creator_source IS NOT NULL
UNION SELECT profile_a FROM profile_links WHERE key_namespace = sqlc.arg(namespace)::text AND removed_at IS NULL
UNION SELECT profile_b FROM profile_links WHERE key_namespace = sqlc.arg(namespace)::text AND removed_at IS NULL
ON CONFLICT DO NOTHING;

-- name: LockIdentityGraph :exec
-- Serializes every change of the identity graph.
SELECT pg_advisory_xact_lock(hashtextextended('identity-graph', 0));

-- name: ClaimReconciliation :one
DELETE FROM identity_reconciliation_queue
WHERE profile_id = (SELECT profile_id FROM identity_reconciliation_queue ORDER BY enqueued_at, profile_id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING profile_id;

-- name: ReconcilableProfile :one
-- A source profile and whether it may be linked: active and unrestricted.
SELECT id, creator_source, (lifecycle_state = 'active' AND NOT restricted)::boolean AS linkable, created_at FROM profiles
WHERE id = $1 AND creator_source IS NOT NULL;

-- name: TrustedKeysForSource :many
SELECT namespace, sources FROM identity_resolution_keys WHERE sqlc.arg(source)::text = ANY(sources);

-- name: ProfileKeyValues :many
-- A profile's values in the given trusted-key namespaces.
SELECT namespace, value FROM external_identifiers WHERE profile_id = $1 AND namespace = ANY(sqlc.arg(namespaces)::text[])
ORDER BY namespace, value;

-- name: KeyHolders :many
-- Linkable source profiles, other than the given one, that a trusted source identifies by this value.
SELECT DISTINCT e.profile_id FROM external_identifiers e JOIN profiles p ON p.id = e.profile_id
WHERE e.namespace = sqlc.arg(namespace)::text AND e.value = sqlc.arg(value)::text AND e.profile_id <> sqlc.arg(profile_id)::uuid
  AND p.creator_source = ANY(sqlc.arg(sources)::text[]) AND p.lifecycle_state = 'active' AND NOT p.restricted
ORDER BY e.profile_id;

-- name: ActiveLinksOf :many
SELECT * FROM profile_links WHERE (profile_a = $1 OR profile_b = $1) AND removed_at IS NULL ORDER BY id;

-- name: CorrectedLink :one
SELECT EXISTS (SELECT 1 FROM profile_links WHERE profile_a = $1 AND profile_b = $2 AND key_namespace = $3 AND removal_reason = 'corrected')::boolean;

-- name: InsertProfileLink :exec
INSERT INTO profile_links (id, profile_a, profile_b, key_namespace) VALUES ($1, $2, $3, $4);

-- name: RemoveProfileLink :exec
UPDATE profile_links SET removed_at = now(), removed_by = sqlc.arg(removed_by)::text, removal_reason = sqlc.arg(removal_reason)::text,
  removal_reference = NULLIF(sqlc.arg(removal_reference)::text, '')
WHERE id = sqlc.arg(id)::uuid AND removed_at IS NULL;

-- name: GetProfileLink :one
SELECT * FROM profile_links WHERE id = $1;

-- name: Neighbours :many
-- Profiles linked to any of the given ones.
SELECT profile_b AS id FROM profile_links WHERE profile_a = ANY(sqlc.arg(ids)::uuid[]) AND removed_at IS NULL
UNION SELECT profile_a FROM profile_links WHERE profile_b = ANY(sqlc.arg(ids)::uuid[]) AND removed_at IS NULL;

-- name: ComponentKeyValues :many
-- Distinct values per trusted-key namespace among profiles whose source is trusted for it.
SELECT k.namespace, count(DISTINCT e.value)::bigint AS values
FROM identity_resolution_keys k
JOIN external_identifiers e ON e.namespace = k.namespace
JOIN profiles p ON p.id = e.profile_id AND p.creator_source = ANY(k.sources)
WHERE e.profile_id = ANY(sqlc.arg(ids)::uuid[])
GROUP BY k.namespace;

-- name: OldestProfile :one
SELECT id FROM profiles WHERE id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY created_at, id LIMIT 1;

-- name: SetAlias :exec
INSERT INTO profile_aliases (profile_id, canonical_id) VALUES ($1, $2)
ON CONFLICT (profile_id) DO UPDATE SET canonical_id = EXCLUDED.canonical_id, updated_at = now();

-- name: ClearAlias :exec
DELETE FROM profile_aliases WHERE profile_id = $1;

-- name: LastPairDecision :one
-- The latest decision about linking a profile to another by a key, so that a repeated step records nothing new.
SELECT coalesce((SELECT action FROM identity_decisions WHERE profile_id = sqlc.arg(profile_id)::uuid
  AND related_profile_id = sqlc.arg(related_profile_id)::uuid AND key_namespace = sqlc.arg(key_namespace)::text
  ORDER BY decided_at DESC, id DESC LIMIT 1), '')::text;

-- name: InsertIdentityDecision :exec
-- Empty optional values are stored as NULL.
INSERT INTO identity_decisions (id, profile_id, action, key_namespace, related_profile_id, link_id, reason, actor)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(profile_id)::uuid, sqlc.arg(action)::text, NULLIF(sqlc.arg(key_namespace)::text, ''),
  NULLIF(sqlc.arg(related_profile_id)::text, '')::uuid, NULLIF(sqlc.arg(link_id)::text, '')::uuid, NULLIF(sqlc.arg(reason)::text, ''),
  sqlc.arg(actor)::text);

-- name: ListIdentityDecisions :many
SELECT * FROM identity_decisions WHERE profile_id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY decided_at, id LIMIT 500;

-- name: ListCustomerLinks :many
SELECT * FROM profile_links WHERE profile_a = ANY(sqlc.arg(ids)::uuid[]) OR profile_b = ANY(sqlc.arg(ids)::uuid[]) ORDER BY created_at, id;

-- name: AppendUnifiedEvent :exec
-- It occurs when written, after the version it carries committed, so the relay orders it after that
-- version's change.
INSERT INTO outbox (id, type, profile_id, version, source, occurred_at, payload)
SELECT $1, 'profile.unified.v1', p.id, p.version, p.creator_source, clock_timestamp(), jsonb_build_object('canonical_profile_id', sqlc.arg(canonical_id)::uuid)
FROM profiles p WHERE p.id = sqlc.arg(profile_id)::uuid;

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

-- name: ListResolutionPolicies :many
SELECT * FROM attribute_resolution_policies ORDER BY path;

-- name: SetResolutionPolicy :one
INSERT INTO attribute_resolution_policies (path, strategy, source_priority, changed_by) VALUES ($1, $2, $3, $4)
ON CONFLICT (path) DO UPDATE SET strategy = EXCLUDED.strategy, source_priority = EXCLUDED.source_priority,
  changed_by = EXCLUDED.changed_by, changed_at = now()
RETURNING *;

-- name: DeleteResolutionPolicy :execrows
DELETE FROM attribute_resolution_policies WHERE path = $1;

-- name: MemberContributions :many
-- Live and withdrawn attributes of a unified customer's members, for resolution.
SELECT profile_id, path, source, tombstone, contributed_at, updated_at FROM attribute_contributions
WHERE profile_id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY profile_id, path;
