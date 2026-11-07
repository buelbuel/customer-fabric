-- name: RegisterSource :exec
INSERT INTO sources(name) VALUES($1) ON CONFLICT DO NOTHING;

-- name: LockMutationKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: GetMutation :one
SELECT request_hash, status, result FROM mutations WHERE source=$1 AND key=$2;

-- name: CreateProfile :exec
INSERT INTO profiles(id,version,attributes) VALUES($1,1,'{}');

-- name: LockProfile :one
SELECT version, attributes FROM profiles WHERE id=$1 FOR UPDATE;

-- name: ListOwnership :many
SELECT path, source FROM attribute_ownership WHERE profile_id=$1;

-- name: RecordMutation :exec
INSERT INTO mutations(id,source,key,request_hash,profile_id,status,result) VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: RecordProvenance :exec
INSERT INTO provenance(profile_id,path,source,mutation_id,outcome,rule) VALUES($1,$2,$3,$4,$5,'first-writer-owns');

-- name: SetOwnership :exec
INSERT INTO attribute_ownership(profile_id,path,source,tombstone) VALUES($1,$2,$3,$4)
ON CONFLICT(profile_id,path) DO UPDATE SET tombstone=EXCLUDED.tombstone;

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

-- name: SourceOwnsProfile :one
SELECT EXISTS (
 SELECT 1 FROM outbox
 WHERE profile_id=sqlc.arg(profile_id)
   AND source=sqlc.arg(source)
   AND version=1
   AND type='profile.created.v1'
);

-- name: ListIdentifiers :many
SELECT source, namespace, value FROM external_identifiers WHERE profile_id=$1 ORDER BY source,namespace,value;

-- name: ListProvenance :many
SELECT path, source, mutation_id, at, outcome, rule FROM provenance WHERE profile_id=$1 ORDER BY sequence DESC LIMIT 100;

-- name: ListProfiles :many
SELECT id,version FROM profiles WHERE id>sqlc.arg(after_id)::uuid ORDER BY id LIMIT sqlc.arg(page_size);
