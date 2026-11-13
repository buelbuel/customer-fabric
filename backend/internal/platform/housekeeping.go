package platform

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Housekeeper keeps stored data within its bounds: it erases webhook signing secrets past their overlap,
// reseals secrets still sealed under an older deployment key, and prunes eventing and notification data
// past its retention. Every step is idempotent and safe to
// run in several processes at once.
type Housekeeper struct {
	Store Store
	// Retention bounds eventing and notification data; unset values take the defaults.
	Retention Retention
}

// Run runs a step every interval until ctx ends.
func (h Housekeeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := h.Step(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "housekeeping failed; retrying next interval")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Step runs every housekeeping task once.
func (h Housekeeper) Step(ctx context.Context) error {
	pruned, err := h.Store.PruneEventing(ctx, h.Retention.WithDefaults(), time.Now())
	for category, n := range pruned {
		if n > 0 {
			slog.InfoContext(ctx, "retention purge", "category", category, "count", n)
		}
	}
	if err != nil {
		return err
	}
	erased, err := h.Store.EraseRetiredSecrets(ctx)
	if err != nil {
		return err
	}
	if erased > 0 {
		slog.InfoContext(ctx, "retired webhook signing secrets erased", "count", erased)
	}
	moved, err := h.Store.ResealSecrets(ctx, 100)
	if moved > 0 {
		slog.InfoContext(ctx, "webhook signing secrets resealed under the sealing key", "count", moved)
	}
	if errors.Is(err, ErrSigningKeyUnavailable) {
		// The sealing key does not match its registered check: nothing is resealed with it.
		slog.WarnContext(ctx, "the webhook sealing key does not match its registered check; nothing resealed")
		return nil
	}
	return err
}
