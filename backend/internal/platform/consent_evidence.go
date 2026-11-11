package platform

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrForbidden = problem(403, "Forbidden", "This consent decision is not permitted for the subject in its current state.")

// EvidenceReceipt acknowledges a stored decision. No journal exists yet, so the only level is
// pending_journal: durable in PostgreSQL, not rollback-safe (M2 incremental scope guardrail 2).
type EvidenceReceipt struct {
	SubjectID      string `json:"subject_id"`
	EvidenceID     string `json:"evidence_id"`
	Created        bool   `json:"created"`
	Acknowledgment string `json:"acknowledgment"`
	// Trust is source_attested for a source's grant: its signed attestation, not verified evidence.
	Trust string `json:"trust,omitempty"`
}

// optionalUUID is NULL for an empty string.
func optionalUUID(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	return mustUUID(s)
}

func mustUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}

// customerSubjectFor returns the customer subject anchored on a customer anchor profile, creating it
// once if absent. Only a profile linked to a customer identity can anchor a customer subject.
func customerSubjectFor(ctx context.Context, q *db.Queries, profileID string) (consent.Subject, error) {
	if _, err := q.GetCustomerLinkForProfile(ctx, profileID); errors.Is(err, pgx.ErrNoRows) {
		return consent.Subject{}, problem(404, "Not found", "The profile is not a customer anchor.")
	} else if err != nil {
		return consent.Subject{}, err
	}
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
		return consent.Subject{}, err
	}
	return consent.Subject{ID: id, Kind: consent.SubjectCustomer, LifecycleState: "active"}, nil
}

// recordSubjectFor returns the subject anchored on one record epoch. A recreated identifier has a new
// epoch, so it gets a new subject and inherits nothing.
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
		return consent.Subject{}, err
	}
	return consent.Subject{ID: id, Kind: consent.SubjectSourceRecord, LifecycleState: "active"}, nil
}

// AppendCustomerEvidence records a decision about the customer anchored on profileID. The actor comes
// from the authenticated caller, never from the request body.
func (s Store) AppendCustomerEvidence(ctx context.Context, profileID, policyVersionID string, a consent.Append) (EvidenceReceipt, error) {
	return s.appendEvidence(ctx, policyVersionID, a, func(q *db.Queries) (consent.Subject, error) {
		return customerSubjectFor(ctx, q, profileID)
	}, nil)
}

// AppendRecordEvidence records a decision about one live source record epoch. It holds the same
// record-epoch lock as M1 link, unlink and revoke, so its receipt time is strictly ordered against the
// association history that evaluation uses to decide which customers it covers.
func (s Store) AppendRecordEvidence(ctx context.Context, epoch, policyVersionID string, a consent.Append) (EvidenceReceipt, error) {
	return s.appendEvidence(ctx, policyVersionID, a, func(q *db.Queries) (consent.Subject, error) {
		if err := q.LockLinkRecord(ctx, epoch); err != nil {
			return consent.Subject{}, err
		}
		if _, err := q.GetRecordEpochProfile(ctx, epoch); errors.Is(err, pgx.ErrNoRows) {
			return consent.Subject{}, problem(404, "Not found", "The record epoch does not belong to a live identifier.")
		} else if err != nil {
			return consent.Subject{}, err
		}
		return recordSubjectFor(ctx, q, epoch)
	}, nil)
}

// appendEvidence commits one decision under the subject lock taken by subjectFor. An optional
// beforeInsert runs after replay detection, as does the policy check, so a retried decision returns
// its original receipt even if the version has since been retired. It may complete the decision, for
// example with the consent authority it was made under.
func (s Store) appendEvidence(ctx context.Context, policyVersionID string, a consent.Append, subjectFor func(*db.Queries) (consent.Subject, error), beforeInsert func(*db.Queries, consent.Subject, *consent.Append) error) (EvidenceReceipt, error) {
	if err := a.Validate(); err != nil {
		return EvidenceReceipt{}, problem(422, "Invalid evidence", err.Error())
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return EvidenceReceipt{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	subject, err := subjectFor(q)
	if err != nil {
		return EvidenceReceipt{}, err
	}
	if subject.LifecycleState == "erased" {
		return EvidenceReceipt{}, ErrForbidden
	}
	receipt := EvidenceReceipt{SubjectID: subject.ID, Acknowledgment: "pending_journal"}
	existing, err := q.GetEvidenceByIdempotencyKey(ctx, db.GetEvidenceByIdempotencyKeyParams{SubjectID: subject.ID, IdempotencyKey: a.IdempotencyKey})
	if err == nil {
		if !sameEvidence(existing, policyVersionID, a) {
			return EvidenceReceipt{}, problem(409, "Idempotency conflict", "This key was used for a different decision; use a new key.")
		}
		receipt.EvidenceID = existing.ID
		return receipt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return EvidenceReceipt{}, err
	}
	if err = verifyPolicyAccepts(ctx, q, policyVersionID, a.Decision); err != nil {
		return EvidenceReceipt{}, err
	}
	if beforeInsert != nil {
		if err = beforeInsert(q, subject, &a); err != nil {
			return EvidenceReceipt{}, err
		}
	}
	var source pgtype.Text
	if a.Source != "" {
		source = pgtype.Text{String: a.Source, Valid: true}
	}
	receipt.EvidenceID, receipt.Created = UUID(), true
	if err = q.InsertEvidence(ctx, db.InsertEvidenceParams{
		ID: receipt.EvidenceID, SubjectID: subject.ID, PolicyVersionID: policyVersionID, Decision: string(a.Decision),
		ActorKind: a.ActorKind, Source: source, OccurredAt: a.OccurredAt, ProofChannel: a.ProofChannel,
		ProofReference: a.ProofReference, IdempotencyKey: a.IdempotencyKey, Quarantined: a.Quarantined,
		AuthorityID: optionalUUID(a.AuthorityID), NoticeReference: pgtype.Text{String: a.NoticeReference, Valid: a.NoticeReference != ""},
		ProofDigest: a.ProofDigest, SourceGeneration: pgtype.Int8{Int64: a.SourceGeneration, Valid: a.SourceGeneration > 0},
		ReceiptKey: optionalUUID(a.ReceiptKey), ReceiptSignature: a.ReceiptSignature,
	}); err != nil {
		return EvidenceReceipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return EvidenceReceipt{}, err
	}
	return receipt, nil
}

// verifyPolicyAccepts allows a grant only against the active version. Withdrawals, denials and objections
// are never refused because a version was retired (ADR 0009 section 5); drafts accept nothing.
func verifyPolicyAccepts(ctx context.Context, q *db.Queries, id string, decision consent.Decision) error {
	pv, err := q.GetPolicyVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return problem(422, "Unknown policy", "The policy version does not exist.")
	}
	if err != nil {
		return err
	}
	if pv.State == "active" || (pv.State == "retired" && decision != consent.Grant) {
		return nil
	}
	return problem(409, "Policy not active", "A grant needs the active policy version; drafts accept no evidence.")
}

// sameEvidence ignores occurred_at for customer decisions: the server sets it on every attempt, so
// it cannot distinguish a retry from a different request.
func sameEvidence(e db.GetEvidenceByIdempotencyKeyRow, policyVersionID string, a consent.Append) bool {
	diff := e.OccurredAt.Sub(a.OccurredAt)
	return e.PolicyVersionID == policyVersionID && e.Decision == string(a.Decision) && e.ActorKind == a.ActorKind &&
		e.Source.String == a.Source && e.ProofChannel == a.ProofChannel && e.ProofReference == a.ProofReference &&
		e.NoticeReference.String == a.NoticeReference && bytes.Equal(e.ProofDigest, a.ProofDigest) &&
		bytes.Equal(e.ReceiptSignature, a.ReceiptSignature) &&
		(a.ActorKind == "customer" || (diff < time.Microsecond && diff > -time.Microsecond))
}
