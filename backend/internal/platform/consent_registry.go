package platform

import (
	"context"
	"errors"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned for a missing purpose or policy version.
var ErrNotFound = problem(404, "Not found", "The requested consent resource does not exist.")

// CreatePurpose registers a purpose name. Duplicate names are rejected by the unique constraint.
func (s Store) CreatePurpose(ctx context.Context, name string) (consent.Purpose, error) {
	if !consent.Name.MatchString(name) {
		return consent.Purpose{}, problem(422, "Invalid purpose", "Purpose names must be lowercase identifiers.")
	}
	id := UUID()
	if err := db.New(s.Pool).InsertPurpose(ctx, db.InsertPurposeParams{ID: id, Name: name}); err != nil {
		return consent.Purpose{}, dbError(err)
	}
	return consent.Purpose{ID: id, Name: name}, nil
}

// ListPolicyVersions returns every version of a purpose in version order.
func (s Store) ListPolicyVersions(ctx context.Context, purposeID string) ([]consent.PolicyVersion, error) {
	rows, err := db.New(s.Pool).ListPolicyVersions(ctx, purposeID)
	if err != nil {
		return nil, err
	}
	out := make([]consent.PolicyVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, policyFromRow(r))
	}
	return out, nil
}

// DraftPolicyVersion appends a new draft version for a purpose. It never edits an existing version.
func (s Store) DraftPolicyVersion(ctx context.Context, purposeID string, v consent.PolicyVersion) (consent.PolicyVersion, error) {
	if err := v.Validate(); err != nil {
		return consent.PolicyVersion{}, problem(422, "Invalid policy", err.Error())
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return consent.PolicyVersion{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockPurposePolicies(ctx, purposeID); err != nil {
		return consent.PolicyVersion{}, err
	}
	if _, err = q.GetPurpose(ctx, purposeID); errors.Is(err, pgx.ErrNoRows) {
		return consent.PolicyVersion{}, ErrNotFound
	} else if err != nil {
		return consent.PolicyVersion{}, err
	}
	next, err := q.NextPolicyVersion(ctx, purposeID)
	if err != nil {
		return consent.PolicyVersion{}, err
	}
	v.ID, v.PurposeID, v.Version, v.State = UUID(), purposeID, next, "draft"
	if err = q.InsertPolicyVersion(ctx, db.InsertPolicyVersionParams{
		ID: v.ID, PurposeID: purposeID, Version: next, State: "draft", LawfulBasis: string(v.LawfulBasis),
		NoticeReference: v.NoticeReference, Jurisdiction: v.Jurisdiction, Locale: v.Locale, Channel: v.Channel,
		RetentionReference: v.RetentionReference, CarryForward: v.CarryForward,
	}); err != nil {
		return consent.PolicyVersion{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return consent.PolicyVersion{}, err
	}
	return v, nil
}

// TransitionPolicyVersion applies a reviewable state change under the purpose lock.
// Retiring the prior active version is a separate, explicit step so no activation is silent.
func (s Store) TransitionPolicyVersion(ctx context.Context, id, to string) (consent.PolicyVersion, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return consent.PolicyVersion{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	current, err := q.GetPolicyVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return consent.PolicyVersion{}, ErrNotFound
	}
	if err != nil {
		return consent.PolicyVersion{}, err
	}
	if !consent.CanTransition(current.State, to) {
		return consent.PolicyVersion{}, problem(409, "Invalid transition", "That policy version cannot move to "+to+".")
	}
	if err = q.LockPurposePolicies(ctx, current.PurposeID); err != nil {
		return consent.PolicyVersion{}, err
	}
	switch to {
	case "active":
		// Retirement of the current active version must already have happened; the partial unique index enforces it.
		if err = q.ActivatePolicyVersion(ctx, id); err != nil {
			return consent.PolicyVersion{}, consentError(err, "Another version of this purpose is already active; retire it first.")
		}
	case "retired":
		if err = q.RetirePolicyVersion(ctx, id); err != nil {
			return consent.PolicyVersion{}, consentError(err, "This version could not be retired.")
		}
	}
	updated, err := q.GetPolicyVersion(ctx, id)
	if err != nil {
		return consent.PolicyVersion{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return consent.PolicyVersion{}, err
	}
	return policyFromRow(updated), nil
}

// consentError maps the registry's database constraint failures to reviewable HTTP problems so a
// conflict is not reported as an internal error. Only SQLSTATE is inspected; no message is echoed.
func consentError(err error, detail string) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "23505": // unique violation: single active version, duplicate name
			return problem(409, "Conflict", detail)
		case "P0001": // trigger raise: immutability guard
			return problem(409, "Conflict", "Active or retired policy versions are immutable.")
		}
	}
	return err
}

func policyFromRow(r db.ConsentPolicyVersion) consent.PolicyVersion {
	return consent.PolicyVersion{
		ID: r.ID, PurposeID: r.PurposeID, Version: r.Version, State: r.State, LawfulBasis: consent.LawfulBasis(r.LawfulBasis),
		NoticeReference: r.NoticeReference, Jurisdiction: r.Jurisdiction, Locale: r.Locale, Channel: r.Channel,
		RetentionReference: r.RetentionReference, CarryForward: r.CarryForward, CreatedAt: r.CreatedAt,
	}
}
