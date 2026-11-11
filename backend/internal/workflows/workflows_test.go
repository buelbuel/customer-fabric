package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/stretchr/testify/mock"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

var change = events.Event{Schema: events.Schema, ID: "2c8fc2a3-6e3b-491f-917e-b67ed41ccb0b", Type: "profile.updated.v1", Tenant: "acme",
	Source: "console", ProfileID: "3734a5d2-a9c5-4c9f-b279-e9a403838eeb", ProfileVersion: 2, OccurredAt: time.Unix(1_800_000_000, 0).UTC()}

// run executes the workflow with the given Notify behaviour and returns its outcome and what it recorded.
func run(t *testing.T, notify func(context.Context, events.Event) (platform.NotificationResult, error)) (string, platform.NotificationResult, int) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var a *Activities
	env.RegisterActivity(a)
	calls := 0
	env.OnActivity(a.Notify, mock.Anything, mock.Anything).Return(func(ctx context.Context, e events.Event) (platform.NotificationResult, error) {
		calls++
		return notify(ctx, e)
	})
	var recorded platform.NotificationResult
	env.OnActivity(a.Record, mock.Anything, mock.Anything, mock.Anything).Return(func(_ context.Context, e events.Event, r platform.NotificationResult) error {
		if e.ID != change.ID {
			t.Errorf("recorded event %s", e.ID)
		}
		recorded = r
		return nil
	})
	env.ExecuteWorkflow(ProfileChange, change)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow: completed %v, %v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	var outcome string
	if err := env.GetWorkflowResult(&outcome); err != nil {
		t.Fatal(err)
	}
	return outcome, recorded, calls
}

func TestProfileChangeRecordsEveryOutcomeOnce(t *testing.T) {
	delivered := platform.NotificationResult{Outcome: platform.NotificationDelivered, Reason: "accepted", StatusCode: 202}
	if outcome, recorded, calls := run(t, func(context.Context, events.Event) (platform.NotificationResult, error) { return delivered, nil }); outcome != "delivered" || recorded != delivered || calls != 1 {
		t.Fatalf("delivered: %s %+v after %d calls", outcome, recorded, calls)
	}
	suppressed := platform.NotificationResult{Outcome: platform.NotificationSuppressed, Reason: "record_restricted"}
	if outcome, recorded, _ := run(t, func(context.Context, events.Event) (platform.NotificationResult, error) { return suppressed, nil }); outcome != "suppressed" || recorded != suppressed {
		t.Fatalf("suppressed: %s %+v", outcome, recorded)
	}
	// A transient failure is retried by the workflow's policy and can still be delivered.
	failures := 2
	outcome, recorded, calls := run(t, func(context.Context, events.Event) (platform.NotificationResult, error) {
		if failures > 0 {
			failures--
			return platform.NotificationResult{}, platform.ErrNotificationUnavailable
		}
		return delivered, nil
	})
	if outcome != "delivered" || recorded != delivered || calls != 3 {
		t.Fatalf("retried: %s %+v after %d calls", outcome, recorded, calls)
	}
	// A rejection is not retried.
	outcome, recorded, calls = run(t, func(context.Context, events.Event) (platform.NotificationResult, error) {
		return platform.NotificationResult{}, temporal.NewNonRetryableApplicationError("rejected", rejected, nil)
	})
	if outcome != "failed" || recorded.Reason != "rejected" || calls != 1 {
		t.Fatalf("rejected: %s %+v after %d calls", outcome, recorded, calls)
	}
	// A failure that never clears ends after the policy's five attempts.
	outcome, recorded, calls = run(t, func(context.Context, events.Event) (platform.NotificationResult, error) {
		return platform.NotificationResult{}, platform.ErrNotificationUnavailable
	})
	if outcome != "failed" || recorded.Reason != "retries_exhausted" || calls != 5 {
		t.Fatalf("exhausted: %s %+v after %d calls", outcome, recorded, calls)
	}
}

// fakeStarter answers ExecuteWorkflow from a list of errors, one per call.
type fakeStarter struct {
	errs    []error
	options []client.StartWorkflowOptions
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
	f.options = append(f.options, options)
	if len(f.errs) == 0 {
		return nil, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return nil, err
}

func record(t *testing.T, e events.Event) *kgo.Record {
	t.Helper()
	k, err := events.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The consumer starts one workflow per event, named by the event, holds its place while Temporal fails,
// and refuses envelopes that are not this tenant's current schema without starting anything.
func TestConsumerStartsOneWorkflowPerEvent(t *testing.T) {
	ctx := context.Background()
	started := &fakeStarter{}
	c := Consumer{Temporal: started, Tenant: "acme", Backoff: time.Millisecond}
	if !c.handle(ctx, record(t, change)) || len(started.options) != 1 || started.options[0].ID != "profile-change-"+change.ID ||
		started.options[0].TaskQueue != TaskQueue || !started.options[0].WorkflowExecutionErrorWhenAlreadyStarted {
		t.Fatalf("start options %+v", started.options)
	}

	// A redelivered event whose workflow already exists is done, not an error.
	already := &fakeStarter{errs: []error{serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "")}}
	if !(Consumer{Temporal: already, Tenant: "acme"}).handle(ctx, record(t, change)) || len(already.options) != 1 {
		t.Fatal("an already started workflow was not accepted")
	}

	// While Temporal is unavailable the same record is retried, never skipped.
	down := errors.New("temporal unavailable")
	flaky := &fakeStarter{errs: []error{down, down, down}}
	if !(Consumer{Temporal: flaky, Tenant: "acme", Backoff: time.Millisecond}).handle(ctx, record(t, change)) || len(flaky.options) != 4 {
		t.Fatalf("retried %d times", len(flaky.options))
	}
	// Shutting down while Temporal is still unavailable leaves the record uncommitted.
	stopped, cancel := context.WithCancel(ctx)
	forever := &fakeStarter{errs: []error{down, down, down, down, down, down, down, down}}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if (Consumer{Temporal: forever, Tenant: "acme", Backoff: 5 * time.Millisecond}).handle(stopped, record(t, change)) {
		t.Fatal("a record whose workflow never started would be committed")
	}

	// Another tenant's event, an unknown schema and a malformed record start nothing and are not retried.
	refused := &fakeStarter{}
	other := change
	other.Tenant = "other"
	future := change
	future.Schema = "customer-fabric.event.v2"
	for _, r := range []*kgo.Record{record(t, other), record(t, future), {Value: []byte("not json")}} {
		if !(Consumer{Temporal: refused, Tenant: "acme"}).handle(ctx, r) {
			t.Fatal("a refused record would be retried forever")
		}
	}
	if len(refused.options) != 0 {
		t.Fatalf("refused records started %d workflows", len(refused.options))
	}
}
