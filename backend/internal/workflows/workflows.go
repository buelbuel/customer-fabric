// Package workflows runs Customer Fabric's durable Temporal workflows (ADR 0014). A workflow's input and
// history hold opaque references only; activities read current, authorized state from PostgreSQL.
package workflows

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the queue the profile-change workflow and its activities run on.
const TaskQueue = "profile-changes"

// Activity error types that end a notification: the destination refused it, or still could not take
// it after destinationAttempts attempts.
const (
	rejected            = "notification_rejected"
	destinationGaveUp   = "destination_unavailable"
	destinationAttempts = 5
)

// ProfileChange notifies the configured destination that a source record changed. The notification is
// re-decided and retried by its activity; whatever the result, it is recorded once. The workflow ID is
// derived from the event ID, so a redelivered event never runs twice while Temporal remembers it, and
// the recorded outcome stops a resend after that.
//
// Only the destination's own failures can end a notification as failed. Customer Fabric's own
// failures, such as its database being unreachable, are retried until they clear, so an outage never
// consumes an event; so is recording the outcome.
func ProfileChange(ctx workflow.Context, e events.Event) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute,
			NonRetryableErrorTypes: []string{rejected, destinationGaveUp}},
	})
	var a *Activities
	var result platform.NotificationResult
	err := workflow.ExecuteActivity(ctx, a.Notify, e).Get(ctx, &result)
	var appErr *temporal.ApplicationError
	switch {
	case err == nil:
	case errors.As(err, &appErr) && appErr.Type() == rejected:
		result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "rejected"}
	case errors.As(err, &appErr) && appErr.Type() == destinationGaveUp:
		result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "retries_exhausted"}
	default:
		return "", err
	}
	if err := workflow.ExecuteActivity(ctx, a.Record, e, result).Get(ctx, nil); err != nil {
		return "", err
	}
	return result.Outcome, nil
}

// Activities adapts the notifier to Temporal. A rejection ends the notification at once; a destination
// that still cannot take it on the destinationAttempts-th attempt ends it too. Every other error is
// retried by the workflow's policy.
type Activities struct {
	Notifier Notifications
}

// Notifications sends and records profile-change notifications; platform.Notifier implements it.
type Notifications interface {
	Notify(context.Context, events.Event) (platform.NotificationResult, error)
	Record(context.Context, events.Event, platform.NotificationResult) error
}

func (a *Activities) Notify(ctx context.Context, e events.Event) (platform.NotificationResult, error) {
	result, err := a.Notifier.Notify(ctx, e)
	switch {
	case errors.Is(err, platform.ErrNotificationRejected):
		return result, temporal.NewNonRetryableApplicationError("the destination rejected the notification", rejected, nil)
	case errors.Is(err, platform.ErrNotificationUnavailable) && activity.GetInfo(ctx).Attempt >= destinationAttempts:
		return result, temporal.NewNonRetryableApplicationError("the destination could not take the notification", destinationGaveUp, nil)
	}
	return result, err
}

func (a *Activities) Record(ctx context.Context, e events.Event, r platform.NotificationResult) error {
	return a.Notifier.Record(ctx, e, r)
}

// RunWorker runs the profile-change worker until ctx ends. Temporal need not be reachable at first: a
// worker that fails to start cannot be started again, so a new one is built for every attempt.
func RunWorker(ctx context.Context, c client.Client, activities *Activities) {
	for ctx.Err() == nil {
		w := worker.New(c, TaskQueue, worker.Options{})
		w.RegisterWorkflow(ProfileChange)
		w.RegisterActivity(activities)
		if err := w.Start(); err == nil {
			<-ctx.Done()
			w.Stop()
			return
		}
		w.Stop()
		slog.ErrorContext(ctx, "temporal worker did not start; retrying")
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
