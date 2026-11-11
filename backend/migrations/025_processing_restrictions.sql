-- +goose Up
-- Processing restriction decisions (#30, slice 7 of #134). An operator with the privacy_admin role
-- restricts a profile for a GDPR Art. 18(1) reason until a review date, or lifts the restriction; the
-- profile's restricted flag, which every write, view, segment, activation, match and notification
-- already enforces, changes in the same transaction as the decision row. Decisions are append-only and
-- hold reason codes, never free text. They are not yet journaled outside the tenant's recovery domain
-- (ADR 0009 section 6; the journal is pending in ADR 0010), so a database restore can lose one.
CREATE TABLE processing_restriction_decisions (
 id uuid PRIMARY KEY,
 profile_id uuid NOT NULL REFERENCES profiles(id),
 action text NOT NULL CHECK (action IN ('restrict','lift')),
 reason text NOT NULL CHECK (reason IN ('accuracy_contested','unlawful_processing','legal_claims','objection_pending',
   'accuracy_verified','claim_resolved','objection_decided','restriction_withdrawn')),
 review_by date,
 actor text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 128),
 decided_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK ((action = 'restrict') = (review_by IS NOT NULL)),
 CHECK ((action = 'restrict') = (reason IN ('accuracy_contested','unlawful_processing','legal_claims','objection_pending')))
);
CREATE INDEX processing_restriction_decisions_profile ON processing_restriction_decisions(profile_id, decided_at DESC);
-- +goose StatementBegin
CREATE FUNCTION processing_restriction_decisions_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'processing restriction decisions are append-only';
END $$;
-- +goose StatementEnd
CREATE TRIGGER processing_restriction_decisions_append_only BEFORE UPDATE OR DELETE ON processing_restriction_decisions
  FOR EACH ROW EXECUTE FUNCTION processing_restriction_decisions_append_only();

-- +goose Down
-- Drops restriction history. Use only in a disposable database.
DROP TABLE processing_restriction_decisions;
DROP FUNCTION processing_restriction_decisions_append_only();
