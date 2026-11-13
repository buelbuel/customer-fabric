package platform

import (
	"context"
	"log/slog"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
)

// EventPublisher returns only after the transport acknowledged every event.
type EventPublisher interface {
	Publish(context.Context, []events.Event) error
}

// Relay moves committed outbox rows to the event transport (ADR 0013). It never writes to the
// transport inside a business transaction: the outbox row commits with the profile change first, and
// the relay publishes it afterwards, at least once.
type Relay struct {
	Store     Store
	Publisher EventPublisher
	Tenant    string
	Batch     int32
	// Prepare, if set, runs before the first batch and is retried until it succeeds, so a transport
	// that is down at startup delays relaying without stopping the API.
	Prepare func(context.Context) error
	// StepTimeout bounds one batch, including the publish; the default is one minute.
	StepTimeout time.Duration
}

// RelayOnce publishes one batch and checkpoints it. A failed publish leaves the batch pending; a crash
// after the acknowledgement but before the checkpoint commits republishes it with the same event IDs.
// It returns zero without publishing while another relay holds the lock.
func (r Relay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := db.New(r.Store.Pool).WithTx(tx)
	if locked, err := q.TryRelayLock(ctx); err != nil || !locked {
		return 0, err
	}
	rows, err := q.PendingOutbox(ctx, db.PendingOutboxParams{RowLimit: r.Batch, MaxRows: 4 * r.Batch})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	batch, ids := make([]events.Event, len(rows)), make([]string, len(rows))
	for i, row := range rows {
		batch[i] = events.Event{Schema: events.Schema, ID: row.ID, Type: row.Type, Tenant: r.Tenant, Source: row.Source,
			ProfileID: row.ProfileID, ProfileVersion: row.Version, OccurredAt: row.OccurredAt.UTC(), CanonicalProfileID: row.CanonicalProfileID}
		ids[i] = row.ID
	}
	if err = r.Publisher.Publish(ctx, batch); err != nil {
		return 0, err
	}
	relayed, err := q.MarkOutboxRelayed(ctx, ids)
	if err != nil {
		return 0, err
	}
	return int(relayed), tx.Commit(ctx)
}

// Run relays every interval until ctx ends, draining a backlog without waiting between full batches.
func (r Relay) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timeout := r.StepTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	prepared := r.Prepare == nil
	for {
		var n int
		var err error
		step, cancel := context.WithTimeout(ctx, timeout)
		if !prepared {
			if err = r.Prepare(step); err == nil {
				prepared = true
			}
		}
		if prepared && err == nil {
			n, err = r.RelayOnce(step)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "outbox relay failed; events stay pending and are retried next interval")
		}
		if err == nil && n >= int(r.Batch) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
