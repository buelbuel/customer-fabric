package platform

import (
	"context"
	"errors"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// EvaluateCustomerPermission returns the effective answer for the customer anchored on profileID and a
// purpose, at "now". It reads committed evidence under a repeatable-read snapshot and never writes.
func (s Store) EvaluateCustomerPermission(ctx context.Context, profileID, purposeID string) (consent.Evaluation, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return consent.Evaluation{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	policy, err := q.GetActivePolicyForPurpose(ctx, purposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No active policy means we cannot establish a lawful basis: fail closed for consent-dependent work.
		return consent.Evaluation{Outcome: consent.FailingClosed, Basis: "no_active_policy", Reasons: []string{"purpose has no active policy version"}}, nil
	}
	if err != nil {
		return consent.Evaluation{}, err
	}
	basis := consent.LawfulBasis(policy.LawfulBasis)
	subject, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		// A consent subject is only required for consent-based processing. Other bases do not depend on it.
		if basis.ConsentBased() {
			return consent.Evaluation{Outcome: consent.FailingClosed, Basis: "no_subject", Reasons: []string{"no consent subject for this customer"}}, nil
		}
		return consent.Evaluate(time.Now(), basis, nil), nil
	}
	if err != nil {
		return consent.Evaluation{}, err
	}
	rows, err := q.ListEvidenceForSubjectPurpose(ctx, db.ListEvidenceForSubjectPurposeParams{SubjectID: subject.ID, PurposeID: purposeID})
	if err != nil {
		return consent.Evaluation{}, err
	}
	evidence := make([]consent.Evidence, 0, len(rows))
	for _, r := range rows {
		evidence = append(evidence, consent.Evidence{
			Decision: consent.Decision(r.Decision), ReceivedAt: r.ReceivedAt, ActorKind: r.ActorKind,
			Quarantined: r.Quarantined,
		})
	}
	return consent.Evaluate(time.Now(), basis, evidence), nil
}
