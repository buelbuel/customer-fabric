package platform

import (
	"context"
	"errors"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

const noSubject = "00000000-0000-0000-0000-000000000000"

// CustomerPolicy is the notice a customer decides on. Operator-only fields such as carry-forward stay out.
type CustomerPolicy struct {
	ID                 string `json:"id"`
	Version            int32  `json:"version"`
	State              string `json:"state"`
	LawfulBasis        string `json:"lawful_basis"`
	Explanation        string `json:"explanation"`
	NoticeReference    string `json:"notice_reference"`
	Jurisdiction       string `json:"jurisdiction"`
	Locale             string `json:"locale"`
	Channel            string `json:"channel"`
	RetentionReference string `json:"retention_reference"`
}

// CustomerConsent is one purpose as the authenticated customer sees it.
type CustomerConsent struct {
	PurposeID          string             `json:"purpose_id"`
	PurposeName        string             `json:"purpose_name"`
	Policy             CustomerPolicy     `json:"policy"`
	Effective          consent.Evaluation `json:"effective"`
	Revision           int64              `json:"revision"`
	LatestDecision     consent.Decision   `json:"latest_decision,omitempty"`
	LatestDecisionAt   *time.Time         `json:"latest_decision_at,omitempty"`
	AvailableDecisions []consent.Decision `json:"available_decisions"`
}

type CustomerConsentPage struct {
	Items      []CustomerConsent `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// CustomerEvidence is one audited decision. Proof references and source names are not disclosed.
type CustomerEvidence struct {
	ID              string           `json:"id"`
	PurposeID       string           `json:"purpose_id"`
	PurposeName     string           `json:"purpose_name"`
	PolicyVersionID string           `json:"policy_version_id"`
	PolicyVersion   int32            `json:"policy_version"`
	Decision        consent.Decision `json:"decision"`
	ActorKind       string           `json:"actor_kind"`
	ReceivedAt      time.Time        `json:"received_at"`
}

type CustomerEvidencePage struct {
	Items      []CustomerEvidence `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// CustomerDecision is a change the authenticated customer asked for. The route sets the subject and
// channel; the body can name only the version and the decision.
type CustomerDecision struct {
	KratosSubject    string
	PolicyVersionID  string
	Decision         consent.Decision
	IdempotencyKey   string
	ProofChannel     string
	ExpectedRevision int64
}

func customerProfileFor(ctx context.Context, q *db.Queries, kratos string) (string, error) {
	profileID, err := q.GetCustomerProfileID(ctx, kratos)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", problem(404, "Not found", "The customer profile has not been provisioned.")
	}
	return profileID, err
}

// availableDecisions offers only what ADR 0009 lets this API record safely. An objection is not
// offered, because M2 has no way for the customer to lift one.
func availableDecisions(basis, state string) []consent.Decision {
	switch {
	case basis != string(consent.Consent):
		return []consent.Decision{}
	case state == "active":
		return []consent.Decision{consent.Grant, consent.Withdraw}
	}
	return []consent.Decision{consent.Withdraw}
}

// ListCustomerConsent returns the customer's purposes and effective state from one snapshot.
func (s Store) ListCustomerConsent(ctx context.Context, kratos, cursor string, limit int) (CustomerConsentPage, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CustomerConsentPage{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	profileID, err := customerProfileFor(ctx, q, kratos)
	if err != nil {
		return CustomerConsentPage{}, err
	}
	subjectID := noSubject
	if subject, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(profileID)); err == nil {
		subjectID = subject.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return CustomerConsentPage{}, err
	}
	if cursor == "" {
		cursor = noSubject
	}
	rows, err := q.ListCustomerConsentPolicies(ctx, db.ListCustomerConsentPoliciesParams{Cursor: cursor, SubjectID: subjectID, RowLimit: int32(limit + 1)})
	if err != nil {
		return CustomerConsentPage{}, err
	}
	page := CustomerConsentPage{Items: make([]CustomerConsent, 0, min(limit, len(rows)))}
	for _, r := range rows[:min(limit, len(rows))] {
		state, err := purposeStateFor(ctx, q, profileID, kratos, r.PurposeID)
		if err != nil {
			return CustomerConsentPage{}, err
		}
		page.Items = append(page.Items, CustomerConsent{
			PurposeID: r.PurposeID, PurposeName: r.PurposeName,
			Policy: CustomerPolicy{ID: r.ID, Version: r.Version, State: r.State, LawfulBasis: r.LawfulBasis, Explanation: r.Explanation,
				NoticeReference: r.NoticeReference, Jurisdiction: r.Jurisdiction, Locale: r.Locale, Channel: r.Channel, RetentionReference: r.RetentionReference},
			Effective: state.Evaluation, Revision: state.Revision, LatestDecision: state.Latest, LatestDecisionAt: state.LatestAt,
			AvailableDecisions: availableDecisions(r.LawfulBasis, r.State),
		})
	}
	if len(rows) > limit {
		page.NextCursor = rows[limit-1].PurposeID
	}
	return page, nil
}

// ListCustomerEvidence returns the customer's own audit trail, newest first.
func (s Store) ListCustomerEvidence(ctx context.Context, kratos, cursor string, limit int) (CustomerEvidencePage, error) {
	q := db.New(s.Pool)
	profileID, err := customerProfileFor(ctx, q, kratos)
	if err != nil {
		return CustomerEvidencePage{}, err
	}
	page := CustomerEvidencePage{Items: []CustomerEvidence{}}
	subject, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return page, nil
	}
	if err != nil {
		return CustomerEvidencePage{}, err
	}
	rows, err := q.ListSubjectEvidenceHistory(ctx, db.ListSubjectEvidenceHistoryParams{SubjectID: subject.ID, Cursor: cursor, RowLimit: int32(limit + 1)})
	if err != nil {
		return CustomerEvidencePage{}, err
	}
	for _, r := range rows[:min(limit, len(rows))] {
		page.Items = append(page.Items, CustomerEvidence{ID: r.ID, PurposeID: r.PurposeID, PurposeName: r.PurposeName, PolicyVersionID: r.PolicyVersionID,
			PolicyVersion: r.PolicyVersion, Decision: consent.Decision(r.Decision), ActorKind: r.ActorKind, ReceivedAt: r.ReceivedAt})
	}
	if len(rows) > limit {
		page.NextCursor = rows[limit-1].ID
	}
	return page, nil
}

// AppendCustomerDecision records a grant or withdrawal by the authenticated customer. A grant must
// be based on the state the customer last saw, so a stale tab cannot undo a newer withdrawal. A
// withdrawal is never refused for staleness (ADR 0009 section 5).
func (s Store) AppendCustomerDecision(ctx context.Context, d CustomerDecision) (EvidenceReceipt, error) {
	profileID, err := db.New(s.Pool).GetCustomerProfileID(ctx, d.KratosSubject)
	if errors.Is(err, pgx.ErrNoRows) {
		return EvidenceReceipt{}, problem(409, "Profile required", "Provision the customer profile before recording a decision.")
	}
	if err != nil {
		return EvidenceReceipt{}, err
	}
	var purposeID string
	return s.appendEvidence(ctx, d.PolicyVersionID, consent.Append{
		Decision: d.Decision, ActorKind: "customer", ProofChannel: d.ProofChannel, ProofReference: "kratos_identity:" + d.KratosSubject,
		OccurredAt: time.Now().UTC(), IdempotencyKey: d.IdempotencyKey,
	}, func(q *db.Queries) (consent.Subject, error) {
		policy, err := q.GetPolicyVersion(ctx, d.PolicyVersionID)
		if err != nil {
			return consent.Subject{}, err
		}
		if policy.LawfulBasis != string(consent.Consent) {
			return consent.Subject{}, problem(422, "Invalid decision", "This purpose does not rest on consent, so it has no consent choice.")
		}
		purposeID = policy.PurposeID
		return customerSubjectFor(ctx, q, profileID)
	}, func(q *db.Queries, subject consent.Subject) error {
		if d.Decision != consent.Grant {
			return nil
		}
		if subject.LifecycleState != "active" {
			return ErrForbidden
		}
		state, err := purposeStateFor(ctx, q, profileID, d.KratosSubject, purposeID)
		if err != nil {
			return err
		}
		if state.Revision != d.ExpectedRevision {
			return problem(412, "Stale consent state", "Your choices changed elsewhere. Read the current state and decide again.")
		}
		return nil
	})
}
