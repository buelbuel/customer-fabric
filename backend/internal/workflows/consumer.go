package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// Starter starts a workflow; client.Client satisfies it.
type Starter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
}

// Consumer turns profile events from Kafka into profile-change workflows, at least once. It commits a
// record's offset only after its workflow was started or found already started, and while Temporal
// cannot be reached it retries the same record instead of moving on, so no event is skipped.
type Consumer struct {
	Kafka    *kgo.Client
	Temporal Starter
	Tenant   string
	// Backoff is the wait between attempts to start a workflow; the default is two seconds.
	Backoff time.Duration
}

// NewConsumerClient joins the consumer group that reads the profile-event topic, with offsets committed
// only by the consumer.
func NewConsumerClient(brokers []string, topic, group string) (*kgo.Client, error) {
	return kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumerGroup(group), kgo.ConsumeTopics(topic),
		kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.DialTimeout(5*time.Second))
}

// Run consumes until ctx ends.
func (c Consumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		fetches := c.Kafka.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		fetches.EachError(func(string, int32, error) {
			slog.ErrorContext(ctx, "profile event fetch failed; it is retried")
		})
		var handled []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			if ctx.Err() == nil && c.handle(ctx, r) {
				handled = append(handled, r)
			}
		})
		if len(handled) > 0 {
			if err := c.Kafka.CommitRecords(ctx, handled...); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "profile event offsets not committed; the events may be redelivered")
			}
		}
	}
}

// handle starts the record's workflow, retrying until it succeeds or ctx ends. It reports whether the
// record may be committed: started, already started, or refused as not ours.
func (c Consumer) handle(ctx context.Context, r *kgo.Record) bool {
	var e events.Event
	if err := json.Unmarshal(r.Value, &e); err != nil || e.Schema != events.Schema || e.ID == "" {
		slog.WarnContext(ctx, "profile event refused", "reason", "unknown_schema", "partition", r.Partition, "offset", r.Offset)
		return true
	}
	if e.Tenant != c.Tenant {
		slog.WarnContext(ctx, "profile event refused", "reason", "other_tenant", "event_id", e.ID)
		return true
	}
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	options := client.StartWorkflowOptions{ID: "profile-change-" + e.ID, TaskQueue: TaskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}
	for {
		_, err := c.Temporal.ExecuteWorkflow(ctx, options, ProfileChange, e)
		var started *serviceerror.WorkflowExecutionAlreadyStarted
		if err == nil || errors.As(err, &started) {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		slog.ErrorContext(ctx, "profile-change workflow not started; retrying", "event_id", e.ID)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
	}
}
