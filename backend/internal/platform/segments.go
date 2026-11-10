package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/customer-fabric/customer-fabric/backend/internal/segments"
	"github.com/jackc/pgx/v5"
)

// segmentPopulationLimit bounds the customers one preview evaluates. A larger population is refused
// rather than sampled, because a partial count would look complete.
const segmentPopulationLimit = 1000

// DefineSegment appends the next revision of a segment. expected is the revision the operator read, 0
// for a new segment. Every condition must name a defined attribute whose access policy grants the
// segmentation reader, so a rule never silently depends on values it can never see.
func (s Store) DefineSegment(ctx context.Context, name string, rule segments.Rule, expected int32) (segments.Segment, error) {
	segment := segments.Segment{Name: name, Rule: rule}
	if !profiles.ID.MatchString(rule.PurposeID) {
		return segment, problem(422, "Invalid segment", "purpose_id must be a consent purpose ID.")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return segment, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockSegment(ctx, name); err != nil {
		return segment, err
	}
	if _, err = q.GetPurpose(ctx, rule.PurposeID); errors.Is(err, pgx.ErrNoRows) {
		return segment, problem(422, "Invalid segment", "purpose_id must be a consent purpose ID.")
	} else if err != nil {
		return segment, err
	}
	var unsupported *segmentRuleError
	if err = checkSegmentGovernance(ctx, q, rule); errors.As(err, &unsupported) {
		return segment, problem(422, "Invalid segment", err.Error())
	} else if err != nil {
		return segment, err
	}
	history, err := q.ListSegmentRevisions(ctx, name)
	if err != nil {
		return segment, err
	}
	segment.Revision = int32(len(history)) + 1
	if len(history) > 0 && expected == 0 {
		return segment, problem(428, "Precondition required", "This segment exists; supply its current revision in If-Match to revise it.")
	}
	if segment.Revision-1 != expected {
		return segment, problem(412, "Stale segment revision", "Read the segment's current revision and decide again.")
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return segment, err
	}
	if segment.CreatedAt, err = q.InsertSegmentRevision(ctx, db.InsertSegmentRevisionParams{Name: name, Revision: segment.Revision, PurposeID: rule.PurposeID, Rule: raw}); err != nil {
		return segment, err
	}
	return segment, tx.Commit(ctx)
}

// checkSegmentGovernance requires every condition to name a defined attribute whose current access
// policy grants the segmentation reader, and its literals to fit the current definition.
func checkSegmentGovernance(ctx context.Context, q *db.Queries, rule segments.Rule) error {
	paths := make([]string, 0, len(rule.Conditions))
	for _, c := range rule.Conditions {
		paths = append(paths, c.Attribute)
	}
	governance, err := loadGovernance(ctx, q, paths)
	if err != nil {
		return err
	}
	if err = rule.Validate(governance.Definitions); err != nil {
		return &segmentRuleError{err}
	}
	for i, c := range rule.Conditions {
		if p, ok := governance.Policies[c.Attribute]; !ok || !slices.Contains(p.Readers, profiles.ReaderSegmentation) || len(p.Sources) == 0 {
			return &segmentRuleError{fmt.Errorf("condition %d: the attribute's access policy does not grant segmentation", i)}
		}
	}
	return nil
}

// segmentRuleError is a rule that current governance does not support, as opposed to a read failure.
type segmentRuleError struct{ error }

// Segments lists the current revision of every segment.
func (s Store) Segments(ctx context.Context) ([]segments.Segment, error) {
	rows, err := db.New(s.Pool).ListCurrentSegments(ctx)
	if err != nil {
		return nil, err
	}
	return segmentsFromRows(rows)
}

// SegmentRevisions lists every revision of one segment, oldest first.
func (s Store) SegmentRevisions(ctx context.Context, name string) ([]segments.Segment, error) {
	rows, err := db.New(s.Pool).ListSegmentRevisions(ctx, name)
	if err == nil && len(rows) == 0 {
		return nil, problem(404, "Not found", "The segment is not defined.")
	}
	if err != nil {
		return nil, err
	}
	return segmentsFromRows(rows)
}

func segmentsFromRows(rows []db.SegmentDefinition) ([]segments.Segment, error) {
	out := make([]segments.Segment, 0, len(rows))
	for _, r := range rows {
		segment := segments.Segment{Name: r.Name, Revision: r.Revision, CreatedAt: r.CreatedAt}
		if err := json.Unmarshal(r.Rule, &segment.Rule); err != nil {
			return nil, err
		}
		out = append(out, segment)
	}
	return out, nil
}

// SegmentPreview is a live evaluation of a segment's current revision. Only members are listed, and
// each lists the provenance of the values that decided it, never the values.
type SegmentPreview struct {
	Segment     string          `json:"segment"`
	Revision    int32           `json:"revision"`
	PurposeID   string          `json:"purpose_id"`
	Logic       string          `json:"logic"`
	EvaluatedAt time.Time       `json:"evaluated_at"`
	Population  int             `json:"population"`
	Members     int             `json:"members"`
	Excluded    SegmentExcluded `json:"excluded"`
	Sample      []SegmentMember `json:"sample"`
	NextCursor  string          `json:"next_cursor,omitempty"`
	// DurationMS is how long the evaluation took.
	DurationMS int64 `json:"duration_ms"`
}

// SegmentExcluded counts non-members by the first gate they failed.
type SegmentExcluded struct {
	Unavailable int `json:"unavailable"`
	Consent     int `json:"consent"`
	Criteria    int `json:"criteria"`
}

type SegmentMember struct {
	AnchorProfileID string             `json:"anchor_profile_id"`
	Consent         consent.Evaluation `json:"consent"`
	Conditions      []segments.Outcome `json:"conditions"`
}

// PreviewSegment evaluates the current revision over every customer anchor in one read-only snapshot.
// A customer is a member only if their segmentation view is available, they permit the segment's
// purpose and the rule matches; each gate fails closed. Nothing is stored, so a withdrawal, revoked
// association or narrowed policy changes the next preview. cursor and limit page the member sample.
func (s Store) PreviewSegment(ctx context.Context, name, cursor string, limit int) (SegmentPreview, error) {
	return bounded(ctx, s, func(ctx context.Context) (SegmentPreview, error) { return s.previewSegment(ctx, name, cursor, limit) })
}

func (s Store) previewSegment(ctx context.Context, name, cursor string, limit int) (SegmentPreview, error) {
	started := time.Now()
	var preview SegmentPreview
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return preview, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := q.GetCurrentSegment(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return preview, problem(404, "Not found", "The segment is not defined.")
	}
	if err != nil {
		return preview, err
	}
	rule, err := governedRule(ctx, q, row)
	if err != nil {
		return preview, err
	}
	anchors, err := q.ListCustomerAnchors(ctx, segmentPopulationLimit+1)
	if err != nil {
		return preview, err
	}
	if len(anchors) > segmentPopulationLimit {
		return preview, problem(409, "Population too large", fmt.Sprintf("Previews evaluate at most %d customers.", segmentPopulationLimit))
	}
	preview = SegmentPreview{Segment: row.Name, Revision: row.Revision, PurposeID: rule.PurposeID, Logic: segments.Logic,
		EvaluatedAt: time.Now().UTC(), Population: len(anchors), Sample: []SegmentMember{}}
	for _, anchor := range anchors {
		m, err := evaluateMember(ctx, q, rule, anchor.ProfileID, anchor.KratosIdentityID, preview.EvaluatedAt)
		if err != nil {
			return preview, err
		}
		switch m.Gate {
		case gateUnavailable:
			preview.Excluded.Unavailable++
			continue
		case gateConsent:
			preview.Excluded.Consent++
			continue
		case gateCriteria:
			preview.Excluded.Criteria++
			continue
		}
		preview.Members++
		if anchor.ProfileID > cursor && len(preview.Sample) < limit {
			preview.Sample = append(preview.Sample, SegmentMember{AnchorProfileID: anchor.ProfileID, Consent: m.Consent, Conditions: matchedOnly(m.Outcomes)})
		} else if anchor.ProfileID > cursor && preview.NextCursor == "" {
			preview.NextCursor = preview.Sample[len(preview.Sample)-1].AnchorProfileID
		}
	}
	preview.DurationMS = time.Since(started).Milliseconds()
	return preview, nil
}

// governedRule decodes a stored revision and refuses it if current governance no longer supports it. A
// narrowed policy makes a value missing, which would silently widen a missing or any-rule to every
// consenting customer; the segment must be revised instead.
func governedRule(ctx context.Context, q *db.Queries, row db.SegmentDefinition) (segments.Rule, error) {
	var rule segments.Rule
	if err := json.Unmarshal(row.Rule, &rule); err != nil {
		return rule, err
	}
	var unsupported *segmentRuleError
	if err := checkSegmentGovernance(ctx, q, rule); errors.As(err, &unsupported) {
		return rule, problem(409, "Segment no longer governed", err.Error()+"; revise the segment or its attributes' access policies.")
	} else if err != nil {
		return rule, err
	}
	return rule, nil
}

// The gates a customer can fail, in the order they are checked.
const (
	gateUnavailable = "view_unavailable"
	gateConsent     = "consent_not_permitted"
	gateCriteria    = "criteria_not_met"
)

// membership is one customer's result against a rule. Gate is empty for a member.
type membership struct {
	Gate     string
	Consent  consent.Evaluation
	Outcomes []segments.Outcome
}

// evaluateMember decides one customer through q, so the caller's transaction decides the snapshot: their
// segmentation view must be available, their effective permission for the purpose permitted, and the
// rule must match. Each gate fails closed.
func evaluateMember(ctx context.Context, q *db.Queries, rule segments.Rule, profileID, kratos string, at time.Time) (membership, error) {
	view, err := customerViewIn(ctx, q, profileID, kratos, profiles.ReaderSegmentation)
	var refused *Problem
	if errors.As(err, &refused) {
		return membership{Gate: gateUnavailable}, nil
	}
	if err != nil {
		return membership{}, err
	}
	state, err := purposeStateFor(ctx, q, profileID, kratos, rule.PurposeID)
	if err != nil {
		return membership{}, err
	}
	if state.Evaluation.Outcome != consent.Permitted {
		return membership{Gate: gateConsent, Consent: state.Evaluation}, nil
	}
	matched, outcomes := rule.Evaluate(view, at)
	if !matched {
		return membership{Gate: gateCriteria, Consent: state.Evaluation}, nil
	}
	return membership{Consent: state.Evaluation, Outcomes: outcomes}, nil
}

// matchedOnly keeps the conditions that admitted a member. Under match any, a failed condition and its
// provenance would reveal more about segmentation-only values than membership does.
func matchedOnly(outcomes []segments.Outcome) []segments.Outcome {
	out := make([]segments.Outcome, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Matched {
			out = append(out, o)
		}
	}
	return out
}

// population is every customer anchor's membership of one rule at one time.
type population struct {
	Members  []string
	Excluded SegmentExcluded
	Size     int
}

// evaluatePopulation decides every customer anchor through q, refusing a population larger than
// segmentPopulationLimit rather than evaluating part of it.
func evaluatePopulation(ctx context.Context, q *db.Queries, rule segments.Rule, at time.Time) (population, error) {
	var p population
	anchors, err := q.ListCustomerAnchors(ctx, segmentPopulationLimit+1)
	if err != nil {
		return p, err
	}
	if len(anchors) > segmentPopulationLimit {
		return p, problem(409, "Population too large", fmt.Sprintf("Segments evaluate at most %d customers.", segmentPopulationLimit))
	}
	p.Size = len(anchors)
	for _, anchor := range anchors {
		m, err := evaluateMember(ctx, q, rule, anchor.ProfileID, anchor.KratosIdentityID, at)
		if err != nil {
			return p, err
		}
		switch m.Gate {
		case gateUnavailable:
			p.Excluded.Unavailable++
		case gateConsent:
			p.Excluded.Consent++
		case gateCriteria:
			p.Excluded.Criteria++
		default:
			p.Members = append(p.Members, anchor.ProfileID)
		}
	}
	return p, nil
}

// defaultEvaluationTimeout bounds previews, snapshots, activations and match candidates, each of which
// reads every customer or source record; past it the evaluation is abandoned instead of holding a connection.
const defaultEvaluationTimeout = 10 * time.Second

// bounded runs one population-scale evaluation under the store's timeout. A timeout is refused as 503, so
// a slow evaluation is visible rather than mistaken for an empty result.
func bounded[T any](ctx context.Context, s Store, evaluate func(context.Context) (T, error)) (T, error) {
	timeout := s.EvaluationTimeout
	if timeout <= 0 {
		timeout = defaultEvaluationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := evaluate(ctx)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, problem(503, "Evaluation timed out", fmt.Sprintf("The evaluation took longer than %s; try again or narrow the population.", timeout))
	}
	return result, err
}
