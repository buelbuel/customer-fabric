-- +goose Up
-- Trust controls for source consent (#154, ADR 0017). A consent authority records how the source was
-- vetted, the arrangement between source and controller, and when it must be reviewed again; after that
-- date it authorizes nothing until renewed. It can be suspended and resumed, or revoked for good. A
-- source signs every grant with an Ed25519 key registered on the authority; a key can be retired (no new
-- grants) or marked compromised (every grant it signed stops counting). Every change is audited.
ALTER TABLE consent_authorities DROP CONSTRAINT consent_authorities_check;
ALTER TABLE consent_authorities DROP CONSTRAINT consent_authorities_state_check;
ALTER TABLE consent_authorities
  ADD COLUMN arrangement_reference text CHECK (arrangement_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
  ADD COLUMN vetting_reference text CHECK (vetting_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
  ADD COLUMN review_by date,
  ADD CONSTRAINT consent_authorities_state_check CHECK (state IN ('active','suspended','revoked')),
  ADD CONSTRAINT consent_authorities_revocation CHECK ((state = 'revoked') = (revoked_at IS NOT NULL) AND (revoked_at IS NULL) = (revoked_by IS NULL));
-- Authorities approved before vetting was recorded are suspended until a privacy admin renews them.
UPDATE consent_authorities SET state = 'suspended' WHERE state = 'active';
ALTER TABLE consent_authorities ADD CONSTRAINT consent_authorities_vetted
  CHECK (state <> 'active' OR (arrangement_reference IS NOT NULL AND vetting_reference IS NOT NULL AND review_by IS NOT NULL));
DROP INDEX consent_authorities_active;
CREATE UNIQUE INDEX consent_authorities_current ON consent_authorities(source, purpose_id) WHERE state <> 'revoked';

CREATE TABLE consent_authority_keys (
 id uuid PRIMARY KEY,
 authority_id uuid NOT NULL REFERENCES consent_authorities(id),
 key_id text NOT NULL CHECK (key_id ~ '^[A-Za-z0-9._:-]{1,64}$'),
 public_key bytea NOT NULL CHECK (octet_length(public_key) = 32),
 state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','retired','compromised')),
 added_by text NOT NULL CHECK (char_length(added_by) BETWEEN 1 AND 128),
 added_at timestamptz NOT NULL DEFAULT now(),
 changed_at timestamptz,
 UNIQUE (authority_id, key_id)
);

CREATE TABLE consent_authority_audit (
 id uuid PRIMARY KEY,
 authority_id uuid NOT NULL REFERENCES consent_authorities(id),
 action text NOT NULL CHECK (action IN ('approved','renewed','suspended','resumed','revoked','key_added','key_retired','key_compromised')),
 key_id text,
 reference text,
 actor text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 128),
 recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX consent_authority_audit_authority ON consent_authority_audit(authority_id, recorded_at);

-- The key that signed a source decision and its signature over the canonical receipt. A source grant
-- needs both.
ALTER TABLE consent_evidence
  ADD COLUMN receipt_key uuid REFERENCES consent_authority_keys(id),
  ADD COLUMN receipt_signature bytea CHECK (octet_length(receipt_signature) = 64),
  ADD CONSTRAINT consent_evidence_signed_receipt CHECK ((receipt_key IS NULL) = (receipt_signature IS NULL));
-- Every new source grant is signed. Grants recorded before receipts were signed carry none, so they no
-- longer count as consent; the constraint does not rewrite them.
ALTER TABLE consent_evidence ADD CONSTRAINT consent_evidence_source_grant_signed
  CHECK (actor_kind <> 'source' OR decision <> 'grant' OR receipt_key IS NOT NULL) NOT VALID;

-- +goose Down
-- Drops keys, signatures and the authority audit. Use only in a disposable database.
ALTER TABLE consent_evidence DROP CONSTRAINT consent_evidence_source_grant_signed, DROP CONSTRAINT consent_evidence_signed_receipt, DROP COLUMN receipt_signature, DROP COLUMN receipt_key;
DROP TABLE consent_authority_audit;
DROP TABLE consent_authority_keys;
DROP INDEX consent_authorities_current;
UPDATE consent_authorities SET state = 'revoked', revoked_at = now(), revoked_by = 'migration-down' WHERE state = 'suspended';
ALTER TABLE consent_authorities DROP CONSTRAINT consent_authorities_vetted, DROP CONSTRAINT consent_authorities_revocation,
  DROP CONSTRAINT consent_authorities_state_check, DROP COLUMN review_by, DROP COLUMN vetting_reference, DROP COLUMN arrangement_reference;
ALTER TABLE consent_authorities ADD CONSTRAINT consent_authorities_state_check CHECK (state IN ('active','revoked')),
  ADD CONSTRAINT consent_authorities_check CHECK ((state = 'revoked') = (revoked_at IS NOT NULL) AND (revoked_at IS NULL) = (revoked_by IS NULL));
CREATE UNIQUE INDEX consent_authorities_active ON consent_authorities(source, purpose_id) WHERE state = 'active';
