// Package workflows runs Customer Fabric's durable Temporal workflows (ADR 0014). A workflow's input and
// history hold opaque references only; activities read current, authorized state from PostgreSQL.
package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the queue the profile-change workflow and its activities run on.
const TaskQueue = "profile-changes"

// rejected marks a destination's refusal, which retrying cannot fix.
const rejected = "notification_rejected"

// unavailable is the outcome Notify reports when the destination could not take the notification now.
// The workflow, not the activity's retry policy, counts these, so that only the destination's failures
// use up the destination's attempts.
const unavailable = "unavailable"

// destinationAttempts is how many times a destination that keeps failing is tried.
const destinationAttempts = 5

// ProfileChange notifies the configured destination that a source record changed. The notification is
// re-decided by its activity on every attempt; whatever the result, it is recorded once. The workflow ID
// is derived from the event ID, so a redelivered event never runs twice while Temporal remembers it, and
// the recorded outcome stops a resend after that.
//
// Only the destination's own failures can end a notification as failed: it is tried destinationAttempts
// times, with backoff, before it is recorded as failed. Customer Fabric's own failures, such as its
// database being unreachable or a shutdown, are retried by the activity's policy until they clear, and
// count against nothing; so is recording the outcome.
func ProfileChange(ctx workflow.Context, e events.Event) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute,
			NonRetryableErrorTypes: []string{rejected}},
	})
	var a *Activities
	var result platform.NotificationResult
	for attempt := 1; ; attempt++ {
		err := workflow.ExecuteActivity(ctx, a.Notify, e).Get(ctx, &result)
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == rejected {
			result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "rejected"}
			break
		}
		if err != nil {
			return "", err
		}
		if result.Outcome != unavailable {
			break
		}
		if attempt == destinationAttempts {
			result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "retries_exhausted", StatusCode: result.StatusCode}
			break
		}
		if err := workflow.Sleep(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
			return "", err
		}
	}
	if err := workflow.ExecuteActivity(ctx, a.Record, e, result).Get(ctx, nil); err != nil {
		return "", err
	}
	return result.Outcome, nil
}

// Activities adapts the notifier to Temporal. A rejection ends the notification at once; a destination
// that cannot take it now is reported as the unavailable outcome for the workflow to count. Every other
// error is Customer Fabric's own and is retried by the workflow's policy.
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
	case errors.Is(err, platform.ErrNotificationUnavailable):
		return platform.NotificationResult{Outcome: unavailable, StatusCode: result.StatusCode}, nil
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
		err := w.Start()
		if err == nil {
			<-ctx.Done()
			w.Stop()
			return
		}
		w.Stop()
		slog.ErrorContext(ctx, "temporal worker did not start; retrying", "error_type", fmt.Sprintf("%T", err))
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
