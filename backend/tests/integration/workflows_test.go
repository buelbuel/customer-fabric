package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/workflows"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
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

	temporal, err := client.Dial(client.Options{HostPort: address, Namespace: "customer-fabric"})
	if err != nil {
		t.Fatal(err)
	}
	defer temporal.Close()
	w := worker.New(temporal, workflows.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflows.ProfileChange)
	w.RegisterActivity(&workflows.Activities{Notifier: platform.NewNotifier(store, s.config.WebhookHosts, "profile_changes")})
	if err = w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
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
		rows, err := s.pool.Query(ctx, `SELECT event_id::text, outcome || ':' || reason FROM profile_change_notifications WHERE profile_id = $1`, record)
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
	waited := time.Now().Add(2 * time.Second)
	consume(unreachable, func() bool { return time.Now().After(waited) })
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
