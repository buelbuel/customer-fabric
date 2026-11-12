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
SELECT version, attributes, lifecycle_state, restricted FROM profiles WHERE id=$1 FOR UPDATE;

-- name: ListOwnership :many
SELECT path, source FROM attribute_ownership WHERE profile_id=$1;

-- name: RecordMutation :exec
INSERT INTO mutations(id,source,key,request_hash,profile_id,status,result) VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: RecordProvenance :exec
-- One decision per path in a single statement. revisions holds each typed path's definition revision, 0 for
-- an untyped path, aligned with paths.
INSERT INTO provenance(profile_id,path,source,mutation_id,outcome,rule,attribute_revision)
SELECT sqlc.arg(profile_id)::uuid, d.path, sqlc.arg(source)::text, sqlc.arg(mutation_id)::uuid, sqlc.arg(outcome)::text, 'first-writer-owns', NULLIF(r.revision, 0)
FROM unnest(sqlc.arg(paths)::text[]) WITH ORDINALITY AS d(path, n)
JOIN unnest(sqlc.arg(revisions)::int[]) WITH ORDINALITY AS r(revision, n) USING (n) ORDER BY n;

-- name: SetOwnership :exec
-- Claims or tombstones every path in a single statement; tombstones is aligned with paths.
INSERT INTO attribute_ownership(profile_id,path,source,tombstone)
SELECT sqlc.arg(profile_id)::uuid, o.path, sqlc.arg(source)::text, t.tombstone
FROM unnest(sqlc.arg(paths)::text[]) WITH ORDINALITY AS o(path, n)
JOIN unnest(sqlc.arg(tombstones)::boolean[]) WITH ORDINALITY AS t(tombstone, n) USING (n)
ON CONFLICT(profile_id,path) DO UPDATE SET tombstone=EXCLUDED.tombstone;

-- name: AssociateIdentifier :one
INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES($1,$2,$3,$4)
ON CONFLICT(source,namespace,value) DO UPDATE SET value=EXCLUDED.value
RETURNING profile_id;

-- name: CountIdentifiers :one
SELECT count(*) FROM external_identifiers WHERE profile_id=$1;

-- name: CountSourceIdentifiers :one
SELECT count(*) FROM external_identifiers WHERE profile_id=$1 AND source=$2;

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

-- name: ListProfiles :many
SELECT id,version FROM profiles WHERE id>sqlc.arg(after_id)::uuid ORDER BY id LIMIT sqlc.arg(page_size);

-- name: GetProfileLifecycle :one
SELECT lifecycle_state, restricted FROM profiles WHERE id=$1;

-- name: SourceMayWriteProfile :one
-- A source writes a profile it created or one it is an active contributor to (ADR 0018). The caller holds
-- the profile's row lock, which revocation also takes, so the answer stays true until commit.
SELECT (EXISTS (SELECT 1 FROM profiles p WHERE p.id = sqlc.arg(profile_id)::uuid AND p.creator_source = sqlc.arg(source)::text)
    OR EXISTS (SELECT 1 FROM profile_contributors c
               WHERE c.profile_id = sqlc.arg(profile_id)::uuid AND c.source = sqlc.arg(source)::text AND c.revoked_at IS NULL))::boolean;

-- name: LockProfileForAuthorization :one
SELECT creator_source FROM profiles WHERE id = $1 FOR UPDATE;

-- name: InsertProfileContributor :one
INSERT INTO profile_contributors (id, profile_id, source, basis, match_review_id, verification_reference, authorized_by)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING authorized_at;

-- name: ListProfileContributors :many
SELECT * FROM profile_contributors WHERE profile_id = $1 ORDER BY authorized_at, id;

-- name: GetProfileContributor :one
SELECT * FROM profile_contributors WHERE id = $1 AND profile_id = $2;

-- name: RevokeProfileContributor :one
UPDATE profile_contributors SET revoked_by = $3, revoked_at = now(), revocation_reason = $4, revocation_reference = $5
WHERE id = $1 AND profile_id = $2 AND revoked_at IS NULL RETURNING revoked_at;

-- name: ContributorsOnReviewedPair :many
-- Active authorizations resting on a review of the same pair, whichever way round it was reviewed.
SELECT c.id, c.profile_id FROM profile_contributors c JOIN match_reviews r ON r.id = c.match_review_id
WHERE c.revoked_at IS NULL
  AND least(r.profile_id, r.candidate_profile_id) = least(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid)
  AND greatest(r.profile_id, r.candidate_profile_id) = greatest(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid);

-- name: LatestPairReview :one
SELECT * FROM match_reviews
WHERE least(profile_id, candidate_profile_id) = least(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid)
  AND greatest(profile_id, candidate_profile_id) = greatest(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid)
ORDER BY reviewed_at DESC, id DESC LIMIT 1;

-- name: GetMatchReviewByID :one
SELECT * FROM match_reviews WHERE id = $1;

-- name: SourceExists :one
SELECT EXISTS (SELECT 1 FROM sources WHERE name = $1);

-- name: ProfileCreator :one
SELECT creator_source FROM profiles WHERE id = $1;
