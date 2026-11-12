package platform

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ProfileContributor authorizes a source to add its own identifiers and attributes to a profile another
// source created (ADR 0018). Attribute ownership, provenance and tombstones still decide every write.
type ProfileContributor struct {
	ID                    string     `json:"id"`
	ProfileID             string     `json:"profile_id"`
	Source                string     `json:"source"`
	Basis                 string     `json:"basis"`
	MatchReviewID         *string    `json:"match_review_id,omitempty"`
	VerificationReference *string    `json:"verification_reference,omitempty"`
	AuthorizedBy          string     `json:"authorized_by"`
	AuthorizedAt          time.Time  `json:"authorized_at"`
	RevokedBy             *string    `json:"revoked_by,omitempty"`
	RevokedAt             *time.Time `json:"revoked_at,omitempty"`
	RevocationReason      *string    `json:"revocation_reason,omitempty"`
	RevocationReference   *string    `json:"revocation_reference,omitempty"`
}

// ContributorRequest is an identity reviewer's authorization of a source. Basis reviewed_match names a
// same_person review between this profile and a profile the source created; verified_reference records
// a verification of the identity association made outside this API.
type ContributorRequest struct {
	Source                string `json:"source"`
	Basis                 string `json:"basis"`
	MatchReviewID         string `json:"match_review_id"`
	VerificationReference string `json:"verification_reference"`
}

var contributorReference = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// errOutsideScope answers a source write to a profile it neither created nor contributes to. An unknown
// profile gets the same answer, so a profile ID reveals nothing.
var errOutsideScope = problem(403, "Forbidden", "This profile is outside the integration source's scope.")

// authorizeWriter takes the profile's row lock, which authorization and revocation also take, and then
// decides whether source may write it: as its creator or as an active contributor. The answer holds until
// the caller's transaction ends.
func authorizeWriter(ctx context.Context, q *db.Queries, id, source string) error {
	if _, err := q.LockProfileForAuthorization(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return errOutsideScope
	} else if err != nil {
		return err
	}
	allowed, err := q.SourceMayWriteProfile(ctx, db.SourceMayWriteProfileParams{ProfileID: id, Source: source})
	if err != nil {
		return err
	}
	if !allowed {
		return errOutsideScope
	}
	return nil
}

// AuthorizeContributor records an identity reviewer's authorization of a source to contribute to a
// profile, after checking the identity association it rests on.
func (s Store) AuthorizeContributor(ctx context.Context, profileID string, req ContributorRequest, actor string) (ProfileContributor, error) {
	c := ProfileContributor{ID: UUID(), ProfileID: profileID, Source: req.Source, Basis: req.Basis, AuthorizedBy: actor}
	switch {
	case !profiles.Name.MatchString(req.Source):
		return c, problem(422, "Invalid authorization", "source must name a configured source.")
	case req.Basis == "reviewed_match" && profiles.ID.MatchString(req.MatchReviewID) && req.VerificationReference == "":
		c.MatchReviewID = &req.MatchReviewID
	case req.Basis == "verified_reference" && contributorReference.MatchString(req.VerificationReference) && req.MatchReviewID == "":
		c.VerificationReference = &req.VerificationReference
	default:
		return c, problem(422, "Invalid authorization", "basis is reviewed_match with match_review_id, or verified_reference with verification_reference.")
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		known, err := q.SourceExists(ctx, req.Source)
		if err != nil {
			return err
		}
		if !known {
			return problem(422, "Invalid authorization", "source must name a configured source.")
		}
		creator, err := q.LockProfileForAuthorization(ctx, profileID)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "Profile does not exist.")
		}
		if err != nil {
			return err
		}
		if !creator.Valid {
			// A customer's own profile anchor has no creating source, and no source ever writes it
			// (ADR 0005, ADR 0007).
			return problem(409, "Not a source profile", "Sources never contribute to a customer's profile anchor.")
		}
		if creator.String == req.Source {
			return problem(409, "Already authorized", "The source created this profile.")
		}
		if c.MatchReviewID != nil {
			if err := checkReviewedMatch(ctx, q, profileID, req.Source, *c.MatchReviewID); err != nil {
				return err
			}
		}
		c.AuthorizedAt, err = q.InsertProfileContributor(ctx, db.InsertProfileContributorParams{ID: c.ID, ProfileID: profileID, Source: req.Source,
			Basis: req.Basis, MatchReviewID: optionalUUID(req.MatchReviewID), VerificationReference: optionalText(c.VerificationReference), AuthorizedBy: actor})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return problem(409, "Already authorized", "The source is already an active contributor to this profile.")
		}
		return err
	})
	return c, err
}

// checkReviewedMatch requires a review of this profile and a profile the source created whose latest
// decision for the pair is same_person.
func checkReviewedMatch(ctx context.Context, q *db.Queries, profileID, source, reviewID string) error {
	invalid := problem(422, "Invalid authorization", "match_review_id must be the latest review, a same_person decision, between this profile and a profile the source created.")
	review, err := q.GetMatchReviewByID(ctx, reviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid
	}
	if err != nil {
		return err
	}
	other := review.CandidateProfileID
	if review.CandidateProfileID == profileID {
		other = review.ProfileID
	} else if review.ProfileID != profileID {
		return invalid
	}
	latest, err := q.LatestPairReview(ctx, db.LatestPairReviewParams{A: review.ProfileID, B: review.CandidateProfileID})
	if err != nil || latest.ID != review.ID || latest.Decision != "same_person" {
		return errors.Join(err, invalid)
	}
	creator, err := q.ProfileCreator(ctx, other)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid
	}
	if err != nil || !creator.Valid || creator.String != source {
		return errors.Join(err, invalid)
	}
	return nil
}

// RevokeContributor ends a source's authorization. It stops every later write by that source, including
// its events and imports for records attached to this profile, and retracts nothing already written.
func (s Store) RevokeContributor(ctx context.Context, profileID, contributorID, reference, actor string) (ProfileContributor, error) {
	if !contributorReference.MatchString(reference) {
		return ProfileContributor{}, problem(422, "Invalid revocation", "reference must cite the reason for revoking.")
	}
	var revoked db.ProfileContributor
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		if _, err := q.LockProfileForAuthorization(ctx, profileID); errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "Profile does not exist.")
		} else if err != nil {
			return err
		}
		if _, err := q.RevokeProfileContributor(ctx, db.RevokeProfileContributorParams{ID: contributorID, ProfileID: profileID,
			RevokedBy: pgtype.Text{String: actor, Valid: true}, RevocationReason: pgtype.Text{String: "revoked", Valid: true},
			RevocationReference: pgtype.Text{String: reference, Valid: true}}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var err error
		revoked, err = q.GetProfileContributor(ctx, db.GetProfileContributorParams{ID: contributorID, ProfileID: profileID})
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "Unknown contributor authorization.")
		}
		return err
	})
	return contributorOf(revoked), err
}

// revokeReversedMatch revokes, in the caller's transaction, every authorization resting on a review of
// the pair once the pair is reviewed as different_people.
func revokeReversedMatch(ctx context.Context, q *db.Queries, a, b, actor string) error {
	affected, err := q.ContributorsOnReviewedPair(ctx, db.ContributorsOnReviewedPairParams{A: a, B: b})
	if err != nil {
		return err
	}
	for _, c := range affected {
		if _, err := q.LockProfileForAuthorization(ctx, c.ProfileID); err != nil {
			return err
		}
		if _, err := q.RevokeProfileContributor(ctx, db.RevokeProfileContributorParams{ID: c.ID, ProfileID: c.ProfileID,
			RevokedBy: pgtype.Text{String: actor, Valid: true}, RevocationReason: pgtype.Text{String: "match_review_reversed", Valid: true}}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

// Contributors lists a profile's contributor authorizations, revoked ones included.
func (s Store) Contributors(ctx context.Context, profileID string) ([]ProfileContributor, error) {
	rows, err := db.New(s.Pool).ListProfileContributors(ctx, profileID)
	out := make([]ProfileContributor, 0, len(rows))
	for _, r := range rows {
		out = append(out, contributorOf(r))
	}
	return out, err
}

func contributorOf(r db.ProfileContributor) ProfileContributor {
	c := ProfileContributor{ID: r.ID, ProfileID: r.ProfileID, Source: r.Source, Basis: r.Basis, AuthorizedBy: r.AuthorizedBy, AuthorizedAt: r.AuthorizedAt}
	if r.MatchReviewID.Valid {
		id := uuidString(r.MatchReviewID)
		c.MatchReviewID = &id
	}
	c.VerificationReference = textPtr(r.VerificationReference)
	c.RevokedBy, c.RevocationReason, c.RevocationReference = textPtr(r.RevokedBy), textPtr(r.RevocationReason), textPtr(r.RevocationReference)
	if r.RevokedAt.Valid {
		at := r.RevokedAt.Time
		c.RevokedAt = &at
	}
	return c
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func optionalText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
