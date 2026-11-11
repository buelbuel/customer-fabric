package platform

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	JobSnapshot   = "snapshot"
	JobActivation = "activation"
	// audienceJobBacklog bounds the jobs waiting or running; past it a submission is refused, not queued.
	audienceJobBacklog = 20
)

// reasonUngoverned ends a job whose segment current governance no longer supports.
const reasonUngoverned = "segment_not_governed"

// AudienceJob is a durable evaluation of one pinned segment revision over the whole customer population.
// A completed job's counts, evaluation time and published snapshot or activation all come from the one
// attempt that committed, which decided every customer from a single database state. A job that is not
// completed has published nothing.
type AudienceJob struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Segment     string `json:"segment"`
	Revision    int32  `json:"revision"`
	PurposeID   string `json:"purpose_id"`
	Destination string `json:"destination,omitempty"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	// Attempts counts evaluations started; each starts over under a new snapshot.
	Attempts int32 `json:"attempts"`
	// Evaluated is how many customers the current attempt has decided, and the population once completed.
	Evaluated int `json:"evaluated"`
	// Members, Excluded and EvaluatedAt are set only by completion.
	Members     int             `json:"members"`
	Excluded    SegmentExcluded `json:"excluded"`
	EvaluatedAt *time.Time      `json:"evaluated_at,omitempty"`
	// SnapshotID or ActivationID names what a completed job published.
	SnapshotID   string     `json:"snapshot_id,omitempty"`
	ActivationID string     `json:"activation_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

func audienceJobFromRow(r db.AudienceJob, evaluated int32) AudienceJob {
	job := AudienceJob{ID: r.ID, Kind: r.Kind, Segment: r.SegmentName, Revision: r.SegmentRevision, PurposeID: r.PurposeID,
		Destination: r.Destination.String, State: r.State, Reason: r.Reason.String, Attempts: r.Attempts,
		Evaluated: int(evaluated), Members: int(r.Members),
		Excluded:  SegmentExcluded{Unavailable: int(r.ExcludedUnavailable), Consent: int(r.ExcludedConsent), Criteria: int(r.ExcludedCriteria)},
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.FinishedAt.Valid {
		job.FinishedAt = &r.FinishedAt.Time
	}
	if r.EvaluatedAt.Valid {
		job.EvaluatedAt = &r.EvaluatedAt.Time
	}
	switch {
	case r.State == "completed":
		job.Evaluated = int(r.Population)
	case r.State != "running":
		job.Evaluated = 0
	}
	if result, _ := r.ResultID.Value(); result != nil {
		if r.Kind == JobSnapshot {
			job.SnapshotID = result.(string)
		} else {
			job.ActivationID = result.(string)
		}
	}
	return job
}

// SubmitAudienceJob queues a snapshot or an activation of a segment's current revision. The revision is
// pinned now and must be governed now; a worker decides every customer later, from one database state.
// A key that was already used returns its job, so a retried submission never queues twice; created
// reports whether this call queued it.
func (s Store) SubmitAudienceJob(ctx context.Context, kind, segment, destination, key string) (job AudienceJob, created bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return job, false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := q.GetCurrentSegment(ctx, segment)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, false, problem(404, "Not found", "The segment is not defined.")
	}
	if err != nil {
		return job, false, err
	}
	if kind == JobActivation {
		if _, err = q.GetDestination(ctx, destination); errors.Is(err, pgx.ErrNoRows) {
			return job, false, problem(422, "Invalid job", "destination must be a registered destination.")
		} else if err != nil {
			return job, false, err
		}
	}
	rule, err := governedRule(ctx, q, row)
	if err != nil {
		return job, false, err
	}
	existing, err := q.GetAudienceJobByKey(ctx, key)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return job, false, err
	}
	if err != nil {
		pending, err := q.CountPendingAudienceJobs(ctx)
		if err != nil {
			return job, false, err
		}
		if pending >= audienceJobBacklog {
			return job, false, problem(503, "Job capacity exhausted", "Too many audience jobs are waiting; try again when some have finished.")
		}
		inserted, err := q.InsertAudienceJob(ctx, db.InsertAudienceJobParams{ID: UUID(), Kind: kind, SegmentName: row.Name, SegmentRevision: row.Revision,
			PurposeID: rule.PurposeID, Destination: pgtype.Text{String: destination, Valid: kind == JobActivation}, IdempotencyKey: key})
		if err != nil {
			return job, false, err
		}
		// Zero rows means a concurrent submission with the same key won; its job is returned.
		created = inserted == 1
		if existing, err = q.GetAudienceJobByKey(ctx, key); err != nil {
			return job, false, err
		}
	}
	if j := existing.AudienceJob; j.Kind != kind || j.SegmentName != segment || j.Destination.String != destination {
		return job, false, problem(409, "Idempotency key reused", "This Idempotency-Key belongs to a different audience job.")
	}
	return audienceJobFromRow(existing.AudienceJob, existing.Evaluated), created, tx.Commit(ctx)
}

func (s Store) AudienceJob(ctx context.Context, id string) (AudienceJob, error) {
	row, err := db.New(s.Pool).GetAudienceJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AudienceJob{}, problem(404, "Not found", "Unknown audience job.")
	}
	return audienceJobFromRow(row.AudienceJob, row.Evaluated), err
}

// AudienceJobs lists the 100 most recent jobs, newest first.
func (s Store) AudienceJobs(ctx context.Context) ([]AudienceJob, error) {
	rows, err := db.New(s.Pool).ListAudienceJobs(ctx)
	out := make([]AudienceJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, audienceJobFromRow(r.AudienceJob, r.Evaluated))
	}
	return out, err
}

// CancelAudienceJob ends a waiting or running job. An evaluation in progress cannot complete a cancelled
// job: its completing update fails and its transaction, with everything it would have published, rolls
// back. A finished job is 409.
func (s Store) CancelAudienceJob(ctx context.Context, id string) (AudienceJob, error) {
	q := db.New(s.Pool)
	ended, err := q.EndAudienceJob(ctx, db.EndAudienceJobParams{ID: id, State: "cancelled", Reason: "cancelled_by_operator"})
	if err != nil {
		return AudienceJob{}, err
	}
	job, err := s.AudienceJob(ctx, id)
	if err == nil && ended == 0 {
		return AudienceJob{}, problem(409, "Job already finished", "Only a waiting or running audience job can be cancelled.")
	}
	return job, err
}

// SnapshotMembers is one page of a snapshot's members in anchor order.
type SnapshotMembers struct {
	Snapshot   SegmentSnapshot `json:"snapshot"`
	Members    []string        `json:"members"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func (s Store) SnapshotMembers(ctx context.Context, id, cursor string, limit int) (SnapshotMembers, error) {
	q := db.New(s.Pool)
	row, err := q.GetSnapshot(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return SnapshotMembers{}, problem(404, "Not found", "Unknown snapshot.")
	}
	if err != nil {
		return SnapshotMembers{}, err
	}
	if cursor == "" {
		cursor = firstAnchorCursor
	}
	page := SnapshotMembers{Snapshot: snapshotFromRow(row)}
	if page.Members, err = q.ListSnapshotMembers(ctx, db.ListSnapshotMembersParams{SnapshotID: id, After: cursor, RowLimit: int32(limit) + 1}); err != nil {
		return page, err
	}
	if len(page.Members) > limit {
		page.Members = page.Members[:limit]
		page.NextCursor = page.Members[limit-1]
	}
	return page, nil
}

// AudienceWorker runs audience jobs outside any request. One attempt decides the whole population in a
// single REPEATABLE READ transaction, with the same evaluator as a synchronous snapshot or activation,
// and publishes the result and completes the job in that transaction. Membership therefore reflects one
// database state, never a mix of moments, and nothing partial is ever stored.
//
// An attempt that fails, is cancelled or is cut short by a restart leaves nothing behind; the job is
// evaluated again from the start under a new snapshot. Queuing an activation is not permission to
// send: the dispatcher re-checks every delivery immediately before each attempt.
type AudienceWorker struct {
	Store Store
	// Chunk is how many customers one round of reads decides inside the attempt's transaction.
	Chunk int32
	// Timeout bounds one attempt; an attempt past it ends the job as failed.
	Timeout time.Duration
	// MaxAttempts is how many evaluations a job may start before it is ended as failed.
	MaxAttempts int32
	// OnChunk, if set, observes an attempt after each round with the customers decided so far. An
	// error aborts the attempt.
	OnChunk func(evaluated int) error
}

func NewAudienceWorker(s Store) AudienceWorker {
	return AudienceWorker{Store: s, Chunk: evaluationChunk, Timeout: 30 * time.Minute, MaxAttempts: 5}
}

// Run works through jobs until ctx ends, pausing for interval when nothing is waiting or an attempt failed.
func (w AudienceWorker) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		worked, err := w.Step(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "audience job attempt failed")
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Step runs one attempt of the job that has waited longest and that no other worker holds. worked is
// false when there was no such job.
func (w AudienceWorker) Step(ctx context.Context) (worked bool, err error) {
	ids, err := db.New(w.Store.Pool).ListPendingAudienceJobs(ctx, audienceJobBacklog)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if worked, err = w.attempt(ctx, id); worked || err != nil {
			return worked, err
		}
	}
	return false, nil
}

var (
	errJobUngoverned = errors.New("the segment is no longer governed")
	errJobNotRunning = errors.New("the job is no longer running")
)

// attempt claims one job and evaluates it. claimed is false when another worker holds the job or it
// finished in the meantime.
func (w AudienceWorker) attempt(ctx context.Context, id string) (claimed bool, err error) {
	conn, err := w.Store.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	q := db.New(conn)
	if locked, err := q.TryAudienceJobLock(ctx, id); err != nil || !locked {
		return false, err
	}
	// The lock belongs to this connection. If it cannot be released, the connection is closed so the
	// lock cannot outlive the attempt inside the pool.
	defer func() {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, unlockErr := q.ReleaseAudienceJobLock(release, id); unlockErr != nil {
			_ = conn.Conn().Close(release)
		}
	}()
	job, err := q.StartAudienceJobAttempt(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	settle, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSettle()
	end := func(reason string) {
		if ended, endErr := q.EndAudienceJob(settle, db.EndAudienceJobParams{ID: id, State: "failed", Reason: reason}); endErr == nil && ended == 1 {
			slog.WarnContext(ctx, "audience job failed", "job_id", id, "reason", reason)
		}
	}
	if job.Attempts > w.MaxAttempts {
		end("retries_exhausted")
		return true, nil
	}
	attemptCtx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	err = w.evaluate(attemptCtx, conn, job)
	switch {
	case err == nil:
		slog.InfoContext(ctx, "audience job completed", "job_id", id, "kind", job.Kind, "attempts", job.Attempts)
		return true, nil
	case ctx.Err() != nil:
		// A shutdown: the job stays as it is and the next worker starts it again.
		return true, err
	case errors.Is(err, errJobUngoverned):
		end(reasonUngoverned)
		return true, nil
	case errors.Is(attemptCtx.Err(), context.DeadlineExceeded):
		end("evaluation_timeout")
		return true, nil
	}
	// Any other failure is retried from the start. Requeuing applies only to a job that is still
	// running, so a job cancelled during the attempt stays cancelled and is not an error.
	requeued, requeueErr := q.RequeueAudienceJob(settle, id)
	if requeueErr == nil && requeued == 0 {
		return true, nil
	}
	return true, err
}

// evaluate runs one attempt on the worker's connection: everything it reads and everything it
// publishes belongs to one REPEATABLE READ transaction.
func (w AudienceWorker) evaluate(ctx context.Context, conn *pgxpool.Conn, job db.AudienceJob) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(conn).WithTx(tx)
	segment, err := q.GetSegmentRevision(ctx, db.GetSegmentRevisionParams{Name: job.SegmentName, Revision: job.SegmentRevision})
	if err != nil {
		return err
	}
	rule, err := governedRule(ctx, q, segment)
	var refused *Problem
	if errors.As(err, &refused) {
		return errJobUngoverned
	}
	if err != nil {
		return err
	}
	// Progress is reported outside the transaction, to its own row, so it is visible while the
	// attempt runs and never touches what the attempt will commit.
	progress := db.New(w.Store.Pool)
	at := time.Now().UTC()
	members, err := evaluatePopulation(ctx, q, rule, at, walk{Chunk: w.Chunk, Progress: func(evaluated int) error {
		if err := progress.ReportAudienceJobProgress(ctx, db.ReportAudienceJobProgressParams{JobID: job.ID, Attempt: job.Attempts, Evaluated: int32(evaluated)}); err != nil {
			return err
		}
		if w.OnChunk != nil {
			return w.OnChunk(evaluated)
		}
		return nil
	}})
	if err != nil {
		return err
	}
	var result string
	if job.Kind == JobSnapshot {
		snapshot, err := storeSnapshot(ctx, q, segment, rule.PurposeID, members)
		if err != nil {
			return err
		}
		result = snapshot.ID
	} else {
		activation, err := storeActivation(ctx, q, segment, rule.PurposeID, job.Destination.String, members)
		if err != nil {
			return err
		}
		result = activation.ID
	}
	completed, err := q.CompleteAudienceJob(ctx, db.CompleteAudienceJobParams{ID: job.ID, ResultID: result, EvaluatedAt: at,
		Population: int32(members.Size), Members: int32(len(members.Members)), ExcludedUnavailable: int32(members.Excluded.Unavailable),
		ExcludedConsent: int32(members.Excluded.Consent), ExcludedCriteria: int32(members.Excluded.Criteria)})
	if err != nil {
		return err
	}
	if completed != 1 {
		return errJobNotRunning
	}
	return tx.Commit(ctx)
}
