-- +goose Up
-- Unified customer profiles (ADR 0018). A source profile is one source's record: only its creating
-- source writes it. An identity graph links source profiles that a trusted identity key says are the same
-- customer; connected profiles form one unified customer, named by its oldest member, and every other
-- member ID is an alias. Unified attributes are resolved per attribute from the members' own values.

-- Profiles holding another source's identifiers or values come only from the removed development mode.
-- They are not restructured here; this refuses, so local data is never changed silently.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM external_identifiers e JOIN profiles p ON p.id = e.profile_id
             WHERE p.creator_source IS DISTINCT FROM e.source)
     OR EXISTS (SELECT 1 FROM attribute_ownership o JOIN profiles p ON p.id = o.profile_id
             WHERE p.creator_source IS DISTINCT FROM o.source) THEN
    RAISE EXCEPTION 'profiles hold another source''s identifiers or values; reset the local database or remove them deliberately';
  END IF;
END $$;
-- +goose StatementEnd

-- A source profile's attributes and its per-attribute tombstones, with when each live value began (for
-- resolution) and last changed.
ALTER TABLE attribute_ownership RENAME TO attribute_contributions;
ALTER INDEX attribute_ownership_path RENAME TO attribute_contributions_path;
ALTER TABLE attribute_contributions
  ADD COLUMN contributed_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
UPDATE attribute_contributions c SET contributed_at = w.first_at, updated_at = w.last_at
FROM (SELECT profile_id, path, min(at) AS first_at, max(at) AS last_at FROM provenance WHERE outcome = 'accepted' GROUP BY profile_id, path) w
WHERE w.profile_id = c.profile_id AND w.path = c.path;
ALTER TABLE provenance DROP CONSTRAINT provenance_rule_check;
ALTER TABLE provenance ADD CONSTRAINT provenance_rule_check CHECK (rule IN ('first-writer-owns','source-record'));

-- Trusted identity keys: identifier namespaces that name the same customer across the listed sources.
CREATE TABLE identity_resolution_keys (
 namespace text PRIMARY KEY CHECK (namespace ~ '^[a-z][a-z0-9_]{0,63}$'),
 sources text[] NOT NULL CHECK (cardinality(sources) BETWEEN 2 AND 20),
 changed_by text NOT NULL CHECK (char_length(changed_by) BETWEEN 1 AND 128),
 changed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX external_identifiers_key ON external_identifiers(namespace, value);

-- Identity links, lower profile ID first. A removed link stays as history; one removed as a correction
-- also keeps the reconciler from linking the pair by that key again.
CREATE TABLE profile_links (
 id uuid PRIMARY KEY,
 profile_a uuid NOT NULL REFERENCES profiles(id),
 profile_b uuid NOT NULL REFERENCES profiles(id),
 key_namespace text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 removed_at timestamptz,
 removed_by text CHECK (char_length(removed_by) BETWEEN 1 AND 128),
 removal_reason text CHECK (removal_reason IN ('key_changed','profile_unavailable','corrected')),
 removal_reference text CHECK (removal_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
 CHECK (profile_a < profile_b),
 CHECK ((removed_at IS NULL) = (removal_reason IS NULL) AND (removed_at IS NULL) = (removed_by IS NULL)),
 CHECK ((removal_reason = 'corrected') = (removal_reference IS NOT NULL))
);
CREATE UNIQUE INDEX profile_links_active ON profile_links(profile_a, profile_b, key_namespace) WHERE removed_at IS NULL;
CREATE INDEX profile_links_a ON profile_links(profile_a) WHERE removed_at IS NULL;
CREATE INDEX profile_links_b ON profile_links(profile_b) WHERE removed_at IS NULL;
CREATE INDEX profile_links_corrected ON profile_links(profile_a, profile_b, key_namespace) WHERE removal_reason = 'corrected';

-- A member whose unified customer is named by another profile. No row: the profile names itself.
CREATE TABLE profile_aliases (
 profile_id uuid PRIMARY KEY REFERENCES profiles(id),
 canonical_id uuid NOT NULL REFERENCES profiles(id),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK (profile_id <> canonical_id)
);
CREATE INDEX profile_aliases_canonical ON profile_aliases(canonical_id);

-- Profiles waiting for the reconciler, queued in the transaction that changed what they resolve by.
CREATE TABLE identity_reconciliation_queue (
 profile_id uuid PRIMARY KEY REFERENCES profiles(id),
 enqueued_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only record of every identity decision.
CREATE TABLE identity_decisions (
 id uuid PRIMARY KEY,
 profile_id uuid NOT NULL REFERENCES profiles(id),
 action text NOT NULL CHECK (action IN ('linked','unlinked','conflict','canonical_changed')),
 key_namespace text,
 related_profile_id uuid REFERENCES profiles(id),
 link_id uuid REFERENCES profile_links(id),
 reason text CHECK (reason ~ '^[a-z_]{1,64}$'),
 actor text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 128),
 decided_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX identity_decisions_profile ON identity_decisions(profile_id, decided_at);
-- +goose StatementBegin
CREATE FUNCTION identity_decision_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'identity decisions are append-only';
END $$;
CREATE TRIGGER identity_decision_immutable BEFORE UPDATE OR DELETE ON identity_decisions
  FOR EACH ROW EXECUTE FUNCTION identity_decision_immutable();
-- +goose StatementEnd

-- Optional per-attribute resolution policies.
CREATE TABLE attribute_resolution_policies (
 path text PRIMARY KEY CHECK (path ~ '^[a-z][a-z0-9_]{0,63}$'),
 strategy text NOT NULL CHECK (strategy IN ('source_priority','most_recent')),
 source_priority text[] NOT NULL DEFAULT '{}' CHECK (cardinality(source_priority) <= 20),
 changed_by text NOT NULL CHECK (char_length(changed_by) BETWEEN 1 AND 128),
 changed_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((strategy = 'source_priority') = (cardinality(source_priority) > 0))
);

-- A profile whose unified customer changed emits profile.unified.v1. It is no mutation and carries the
-- member's current version, which a source's next write still expects.
ALTER TABLE outbox DROP CONSTRAINT outbox_type_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_type_check CHECK (type IN ('profile.created.v1','profile.updated.v1','profile.unified.v1'));
ALTER TABLE outbox DROP CONSTRAINT outbox_id_fkey;
ALTER TABLE outbox DROP CONSTRAINT outbox_profile_id_version_key;
CREATE UNIQUE INDEX outbox_profile_version ON outbox(profile_id, version) WHERE type <> 'profile.unified.v1';

-- +goose Down
DELETE FROM outbox WHERE type = 'profile.unified.v1';
DROP INDEX outbox_profile_version;
ALTER TABLE outbox ADD CONSTRAINT outbox_profile_id_version_key UNIQUE (profile_id, version);
ALTER TABLE outbox ADD CONSTRAINT outbox_id_fkey FOREIGN KEY (id) REFERENCES mutations(id);
ALTER TABLE outbox DROP CONSTRAINT outbox_type_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_type_check CHECK (type IN ('profile.created.v1','profile.updated.v1'));
DROP TABLE attribute_resolution_policies;
DROP TABLE identity_decisions;
DROP FUNCTION identity_decision_immutable();
DROP TABLE identity_reconciliation_queue;
DROP TABLE profile_aliases;
DROP TABLE profile_links;
DROP INDEX external_identifiers_key;
DROP TABLE identity_resolution_keys;
ALTER TABLE provenance DROP CONSTRAINT provenance_rule_check;
UPDATE provenance SET rule = 'first-writer-owns' WHERE rule = 'source-record';
ALTER TABLE provenance ADD CONSTRAINT provenance_rule_check CHECK (rule IN ('first-writer-owns'));
ALTER TABLE attribute_contributions DROP COLUMN updated_at, DROP COLUMN contributed_at;
ALTER INDEX attribute_contributions_path RENAME TO attribute_ownership_path;
ALTER TABLE attribute_contributions RENAME TO attribute_ownership;
