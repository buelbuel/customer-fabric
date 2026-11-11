package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/twmb/franz-go/pkg/kgo"
)

// recordingPublisher stands in for the transport: it records what was acknowledged and can be told
// to fail, as a broker outage would.
type recordingPublisher struct {
	mu        sync.Mutex
	published []events.Event
	fail      error
}

func (p *recordingPublisher) Publish(_ context.Context, batch []events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	p.published = append(p.published, batch...)
	return nil
}

func (p *recordingPublisher) events() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.published)
}

// createRecord creates a console profile with its own identifier.
func (s *suite) createRecord() profiles.Receipt {
	s.t.Helper()
	w := s.request("POST", "/v1/profiles", `{"set":{"name":"Synthetic"},"identifiers":[{"namespace":"crm","value":"relay-`+platform.UUID()+`"}]}`, platform.UUID(), tokenA, "")
	s.expect(w, 201)
	var r profiles.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		s.t.Fatal(err)
	}
	return r
}

func (s *suite) pendingOutbox() int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE relayed_at IS NULL`).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// The relay publishes every committed profile change once it is committed, in version order per
// profile, with opaque references only; a failed publish leaves the batch pending for the next try.
func TestOutboxRelayPublishesCommittedChangesInOrder(t *testing.T) {
	s := setup(t)
	pub := &recordingPublisher{}
	relay := platform.Relay{Store: platform.Store{Pool: s.pool}, Publisher: pub, Tenant: "acme", Batch: 3}
	ctx := context.Background()

	first := s.createRecord()
	for v := 1; v <= 4; v++ {
		s.expect(s.request("PATCH", "/v1/profiles/"+first.ID, `{"set":{"tier":"gold","rank":`+string(rune('0'+v))+`}}`, platform.UUID(), tokenA, `"`+string(rune('0'+v))+`"`), 200)
	}
	second := s.createRecord()
	if s.pendingOutbox() != 6 {
		t.Fatalf("%d pending outbox rows, want 6", s.pendingOutbox())
	}

	// A broker outage publishes nothing and checkpoints nothing.
	pub.fail = errors.New("broker unavailable")
	if n, err := relay.RelayOnce(ctx); err == nil || n != 0 || s.pendingOutbox() != 6 {
		t.Fatalf("a failed publish: relayed %d, %v, %d pending", n, err, s.pendingOutbox())
	}
	pub.fail = nil
	for {
		n, err := relay.RelayOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	got := pub.events()
	if len(got) != 6 || s.pendingOutbox() != 0 {
		t.Fatalf("published %d events, %d still pending", len(got), s.pendingOutbox())
	}
	var versions []int64
	for _, e := range got {
		if e.Schema != events.Schema || e.Tenant != "acme" || e.Source != "console" || e.ID == "" || e.OccurredAt.IsZero() {
			t.Fatalf("envelope %+v", e)
		}
		if e.ProfileID == first.ID {
			versions = append(versions, e.ProfileVersion)
		}
	}
	if !slices.Equal(versions, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("first profile's versions in publish order: %v", versions)
	}
	if got[0].Type != "profile.created.v1" || !slices.ContainsFunc(got, func(e events.Event) bool { return e.ProfileID == second.ID }) {
		t.Fatalf("events %+v", got)
	}
	// Nothing in an envelope is a customer value.
	for _, e := range got {
		if strings.Contains(e.ProfileID+e.Source+e.Type, "gold") {
			t.Fatalf("an envelope carried a value: %+v", e)
		}
	}
	// A relay that finds nothing pending publishes nothing.
	if n, err := relay.RelayOnce(ctx); n != 0 || err != nil || len(pub.events()) != 6 {
		t.Fatalf("an idle relay: %d %v", n, err)
	}
}

// Two relays never publish the same row or interleave one profile's versions: the second waits its
// turn and finds nothing left.
func TestOutboxRelaysTakeTurns(t *testing.T) {
	s := setup(t)
	for range 20 {
		s.createRecord()
	}
	pub := &recordingPublisher{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			relay := platform.Relay{Store: platform.Store{Pool: s.pool}, Publisher: pub, Tenant: "acme", Batch: 5}
			for range 10 {
				if _, err := relay.RelayOnce(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	ids := map[string]int{}
	for _, e := range pub.events() {
		ids[e.ID]++
	}
	if len(ids) != 20 || s.pendingOutbox() != 0 {
		t.Fatalf("%d distinct events published, %d pending", len(ids), s.pendingOutbox())
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("event %s published %d times", id, n)
		}
	}
}

// With a real broker (make events-test), relayed events reach the topic keyed by profile, and a relay
// pointed at a broker that is down fails without checkpointing.
func TestOutboxRelayToKafka(t *testing.T) {
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS unset: run make events-test")
	}
	s := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	topic := "test." + strings.ReplaceAll(platform.UUID(), "-", "")
	kafka, err := events.NewKafka(strings.Split(brokers, ","), topic)
	if err != nil {
		t.Fatal(err)
	}
	defer kafka.Close()
	if err = kafka.EnsureTopic(ctx, 3, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = kafka.EnsureTopic(ctx, 3, time.Hour); err != nil {
		t.Fatalf("an existing topic: %v", err)
	}
	created := s.createRecord()
	s.expect(s.request("PATCH", "/v1/profiles/"+created.ID, `{"set":{"tier":"gold"}}`, platform.UUID(), tokenA, `"1"`), 200)
	relay := platform.Relay{Store: platform.Store{Pool: s.pool}, Publisher: kafka, Tenant: "acme", Batch: 100}
	if n, err := relay.RelayOnce(ctx); n != 2 || err != nil {
		t.Fatalf("relayed %d: %v", n, err)
	}

	consumer, err := kgo.NewClient(kgo.SeedBrokers(strings.Split(brokers, ",")...), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	var received []*kgo.Record
	for len(received) < 2 && ctx.Err() == nil {
		fetches := consumer.PollFetches(ctx)
		fetches.EachRecord(func(r *kgo.Record) { received = append(received, r) })
	}
	if len(received) != 2 || string(received[0].Key) != created.ID || received[0].Partition != received[1].Partition ||
		!strings.Contains(string(received[1].Value), `"profile_version":2`) || strings.Contains(string(received[1].Value), "gold") {
		t.Fatalf("received %d records", len(received))
	}

	// A broker that is down: nothing is checkpointed, so the event is still pending afterwards.
	down, err := events.NewKafka([]string{"127.0.0.1:1"}, topic)
	if err != nil {
		t.Fatal(err)
	}
	defer down.Close()
	s.expect(s.request("PATCH", "/v1/profiles/"+created.ID, `{"set":{"tier":"silver"}}`, platform.UUID(), tokenA, `"2"`), 200)
	short, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	broken := platform.Relay{Store: platform.Store{Pool: s.pool}, Publisher: down, Tenant: "acme", Batch: 100}
	if n, err := broken.RelayOnce(short); err == nil || n != 0 || s.pendingOutbox() != 1 {
		t.Fatalf("relay to a down broker: %d %v, %d pending", n, err, s.pendingOutbox())
	}
	if n, err := relay.RelayOnce(ctx); n != 1 || err != nil {
		t.Fatalf("relay after the outage: %d %v", n, err)
	}
}
