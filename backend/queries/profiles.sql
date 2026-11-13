-- name: RegisterSource :exec
INSERT INTO sources(name) VALUES($1) ON CONFLICT DO NOTHING;

-- name: LockMutationKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: GetMutation :one
SELECT request_hash, status, result FROM mutations WHERE source=$1 AND key=$2;

-- name: CreateProfile :exec
INSERT INTO profiles(id,version,attributes) VALUES($1,1,'{}');

-- name: CreateSourceProfile :exec
INSERT INTO profiles(id,version,attributes,creator_source) VALUES($1,1,'{}',sqlc.arg(source)::text);

-- name: LockProfile :one
-- A writer never changes the profile's key, so it does not block the key-share locks that rows
-- referencing the profile take, such as the reconciler's links and events (ADR 0018).
SELECT version, attributes, lifecycle_state, restricted FROM profiles WHERE id=$1 FOR NO KEY UPDATE;

-- name: RecordMutation :exec
INSERT INTO mutations(id,source,key,request_hash,profile_id,status,result) VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: RecordProvenance :exec
-- One decision per path in a single statement. revisions holds each typed path's definition revision, 0 for
-- an untyped path, aligned with paths.
INSERT INTO provenance(profile_id,path,source,mutation_id,outcome,rule,attribute_revision)
SELECT sqlc.arg(profile_id)::uuid, d.path, sqlc.arg(source)::text, sqlc.arg(mutation_id)::uuid, sqlc.arg(outcome)::text, 'source-record', NULLIF(r.revision, 0)
FROM unnest(sqlc.arg(paths)::text[]) WITH ORDINALITY AS d(path, n)
JOIN unnest(sqlc.arg(revisions)::int[]) WITH ORDINALITY AS r(revision, n) USING (n) ORDER BY n;

-- name: AssociateIdentifier :one
INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES($1,$2,$3,$4)
ON CONFLICT(source,namespace,value) DO UPDATE SET value=EXCLUDED.value
RETURNING profile_id;

-- name: CountIdentifiers :one
SELECT count(*) FROM external_identifiers WHERE profile_id=$1;

-- name: UpdateProfile :exec
UPDATE profiles SET attributes=$2,version=$3,updated_at=now() WHERE id=$1;

-- name: AppendOutbox :exec
INSERT INTO outbox(id,type,profile_id,version,source,payload) VALUES($1,$2,$3,$4,$5,$6);

-- name: GetProfile :one
SELECT id, version, attributes, created_at, updated_at FROM profiles WHERE id=$1;

-- name: ListIdentifiers :many
SELECT source, namespace, value FROM external_identifiers WHERE profile_id=$1 ORDER BY source,namespace,value;

-- name: ListProvenance :many
SELECT path, source, mutation_id, at, outcome, rule, attribute_revision FROM provenance WHERE profile_id=$1 ORDER BY sequence DESC LIMIT 100;

-- name: ListCustomers :many
-- Unified customers by canonical ID, with how many profiles each unites (ADR 0018).
SELECT p.id, (1 + (SELECT count(*) FROM profile_aliases a WHERE a.canonical_id = p.id))::bigint AS records
FROM profiles p WHERE p.id > sqlc.arg(after_id)::uuid AND NOT EXISTS (SELECT 1 FROM profile_aliases a WHERE a.profile_id = p.id)
ORDER BY p.id LIMIT sqlc.arg(page_size);

-- name: GetProfileLifecycle :one
SELECT lifecycle_state, restricted FROM profiles WHERE id=$1;

-- name: SetContributions :exec
-- Sets or tombstones every written path of a source profile in one statement; tombstones is aligned with
-- paths. A value that becomes live again starts a new contribution; any write updates it.
INSERT INTO attribute_contributions(profile_id, path, source, tombstone, contributed_at, updated_at)
SELECT sqlc.arg(profile_id)::uuid, o.path, sqlc.arg(source)::text, t.tombstone, now(), now()
FROM unnest(sqlc.arg(paths)::text[]) WITH ORDINALITY AS o(path, n)
JOIN unnest(sqlc.arg(tombstones)::boolean[]) WITH ORDINALITY AS t(tombstone, n) USING (n)
ON CONFLICT(profile_id, path) DO UPDATE SET tombstone = EXCLUDED.tombstone, updated_at = now(),
  contributed_at = CASE WHEN attribute_contributions.tombstone AND NOT EXCLUDED.tombstone THEN now() ELSE attribute_contributions.contributed_at END;

-- name: OtherOrderedRecords :one
-- Another record of the profile that already has an event order.
SELECT count(*) FROM external_identifiers e JOIN source_record_watermarks w ON w.record_epoch = e.record_epoch
WHERE e.profile_id = $1 AND e.record_epoch <> $2;
