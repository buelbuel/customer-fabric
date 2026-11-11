-- +goose Up
-- Verification of source-attested consent (#154 part 2, ADR 0017). A verification asks the source for
-- the structured proof document behind one grant, by a deadline. Customer Fabric checks the document's
-- consistency with the recorded digest and the grant automatically (checked), keeps only the result and
-- the document's digest, and never stores the document. Only a privacy admin's recorded review of the
-- proof, obtained from the source under the controller arrangement, makes it verified. Each consent
-- authority has a trust policy: under attestation a source's signed grant counts until a verification of
-- it fails; under verified it counts only once reviewed and verified. A failure suspends the authority,
-- and an unanswered verification past its deadline stops the whole authority until it is reviewed.
ALTER TABLE consent_authorities ADD COLUMN trust_policy text NOT NULL DEFAULT 'attestation'
  CHECK (trust_policy IN ('attestation', 'verified'));
ALTER TABLE consent_authorities ALTER COLUMN trust_policy DROP DEFAULT;

ALTER TABLE consent_authority_audit DROP CONSTRAINT consent_authority_audit_action_check;
ALTER TABLE consent_authority_audit ADD CONSTRAINT consent_authority_audit_action_check CHECK (action IN
  ('approved','renewed','suspended','resumed','revoked','key_added','key_retired','key_compromised','audit_sampled'));

CREATE TABLE consent_evidence_verifications (
 id uuid PRIMARY KEY,
 evidence_id uuid NOT NULL REFERENCES consent_evidence(id) DEFERRABLE INITIALLY DEFERRED,
 authority_id uuid NOT NULL REFERENCES consent_authorities(id),
 reason text NOT NULL CHECK (reason IN ('policy','audit','dispute','access_request')),
 requested_by text NOT NULL CHECK (char_length(requested_by) BETWEEN 1 AND 128),
 requested_at timestamptz NOT NULL DEFAULT now(),
 due_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested','checked','verified','failed')),
 result text CHECK (result ~ '^[a-z_]{1,64}$'),
 document_digest bytea CHECK (octet_length(document_digest) = 32),
 decided_at timestamptz,
 reviewed_by text CHECK (char_length(reviewed_by) BETWEEN 1 AND 128),
 review_reference text CHECK (review_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
 reviewed_at timestamptz,
 CHECK ((state = 'requested') = (decided_at IS NULL)),
 CHECK ((state = 'requested') = (result IS NULL)),
 CHECK (state <> 'verified' OR reviewed_by IS NOT NULL),
 CHECK ((reviewed_by IS NULL) = (review_reference IS NULL) AND (reviewed_by IS NULL) = (reviewed_at IS NULL))
);
-- One verification at a time per grant, from request until review.
CREATE UNIQUE INDEX consent_evidence_verifications_open ON consent_evidence_verifications(evidence_id) WHERE state IN ('requested', 'checked');
CREATE INDEX consent_evidence_verifications_evidence ON consent_evidence_verifications(evidence_id, requested_at);
CREATE INDEX consent_evidence_verifications_authority ON consent_evidence_verifications(authority_id, due_at) WHERE state = 'requested';

-- Trust policy changes are audited with the review that made them.
ALTER TABLE consent_authority_audit ADD COLUMN trust_policy text CHECK (trust_policy IN ('attestation', 'verified'));

-- Consent decisions a source reported per minute, for its rate limit.
CREATE TABLE source_consent_rate (
 source text NOT NULL REFERENCES sources(name),
 window_start timestamptz NOT NULL,
 requests integer NOT NULL CHECK (requests > 0),
 PRIMARY KEY (source, window_start)
);

-- +goose Down
-- Drops verification history and rate counters. Use only in a disposable database.
DROP TABLE source_consent_rate;
DROP TABLE consent_evidence_verifications;
DELETE FROM consent_authority_audit WHERE action = 'audit_sampled';
ALTER TABLE consent_authority_audit DROP COLUMN trust_policy;
ALTER TABLE consent_authority_audit DROP CONSTRAINT consent_authority_audit_action_check;
ALTER TABLE consent_authority_audit ADD CONSTRAINT consent_authority_audit_action_check CHECK (action IN
  ('approved','renewed','suspended','resumed','revoked','key_added','key_retired','key_compromised'));
ALTER TABLE consent_authorities DROP COLUMN trust_policy;
