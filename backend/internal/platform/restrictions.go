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

// Restriction reasons. A restriction names the GDPR Art. 18(1) ground it rests on; lifting names why it
// ended. Codes only: decisions never carry free text.
var (
	restrictReasons = []string{"accuracy_contested", "unlawful_processing", "legal_claims", "objection_pending"}
	liftReasons     = []string{"accuracy_verified", "claim_resolved", "objection_decided", "restriction_withdrawn"}
)

// maxRestrictionReview bounds a restriction's review date, so no restriction is open-ended.
const maxRestrictionReview = 366 * 24 * time.Hour

// RestrictionDecision is one restrict or lift decision about a profile.
type RestrictionDecision struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	ReviewBy  string    `json:"review_by,omitempty"`
	Actor     string    `json:"actor"`
	DecidedAt time.Time `json:"decided_at"`
}

// Restriction is a profile's restriction state and its decisions, newest first.
type Restriction struct {
	ProfileID  string                `json:"profile_id"`
	Restricted bool                  `json:"restricted"`
	Decisions  []RestrictionDecision `json:"decisions"`
}

func restrictionDecisionFromRow(r db.ProcessingRestrictionDecision) RestrictionDecision {
	d := RestrictionDecision{ID: r.ID, Action: r.Action, Reason: r.Reason, Actor: r.Actor, DecidedAt: r.DecidedAt}
	if r.ReviewBy.Valid {
		d.ReviewBy = r.ReviewBy.Time.Format(time.DateOnly)
	}
	return d
}

// DecideRestriction restricts or lifts processing of a profile. The flag and the decision change in one
// transaction under the profile's row lock, so writes, views and dispatch see either both or neither. A
// restriction needs an Art. 18(1) reason and a review date at most a year ahead; restricting a restricted
// profile, or lifting an unrestricted one, is refused rather than repeated. Only an active profile's
// restriction can change: an erasure fence is never loosened here.
func (s Store) DecideRestriction(ctx context.Context, profileID, action, reason, reviewBy, actor string) (Restriction, error) {
	var review pgtype.Date
	switch action {
	case "restrict":
		if !slices.Contains(restrictReasons, reason) {
			return Restriction{}, problem(422, "Invalid restriction", "reason must be accuracy_contested, unlawful_processing, legal_claims or objection_pending.")
		}
		date, err := time.Parse(time.DateOnly, reviewBy)
		today := time.Now().UTC().Truncate(24 * time.Hour)
		if err != nil || !date.After(today) || date.Sub(today) > maxRestrictionReview {
			return Restriction{}, problem(422, "Invalid restriction", "review_by must be a date after today and at most a year ahead.")
		}
		review = pgtype.Date{Time: date, Valid: true}
	case "lift":
		if !slices.Contains(liftReasons, reason) || reviewBy != "" {
			return Restriction{}, problem(422, "Invalid restriction", "Lifting needs reason accuracy_verified, claim_resolved, objection_decided or restriction_withdrawn, and no review_by.")
		}
	default:
		return Restriction{}, problem(422, "Invalid restriction", "action must be restrict or lift.")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Restriction{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	state, err := q.LockProfileForRestriction(ctx, profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Restriction{}, problem(404, "Not found", "Profile does not exist.")
	}
	if err != nil {
		return Restriction{}, err
	}
	switch {
	case state.LifecycleState != "active":
		return Restriction{}, problem(409, "Profile not active", "Only an active profile's restriction can change.")
	case action == "restrict" && state.Restricted:
		return Restriction{}, problem(409, "Already restricted", "Lift the current restriction before restricting again.")
	case action == "lift" && !state.Restricted:
		return Restriction{}, problem(409, "Not restricted", "The profile is not restricted.")
	}
	if err = q.SetProfileRestricted(ctx, db.SetProfileRestrictedParams{ID: profileID, Restricted: action == "restrict"}); err != nil {
		return Restriction{}, err
	}
	// A restricted profile is never linked (ADR 0018); the reconciler unlinks or relinks it.
	if err = q.EnqueueReconciliation(ctx, profileID); err != nil {
		return Restriction{}, err
	}
	if _, err = q.InsertRestrictionDecision(ctx, db.InsertRestrictionDecisionParams{ID: UUID(), ProfileID: profileID, Action: action,
		Reason: reason, ReviewBy: review, Actor: actor}); err != nil {
		return Restriction{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Restriction{}, err
	}
	return s.Restriction(ctx, profileID)
}

// Restriction returns a profile's restriction state and its latest decisions.
func (s Store) Restriction(ctx context.Context, profileID string) (Restriction, error) {
	q := db.New(s.Pool)
	state, err := q.GetProfileLifecycleState(ctx, profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Restriction{}, problem(404, "Not found", "Profile does not exist.")
	}
	if err != nil {
		return Restriction{}, err
	}
	rows, err := q.ListRestrictionDecisions(ctx, profileID)
	out := Restriction{ProfileID: profileID, Restricted: state.Restricted, Decisions: make([]RestrictionDecision, 0, len(rows))}
	for _, r := range rows {
		out.Decisions = append(out.Decisions, restrictionDecisionFromRow(r))
	}
	return out, err
}

// RestrictionDue is a restriction whose review date has passed; it stays in force until lifted.
type RestrictionDue struct {
	ProfileID string    `json:"profile_id"`
	Reason    string    `json:"reason"`
	ReviewBy  string    `json:"review_by"`
	DecidedAt time.Time `json:"decided_at"`
}

// RestrictionsDue lists up to 500 restrictions due for review, oldest review date first.
func (s Store) RestrictionsDue(ctx context.Context) ([]RestrictionDue, error) {
	today := pgtype.Date{Time: time.Now().UTC().Truncate(24 * time.Hour), Valid: true}
	rows, err := db.New(s.Pool).ListRestrictionsDueForReview(ctx, today)
	out := make([]RestrictionDue, 0, len(rows))
	for _, r := range rows {
		out = append(out, RestrictionDue{ProfileID: r.ProfileID, Reason: r.Reason, ReviewBy: r.ReviewBy.Time.Format(time.DateOnly), DecidedAt: r.DecidedAt})
	}
	return out, err
}
