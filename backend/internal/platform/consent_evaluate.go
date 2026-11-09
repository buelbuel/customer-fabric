package platform

import (
	"context"
	"errors"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// evidenceLimit bounds one evaluation. More evidence than this is treated as ambiguous and fails
// closed, so an older withdrawal can never fall out of a window.
const evidenceLimit = 500

// EvaluateCustomerPermission returns the effective answer for one customer anchor and purpose from
// committed evidence and association history, in one read-only snapshot. It never writes.
func (s Store) EvaluateCustomerPermission(ctx context.Context, profileID, purposeID string) (consent.Evaluation, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return consent.Evaluation{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	kratos, err := q.GetCustomerLinkForProfile(ctx, profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return consent.Evaluation{}, problem(404, "Not found", "The profile is not a customer anchor.")
	}
	if err != nil {
		return consent.Evaluation{}, err
	}
	if _, err = q.GetPurpose(ctx, purposeID); errors.Is(err, pgx.ErrNoRows) {
		return consent.Evaluation{}, problem(404, "Not found", "Unknown purpose.")
	} else if err != nil {
		return consent.Evaluation{}, err
	}
	fail := func(basis, reason string) (consent.Evaluation, error) {
		return consent.Evaluation{Outcome: consent.FailingClosed, Basis: basis, Reasons: []string{reason}}, nil
	}
	active, err := q.GetActivePolicyForPurpose(ctx, purposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("no_active_policy", "purpose has no active policy version")
	}
	if err != nil {
		return consent.Evaluation{}, err
	}
	rows, err := q.ListPolicyVersionsForPurpose(ctx, purposeID)
	if err != nil {
		return consent.Evaluation{}, err
	}
	versions := make([]consent.PolicyVersion, 0, len(rows))
	for _, r := range rows {
		versions = append(versions, consent.PolicyVersion{Version: r.Version, LawfulBasis: consent.LawfulBasis(r.LawfulBasis), CarryForward: r.CarryForward})
	}
	var evidence []consent.Evidence
	subject, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(profileID))
	switch {
	case err == nil:
		if subject.LifecycleState != "active" {
			return fail("subject_not_active", "the consent subject is not active")
		}
		own, err := q.ListCustomerEvidence(ctx, db.ListCustomerEvidenceParams{SubjectID: subject.ID, PurposeID: purposeID})
		if err != nil {
			return consent.Evaluation{}, err
		}
		if len(own) > evidenceLimit {
			return fail("evidence_volume", "too much evidence to evaluate safely")
		}
		for _, r := range own {
			evidence = append(evidence, consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.ReceivedAt, ActorKind: r.ActorKind,
				Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis)})
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return consent.Evaluation{}, err
	}
	carried, err := q.ListCarriedRecordEvidence(ctx, db.ListCarriedRecordEvidenceParams{CustomerSubject: kratos, PurposeID: purposeID})
	if err != nil {
		return consent.Evaluation{}, err
	}
	if len(carried) > evidenceLimit {
		return fail("evidence_volume", "too much evidence to evaluate safely")
	}
	for _, r := range carried {
		evidence = append(evidence, consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.EffectiveAt, ActorKind: r.ActorKind,
			Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis), FromRecord: true})
	}
	return consent.Evaluate(consent.PolicyVersion{Version: active.Version, LawfulBasis: consent.LawfulBasis(active.LawfulBasis), CarryForward: active.CarryForward}, versions, evidence), nil
}
