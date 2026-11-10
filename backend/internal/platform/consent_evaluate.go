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
	states, err := purposeStatesFor(ctx, q, []customerAnchor{{ProfileID: profileID, Kratos: kratos}}, purposeID)
	if err != nil {
		return purposeState{}, err
	}
	return states[0], nil
}

// purposeStatesFor evaluates one purpose for many customers through q with a fixed number of reads, in
// the order of anchors. Each state is exactly what a one-customer evaluation decides.
func purposeStatesFor(ctx context.Context, q *db.Queries, anchors []customerAnchor, purposeID string) ([]purposeState, error) {
	active, err := q.GetActivePolicyForPurpose(ctx, purposeID)
	hasPolicy := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows, err := q.ListPolicyVersionsForPurpose(ctx, purposeID)
	if err != nil {
		return nil, err
	}
	versions := make([]consent.PolicyVersion, 0, len(rows))
	for _, r := range rows {
		versions = append(versions, consent.PolicyVersion{Version: r.Version, LawfulBasis: consent.LawfulBasis(r.LawfulBasis), CarryForward: r.CarryForward})
	}
	ids, kratos := make([]string, len(anchors)), make([]string, len(anchors))
	for i, a := range anchors {
		ids[i], kratos[i] = a.ProfileID, a.Kratos
	}
	subjectRows, err := q.ListCustomerSubjectsByAnchors(ctx, ids)
	if err != nil {
		return nil, err
	}
	subjects := map[string]db.ListCustomerSubjectsByAnchorsRow{}
	subjectIDs := make([]string, 0, len(subjectRows))
	for _, s := range subjectRows {
		subjects[s.AnchorProfileID] = s
		subjectIDs = append(subjectIDs, s.ID)
	}
	ownRows, err := q.ListCustomerEvidenceFor(ctx, db.ListCustomerEvidenceForParams{SubjectIds: subjectIDs, PurposeID: purposeID, RowLimit: evidenceLimit + 1})
	if err != nil {
		return nil, err
	}
	own := map[string][]db.ListCustomerEvidenceForRow{}
	for _, r := range ownRows {
		own[r.SubjectID] = append(own[r.SubjectID], r)
	}
	carriedRows, err := q.ListCarriedRecordEvidenceFor(ctx, db.ListCarriedRecordEvidenceForParams{CustomerSubjects: kratos, PurposeID: purposeID, RowLimit: evidenceLimit + 1})
	if err != nil {
		return nil, err
	}
	carried := map[string][]db.ListCarriedRecordEvidenceForRow{}
	for _, r := range carriedRows {
		carried[r.CustomerSubject] = append(carried[r.CustomerSubject], r)
	}
	states := make([]purposeState, len(anchors))
	for i, a := range anchors {
		state := purposeState{Revision: 1}
		var evidence []consent.Evidence
		subjectActive, tooMuch := true, false
		if subject, ok := subjects[a.ProfileID]; ok {
			subjectActive = subject.LifecycleState == "active"
			mine := own[subject.ID]
			tooMuch = len(mine) > evidenceLimit
			state.Revision += int64(len(mine))
			for _, r := range mine {
				evidence = append(evidence, consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.ReceivedAt, ActorKind: r.ActorKind,
					Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis)})
				if state.LatestAt == nil || r.ReceivedAt.After(*state.LatestAt) {
					at := r.ReceivedAt
					state.Latest, state.LatestAt = consent.Decision(r.Decision), &at
				}
			}
		}
		fromRecords := carried[a.Kratos]
		state.Revision += int64(len(fromRecords))
		tooMuch = tooMuch || len(fromRecords) > evidenceLimit
		failClosed := func(basis, reason string) purposeState {
			state.Evaluation = consent.Evaluation{Outcome: consent.FailingClosed, Basis: basis, Reasons: []string{reason}}
			return state
		}
		switch {
		case !hasPolicy:
			states[i] = failClosed("no_active_policy", "purpose has no active policy version")
			continue
		case !subjectActive:
			states[i] = failClosed("subject_not_active", "the consent subject is not active")
			continue
		case tooMuch:
			states[i] = failClosed("evidence_volume", "too much evidence to evaluate safely")
			continue
		}
		for _, r := range fromRecords {
			evidence = append(evidence, consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.EffectiveAt, ActorKind: r.ActorKind,
				Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis), FromRecord: true})
		}
		state.Evaluation = consent.Evaluate(consent.PolicyVersion{Version: active.Version, LawfulBasis: consent.LawfulBasis(active.LawfulBasis), CarryForward: active.CarryForward}, versions, evidence)
		states[i] = state
	}
	return states, nil
}
