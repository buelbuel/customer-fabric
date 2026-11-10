package platform

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/customer-fabric/customer-fabric/backend/internal/matching"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

const (
	matchPopulationLimit = 5000
	matchCandidateLimit  = 100
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
}

// FindMatchCandidates compares one source record with every other active source record in one read-only
// snapshot. A value takes part only if its attribute is defined, its access policy grants the matching
// reader for the record's source, and that source owns it. A population over matchPopulationLimit is
// refused rather than compared in part.
func (s Store) FindMatchCandidates(ctx context.Context, profileID string) (MatchCandidates, error) {
	result := MatchCandidates{ProfileID: profileID, Logic: MatchLogic, Candidates: []MatchCandidate{}}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	rows, err := q.ListMatchableRecords(ctx, matchPopulationLimit+1)
	if err != nil {
		return result, err
	}
	if len(rows) > matchPopulationLimit {
		return result, problem(409, "Population too large", fmt.Sprintf("Match candidates compare at most %d source records.", matchPopulationLimit))
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	owners, err := q.ListCustomerViewEvidence(ctx, ids)
	if err != nil {
		return result, err
	}
	owned := map[string]map[string]string{}
	for _, o := range owners {
		if owned[o.ProfileID] == nil {
			owned[o.ProfileID] = map[string]string{}
		}
		owned[o.ProfileID][o.Path] = o.Owner
	}
	governance, err := loadGovernance(ctx, q, matchPaths)
	if err != nil {
		return result, err
	}
	signals := make([]matching.Signals, len(rows))
	target := -1
	for i, r := range rows {
		var attributes map[string]json.RawMessage
		if err = json.Unmarshal(r.Attributes, &attributes); err != nil {
			return result, err
		}
		granted := map[string]json.RawMessage{}
		for _, path := range matchPaths {
			policy, governed := governance.Policies[path]
			_, defined := governance.Definitions[path]
			if value, ok := attributes[path]; ok && defined && governed && policy.Allows(profiles.ReaderMatching, r.Source) && owned[r.ID][path] == r.Source {
				granted[path] = value
			}
		}
		signals[i] = matching.SignalsFrom(granted)
		if r.ID == profileID {
			target, result.Source = i, r.Source
		}
	}
	if target < 0 {
		return result, problem(404, "Not found", "The profile is not an active source record.")
	}
	result.Population = len(rows)
	for i, r := range rows {
		if i == target {
			continue
		}
		if c, ok := matching.Compare(signals[target], signals[i]); ok {
			result.Candidates = append(result.Candidates, MatchCandidate{ProfileID: r.ID, Source: r.Source, Candidate: c})
		}
	}
	rank := map[string]int{matching.Strong: 0, matching.Probable: 1, matching.Conflicting: 2}
	slices.SortFunc(result.Candidates, func(a, b MatchCandidate) int {
		return cmp.Or(cmp.Compare(rank[a.Class], rank[b.Class]), cmp.Compare(a.ProfileID, b.ProfileID))
	})
	result.Total = len(result.Candidates)
	if len(result.Candidates) > matchCandidateLimit {
		result.Candidates = result.Candidates[:matchCandidateLimit]
	}
	return result, nil
}
