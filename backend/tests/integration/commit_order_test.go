package integration

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// Changes are ordered against subscriptions by commit, never by a clock (ADR 0016). A subscription is
// created only once the writes in flight have committed, and writes that start meanwhile wait for it:
// the first never reach it, the second always do, however their transactions overlap.
func TestSubscriptionsAreOrderedAgainstChangesByCommit(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	n := platform.NewNotifier(s.store, s.config.Egress())
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("ordered_sync", "legitimate_interests")
	record := s.viewRecord(tokenA, "ordered-record", `{"tier":"gold"}`)

	// Outbox rows can only be written inside the commit fence.
	if _, err := s.pool.Exec(ctx, `INSERT INTO outbox (id, type, profile_id, version, source, payload) VALUES ($1, 'profile.updated.v1', $2, 900, 'console', '{}')`,
		platform.UUID(), record); err == nil || !strings.Contains(err.Error(), "outside the commit fence") {
		t.Fatalf("an unfenced outbox write: %v", err)
	}

	// A write in flight: it entered the fence and wrote its change, and has not committed yet.
	inflight := platform.UUID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('commit-fence', 0))`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox (id, type, profile_id, version, source, payload) VALUES ($1, 'profile.updated.v1', $2, 1000, 'console', '{}')`,
		inflight, record); err != nil {
		t.Fatal(err)
	}

	// The subscription waits for it, and a write that starts meanwhile waits for the subscription.
	created := make(chan int, 1)
	go func() {
		created <- s.request("POST", subscriptionsPath+"/ordered_sync", subscriptionBody("crm_changes", purpose, "console"), "", viewOperator, "").Code
	}()
	waiting := func(c chan int, what string) {
		t.Helper()
		select {
		case code := <-c:
			t.Fatalf("%s did not wait: %d", what, code)
		case <-time.After(300 * time.Millisecond):
		}
	}
	waiting(created, "the subscription")
	later := make(chan int, 1)
	go func() {
		later <- s.request("PATCH", "/v1/profiles/"+record, `{"set":{"tier":"silver"}}`, platform.UUID(), tokenA, `"1"`).Code
	}()
	waiting(later, "the later write")
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-created; code != 201 {
		t.Fatalf("subscription %d", code)
	}
	if code := <-later; code != 200 {
		t.Fatalf("later write %d", code)
	}

	subscription := ""
	if err = s.pool.QueryRow(ctx, `SELECT id FROM notification_subscriptions WHERE name = 'ordered_sync'`).Scan(&subscription); err != nil {
		t.Fatal(err)
	}
	if tried, err := n.Subscriptions(ctx, events.Event{ID: inflight}); err != nil || slices.Contains(tried, subscription) {
		t.Fatalf("a change committed before the subscription was offered to it: %v %v", tried, err)
	}
	var after events.Event
	for _, e := range s.outboxEvents(record) {
		if e.ProfileVersion == 2 {
			after = e
		}
	}
	if tried, err := n.Subscriptions(ctx, after); err != nil || !slices.Contains(tried, subscription) {
		t.Fatalf("a change committed after the subscription missed it: %v %v", tried, err)
	}
	// An event without its committed outbox row selects nothing.
	if tried, err := n.Subscriptions(ctx, events.Event{ID: platform.UUID(), Type: "profile.updated.v1", Source: "console"}); err != nil || len(tried) != 0 {
		t.Fatalf("an unknown event selected %v %v", tried, err)
	}
}
