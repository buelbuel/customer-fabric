-- +goose Up
-- Legal holds (#30; ADR 0003, ADR 0009 sections 9 and 12). A hold preserves named categories of one
-- scope's data against deletion: a source record, or a customer with the records verifiably associated
-- with it while the hold is in force. It never preserves another subject's independent records and never
-- a category it does not name. A privacy admin requests it with a reason code, a legal reference and a
-- review date; a different privacy admin approves it. A request protects while it awaits approval, for
-- at most 14 days. An approved hold stays in force until released, also past its review date, when it is
-- listed for review. Decisions are append-only. Like restrictions, they are not yet journaled outside the
-- tenant's recovery domain (ADR 0010 pending).
CREATE TABLE legal_holds (
 id uuid PRIMARY KEY,
 scope text NOT NULL CHECK (scope IN ('record','customer')),
 profile_id uuid NOT NULL REFERENCES profiles(id),
 categories text[] NOT NULL CHECK (cardinality(categories) BETWEEN 1 AND 4
   AND categories <@ ARRAY['profile_data','consent_evidence','notifications','eventing']::text[]),
 reason text NOT NULL CHECK (reason IN ('litigation','regulatory_investigation','statutory_obligation','internal_investigation')),
 legal_reference text NOT NULL CHECK (legal_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
 review_by date NOT NULL,
 state text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested','active','released')),
 requested_by text NOT NULL CHECK (char_length(requested_by) BETWEEN 1 AND 128),
 requested_at timestamptz NOT NULL DEFAULT now(),
 approved_by text CHECK (char_length(approved_by) BETWEEN 1 AND 128),
 approved_at timestamptz,
 released_by text CHECK (char_length(released_by) BETWEEN 1 AND 128),
 released_at timestamptz,
 release_reason text CHECK (release_reason IN ('matter_closed','obligation_ended','request_withdrawn','not_approved')),
 CHECK ((approved_by IS NULL) = (approved_at IS NULL) AND approved_by IS DISTINCT FROM requested_by),
 CHECK ((state = 'released') = (released_at IS NOT NULL) AND (released_at IS NULL) = (released_by IS NULL)
   AND (released_at IS NULL) = (release_reason IS NULL)),
 CHECK (state <> 'active' OR approved_at IS NOT NULL)
);
CREATE INDEX legal_holds_profile ON legal_holds(profile_id) WHERE state <> 'released';
-- A hold's grounds never change, and it moves only forward: requested to active or released, active to
-- released. Corrections are a new hold.
-- +goose StatementBegin
CREATE FUNCTION legal_holds_forward_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'legal holds are never deleted';
  END IF;
  IF (NEW.id, NEW.scope, NEW.profile_id, NEW.categories, NEW.reason, NEW.legal_reference, NEW.review_by, NEW.requested_by, NEW.requested_at)
     IS DISTINCT FROM (OLD.id, OLD.scope, OLD.profile_id, OLD.categories, OLD.reason, OLD.legal_reference, OLD.review_by, OLD.requested_by, OLD.requested_at)
     OR OLD.state = 'released'
     OR OLD.state = 'active' AND NEW.state <> 'released'
     OR OLD.approved_at IS NOT NULL AND (NEW.approved_by, NEW.approved_at) IS DISTINCT FROM (OLD.approved_by, OLD.approved_at) THEN
    RAISE EXCEPTION 'legal holds change only forward';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER legal_holds_forward_only BEFORE UPDATE OR DELETE ON legal_holds
  FOR EACH ROW EXECUTE FUNCTION legal_holds_forward_only();

-- Every decision on a hold, append-only.
CREATE TABLE legal_hold_decisions (
 id uuid PRIMARY KEY,
 hold_id uuid NOT NULL REFERENCES legal_holds(id),
 action text NOT NULL CHECK (action IN ('requested','approved','released')),
 reason text NOT NULL CHECK (reason ~ '^[a-z_]{1,64}$'),
 actor text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 128),
 decided_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX legal_hold_decisions_hold ON legal_hold_decisions(hold_id, decided_at);
-- +goose StatementBegin
CREATE FUNCTION legal_hold_decisions_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'legal hold decisions are append-only';
END $$;
-- +goose StatementEnd
CREATE TRIGGER legal_hold_decisions_append_only BEFORE UPDATE OR DELETE ON legal_hold_decisions
  FOR EACH ROW EXECUTE FUNCTION legal_hold_decisions_append_only();

-- The profiles whose data of a category a hold preserves now: requested holds for 14 days, approved ones
-- until released; for a customer, its anchor and the records verifiably associated with it.
-- +goose StatementBegin
CREATE FUNCTION held_profiles(category text) RETURNS TABLE (profile_id uuid) LANGUAGE sql STABLE AS $$
  SELECT h.profile_id FROM legal_holds h
  WHERE category = ANY(h.categories) AND (h.state = 'active' OR h.state = 'requested' AND h.requested_at > now() - interval '14 days')
  UNION
  SELECT e.profile_id FROM legal_holds h
  JOIN customer_identity_links l ON l.profile_id = h.profile_id
  JOIN verified_associations a ON a.customer_subject = l.kratos_identity_id AND a.revoked_at IS NULL
  JOIN external_identifiers e ON e.record_epoch = a.record_epoch
  WHERE h.scope = 'customer' AND category = ANY(h.categories)
    AND (h.state = 'active' OR h.state = 'requested' AND h.requested_at > now() - interval '14 days')
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION held_profiles(text);
DROP TABLE legal_hold_decisions;
DROP FUNCTION legal_hold_decisions_append_only();
DROP TABLE legal_holds;
DROP FUNCTION legal_holds_forward_only();
