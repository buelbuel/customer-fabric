// Package workflows runs Customer Fabric's durable Temporal workflows (ADR 0014). A workflow's input and
// history hold opaque references only; activities read current, authorized state from PostgreSQL.
package workflows

import (
	"context"
	"errors"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the queue the profile-change workflow and its activities run on.
const TaskQueue = "profile-changes"

// rejected marks an activity error that retrying cannot fix.
const rejected = "notification_rejected"

// ProfileChange notifies the configured destination that a source record changed. The notification is
// re-decided and retried by its activity; whatever the result, it is recorded once. The workflow ID is
// derived from the event ID, so a redelivered event never runs twice while Temporal remembers it, and
// the recorded outcome stops a resend after that.
func ProfileChange(ctx workflow.Context, e events.Event) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute,
			MaximumAttempts: 5, NonRetryableErrorTypes: []string{rejected}},
	})
	var a *Activities
	var result platform.NotificationResult
	err := workflow.ExecuteActivity(ctx, a.Notify, e).Get(ctx, &result)
	var appErr *temporal.ApplicationError
	switch {
	case err == nil:
	case errors.As(err, &appErr) && appErr.Type() == rejected:
		result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "rejected"}
	default:
		result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "retries_exhausted"}
	}
	if err := workflow.ExecuteActivity(ctx, a.Record, e, result).Get(ctx, nil); err != nil {
		return "", err
	}
	return result.Outcome, nil
}

// Activities adapts the notifier to Temporal: a rejection becomes non-retryable, everything else is
// retried by the workflow's policy.
type Activities struct {
	Notifier platform.Notifier
}

func (a *Activities) Notify(ctx context.Context, e events.Event) (platform.NotificationResult, error) {
	result, err := a.Notifier.Notify(ctx, e)
	if errors.Is(err, platform.ErrNotificationRejected) {
		return result, temporal.NewNonRetryableApplicationError("the destination rejected the notification", rejected, nil)
	}
	return result, err
}

func (a *Activities) Record(ctx context.Context, e events.Event, r platform.NotificationResult) error {
	return a.Notifier.Record(ctx, e, r)
}
