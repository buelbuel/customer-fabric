-- +goose Up
ALTER TABLE profiles ADD COLUMN creator_source text REFERENCES sources(name);

-- An ambiguous or missing creation receipt grants no source authority.
WITH creators AS (
 SELECT profile_id, MIN(source) AS source
 FROM mutations
 WHERE status=201
 GROUP BY profile_id
 HAVING COUNT(*)=1
)
UPDATE profiles p
SET creator_source = c.source
FROM creators c
WHERE p.id=c.profile_id AND NOT EXISTS (
 SELECT 1 FROM customer_identity_links l WHERE l.profile_id=p.id
);

-- +goose Down
ALTER TABLE profiles DROP COLUMN creator_source;
