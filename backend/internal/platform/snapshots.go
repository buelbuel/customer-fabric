package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// snapshotChangeLimit bounds one side of a comparison; a larger change is refused rather than truncated.
const snapshotChangeLimit = 1000

// SegmentSnapshot is a segment's membership frozen at one evaluation. Members are stored as anchor IDs.
type SegmentSnapshot struct {
	ID         string          `json:"id"`
	Segment    string          `json:"segment"`
	Revision   int32           `json:"revision"`
	PurposeID  string          `json:"purpose_id"`
	TakenAt    time.Time       `json:"taken_at"`
	Population int             `json:"population"`
	Members    int             `json:"members"`
	Excluded   SegmentExcluded `json:"excluded"`
}

func snapshotFromRow(r db.SegmentSnapshot) SegmentSnapshot {
	return SegmentSnapshot{ID: r.ID, Segment: r.SegmentName, Revision: r.SegmentRevision, PurposeID: r.PurposeID, TakenAt: r.TakenAt,
		Population: int(r.Population), Members: int(r.Members),
		Excluded: SegmentExcluded{Unavailable: int(r.ExcludedUnavailable), Consent: int(r.ExcludedConsent), Criteria: int(r.ExcludedCriteria)}}
}

// TakeSnapshot evaluates a segment's current revision exactly as a preview does and stores who is a
// member, in one transaction.
func (s Store) TakeSnapshot(ctx context.Context, segment string) (SegmentSnapshot, error) {
	var snapshot SegmentSnapshot
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := q.GetCurrentSegment(ctx, segment)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, problem(404, "Not found", "The segment is not defined.")
	}
	if err != nil {
		return snapshot, err
	}
	rule, err := governedRule(ctx, q, row)
	if err != nil {
		return snapshot, err
	}
	members, err := evaluatePopulation(ctx, q, rule, time.Now().UTC())
	if err != nil {
		return snapshot, err
	}
	snapshot = SegmentSnapshot{ID: UUID(), Segment: row.Name, Revision: row.Revision, PurposeID: rule.PurposeID, Population: members.Size,
		Members: len(members.Members), Excluded: members.Excluded}
	if snapshot.TakenAt, err = q.InsertSnapshot(ctx, db.InsertSnapshotParams{ID: snapshot.ID, SegmentName: row.Name, SegmentRevision: row.Revision,
		PurposeID: rule.PurposeID, Population: int32(members.Size), Members: int32(len(members.Members)),
		ExcludedUnavailable: int32(members.Excluded.Unavailable), ExcludedConsent: int32(members.Excluded.Consent),
		ExcludedCriteria: int32(members.Excluded.Criteria)}); err != nil {
		return snapshot, err
	}
	for _, anchor := range members.Members {
		if err = q.InsertSnapshotMember(ctx, db.InsertSnapshotMemberParams{SnapshotID: snapshot.ID, AnchorProfileID: anchor}); err != nil {
			return snapshot, err
		}
	}
	return snapshot, tx.Commit(ctx)
}

// Snapshots lists a segment's 100 most recent snapshots, newest first.
func (s Store) Snapshots(ctx context.Context, segment string) ([]SegmentSnapshot, error) {
	q := db.New(s.Pool)
	if _, err := q.GetCurrentSegment(ctx, segment); errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "The segment is not defined.")
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListSnapshots(ctx, segment)
	out := make([]SegmentSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, snapshotFromRow(r))
	}
	return out, err
}

// SnapshotChanges says who joined and who left a segment between two of its snapshots.
type SnapshotChanges struct {
	Snapshot SegmentSnapshot `json:"snapshot"`
	Since    SegmentSnapshot `json:"since"`
	Joined   []string        `json:"joined"`
	Left     []string        `json:"left"`
}

func (s Store) CompareSnapshots(ctx context.Context, id, since string) (SnapshotChanges, error) {
	var changes SnapshotChanges
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return changes, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	for target, snapshotID := range map[*SegmentSnapshot]string{&changes.Snapshot: id, &changes.Since: since} {
		row, err := q.GetSnapshot(ctx, snapshotID)
		if errors.Is(err, pgx.ErrNoRows) {
			return changes, problem(404, "Not found", "Unknown snapshot.")
		}
		if err != nil {
			return changes, err
		}
		*target = snapshotFromRow(row)
	}
	if changes.Snapshot.Segment != changes.Since.Segment {
		return changes, problem(422, "Invalid comparison", "Compare two snapshots of the same segment.")
	}
	if changes.Joined, err = q.ListSnapshotDifference(ctx, db.ListSnapshotDifferenceParams{Included: id, Excluded: since, RowLimit: snapshotChangeLimit + 1}); err != nil {
		return changes, err
	}
	if changes.Left, err = q.ListSnapshotDifference(ctx, db.ListSnapshotDifferenceParams{Included: since, Excluded: id, RowLimit: snapshotChangeLimit + 1}); err != nil {
		return changes, err
	}
	if len(changes.Joined) > snapshotChangeLimit || len(changes.Left) > snapshotChangeLimit {
		return changes, problem(409, "Too many changes", fmt.Sprintf("More than %d customers joined or left; compare closer snapshots.", snapshotChangeLimit))
	}
	return changes, nil
}
