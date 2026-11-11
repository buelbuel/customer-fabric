package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

var change = events.Event{Schema: events.Schema, ID: "2c8fc2a3-6e3b-491f-917e-b67ed41ccb0b", Type: "profile.updated.v1", Tenant: "acme",
	Source: "console", ProfileID: "3734a5d2-a9c5-4c9f-b279-e9a403838eeb", ProfileVersion: 2, OccurredAt: time.Unix(1_800_000_000, 0).UTC()}

// fakeNotifications answers Notify and Record from scripted functions and counts the calls.
type fakeNotifications struct {
	notify   func(call int) (platform.NotificationResult, error)
	record   func(call int) error
	notified int
	recorded []platform.NotificationResult
	records  int
}

func (f *fakeNotifications) Notify(_ context.Context, e events.Event) (platform.NotificationResult, error) {
	f.notified++
	return f.notify(f.notified)
}

func (f *fakeNotifications) Record(_ context.Context, e events.Event, r platform.NotificationResult) error {
	f.records++
	if f.record != nil {
		if err := f.record(f.records); err != nil {
			return err
		}
	}
	f.recorded = append(f.recorded, r)
	return nil
}

// run executes the workflow with the real activities over a scripted notifier, so the activities'
// own handling of attempts and errors is exercised too.
func run(t *testing.T, f *fakeNotifications) string {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Notifier: f})
	env.ExecuteWorkflow(ProfileChange, change)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow: completed %v, %v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	var outcome string
	if err := env.GetWorkflowResult(&outcome); err != nil {
		t.Fatal(err)
	}
	if len(f.recorded) != 1 {
		t.Fatalf("recorded %d outcomes", len(f.recorded))
	}
	return outcome
}

func TestProfileChangeRecordsEveryOutcomeOnce(t *testing.T) {
	delivered := platform.NotificationResult{Outcome: platform.NotificationDelivered, Reason: "accepted", StatusCode: 202}
	ok := func(int) (platform.NotificationResult, error) { return delivered, nil }
	if f := (&fakeNotifications{notify: ok}); run(t, f) != "delivered" || f.recorded[0] != delivered || f.notified != 1 {
		t.Fatalf("delivered after %d calls: %+v", f.notified, f.recorded)
	}
	suppressed := platform.NotificationResult{Outcome: platform.NotificationSuppressed, Reason: "record_restricted"}
	if f := (&fakeNotifications{notify: func(int) (platform.NotificationResult, error) { return suppressed, nil }}); run(t, f) != "suppressed" || f.recorded[0] != suppressed {
		t.Fatalf("suppressed: %+v", f.recorded)
	}
	// A destination that fails twice and then takes the notification.
	flaky := &fakeNotifications{notify: func(call int) (platform.NotificationResult, error) {
		if call <= 2 {
			return platform.NotificationResult{}, platform.ErrNotificationUnavailable
		}
		return delivered, nil
	}}
	if run(t, flaky) != "delivered" || flaky.notified != 3 {
		t.Fatalf("retried: %d calls", flaky.notified)
	}
	// A rejection is not retried.
	refused := &fakeNotifications{notify: func(int) (platform.NotificationResult, error) {
		return platform.NotificationResult{StatusCode: 400}, platform.ErrNotificationRejected
	}}
	if run(t, refused) != "failed" || refused.recorded[0].Reason != "rejected" || refused.notified != 1 {
		t.Fatalf("rejected: %+v after %d calls", refused.recorded, refused.notified)
	}
	// A destination that never takes it ends the notification after five attempts.
	down := &fakeNotifications{notify: func(int) (platform.NotificationResult, error) {
		return platform.NotificationResult{}, platform.ErrNotificationUnavailable
	}}
	if run(t, down) != "failed" || down.recorded[0].Reason != "retries_exhausted" || down.notified != 5 {
		t.Fatalf("exhausted: %+v after %d calls", down.recorded, down.notified)
	}
	// Customer Fabric's own outage, longer than five attempts, does not consume the event: it is
	// retried until it clears and the notification is then delivered.
	outage := &fakeNotifications{notify: func(call int) (platform.NotificationResult, error) {
		if call <= 8 {
			return platform.NotificationResult{}, errors.New("database unavailable")
		}
		return delivered, nil
	}}
	if run(t, outage) != "delivered" || outage.notified != 9 {
		t.Fatalf("after an outage: %d calls", outage.notified)
	}
	// Recording is retried until it succeeds.
	unrecorded := &fakeNotifications{notify: ok, record: func(call int) error {
		if call <= 6 {
			return errors.New("database unavailable")
		}
		return nil
	}}
	if run(t, unrecorded) != "delivered" || unrecorded.records != 7 || unrecorded.notified != 1 {
		t.Fatalf("record retried %d times, notified %d", unrecorded.records, unrecorded.notified)
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
