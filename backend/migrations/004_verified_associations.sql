-- +goose Up
ALTER TABLE external_identifiers ADD COLUMN record_epoch uuid NOT NULL UNIQUE DEFAULT gen_random_uuid();
CREATE TABLE linking_grants (
 id uuid PRIMARY KEY,
 secret_hash bytea NOT NULL UNIQUE CHECK (octet_length(secret_hash) = 32),
 source text NOT NULL REFERENCES sources(name),
 record_profile_id uuid NOT NULL,
 record_epoch uuid NOT NULL,
 customer_subject uuid NOT NULL,
 proof_method text NOT NULL CHECK (proof_method = 'synthetic_challenge'),
 proof_reference uuid NOT NULL,
 issued_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK (expires_at > issued_at),
 consumed_at timestamptz,
 revoked_at timestamptz,
 UNIQUE (source, proof_reference)
);
CREATE INDEX linking_grants_pending_target ON linking_grants(record_epoch)
 WHERE consumed_at IS NULL AND revoked_at IS NULL;

CREATE TABLE verified_associations (
 id uuid PRIMARY KEY,
 grant_id uuid NOT NULL UNIQUE REFERENCES linking_grants(id),
 customer_subject uuid NOT NULL REFERENCES customer_identity_links(kratos_identity_id),
 source text NOT NULL REFERENCES sources(name),
 record_profile_id uuid NOT NULL,
 record_epoch uuid NOT NULL,
 linked_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 revoked_at timestamptz,
 revoked_by text CHECK (revoked_by IN ('issuer', 'customer')),
 CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE UNIQUE INDEX verified_associations_active_record ON verified_associations(record_epoch)
 WHERE revoked_at IS NULL;
CREATE INDEX verified_associations_active_customer ON verified_associations(customer_subject, id)
 WHERE revoked_at IS NULL;

-- +goose Down
-- Drops grants and association history. Use only in a disposable database.
DROP TABLE verified_associations;
DROP TABLE linking_grants;
ALTER TABLE external_identifiers DROP COLUMN record_epoch;
