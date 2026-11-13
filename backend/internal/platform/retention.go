package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// Retention is the maximum age of each eventing and notification category (ADR 0014, #28). Nothing is
// kept indefinitely. The values are the controller's to set; the bounds keep the delivery guarantees:
//   - notification outcomes, withdrawal notices and recoveries outlive the recovery window, within
//     which a suppressed change can still be recovered;
//   - outbox rows outlive both the recovery window, because recovery rebuilds envelopes from them, and
//     the Kafka topic's retention, because a redelivered event is matched to subscriptions by its row;
//   - Kafka topics keep events at most 14 days (ADR 0009 section 17).
type Retention struct {
	Outbox        time.Duration
	Notifications time.Duration
	Kafka         time.Duration
	// Relay is whether an outbox relay is configured. Without one, outbox rows are never published and age
	// out like relayed ones.
	Relay bool
}

const (
	day              = 24 * time.Hour
	maxRetention     = 365 * day
	maxKafkaRetained = 14 * day
)

// DefaultRetention is used for every value not configured.
func DefaultRetention() Retention {
	return Retention{Outbox: 35 * day, Notifications: 90 * day, Kafka: 7 * day}
}

// WithDefaults fills every unset value with its default, so nothing is kept indefinitely by omission.
func (r Retention) WithDefaults() Retention {
	d := DefaultRetention()
	if r.Outbox == 0 {
		r.Outbox = d.Outbox
	}
	if r.Notifications == 0 {
		r.Notifications = d.Notifications
	}
	if r.Kafka == 0 {
		r.Kafka = d.Kafka
	}
	return r
}

// Validate refuses a retention that would break recovery or redelivery, or keep data unbounded.
func (r Retention) Validate() error {
	switch {
	case r.Kafka < day || r.Kafka > maxKafkaRetained:
		return fmt.Errorf("Kafka retention must be 1 to 14 days")
	case r.Notifications <= recoveryWindow || r.Notifications > maxRetention:
		return fmt.Errorf("notification retention must be longer than the %d-day recovery window and at most 365 days", recoveryWindow/day)
	case r.Outbox <= max(recoveryWindow, r.Kafka) || r.Outbox > maxRetention:
		return fmt.Errorf("outbox retention must be longer than both the recovery window and the Kafka retention, and at most 365 days")
	}
	return nil
}

// retentionBatch bounds each delete, so a purge never holds locks for long.
const retentionBatch = 1000

// PruneEventing removes what each category has kept past its maximum age, in batches. Every batch that
// removed rows commits with its receipt. It returns how many rows each category lost.
func (s Store) PruneEventing(ctx context.Context, r Retention, now time.Time) (map[string]int64, error) {
	pruned := map[string]int64{}
	steps := []struct {
		category string
		age      time.Duration
		batch    func(context.Context, *db.Queries, time.Time) (int64, error)
	}{
		{"notification_outcomes", r.Notifications, func(ctx context.Context, q *db.Queries, horizon time.Time) (int64, error) {
			return q.PruneNotificationOutcomes(ctx, db.PruneNotificationOutcomesParams{Horizon: horizon, RowLimit: retentionBatch})
		}},
		{"withdrawal_notices", r.Notifications, pruneWithdrawals},
		{"notification_recoveries", r.Notifications, pruneRecoveries},
		{"outbox", r.Outbox, func(ctx context.Context, q *db.Queries, horizon time.Time) (int64, error) {
			return q.PruneOutbox(ctx, db.PruneOutboxParams{Horizon: horizon, Relay: r.Relay, RowLimit: retentionBatch})
		}},
	}
	for _, step := range steps {
		horizon := now.Add(-step.age)
		for ctx.Err() == nil {
			n, err := s.purgeBatch(ctx, step.category, horizon, step.batch)
			if err != nil {
				return pruned, err
			}
			pruned[step.category] += n
			if n < retentionBatch {
				break
			}
		}
	}
	return pruned, ctx.Err()
}

func (s Store) purgeBatch(ctx context.Context, category string, horizon time.Time, batch func(context.Context, *db.Queries, time.Time) (int64, error)) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.AllowRetentionPurge(ctx); err != nil {
			return err
		}
		var err error
		if n, err = batch(ctx, q, horizon); err != nil || n == 0 {
			return err
		}
		return q.InsertRetentionPurge(ctx, db.InsertRetentionPurgeParams{ID: UUID(), Category: category, Horizon: horizon, Purged: n})
	})
	return n, err
}

func pruneWithdrawals(ctx context.Context, q *db.Queries, horizon time.Time) (int64, error) {
	ids, err := q.PrunableWithdrawals(ctx, db.PrunableWithdrawalsParams{Horizon: horizon, RowLimit: retentionBatch})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if err = q.DeleteWithdrawalAttempts(ctx, ids); err != nil {
		return 0, err
	}
	return q.DeleteWithdrawals(ctx, ids)
}

func pruneRecoveries(ctx context.Context, q *db.Queries, horizon time.Time) (int64, error) {
	ids, err := q.PrunableRecoveries(ctx, db.PrunableRecoveriesParams{Horizon: horizon, RowLimit: retentionBatch})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if _, err = q.DeleteRecoveryItems(ctx, ids); err != nil {
		return 0, err
	}
	return q.DeleteRecoveries(ctx, ids)
}

// RetentionCategory is one category's configured maximum age, its oldest row and its latest purge.
type RetentionCategory struct {
	Category   string          `json:"category"`
	MaxAgeDays int             `json:"max_age_days"`
	OldestAt   *time.Time      `json:"oldest_at,omitempty"`
	LastPurge  *RetentionPurge `json:"last_purge,omitempty"`
}

// RetentionPurge is a purge receipt: how many rows a purge removed under which horizon, never which.
type RetentionPurge struct {
	Category string    `json:"category"`
	Horizon  time.Time `json:"horizon"`
	Purged   int64     `json:"purged"`
	RanAt    time.Time `json:"ran_at"`
}

// RetentionReport is the configured retention, what each category holds, and the recent receipts.
type RetentionReport struct {
	Categories         []RetentionCategory `json:"categories"`
	KafkaRetentionDays int                 `json:"kafka_retention_days"`
	Purges             []RetentionPurge    `json:"purges"`
}

// RetentionStatus reports retention for audit readers.
func (s Store) RetentionStatus(ctx context.Context, r Retention) (RetentionReport, error) {
	q := db.New(s.Pool)
	oldest, err := q.RetentionOldest(ctx)
	if err != nil {
		return RetentionReport{}, err
	}
	latest, err := q.LatestRetentionPurges(ctx)
	if err != nil {
		return RetentionReport{}, err
	}
	recent, err := q.ListRetentionPurges(ctx)
	if err != nil {
		return RetentionReport{}, err
	}
	report := RetentionReport{KafkaRetentionDays: int(r.Kafka / day), Purges: make([]RetentionPurge, 0, len(recent))}
	for _, p := range recent {
		report.Purges = append(report.Purges, RetentionPurge{Category: p.Category, Horizon: p.Horizon, Purged: p.Purged, RanAt: p.RanAt})
	}
	for _, c := range []struct {
		name string
		age  time.Duration
	}{{"outbox", r.Outbox}, {"notification_outcomes", r.Notifications}, {"withdrawal_notices", r.Notifications}, {"notification_recoveries", r.Notifications}} {
		category := RetentionCategory{Category: c.name, MaxAgeDays: int(c.age / day)}
		for _, o := range oldest {
			if o.Category == c.name {
				category.OldestAt = &o.Oldest
			}
		}
		for _, p := range latest {
			if p.Category == c.name {
				category.LastPurge = &RetentionPurge{Category: p.Category, Horizon: p.Horizon, Purged: p.Purged, RanAt: p.RanAt}
			}
		}
		report.Categories = append(report.Categories, category)
	}
	return report, nil
}
