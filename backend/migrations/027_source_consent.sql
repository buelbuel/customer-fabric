-- +goose Up
-- Governed source consent (#64, ADR 0015). A consent authority, approved by privacy_admin, lets one source
-- report consent grants for one purpose, collected on behalf of a named controller by approved collection
-- methods. A source grant counts as consent only while its authority is active, and only when it names
-- the notice the purpose's policy version records, the collection method, the source's own receipt and a
-- digest of the stored proof. Withdrawals, denials and objections from a source are always recorded.
CREATE TABLE consent_authorities (
 id uuid PRIMARY KEY,
 source text NOT NULL REFERENCES sources(name),
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 controller text NOT NULL CHECK (controller ~ '^[A-Za-z0-9][A-Za-z0-9 ._:/-]{0,127}$'),
 collection_methods text[] NOT NULL CHECK (cardinality(collection_methods) BETWEEN 1 AND 4
   AND collection_methods <@ ARRAY['double_opt_in','signed_form','recorded_web_form','recorded_call']::text[]),
 state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','revoked')),
 approved_by text NOT NULL CHECK (char_length(approved_by) BETWEEN 1 AND 128),
 approved_at timestamptz NOT NULL DEFAULT now(),
 revoked_at timestamptz,
 CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE UNIQUE INDEX consent_authorities_active ON consent_authorities(source, purpose_id) WHERE state = 'active';

-- What a source decision was collected under. The source's epoch at receipt is kept as a number; the
-- older uuid source_epoch column was never written and stays unused.
ALTER TABLE consent_evidence
  ADD COLUMN authority_id uuid REFERENCES consent_authorities(id),
  ADD COLUMN notice_reference text CHECK (char_length(notice_reference) BETWEEN 1 AND 256),
  ADD COLUMN proof_digest bytea CHECK (octet_length(proof_digest) = 32),
  ADD COLUMN source_generation bigint CHECK (source_generation > 0),
  ADD CONSTRAINT consent_evidence_source_grant CHECK (
    actor_kind <> 'source' OR decision <> 'grant' OR
    (authority_id IS NOT NULL AND notice_reference IS NOT NULL AND proof_digest IS NOT NULL AND source_generation IS NOT NULL));

-- +goose Down
-- Drops source consent authorities and what source decisions recorded about them. Use only in a
-- disposable database.
ALTER TABLE consent_evidence DROP CONSTRAINT consent_evidence_source_grant,
  DROP COLUMN source_generation, DROP COLUMN proof_digest, DROP COLUMN notice_reference, DROP COLUMN authority_id;
DROP TABLE consent_authorities;
