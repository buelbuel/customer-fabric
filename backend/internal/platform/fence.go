package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// FenceLimits bound how long the commit fence (ADR 0016) can hold anything up. Unset values take the
// defaults, so nothing waits or runs for ever by omission.
type FenceLimits struct {
	// ChangeWait is how long a subscription change waits for a lock, the fence included, which means for
	// the writes in flight. Past it the change is refused as retriable and nothing is changed.
	ChangeWait time.Duration
	// WriteWait is how long a write waits for any lock, the fence included (which means for a subscription
	// change that is itself waiting) and the row and key locks after it. Past it the write is refused as
	// retriable and nothing is written. Keep it above ChangeWait.
	WriteWait time.Duration
	// TransactionTimeout is the deadline of every transaction that writes the outbox or changes a
	// subscription. The database enforces it from the transaction's first statement, so a client that
	// stalls or vanishes cannot hold the fence past it.
	TransactionTimeout time.Duration
}

const (
	minFenceLimit = 100 * time.Millisecond
	maxFenceLimit = 10 * time.Minute
	// blockedAfter is how long a write waits for the fence before it counts as blocked: a wait that
	// short is lock traffic, a longer one is a write held up by a subscription change.
	blockedAfter = 50 * time.Millisecond
	// slowFenceWait is the wait that is logged.
	slowFenceWait = time.Second
)

// DefaultFenceLimits are used for every value not configured. WriteWait exceeds ChangeWait, so a write
// queued behind a subscription change that gives up is not refused at the same moment.
func DefaultFenceLimits() FenceLimits {
	return FenceLimits{ChangeWait: 10 * time.Second, WriteWait: 15 * time.Second, TransactionTimeout: 30 * time.Second}
}

// WithDefaults fills every unset value with its default.
func (l FenceLimits) WithDefaults() FenceLimits {
	d := DefaultFenceLimits()
	if l.ChangeWait == 0 {
		l.ChangeWait = d.ChangeWait
	}
	if l.WriteWait == 0 {
		l.WriteWait = d.WriteWait
	}
	if l.TransactionTimeout == 0 {
		l.TransactionTimeout = d.TransactionTimeout
	}
	return l
}

// Validate refuses limits outside their bounds or with write wait not greater than change wait.
func (l FenceLimits) Validate() error {
	for name, d := range map[string]time.Duration{"change wait": l.ChangeWait, "write wait": l.WriteWait, "transaction timeout": l.TransactionTimeout} {
		if d < minFenceLimit || d > maxFenceLimit {
			return fmt.Errorf("the commit fence %s must be between %s and %s", name, minFenceLimit, maxFenceLimit)
		}
	}
	if l.WriteWait <= l.ChangeWait {
		return errors.New("the commit fence write wait must be greater than the change wait")
	}
	return nil
}

func (l FenceLimits) ms(d time.Duration) int32 {
	return int32((d + time.Millisecond - 1) / time.Millisecond)
}

// fenceLimits are the limits in force: the configured ones, or the defaults for any that is not valid.
func (s Store) fenceLimits() FenceLimits {
	l := s.Fence.WithDefaults()
	if l.Validate() != nil {
		return DefaultFenceLimits()
	}
	return l
}

// enterFence starts an outbox writer's bounds and takes the commit fence shared, before any other lock.
// Its wait is measured; a wait that reaches WriteWait fails with a lock timeout, which fail reports as 503.
func (s Store) enterFence(ctx context.Context, q *db.Queries) error {
	l := s.fenceLimits()
	done := s.Monitor.begin(sideWrites)
	err := q.EnterCommitFence(ctx, db.EnterCommitFenceParams{LockWaitMs: l.ms(l.WriteWait), TransactionMs: l.ms(l.TransactionTimeout)})
	done(err)
	return err
}

// beginChange starts a transaction that changes a subscription, bounded from its first statement.
func (s Store) beginChange(ctx context.Context) (pgx.Tx, *db.Queries, error) {
	l := s.fenceLimits()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	q := db.New(s.Pool).WithTx(tx)
	if err = q.BoundTransaction(ctx, db.BoundTransactionParams{LockWaitMs: l.ms(l.ChangeWait), TransactionMs: l.ms(l.TransactionTimeout)}); err != nil {
		tx.Rollback(ctx)
		return nil, nil, err
	}
	return tx, q, nil
}

// takeFenceExclusive waits for the writes in flight, which no new write then overtakes. It gives up at
// ChangeWait: the change is refused, retriable, and the writes that queued behind it go on.
func (s Store) takeFenceExclusive(ctx context.Context, q *db.Queries) error {
	done := s.Monitor.begin(sideChanges)
	err := q.TakeCommitFenceExclusive(ctx)
	done(err)
	if isLockTimeout(err) {
		return &Problem{Type: "about:blank", Status: 503, Title: "Writes in flight",
			Detail: "The change waited too long for writes in flight to commit and was not made; retry it.", RetryAfter: 1}
	}
	return err
}

const (
	sqlLockNotAvailable    = "55P03"
	sqlTransactionTimedOut = "25P04"
)

func isLockTimeout(err error) bool {
	var p *pgconn.PgError
	return errors.As(err, &p) && p.Code == sqlLockNotAvailable
}

// busyProblem reports a transaction the database refused for a bound, as a retriable 503. The write was
// not applied: a lock wait ended before it took effect, and a transaction ended by its deadline never
// committed. It returns nil for any other error.
func busyProblem(err error) *Problem {
	var p *pgconn.PgError
	if !errors.As(err, &p) {
		return nil
	}
	switch p.Code {
	case sqlLockNotAvailable:
		return &Problem{Type: "about:blank", Status: 503, Title: "Temporarily busy", RetryAfter: 1,
			Detail: "The request waited too long for a lock and was not applied; retry it."}
	case sqlTransactionTimedOut:
		slog.Warn("a transaction reached its deadline and was ended", "sqlstate", p.Code)
		return &Problem{Type: "about:blank", Status: 503, Title: "Transaction timed out", RetryAfter: 1,
			Detail: "The request did not finish within its time limit and was not applied; retry it."}
	}
	return nil
}

// FenceMonitor measures what the commit fence costs one API process: how long writes and subscription
// changes wait for it, and how many writes it blocks. A nil monitor measures nothing.
type FenceMonitor struct {
	mu              sync.Mutex
	writes, changes fenceSide
}

func NewFenceMonitor() *FenceMonitor { return &FenceMonitor{} }

type fenceSide struct {
	waiting, completed, blocked, timedOut int64
	total, longest                        time.Duration
	buckets                               [5]int64
}

var (
	sideWrites  = func(m *FenceMonitor) *fenceSide { return &m.writes }
	sideChanges = func(m *FenceMonitor) *fenceSide { return &m.changes }
)

// fenceBuckets are the upper bounds, inclusive, of the wait histogram; the last bucket is everything longer.
var fenceBuckets = [4]time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second}

// begin starts measuring one wait; the returned function ends it with the outcome of the lock request.
func (m *FenceMonitor) begin(side func(*FenceMonitor) *fenceSide) func(error) {
	if m == nil {
		return func(error) {}
	}
	start := time.Now()
	m.mu.Lock()
	side(m).waiting++
	m.mu.Unlock()
	return func(err error) {
		waited := time.Since(start)
		m.mu.Lock()
		s := side(m)
		s.waiting--
		timedOut := isLockTimeout(err)
		switch {
		case timedOut:
			s.timedOut++
		case err == nil:
			s.completed++
		}
		// Every wait is measured, whatever ended it; a request that failed for another reason counts in
		// neither completed nor timed out.
		s.total += waited
		s.longest = max(s.longest, waited)
		s.buckets[bucketOf(waited)]++
		if waited >= blockedAfter {
			s.blocked++
		}
		m.mu.Unlock()
		if timedOut || waited >= slowFenceWait {
			kind := "write"
			if side(m) == &m.changes {
				kind = "subscription change"
			}
			slog.Warn("commit fence wait", "kind", kind, "waited_ms", waited.Milliseconds(), "timed_out", timedOut)
		}
	}
}

func bucketOf(d time.Duration) int {
	for i, bound := range fenceBuckets {
		if d <= bound {
			return i
		}
	}
	return len(fenceBuckets)
}

// FenceWaits is one kind of request's waits for the commit fence, since the process started.
type FenceWaits struct {
	// Waiting is how many requests wait right now.
	Waiting int64 `json:"waiting"`
	// Completed is how many got the fence, TimedOut how many gave up at their lock wait, and Blocked how
	// many waited at least 50 ms, however the wait ended.
	Completed int64 `json:"completed"`
	Blocked   int64 `json:"blocked"`
	TimedOut  int64 `json:"timed_out"`
	// WaitMsTotal and WaitMsMax are the waits of all requests, however they ended.
	WaitMsTotal int64          `json:"wait_ms_total"`
	WaitMsMax   int64          `json:"wait_ms_max"`
	Histogram   FenceHistogram `json:"wait_histogram"`
}

// FenceHistogram counts waits by their length, in milliseconds.
type FenceHistogram struct {
	UpTo10    int64 `json:"up_to_10_ms"`
	UpTo100   int64 `json:"up_to_100_ms"`
	UpTo1000  int64 `json:"up_to_1000_ms"`
	UpTo10000 int64 `json:"up_to_10000_ms"`
	Over      int64 `json:"over_10000_ms"`
}

// FenceReport is what the monitor measured, with the limits in force.
type FenceReport struct {
	Writes  FenceWaits `json:"writes"`
	Changes FenceWaits `json:"subscription_changes"`
	// PendingSuspensions are the suspensions that stopped delivery and have not completed, in the database.
	PendingSuspensions PendingSuspensions `json:"pending_suspensions"`
	Limits             struct {
		ChangeWaitMs         int64 `json:"change_wait_ms"`
		WriteWaitMs          int64 `json:"write_wait_ms"`
		TransactionTimeoutMs int64 `json:"transaction_timeout_ms"`
	} `json:"limits"`
}

func (s *fenceSide) report() FenceWaits {
	return FenceWaits{Waiting: s.waiting, Completed: s.completed, Blocked: s.blocked, TimedOut: s.timedOut,
		WaitMsTotal: s.total.Milliseconds(), WaitMsMax: s.longest.Milliseconds(),
		Histogram: FenceHistogram{s.buckets[0], s.buckets[1], s.buckets[2], s.buckets[3], s.buckets[4]}}
}

// Report returns the measurements so far. The monitor belongs to one process; with several API
// processes, each reports its own.
func (m *FenceMonitor) Report(limits FenceLimits) FenceReport {
	var r FenceReport
	if m != nil {
		m.mu.Lock()
		r.Writes, r.Changes = m.writes.report(), m.changes.report()
		m.mu.Unlock()
	}
	r.Limits.ChangeWaitMs, r.Limits.WriteWaitMs = limits.ChangeWait.Milliseconds(), limits.WriteWait.Milliseconds()
	r.Limits.TransactionTimeoutMs = limits.TransactionTimeout.Milliseconds()
	return r
}
