package integration

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/workflows"
	"go.temporal.io/sdk/client"
)

// The whole slice with a real broker and Temporal (make events-test): a committed change goes from the
// outbox through the relay and Kafka to a workflow whose activity re-checks the record and notifies a
// destination. A redelivered event is not sent twice, a fenced record is suppressed, and an event that
// arrives while Temporal is unreachable waits, uncommitted, until it can be started.
func TestProfileChangeWorkflowEndToEnd(t *testing.T) {
	brokers, address := os.Getenv("TEST_KAFKA_BROKERS"), os.Getenv("TEST_TEMPORAL_ADDRESS")
	if brokers == "" || address == "" {
		t.Skip("TEST_KAFKA_BROKERS or TEST_TEMPORAL_ADDRESS unset: run make events-test")
	}
	r := newReceiver(t)
	s := setupActivation(t, r)
	s.destination("profile_changes", r)
	// A processor notified under legitimate interests, for changes from console.
	purpose, policy := s.purposeWithBasis("crm_sync", "legitimate_interests")
	s.subscribe("crm_sync", subscriptionBody("profile_changes", purpose, "console"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store := platform.Store{Pool: s.pool}

	topic := "test." + strings.ReplaceAll(platform.UUID(), "-", "")
	kafka, err := events.NewKafka(strings.Split(brokers, ","), topic)
	if err != nil {
		t.Fatal(err)
	}
	defer kafka.Close()
	if err = kafka.EnsureTopic(ctx, 3, time.Hour); err != nil {
		t.Fatal(err)
	}
	relay := platform.Relay{Store: store, Publisher: kafka, Tenant: "acme", Batch: 100}
	relayAll := func() {
		t.Helper()
		for {
			n, err := relay.RelayOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return
			}
		}
	}

	// The worker is the API's own, started while nothing listens at its Temporal address. Only later does
	// a proxy to the real server open there: the worker must start then and process workflows.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddress := listener.Addr().String()
	listener.Close()
	temporal, err := client.NewLazyClient(client.Options{HostPort: proxyAddress, Namespace: "customer-fabric"})
	if err != nil {
		t.Fatal(err)
	}
	defer temporal.Close()
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		workflows.RunWorker(workerCtx, temporal, &workflows.Activities{Notifier: platform.NewNotifier(store, s.config.WebhookHosts)})
		close(workerDone)
	}()
	defer func() { stopWorker(); <-workerDone }()
	time.Sleep(time.Second)
	proxy, err := net.Listen("tcp", proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		for {
			in, err := proxy.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", address)
			if err != nil {
				in.Close()
				continue
			}
			go func() { _, _ = io.Copy(out, in); out.Close() }()
			go func() { _, _ = io.Copy(in, out); in.Close() }()
		}
	}()
	group := "test-" + platform.UUID()
	consume := func(starter workflows.Starter, until func() bool) {
		t.Helper()
		k, err := workflows.NewConsumerClient(strings.Split(brokers, ","), topic, group)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Close()
		run, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			workflows.Consumer{Kafka: k, Temporal: starter, Tenant: "acme", Backoff: 100 * time.Millisecond}.Run(run)
			close(done)
		}()
		for deadline := time.Now().Add(time.Minute); !until(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				stop()
				<-done
				t.Fatal("the slice did not reach the expected state")
			}
		}
		stop()
		<-done
	}
	// Only this test's record is counted: the suite's setup leaves other source records in the outbox.
	var record string
	outcomes := func() map[string]string {
		rows, err := s.pool.Query(ctx, `SELECT event_id::text, outcome || ':' || reason FROM notification_deliveries WHERE profile_id = $1`, record)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, outcome string
			if err := rows.Scan(&id, &outcome); err != nil {
				t.Fatal(err)
			}
			out[id] = outcome
		}
		return out
	}

	// Temporal unreachable: the consumer starts nothing and commits nothing while it waits.
	record = s.viewRecord(tokenA, "workflow-ada", `{"tier":"gold"}`)
	relayAll()
	unreachable, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1", Namespace: "customer-fabric"})
	if err != nil {
		t.Fatal(err)
	}
	attempts := &countingStarter{Starter: unreachable}
	consume(attempts, func() bool { return attempts.calls.Load() >= 3 })
	unreachable.Close()
	if len(outcomes()) != 0 || len(sentFor(r, record)) != 0 {
		t.Fatal("an event was handled while Temporal was unreachable")
	}

	// Temporal reachable: the waiting event and a later change are both delivered, once each.
	s.expect(s.request("PATCH", "/v1/profiles/"+record, `{"set":{"tier":"silver"}}`, platform.UUID(), tokenA, `"1"`), 200)
	relayAll()
	consume(temporal, func() bool { return len(outcomes()) == 2 })
	for id, outcome := range outcomes() {
		if outcome != "delivered:accepted" {
			t.Fatalf("event %s: %s", id, outcome)
		}
	}
	sent := sentFor(r, record)
	if len(sent) != 2 || sent[0].key == sent[1].key {
		t.Fatalf("%d notifications sent", len(sent))
	}
	for _, d := range sent {
		if strings.Contains(d.body, "gold") || strings.Contains(d.body, "silver") {
			t.Fatalf("a notification carried a value or the wrong record: %s", d.body)
		}
	}

	// The same event published again is not sent again.
	var event events.Event
	if err = s.pool.QueryRow(ctx, `SELECT o.id, o.type, o.profile_id, o.version, o.source, o.occurred_at FROM outbox o WHERE o.profile_id = $1 ORDER BY o.version DESC LIMIT 1`, record).
		Scan(&event.ID, &event.Type, &event.ProfileID, &event.ProfileVersion, &event.Source, &event.OccurredAt); err != nil {
		t.Fatal(err)
	}
	event.Schema, event.Tenant = events.Schema, "acme"
	if err = kafka.Publish(ctx, []events.Event{event}); err != nil {
		t.Fatal(err)
	}
	// A change to a record that is then restricted is suppressed at the moment of sending.
	s.expect(s.request("PATCH", "/v1/profiles/"+record, `{"set":{"tier":"gold"}}`, platform.UUID(), tokenA, `"2"`), 200)
	if _, err = s.pool.Exec(ctx, `UPDATE profiles SET restricted = true WHERE id = $1`, record); err != nil {
		t.Fatal(err)
	}
	relayAll()
	consume(temporal, func() bool { return len(outcomes()) == 3 })
	suppressed := 0
	for _, outcome := range outcomes() {
		if outcome == "suppressed:record_restricted" {
			suppressed++
		}
	}
	if suppressed != 1 || len(sentFor(r, record)) != 2 {
		t.Fatalf("after a redelivery and a restriction: %v, %d sent", outcomes(), len(r.deliveries()))
	}

	// A policy change interrupts the subscription: a change made meanwhile is suppressed, the new
	// version and the reapproval send nothing, and only an operator's recovery sends it, once, through a
	// RecoverNotification workflow that authorizes it again.
	s.retirePolicy(policy)
	record = s.viewRecord(tokenA, "workflow-gap", `{"tier":"gold"}`)
	relayAll()
	consume(temporal, func() bool { return len(outcomes()) == 1 })
	for _, outcome := range outcomes() {
		if outcome != "suppressed:subscription_needs_reapproval" {
			t.Fatalf("during the interruption: %s", outcome)
		}
	}
	s.activateNextPolicy(purpose)
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync/reapprove", `{"name":"crm_sync_v2","basis_assessment":"LIA-2026-018"}`, "", viewOperator, ""), 201)
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync_v2/recoveries", "", "workflow-recovery-0001", viewOperator, ""), 201)
	if len(sentFor(r, record)) != 0 {
		t.Fatal("the backlog was sent before a recovery ran")
	}
	recovered := func() bool {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries d JOIN notification_subscriptions s ON s.id = d.subscription_id
			WHERE s.name = 'crm_sync_v2' AND d.profile_id = $1 AND d.outcome = 'delivered'`, record).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	run, stop := context.WithCancel(ctx)
	starterDone := make(chan struct{})
	go func() {
		workflows.RecoveryStarter{Store: store, Temporal: temporal, Tenant: "acme", Interval: 100 * time.Millisecond}.Run(run)
		close(starterDone)
	}()
	for deadline := time.Now().Add(time.Minute); !recovered(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			stop()
			<-starterDone
			t.Fatal("the recovery was not delivered")
		}
	}
	stop()
	<-starterDone
	if sent := sentFor(r, record); len(sent) != 1 {
		t.Fatalf("the recovered change was sent %d times", len(sent))
	}
}

// sentFor is what the receiver got about one record.
func sentFor(r *receiver, record string) []receivedDelivery {
	var out []receivedDelivery
	for _, d := range r.deliveries() {
		if strings.Contains(d.body, `"profile_id":"`+record+`"`) {
			out = append(out, d)
		}
	}
	return out
}

// countingStarter counts attempts to start a workflow, so a test can wait for the consumer to have
// tried rather than for time to pass.
type countingStarter struct {
	workflows.Starter
	calls atomic.Int32
}

func (c *countingStarter) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error) {
	c.calls.Add(1)
	return c.Starter.ExecuteWorkflow(ctx, options, workflow, args...)
}
