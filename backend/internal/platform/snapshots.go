package platform

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// insertChunk bounds the rows one set-based insert writes.
const insertChunk = 10000

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
	return bounded(ctx, s, func(ctx context.Context) (SegmentSnapshot, error) { return s.takeSnapshot(ctx, segment) })
}

func (s Store) takeSnapshot(ctx context.Context, segment string) (SegmentSnapshot, error) {
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
	members, err := evaluatePopulation(ctx, q, rule, time.Now().UTC(), walk{})
	if err != nil {
		return snapshot, err
	}
	if snapshot, err = storeSnapshot(ctx, q, row, rule.PurposeID, members); err != nil {
		return snapshot, err
	}
	// The commit is outside the deadline, as in Activate.
	return snapshot, tx.Commit(context.WithoutCancel(ctx))
}

// storeSnapshot stores an evaluated population as a snapshot of one segment revision, through q.
func storeSnapshot(ctx context.Context, q *db.Queries, row db.SegmentDefinition, purposeID string, members population) (SegmentSnapshot, error) {
	snapshot := SegmentSnapshot{ID: UUID(), Segment: row.Name, Revision: row.Revision, PurposeID: purposeID, Population: members.Size,
		Members: len(members.Members), Excluded: members.Excluded}
	var err error
	if snapshot.TakenAt, err = q.InsertSnapshot(ctx, db.InsertSnapshotParams{ID: snapshot.ID, SegmentName: row.Name, SegmentRevision: row.Revision,
		PurposeID: purposeID, Population: int32(members.Size), Members: int32(len(members.Members)),
		ExcludedUnavailable: int32(members.Excluded.Unavailable), ExcludedConsent: int32(members.Excluded.Consent),
		ExcludedCriteria: int32(members.Excluded.Criteria)}); err != nil {
		return snapshot, err
	}
	for chunk := range slices.Chunk(members.Members, insertChunk) {
		if err = q.InsertSnapshotMembers(ctx, db.InsertSnapshotMembersParams{SnapshotID: snapshot.ID, AnchorIds: chunk}); err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
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

// SnapshotChanges says who joined and who left a segment between two of its snapshots. Each list is one
// page in anchor order; a next cursor is set while that list has more.
type SnapshotChanges struct {
	Snapshot         SegmentSnapshot `json:"snapshot"`
	Since            SegmentSnapshot `json:"since"`
	Joined           []string        `json:"joined"`
	Left             []string        `json:"left"`
	NextJoinedCursor string          `json:"next_joined_cursor,omitempty"`
	NextLeftCursor   string          `json:"next_left_cursor,omitempty"`
}

// ChangePage selects one page of each list of a comparison. An empty cursor starts at the beginning.
type ChangePage struct {
	JoinedAfter, LeftAfter string
	Limit                  int
}

func (s Store) CompareSnapshots(ctx context.Context, id, since string, page ChangePage) (SnapshotChanges, error) {
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
	for _, side := range []struct {
		list            *[]string
		next            *string
		included, after string
		excluded        string
	}{{&changes.Joined, &changes.NextJoinedCursor, id, page.JoinedAfter, since}, {&changes.Left, &changes.NextLeftCursor, since, page.LeftAfter, id}} {
		after := side.after
		if after == "" {
			after = "00000000-0000-0000-0000-000000000000"
		}
		rows, err := q.ListSnapshotDifference(ctx, db.ListSnapshotDifferenceParams{Included: side.included, Excluded: side.excluded, After: after, RowLimit: int32(page.Limit) + 1})
		if err != nil {
			return changes, err
		}
		if len(rows) > page.Limit {
			rows = rows[:page.Limit]
			*side.next = rows[len(rows)-1]
		}
		*side.list = rows
	}
	return changes, nil
}
