package platform

import (
	"context"
	"errors"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrForbidden = problem(403, "Forbidden", "This consent decision is not permitted for the subject in its current state.")

func mustUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}

// customerSubjectFor returns the customer subject anchored on a profile, creating it once if absent.
func customerSubjectFor(ctx context.Context, q *db.Queries, profileID string) (consent.Subject, error) {
	if err := q.LockConsentSubject(ctx, "consent-subject:anchor:"+profileID); err != nil {
		return consent.Subject{}, err
	}
	row, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(profileID))
	if err == nil {
		return consent.Subject{ID: row.ID, Kind: consent.SubjectCustomer, LifecycleState: row.LifecycleState}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return consent.Subject{}, err
	}
	id := UUID()
	if err = q.InsertCustomerSubject(ctx, db.InsertCustomerSubjectParams{ID: id, AnchorProfileID: mustUUID(profileID)}); err != nil {
		return consent.Subject{}, consentError(err, "A customer subject already exists for this profile.")
	}
	return consent.Subject{ID: id, Kind: consent.SubjectCustomer, LifecycleState: "active"}, nil
}

// recordSubjectFor returns the source_record subject anchored on a record epoch, creating it once if absent.
func recordSubjectFor(ctx context.Context, q *db.Queries, epoch string) (consent.Subject, error) {
	if err := q.LockConsentSubject(ctx, "consent-subject:epoch:"+epoch); err != nil {
		return consent.Subject{}, err
	}
	row, err := q.GetRecordSubjectByAnchor(ctx, mustUUID(epoch))
	if err == nil {
		return consent.Subject{ID: row.ID, Kind: consent.SubjectSourceRecord, LifecycleState: row.LifecycleState}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return consent.Subject{}, err
	}
	id := UUID()
	if err = q.InsertRecordSubject(ctx, db.InsertRecordSubjectParams{ID: id, AnchorRecordEpoch: mustUUID(epoch)}); err != nil {
		return consent.Subject{}, consentError(err, "A record subject already exists for this epoch.")
	}
	return consent.Subject{ID: id, Kind: consent.SubjectSourceRecord, LifecycleState: "active"}, nil
}

// AppendCustomerEvidence records a decision about the customer anchored on profileID, under an active policy version.
func (s Store) AppendCustomerEvidence(ctx context.Context, profileID, policyVersionID string, a consent.Append) (consent.Subject, bool, error) {
	if err := a.Validate(); err != nil {
		return consent.Subject{}, false, problem(422, "Invalid evidence", err.Error())
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return consent.Subject{}, false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = verifyPolicyActive(ctx, q, policyVersionID); err != nil {
		return consent.Subject{}, false, err
	}
	subject, err := customerSubjectFor(ctx, q, profileID)
	if err != nil {
		return consent.Subject{}, false, err
	}
	created, err := appendEvidence(ctx, q, subject, policyVersionID, a)
	if err != nil {
		return consent.Subject{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return consent.Subject{}, false, err
	}
	return subject, created, nil
}

// AppendRecordEvidence records a decision about a source record epoch and materializes rights across active associations.
func (s Store) AppendRecordEvidence(ctx context.Context, epoch, policyVersionID string, a consent.Append) (consent.Subject, bool, error) {
	if err := a.Validate(); err != nil {
		return consent.Subject{}, false, problem(422, "Invalid evidence", err.Error())
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return consent.Subject{}, false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = verifyPolicyActive(ctx, q, policyVersionID); err != nil {
		return consent.Subject{}, false, err
	}
	if _, err = q.GetRecordEpochProfile(ctx, epoch); errors.Is(err, pgx.ErrNoRows) {
		return consent.Subject{}, false, problem(422, "Unknown record", "The record epoch does not belong to a known identifier.")
	} else if err != nil {
		return consent.Subject{}, false, err
	}
	subject, err := recordSubjectFor(ctx, q, epoch)
	if err != nil {
		return consent.Subject{}, false, err
	}
	created, err := appendEvidence(ctx, q, subject, policyVersionID, a)
	if err != nil {
		return consent.Subject{}, false, err
	}
	if err = materializeAcrossAssociations(ctx, q, epoch, policyVersionID, a); err != nil {
		return consent.Subject{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return consent.Subject{}, false, err
	}
	return subject, created, nil
}

func verifyPolicyActive(ctx context.Context, q *db.Queries, id string) error {
	pv, err := q.GetPolicyVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return problem(422, "Unknown policy", "The policy version does not exist.")
	}
	if err != nil {
		return err
	}
	if pv.State != "active" {
		return problem(409, "Policy not active", "Evidence may only be recorded against an active policy version.")
	}
	return nil
}

func appendEvidence(ctx context.Context, q *db.Queries, subject consent.Subject, policyVersionID string, a consent.Append) (bool, error) {
	if subject.LifecycleState == "erased" {
		return false, ErrForbidden
	}
	if _, err := q.GetEvidenceByIdempotencyKey(ctx, db.GetEvidenceByIdempotencyKeyParams{SubjectID: subject.ID, IdempotencyKey: a.IdempotencyKey}); err == nil {
		return false, nil // idempotent replay, no new row
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var source pgtype.Text
	if a.Source != "" {
		source = pgtype.Text{String: a.Source, Valid: true}
	}
	if err := q.InsertEvidence(ctx, db.InsertEvidenceParams{
		ID: UUID(), SubjectID: subject.ID, PolicyVersionID: policyVersionID, Decision: string(a.Decision),
		ActorKind: a.ActorKind, Source: source, OccurredAt: a.OccurredAt, ProofChannel: a.ProofChannel,
		ProofReference: a.ProofReference, IdempotencyKey: a.IdempotencyKey, Quarantined: a.Quarantined,
	}); err != nil {
		return false, consentError(err, "Duplicate or conflicting evidence.")
	}
	return true, nil
}

// materializeAcrossAssociations copies a withdrawal, denial or objection onto the customer side of each
// active association, so unlinking cannot revive a permission (ADR 0009 section 3).
func materializeAcrossAssociations(ctx context.Context, q *db.Queries, epoch, policyVersionID string, a consent.Append) error {
	if a.Decision != consent.Withdraw && a.Decision != consent.Deny && a.Decision != consent.Object {
		return nil
	}
	assocs, err := q.ListActiveAssociationsForRecord(ctx, epoch)
	if err != nil {
		return err
	}
	for _, assoc := range assocs {
		// verified_associations.customer_subject is a Kratos identity; resolve it to a profile, then a subject.
		profileID, err := q.ProfileForIdentity(ctx, assoc.CustomerSubject)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the identity has no profile yet
		}
		if err != nil {
			return err
		}
		cs, err := customerSubjectFor(ctx, q, profileID)
		if err != nil {
			return err
		}
		if err = mirror(ctx, q, cs.ID, policyVersionID, a, assoc.ID); err != nil {
			return err
		}
	}
	return nil
}

func mirror(ctx context.Context, q *db.Queries, target, policyVersionID string, a consent.Append, associationID string) error {
	key := "mirror:" + a.IdempotencyKey
	if _, err := q.GetEvidenceByIdempotencyKey(ctx, db.GetEvidenceByIdempotencyKeyParams{SubjectID: target, IdempotencyKey: key}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	evID := UUID()
	if err := q.InsertEvidence(ctx, db.InsertEvidenceParams{
		ID: evID, SubjectID: target, PolicyVersionID: policyVersionID, Decision: string(a.Decision),
		ActorKind: a.ActorKind, OccurredAt: a.OccurredAt, ProofChannel: a.ProofChannel,
		ProofReference: a.ProofReference, IdempotencyKey: key, Quarantined: a.Quarantined,
	}); err != nil {
		return consentError(err, "Duplicate materialized evidence.")
	}
	return q.InsertMaterialization(ctx, db.InsertMaterializationParams{ID: UUID(), EvidenceID: evID, SubjectID: target, AssociationID: associationID})
}
