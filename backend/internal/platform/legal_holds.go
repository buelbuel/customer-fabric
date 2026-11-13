package platform

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Legal holds (#30). A hold preserves named categories of one scope's data against deletion: a source
// record, or a customer with the records verifiably associated with it. One privacy admin requests it and
// a different one approves it; a request protects while it awaits approval, for at most holdApprovalWindow.
// An approved hold stays in force until released, also past its review date, when it is listed for
// review. Retention pruning skips what holds preserve; erasure (#27) will consult the same holds.
var (
	holdCategories     = []string{"profile_data", "consent_evidence", "notifications", "eventing"}
	holdReasons        = []string{"litigation", "regulatory_investigation", "statutory_obligation", "internal_investigation"}
	holdReleaseReasons = []string{"matter_closed", "obligation_ended", "request_withdrawn", "not_approved"}
)

const (
	holdApprovalWindow = 14 * 24 * time.Hour
	maxHoldReview      = 366 * 24 * time.Hour
)

// LegalHoldDecision is one decision on a hold.
type LegalHoldDecision struct {
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	Actor     string    `json:"actor"`
	DecidedAt time.Time `json:"decided_at"`
}

// LegalHold is a hold and its decisions. Status is in_force, awaiting approval or not: requested,
// active, lapsed (a request not approved in time), due (active past its review date) or released. The
// legal reference is shown to privacy admins only.
type LegalHold struct {
	ID             string              `json:"id"`
	Scope          string              `json:"scope"`
	ProfileID      string              `json:"profile_id"`
	Categories     []string            `json:"categories"`
	Reason         string              `json:"reason"`
	LegalReference string              `json:"legal_reference,omitempty"`
	ReviewBy       string              `json:"review_by"`
	Status         string              `json:"status"`
	InForce        bool                `json:"in_force"`
	RequestedBy    string              `json:"requested_by"`
	RequestedAt    time.Time           `json:"requested_at"`
	ApprovedBy     string              `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time          `json:"approved_at,omitempty"`
	ReleasedBy     string              `json:"released_by,omitempty"`
	ReleasedAt     *time.Time          `json:"released_at,omitempty"`
	ReleaseReason  string              `json:"release_reason,omitempty"`
	Decisions      []LegalHoldDecision `json:"decisions"`
}

func legalHold(r db.LegalHold, decisions []LegalHoldDecision, redact bool, now time.Time) LegalHold {
	h := LegalHold{ID: r.ID, Scope: r.Scope, ProfileID: r.ProfileID, Categories: r.Categories, Reason: r.Reason,
		ReviewBy: r.ReviewBy.Time.Format(time.DateOnly), RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt,
		ApprovedBy: r.ApprovedBy.String, ReleasedBy: r.ReleasedBy.String, ReleaseReason: r.ReleaseReason.String, Decisions: decisions}
	if !redact {
		h.LegalReference = r.LegalReference
	}
	if r.ApprovedAt.Valid {
		h.ApprovedAt = &r.ApprovedAt.Time
	}
	if r.ReleasedAt.Valid {
		h.ReleasedAt = &r.ReleasedAt.Time
	}
	switch {
	case r.State == "released":
		h.Status = "released"
	case r.State == "requested" && now.Sub(r.RequestedAt) >= holdApprovalWindow:
		h.Status = "lapsed"
	case r.State == "requested":
		h.Status, h.InForce = "requested", true
	case !r.ReviewBy.Time.After(now.UTC().Truncate(24 * time.Hour)):
		h.Status, h.InForce = "due", true
	default:
		h.Status, h.InForce = "active", true
	}
	if h.Decisions == nil {
		h.Decisions = []LegalHoldDecision{}
	}
	return h
}

// HoldRequest is what a privacy admin asks to preserve, why, and until when it is reviewed.
type HoldRequest struct {
	Scope          string   `json:"scope"`
	ProfileID      string   `json:"profile_id"`
	Categories     []string `json:"categories"`
	Reason         string   `json:"reason"`
	LegalReference string   `json:"legal_reference"`
	ReviewBy       string   `json:"review_by"`
}

// RequestHold records a hold request. It preserves its categories at once, while it awaits approval.
func (s Store) RequestHold(ctx context.Context, req HoldRequest, actor string) (LegalHold, error) {
	invalid := func(detail string) (LegalHold, error) { return LegalHold{}, problem(422, "Invalid legal hold", detail) }
	categories := slices.Compact(slices.Sorted(slices.Values(req.Categories)))
	switch {
	case req.Scope != "record" && req.Scope != "customer":
		return invalid("scope must be record or customer.")
	case len(categories) == 0 || len(categories) != len(req.Categories) || slices.ContainsFunc(categories, func(c string) bool { return !slices.Contains(holdCategories, c) }):
		return invalid("categories must name distinct ones of profile_data, consent_evidence, notifications and eventing.")
	case !slices.Contains(holdReasons, req.Reason):
		return invalid("reason must be litigation, regulatory_investigation, statutory_obligation or internal_investigation.")
	case !governanceRef.MatchString(req.LegalReference):
		return invalid("legal_reference must be 1 to 128 letters, digits or ._:/- characters.")
	}
	review, err := time.Parse(time.DateOnly, req.ReviewBy)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err != nil || !review.After(today) || review.Sub(today) > maxHoldReview {
		return invalid("review_by must be a date after today and at most a year ahead.")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return LegalHold{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	target, err := q.HoldScopeTarget(ctx, req.ProfileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, problem(404, "Not found", "Unknown profile.")
	}
	if err != nil {
		return LegalHold{}, err
	}
	if req.Scope == "record" && !target.SourceRecord || req.Scope == "customer" && !target.CustomerAnchor {
		return invalid("A record hold names a source record, and a customer hold a customer's profile anchor.")
	}
	row, err := q.InsertLegalHold(ctx, db.InsertLegalHoldParams{ID: UUID(), Scope: req.Scope, ProfileID: req.ProfileID, Categories: categories,
		Reason: req.Reason, LegalReference: req.LegalReference, ReviewBy: pgtype.Date{Time: review, Valid: true}, RequestedBy: actor})
	if err != nil {
		return LegalHold{}, err
	}
	if err = q.InsertLegalHoldDecision(ctx, db.InsertLegalHoldDecisionParams{ID: UUID(), HoldID: row.ID, Action: "requested", Reason: req.Reason, Actor: actor}); err != nil {
		return LegalHold{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LegalHold{}, err
	}
	return s.Hold(ctx, row.ID, false)
}

// ApproveHold puts a requested hold in force. The approver must not be the requester, and a request
// that lapsed must be requested again.
func (s Store) ApproveHold(ctx context.Context, id, actor string) (LegalHold, error) {
	return s.decideHold(ctx, id, func(q *db.Queries, h db.LegalHold) (string, error) {
		switch {
		case h.State != "requested":
			return "", problem(409, "Not awaiting approval", "Only a requested hold can be approved.")
		case time.Since(h.RequestedAt) >= holdApprovalWindow:
			return "", problem(409, "Request lapsed", "The request was not approved within 14 days; request the hold again.")
		case h.RequestedBy == actor:
			return "", problem(403, "Forbidden", "A hold is approved by a different privacy admin than the one who requested it.")
		}
		_, err := q.ApproveLegalHold(ctx, db.ApproveLegalHoldParams{ID: id, Actor: actor})
		return "approved", err
	}, "approved", actor)
}

// ReleaseHold ends a hold, citing why. Releasing preserves nothing further and resumes nothing: data it
// kept is pruned or erased only by the processes that apply once it is gone.
func (s Store) ReleaseHold(ctx context.Context, id, reason, actor string) (LegalHold, error) {
	if !slices.Contains(holdReleaseReasons, reason) {
		return LegalHold{}, problem(422, "Invalid release", "reason must be matter_closed, obligation_ended, request_withdrawn or not_approved.")
	}
	return s.decideHold(ctx, id, func(q *db.Queries, h db.LegalHold) (string, error) {
		if h.State == "released" {
			return "", problem(409, "Released", "The hold is already released.")
		}
		_, err := q.ReleaseLegalHold(ctx, db.ReleaseLegalHoldParams{ID: id, Actor: actor, Reason: reason})
		return reason, err
	}, "released", actor)
}

func (s Store) decideHold(ctx context.Context, id string, decide func(*db.Queries, db.LegalHold) (string, error), action, actor string) (LegalHold, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return LegalHold{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	h, err := q.LockLegalHold(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, problem(404, "Not found", "Unknown legal hold.")
	}
	if err != nil {
		return LegalHold{}, err
	}
	reason, err := decide(q, h)
	if err != nil {
		return LegalHold{}, err
	}
	if err = q.InsertLegalHoldDecision(ctx, db.InsertLegalHoldDecisionParams{ID: UUID(), HoldID: id, Action: action, Reason: reason, Actor: actor}); err != nil {
		return LegalHold{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return LegalHold{}, err
	}
	return s.Hold(ctx, id, false)
}

// Hold reads one hold with its decisions.
func (s Store) Hold(ctx context.Context, id string, redact bool) (LegalHold, error) {
	q := db.New(s.Pool)
	row, err := q.GetLegalHold(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return LegalHold{}, problem(404, "Not found", "Unknown legal hold.")
	}
	if err != nil {
		return LegalHold{}, err
	}
	holds, err := s.withDecisions(ctx, q, []db.LegalHold{row}, redact)
	if err != nil {
		return LegalHold{}, err
	}
	return holds[0], nil
}

// Holds lists the holds not released, or every hold of one profile; due lists only those needing review.
func (s Store) Holds(ctx context.Context, profileID string, due, redact bool) ([]LegalHold, error) {
	q := db.New(s.Pool)
	var rows []db.LegalHold
	var err error
	if due {
		rows, err = q.ListLegalHoldsDue(ctx, pgtype.Date{Time: time.Now().UTC().Truncate(24 * time.Hour), Valid: true})
	} else {
		rows, err = q.ListLegalHolds(ctx, profileID)
	}
	if err != nil {
		return nil, err
	}
	return s.withDecisions(ctx, q, rows, redact)
}

func (s Store) withDecisions(ctx context.Context, q *db.Queries, rows []db.LegalHold, redact bool) ([]LegalHold, error) {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	decisions, err := q.ListLegalHoldDecisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	byHold := map[string][]LegalHoldDecision{}
	for _, d := range decisions {
		byHold[d.HoldID] = append(byHold[d.HoldID], LegalHoldDecision{Action: d.Action, Reason: d.Reason, Actor: d.Actor, DecidedAt: d.DecidedAt})
	}
	now := time.Now()
	out := make([]LegalHold, 0, len(rows))
	for _, r := range rows {
		out = append(out, legalHold(r, byHold[r.ID], redact, now))
	}
	return out, nil
}
