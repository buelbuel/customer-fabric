package workflows

import (
	"context"
	"errors"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/testsuite"
)

func runRecovery(t *testing.T, f *fakeNotifications) string {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Notifier: f})
	env.ExecuteWorkflow(RecoverNotification, change, "s2")
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow: completed %v, %v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	var outcome string
	if err := env.GetWorkflowResult(&outcome); err != nil {
		t.Fatal(err)
	}
	return outcome
}

// A recovered notification goes to its one subscription only, is re-decided by Notify like any other,
// and counts the destination's failures the same way.
func TestRecoverNotificationNotifiesOneSubscription(t *testing.T) {
	delivered := platform.NotificationResult{Outcome: platform.NotificationDelivered, Reason: "accepted", StatusCode: 200}
	flaky := &fakeNotifications{subscriptions: []string{"s1", "s2", "s3"}, notify: func(call int) (platform.NotificationResult, error) {
		if call == 1 {
			return platform.NotificationResult{}, platform.ErrNotificationUnavailable
		}
		return delivered, nil
	}}
	if got := runRecovery(t, flaky); got != "delivered" || flaky.notified != 2 || len(flaky.recordedFor) != 1 || flaky.recordedFor[0] != "s2" ||
		flaky.notifiedFor[0] != "s2" || flaky.recorded[0].Attempts != 2 {
		t.Fatalf("%s: notified %v, recorded %v %+v", got, flaky.notifiedFor, flaky.recordedFor, flaky.recorded)
	}
	withheld := platform.NotificationResult{Outcome: platform.NotificationSuppressed, Reason: "purpose_not_permitted"}
	if f := (&fakeNotifications{notify: func(int) (platform.NotificationResult, error) { return withheld, nil }}); runRecovery(t, f) != "suppressed" || f.recorded[0] != withheld {
		t.Fatalf("withheld: %+v", f.recorded)
	}
}

type fakeRecoveries struct {
	queued []platform.QueuedRecovery
	marked []string
	fail   error
}

func (f *fakeRecoveries) QueuedRecoveries(context.Context, string, int32) ([]platform.QueuedRecovery, error) {
	return f.queued, nil
}

func (f *fakeRecoveries) MarkRecoveryStarted(_ context.Context, subscription, eventID string) error {
	if f.fail != nil {
		return f.fail
	}
	f.marked = append(f.marked, subscription+"/"+eventID)
	return nil
}

// Each item starts one workflow, named for its subscription and event; one already started is marked,
// not started again, and a failed start leaves the item queued.
func TestRecoveryStarterStartsEachItemOnce(t *testing.T) {
	store := &fakeRecoveries{queued: []platform.QueuedRecovery{{Subscription: "s2", Event: change}}}
	started := &fakeStarter{}
	if n, err := (RecoveryStarter{Store: store, Temporal: started, Tenant: "acme"}).StartOnce(context.Background()); n != 1 || err != nil {
		t.Fatalf("started %d: %v", n, err)
	}
	options := started.options[0]
	if options.ID != "notification-recovery-s2-"+change.ID || options.TaskQueue != TaskQueue || !options.WorkflowExecutionErrorWhenAlreadyStarted ||
		len(store.marked) != 1 {
		t.Fatalf("options %+v, marked %v", options, store.marked)
	}
	already := &fakeStarter{errs: []error{serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "")}}
	if n, err := (RecoveryStarter{Store: store, Temporal: already, Tenant: "acme"}).StartOnce(context.Background()); n != 1 || err != nil || len(store.marked) != 2 {
		t.Fatalf("already started: %d %v %v", n, err, store.marked)
	}
	down := &fakeStarter{errs: []error{errors.New("unreachable")}}
	if n, err := (RecoveryStarter{Store: store, Temporal: down, Tenant: "acme"}).StartOnce(context.Background()); n != 0 || err == nil || len(store.marked) != 2 {
		t.Fatalf("unreachable: %d %v %v", n, err, store.marked)
	}
}
