package platform

import (
	"context"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Collection methods a consent authority can approve. Each names how the source proves the subject's
// consent; a boolean opt-in field in a source system is not one of them.
var collectionMethods = []string{"double_opt_in", "signed_form", "recorded_web_form", "recorded_call"}

// Channels a source can report a withdrawal, denial or objection through, besides the collection methods.
// Stopping processing needs no proof of consent, so these need neither a notice nor a digest.
var withdrawalChannels = []string{"unsubscribe_link", "support_request", "source_record_update"}

var controllerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/-]{0,127}$`)

// ConsentAuthority lets one source report consent grants for one purpose (ADR 0015).
type ConsentAuthority struct {
	ID                string     `json:"id"`
	Source            string     `json:"source"`
	PurposeID         string     `json:"purpose_id"`
	Controller        string     `json:"controller"`
	CollectionMethods []string   `json:"collection_methods"`
	State             string     `json:"state"`
	ApprovedBy        string     `json:"approved_by"`
	ApprovedAt        time.Time  `json:"approved_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	RevokedBy         string     `json:"revoked_by,omitempty"`
}

func consentAuthorityFromRow(r db.ConsentAuthority) ConsentAuthority {
	a := ConsentAuthority{ID: r.ID, Source: r.Source, PurposeID: r.PurposeID, Controller: r.Controller, CollectionMethods: r.CollectionMethods,
		State: r.State, ApprovedBy: r.ApprovedBy, ApprovedAt: r.ApprovedAt}
	if r.RevokedAt.Valid {
		a.RevokedAt = &r.RevokedAt.Time
	}
	a.RevokedBy = r.RevokedBy.String
	return a
}

// ConsentAuthorityRequest is what an operator approves.
type ConsentAuthorityRequest struct {
	Source            string   `json:"source"`
	PurposeID         string   `json:"purpose_id"`
	Controller        string   `json:"controller"`
	CollectionMethods []string `json:"collection_methods"`
}

// ApproveConsentAuthority lets a registered source report consent grants for a consent-based purpose, as
// collector for the named controller, by the given methods. One authority per source and purpose is
// active at a time.
func (s Store) ApproveConsentAuthority(ctx context.Context, req ConsentAuthorityRequest, actor string) (ConsentAuthority, error) {
	invalid := func(detail string) (ConsentAuthority, error) {
		return ConsentAuthority{}, problem(422, "Invalid consent authority", detail)
	}
	switch {
	case !profiles.Name.MatchString(req.Source):
		return invalid("source must name a registered source.")
	case !profiles.ID.MatchString(req.PurposeID):
		return invalid("purpose_id must be a consent purpose ID.")
	case !controllerName.MatchString(req.Controller):
		return invalid("controller must identify the controller the consent is collected for (1 to 128 characters).")
	case len(req.CollectionMethods) == 0 || !subset(req.CollectionMethods, collectionMethods):
		return invalid("collection_methods must name double_opt_in, signed_form, recorded_web_form or recorded_call.")
	}
	q := db.New(s.Pool)
	if registered, err := q.ListRegisteredSources(ctx, []string{req.Source}); err != nil {
		return ConsentAuthority{}, err
	} else if len(registered) != 1 {
		return invalid("source must name a registered source.")
	}
	active, err := q.GetActivePolicyForPurpose(ctx, req.PurposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid("The purpose needs an active policy version.")
	}
	if err != nil {
		return ConsentAuthority{}, err
	}
	if active.LawfulBasis != string(consent.Consent) {
		return invalid("Only a consent-based purpose collects consent.")
	}
	row, err := q.InsertConsentAuthority(ctx, db.InsertConsentAuthorityParams{ID: UUID(), Source: req.Source, PurposeID: req.PurposeID,
		Controller: req.Controller, CollectionMethods: dedupe(req.CollectionMethods), ApprovedBy: actor})
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return ConsentAuthority{}, problem(409, "Authority exists", "This source already has an active authority for this purpose; revoke it first.")
	}
	if err != nil {
		return ConsentAuthority{}, err
	}
	return consentAuthorityFromRow(row), nil
}

// RevokeConsentAuthority ends an authority. Grants made under it stop counting as consent at once;
// withdrawals and objections made under it keep counting.
func (s Store) RevokeConsentAuthority(ctx context.Context, id, actor string) (ConsentAuthority, error) {
	q := db.New(s.Pool)
	row, err := q.RevokeConsentAuthority(ctx, db.RevokeConsentAuthorityParams{ID: id, RevokedBy: pgtype.Text{String: actor, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = q.GetConsentAuthority(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return ConsentAuthority{}, problem(404, "Not found", "Unknown consent authority.")
		}
		if err != nil {
			return ConsentAuthority{}, err
		}
		return ConsentAuthority{}, problem(409, "Authority revoked", "The consent authority is already revoked.")
	}
	return consentAuthorityFromRow(row), err
}

func (s Store) ConsentAuthorities(ctx context.Context) ([]ConsentAuthority, error) {
	rows, err := db.New(s.Pool).ListConsentAuthorities(ctx)
	out := make([]ConsentAuthority, 0, len(rows))
	for _, r := range rows {
		out = append(out, consentAuthorityFromRow(r))
	}
	return out, err
}

// SourceConsentDecision is one consent decision a source reports about one of its own records.
type SourceConsentDecision struct {
	Record          ingest.Record `json:"record"`
	PolicyVersionID string        `json:"policy_version_id"`
	Decision        string        `json:"decision"`
	CollectedAt     time.Time     `json:"collected_at"`
	SourceEpoch     int64         `json:"source_epoch"`
	Proof           struct {
		Method          string `json:"method"`
		Reference       string `json:"reference"`
		NoticeReference string `json:"notice_reference"`
		SHA256          string `json:"sha256"`
	} `json:"proof"`
}

// sourceClockSkew bounds how far in the future a source's collection time may lie.
const sourceClockSkew = 5 * time.Minute

// AppendSourceConsent records a source's consent decision about one of its own records (ADR 0015). The
// decision binds to the record's current lifetime, under the same record lock as linking, and is
// idempotent per record subject and key.
//
// A grant is refused unless it carries the source's current epoch, read under the lock a re-key waits
// for, so grants queued before a source re-keyed are refused; the source holds an active consent
// authority for the purpose; it was collected by one of the authority's methods; it names the notice the
// policy version records; its collection time lies between the version's creation and now (with
// sourceClockSkew of tolerance) and after the subject's latest withdrawal, denial or objection for the
// purpose, so a late or replayed grant never lifts a later protective decision; and it carries the
// source's receipt reference and a SHA-256 digest of the stored proof. Withdrawals, denials and
// objections are always recorded, under any epoch and with or without authority, since stopping
// processing never needs either; they name the channel they came through.
func (s Store) AppendSourceConsent(ctx context.Context, source, key string, d SourceConsentDecision) (EvidenceReceipt, error) {
	invalid := func(detail string) (EvidenceReceipt, error) {
		return EvidenceReceipt{}, problem(422, "Invalid consent decision", detail)
	}
	grant := d.Decision == string(consent.Grant)
	digest, err := hex.DecodeString(d.Proof.SHA256)
	if len(digest) == 0 {
		digest = nil
	}
	switch {
	case !profiles.ID.MatchString(d.PolicyVersionID):
		return invalid("policy_version_id must be a policy version ID.")
	case !consent.Decision(d.Decision).Valid():
		return invalid("decision must be grant, withdraw, deny or object.")
	case grant && !slices.Contains(collectionMethods, d.Proof.Method):
		return invalid("proof.method must be double_opt_in, signed_form, recorded_web_form or recorded_call.")
	case !grant && !slices.Contains(collectionMethods, d.Proof.Method) && !slices.Contains(withdrawalChannels, d.Proof.Method):
		return invalid("proof.method must name the channel the decision came through.")
	case d.Proof.Reference == "" || len(d.Proof.Reference) > 256:
		return invalid("proof.reference must name the source's receipt (1 to 256 characters).")
	case grant && d.Proof.NoticeReference == "" || len(d.Proof.NoticeReference) > 256:
		return invalid("proof.notice_reference must name the notice the subject was shown.")
	case err != nil || (grant || digest != nil) && len(digest) != 32:
		return invalid("proof.sha256 must be the hex SHA-256 digest of the stored proof.")
	case d.CollectedAt.IsZero() || d.CollectedAt.After(time.Now().Add(sourceClockSkew)):
		return invalid("collected_at must be a time no later than now.")
	case grant && d.SourceEpoch <= 0 || d.SourceEpoch < 0:
		return invalid("source_epoch must be the source's current epoch.")
	}
	decision := consent.Append{Decision: consent.Decision(d.Decision), ActorKind: "source", Source: source, ProofChannel: d.Proof.Method,
		ProofReference: d.Proof.Reference, OccurredAt: d.CollectedAt.UTC(), IdempotencyKey: key, NoticeReference: d.Proof.NoticeReference,
		ProofDigest: digest, SourceGeneration: d.SourceEpoch}
	return s.appendEvidence(ctx, d.PolicyVersionID, decision, func(q *db.Queries) (consent.Subject, error) {
		if grant {
			// A re-key waits for this shared lock, so no grant from before it commits after it.
			if err := q.LockSourceEpochShared(ctx, source); err != nil {
				return consent.Subject{}, err
			}
			epoch, err := q.GetSourceEpoch(ctx, source)
			if err != nil {
				return consent.Subject{}, err
			}
			if epoch != d.SourceEpoch {
				return consent.Subject{}, problem(409, "Source epoch mismatch", "Send this grant with the source's current epoch.")
			}
		}
		record, err := lockSourceRecord(ctx, q, source, d.Record)
		if err != nil {
			return consent.Subject{}, err
		}
		return recordSubjectFor(ctx, q, record.RecordEpoch)
	}, func(q *db.Queries, subject consent.Subject, a *consent.Append) error {
		version, err := q.GetPolicyVersion(ctx, d.PolicyVersionID)
		if err != nil {
			return err
		}
		authority, err := q.GetActiveConsentAuthority(ctx, db.GetActiveConsentAuthorityParams{Source: source, PurposeID: version.PurposeID})
		if errors.Is(err, pgx.ErrNoRows) {
			if grant {
				return problem(403, "No consent authority", "This source is not approved to report consent for this purpose.")
			}
			return nil
		}
		if err != nil {
			return err
		}
		a.AuthorityID = authority.ID
		if !grant {
			return nil
		}
		protected, err := q.LatestProtectiveDecision(ctx, db.LatestProtectiveDecisionParams{SubjectID: subject.ID, PurposeID: version.PurposeID})
		if err != nil {
			return err
		}
		switch {
		case !slices.Contains(authority.CollectionMethods, d.Proof.Method):
			return problem(422, "Invalid consent decision", "The source's authority does not approve this collection method.")
		case d.Proof.NoticeReference != version.NoticeReference:
			return problem(422, "Invalid consent decision", "proof.notice_reference must be the notice the policy version records.")
		case d.CollectedAt.Before(version.CreatedAt):
			return problem(422, "Invalid consent decision", "collected_at precedes the policy version, so the subject cannot have seen its notice.")
		case !d.CollectedAt.After(protected):
			return problem(409, "Withdrawn since", "The subject withdrew, denied or objected after this grant was collected; only a grant collected later counts.")
		}
		return nil
	})
}
