package platform

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Housekeeper keeps stored data within its bounds: it erases webhook signing secrets past their overlap,
// seals plaintext secrets left from before sealing (keeping the plaintext until an operator finalizes),
// and reseals secrets still sealed under an older deployment key. Every step is idempotent and safe to
// run in several processes at once.
type Housekeeper struct{ Store Store }

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
	erased, err := h.Store.EraseRetiredSecrets(ctx)
	if err != nil {
		return err
	}
	if erased > 0 {
		slog.InfoContext(ctx, "retired webhook signing secrets erased", "count", erased)
	}
	for ctx.Err() == nil {
		sealed, err := h.Store.SealPlaintextSecrets(ctx, 100)
		if errors.Is(err, ErrSigningKeyUnavailable) {
			// The configured sealing key is not the one registered under its ID: seal nothing with it.
			slog.WarnContext(ctx, "the webhook sealing key does not match its registered check; nothing sealed")
			return nil
		}
		if err != nil {
			return err
		}
		if sealed == 0 {
			break
		}
		slog.InfoContext(ctx, "plaintext webhook signing secrets sealed; finalize to clear the plaintext", "count", sealed)
	}
	moved, err := h.Store.ResealSecrets(ctx, 100)
	if moved > 0 {
		slog.InfoContext(ctx, "webhook signing secrets resealed under the sealing key", "count", moved)
	}
	if errors.Is(err, ErrSigningKeyUnavailable) {
		return nil
	}
	return err
}
