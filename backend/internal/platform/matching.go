package platform

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/matching"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

const (
	matchCandidateLimit = 100
	// matchScanPage is how many records' identifying values one read of the scan holds in memory, and
	// matchComparePage how many shortlisted records are loaded and compared at a time.
	matchScanPage    = 20000
	matchComparePage = 5000
	// MatchLogic names the rules a candidate list was produced with.
	MatchLogic = "match-candidates-v1"
)

var matchPaths = []string{matching.PathEmail, matching.PathPhone, matching.PathGivenName, matching.PathFamilyName, matching.PathReference}

// MatchCandidates lists other source records that may be the same person as one source record. It is a
// proposal for an operator: nothing is linked, merged or authorized, and values are never returned.
type MatchCandidates struct {
	ProfileID  string `json:"profile_id"`
	Source     string `json:"source"`
	Logic      string `json:"logic"`
	Population int    `json:"population"`
	// Total counts every candidate; Candidates holds the first matchCandidateLimit by class and ID.
	Total      int              `json:"total"`
	Candidates []MatchCandidate `json:"candidates"`
}

type MatchCandidate struct {
	ProfileID string `json:"profile_id"`
	Source    string `json:"source"`
	matching.Candidate
	// Review is the latest operator decision about this pair, if any.
	Review string `json:"review,omitempty"`
}

// FindMatchCandidates compares one source record with every other active source record that could be a
// candidate, in one read-only snapshot. A value takes part only if its attribute is defined, its access
// policy grants the matching reader for the record's source, and that source owns it. The population is
// not capped: the snapshot is scanned for records sharing an identifier with the target, and only those
// are loaded and compared. Nothing derived from the values is stored.
func (s Store) FindMatchCandidates(ctx context.Context, profileID string) (MatchCandidates, error) {
	return bounded(ctx, s, func(ctx context.Context) (MatchCandidates, error) { return s.compareRecords(ctx, profileID, "") })
}

// compareRecords lists the target's candidates, keeping the first matchCandidateLimit by class and ID and
// counting the rest. A non-empty only compares the target with that one record instead of searching.
func (s Store) compareRecords(ctx context.Context, profileID, only string) (MatchCandidates, error) {
	result := MatchCandidates{ProfileID: profileID, Logic: MatchLogic, Candidates: []MatchCandidate{}}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	target, err := q.GetMatchableRecord(ctx, profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, problem(404, "Not found", "The profile is not an active source record.")
	}
	if err != nil {
		return result, err
	}
	result.Source = target.Source
	population, err := q.CountMatchableRecords(ctx)
	if err != nil {
		return result, err
	}
	result.Population = int(population)
	governance, err := loadGovernance(ctx, q, matchPaths)
	if err != nil {
		return result, err
	}
	// granted keeps the values matching may use: defined, granted to the matching reader for the
	// record's source, and owned by that source.
	granted := func(ctx context.Context, records []matchRecord) ([]matching.Signals, error) {
		ids := make([]string, len(records))
		for i, r := range records {
			ids[i] = r.ID
		}
		owners, err := q.ListCustomerViewEvidence(ctx, db.ListCustomerViewEvidenceParams{ProfileIds: ids, Paths: matchPaths})
		if err != nil {
			return nil, err
		}
		owned := map[string]map[string]string{}
		for _, o := range owners {
			if owned[o.ProfileID] == nil {
				owned[o.ProfileID] = map[string]string{}
			}
			owned[o.ProfileID][o.Path] = o.Owner
		}
		signals := make([]matching.Signals, len(records))
		for i, r := range records {
			var attributes map[string]json.RawMessage
			if err := json.Unmarshal(r.Attributes, &attributes); err != nil {
				return nil, err
			}
			values := map[string]json.RawMessage{}
			for _, path := range matchPaths {
				policy, governed := governance.Policies[path]
				_, defined := governance.Definitions[path]
				if value, ok := attributes[path]; ok && defined && governed && policy.Allows(profiles.ReaderMatching, r.Source) && owned[r.ID][path] == r.Source {
					values[path] = value
				}
			}
			signals[i] = matching.SignalsFrom(values)
		}
		return signals, nil
	}
	targetSignals, err := granted(ctx, []matchRecord{{ID: target.ID, Source: target.Source, Attributes: target.Attributes}})
	if err != nil {
		return result, err
	}
	// The shortlist is every record whose stored values share an identifier with the target's governed
	// values. Stored values are a superset of what matching may use for a record, so no candidate is
	// missed, and the comparison below applies governance to each shortlisted record before it decides.
	var shortlist []string
	if only != "" && only != profileID {
		shortlist = []string{only}
	}
	if only == "" && (targetSignals[0] != matching.Signals{}) {
		after := "00000000-0000-0000-0000-000000000000"
		for {
			page, err := q.ListMatchIdentifiers(ctx, db.ListMatchIdentifiersParams{After: after, RowLimit: matchScanPage})
			if err != nil {
				return result, err
			}
			for _, r := range page {
				stored := matching.SignalsFrom(map[string]json.RawMessage{matching.PathEmail: r.Email, matching.PathPhone: r.Phone, matching.PathReference: r.Reference})
				if r.ID != profileID && targetSignals[0].SharesIdentifier(stored) {
					shortlist = append(shortlist, r.ID)
				}
			}
			if len(page) < matchScanPage {
				break
			}
			after = page[len(page)-1].ID
		}
	}
	for ids := range slices.Chunk(shortlist, matchComparePage) {
		rows, err := q.ListMatchRecords(ctx, ids)
		if err != nil {
			return result, err
		}
		records := make([]matchRecord, len(rows))
		for i, r := range rows {
			records[i] = matchRecord{ID: r.ID, Source: r.Source, Attributes: r.Attributes}
		}
		signals, err := granted(ctx, records)
		if err != nil {
			return result, err
		}
		for i, r := range records {
			if c, ok := matching.Compare(targetSignals[0], signals[i]); ok {
				result.Candidates = append(result.Candidates, MatchCandidate{ProfileID: r.ID, Source: r.Source, Candidate: c})
				result.Total++
			}
		}
		// Only the listed candidates are kept, so one identifier shared by very many records cannot
		// grow the list without bound.
		slices.SortFunc(result.Candidates, byClassThenID)
		if len(result.Candidates) > matchCandidateLimit {
			result.Candidates = result.Candidates[:matchCandidateLimit]
		}
	}
	reviews, err := q.LatestMatchReviews(ctx, profileID)
	if err != nil {
		return result, err
	}
	latest := map[string]string{}
	for _, r := range reviews {
		latest[r.Other] = r.Decision
	}
	for i := range result.Candidates {
		result.Candidates[i].Review = latest[result.Candidates[i].ProfileID]
	}
	return result, nil
}

var matchClassRank = map[string]int{matching.Strong: 0, matching.Probable: 1, matching.Conflicting: 2}

func byClassThenID(a, b MatchCandidate) int {
	return cmp.Or(cmp.Compare(matchClassRank[a.Class], matchClassRank[b.Class]), cmp.Compare(a.ProfileID, b.ProfileID))
}

// matchRecord is a matchable source record as read for comparison.
type matchRecord struct {
	ID, Source string
	Attributes []byte
}

// MatchReview is an operator's decision about a candidate pair and the evidence it was made on.
type MatchReview struct {
	ID                 string    `json:"id"`
	ProfileID          string    `json:"profile_id"`
	CandidateProfileID string    `json:"candidate_profile_id"`
	Decision           string    `json:"decision"`
	Class              string    `json:"class"`
	Agree              []string  `json:"agree"`
	Disagree           []string  `json:"disagree"`
	ReviewedAt         time.Time `json:"reviewed_at"`
}

// ReviewMatch records an operator decision about a pair that is currently a candidate, with the class and
// signals it was shown. The decision itself links, merges or authorizes nothing; a same_person review can
// be the basis an identity reviewer authorizes a contributor on (ADR 0018), and a different_people review
// revokes every authorization resting on an earlier review of the pair, in the same transaction.
func (s Store) ReviewMatch(ctx context.Context, profileID, candidateID, decision, actor string) (MatchReview, error) {
	review := MatchReview{ID: UUID(), ProfileID: profileID, CandidateProfileID: candidateID, Decision: decision}
	if decision != "same_person" && decision != "different_people" {
		return review, problem(422, "Invalid review", "decision must be same_person or different_people.")
	}
	current, err := bounded(ctx, s, func(ctx context.Context) (MatchCandidates, error) {
		return s.compareRecords(ctx, profileID, candidateID)
	})
	if err != nil {
		return review, err
	}
	i := slices.IndexFunc(current.Candidates, func(c MatchCandidate) bool { return c.ProfileID == candidateID })
	if i < 0 {
		return review, problem(409, "Not a candidate", "The record is not currently a match candidate; review what the candidates list shows.")
	}
	// The evidence is read in its own snapshot just before the insert; a concurrent source write can still
	// land between them, and the stored class and signals then describe the evidence the decision was checked against.
	c := current.Candidates[i]
	review.Class, review.Agree, review.Disagree = c.Class, c.Agree, c.Disagree
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		var err error
		review.ReviewedAt, err = q.InsertMatchReview(ctx, db.InsertMatchReviewParams{ID: review.ID, ProfileID: profileID,
			CandidateProfileID: candidateID, Decision: decision, Class: c.Class, Agree: c.Agree, Disagree: c.Disagree})
		if err != nil || decision != "different_people" {
			return err
		}
		return revokeReversedMatch(ctx, q, profileID, candidateID, actor)
	})
	return review, err
}

// MatchReviews lists the decisions involving a record, newest first.
func (s Store) MatchReviews(ctx context.Context, profileID string) ([]MatchReview, error) {
	rows, err := db.New(s.Pool).ListMatchReviews(ctx, profileID)
	out := make([]MatchReview, 0, len(rows))
	for _, r := range rows {
		out = append(out, MatchReview{ID: r.ID, ProfileID: r.ProfileID, CandidateProfileID: r.CandidateProfileID, Decision: r.Decision,
			Class: r.Class, Agree: r.Agree, Disagree: r.Disagree, ReviewedAt: r.ReviewedAt})
	}
	return out, err
}
