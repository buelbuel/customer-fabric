package platform

import (
	"context"
	"errors"
	"time"

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
	state, err := purposeStateFor(ctx, q, profileID, kratos, purposeID)
	return state.Evaluation, err
}

// purposeState is what one customer's committed evidence means for one purpose.
type purposeState struct {
	Evaluation consent.Evaluation
	// Revision counts every decision that can affect the purpose. Evidence is append-only and an
	// ended association keeps the decisions it already carried, so it only grows while evaluation
	// is possible; past evidenceLimit it stops growing, but evaluation has then failed closed.
	Revision int64
	Latest   consent.Decision
	LatestAt *time.Time
}

// purposeStateFor reads through q, so the caller's transaction decides the snapshot.
func purposeStateFor(ctx context.Context, q *db.Queries, profileID, kratos, purposeID string) (purposeState, error) {
	state := purposeState{Revision: 1}
	active, err := q.GetActivePolicyForPurpose(ctx, purposeID)
	hasPolicy := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	rows, err := q.ListPolicyVersionsForPurpose(ctx, purposeID)
	if err != nil {
		return state, err
	}
	versions := make([]consent.PolicyVersion, 0, len(rows))
	for _, r := range rows {
		versions = append(versions, consent.PolicyVersion{Version: r.Version, LawfulBasis: consent.LawfulBasis(r.LawfulBasis), CarryForward: r.CarryForward})
	}
	var evidence []consent.Evidence
	subjectActive, tooMuch := true, false
	subject, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(profileID))
	switch {
	case err == nil:
		subjectActive = subject.LifecycleState == "active"
		own, err := q.ListCustomerEvidence(ctx, db.ListCustomerEvidenceParams{SubjectID: subject.ID, PurposeID: purposeID})
		if err != nil {
			return state, err
		}
		tooMuch = len(own) > evidenceLimit
		state.Revision += int64(len(own))
		for _, r := range own {
			evidence = append(evidence, consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.ReceivedAt, ActorKind: r.ActorKind,
				Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis)})
			if state.LatestAt == nil || r.ReceivedAt.After(*state.LatestAt) {
				at := r.ReceivedAt
				state.Latest, state.LatestAt = consent.Decision(r.Decision), &at
			}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return state, err
	}
	carried, err := q.ListCarriedRecordEvidence(ctx, db.ListCarriedRecordEvidenceParams{CustomerSubject: kratos, PurposeID: purposeID})
	if err != nil {
		return state, err
	}
	state.Revision += int64(len(carried))
	tooMuch = tooMuch || len(carried) > evidenceLimit
	failClosed := func(basis, reason string) (purposeState, error) {
		state.Evaluation = consent.Evaluation{Outcome: consent.FailingClosed, Basis: basis, Reasons: []string{reason}}
		return state, nil
	}
	switch {
	case !hasPolicy:
		return failClosed("no_active_policy", "purpose has no active policy version")
	case !subjectActive:
		return failClosed("subject_not_active", "the consent subject is not active")
	case tooMuch:
		return failClosed("evidence_volume", "too much evidence to evaluate safely")
	}
	for _, r := range carried {
		evidence = append(evidence, consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.EffectiveAt, ActorKind: r.ActorKind,
			Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis), FromRecord: true})
	}
	state.Evaluation = consent.Evaluate(consent.PolicyVersion{Version: active.Version, LawfulBasis: consent.LawfulBasis(active.LawfulBasis), CarryForward: active.CarryForward}, versions, evidence)
	return state, nil
}
