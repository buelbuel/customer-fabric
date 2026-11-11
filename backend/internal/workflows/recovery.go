package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// Recoveries lists queued recovery items and marks them started; platform.Store implements it.
type Recoveries interface {
	QueuedRecoveries(ctx context.Context, tenant string, limit int32) ([]platform.QueuedRecovery, error)
	MarkRecoveryStarted(ctx context.Context, subscription, eventID string) error
}

// RecoveryStarter starts a RecoverNotification workflow for every queued recovery item. Only an
// operator's recovery request queues items; nothing is replayed on a policy change or a reapproval. The
// workflow ID is derived from the subscription and event, so an item started again after a crash, or by
// a second API process, finds its workflow already started; the recorded outcome stops a resend after
// Temporal forgets it.
type RecoveryStarter struct {
	Store    Recoveries
	Temporal Starter
	Tenant   string
	// Interval is the wait between polls; the default is five seconds.
	Interval time.Duration
}

// Run starts queued items until ctx ends.
func (r RecoveryStarter) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for ctx.Err() == nil {
		started, err := r.StartOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "notification recoveries not started; retrying", "error_type", fmt.Sprintf("%T", err))
		}
		if started > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// StartOnce starts up to 100 queued items and reports how many it started.
func (r RecoveryStarter) StartOnce(ctx context.Context) (int, error) {
	items, err := r.Store.QueuedRecoveries(ctx, r.Tenant, 100)
	if err != nil {
		return 0, err
	}
	for i, item := range items {
		options := client.StartWorkflowOptions{ID: "notification-recovery-" + item.Subscription + "-" + item.Event.ID, TaskQueue: TaskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}
		_, err := r.Temporal.ExecuteWorkflow(ctx, options, RecoverNotification, item.Event, item.Subscription)
		var started *serviceerror.WorkflowExecutionAlreadyStarted
		if err != nil && !errors.As(err, &started) {
			return i, err
		}
		if err := r.Store.MarkRecoveryStarted(ctx, item.Subscription, item.Event.ID); err != nil {
			return i, err
		}
	}
	return len(items), nil
}
