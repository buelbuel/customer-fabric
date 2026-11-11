package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// The notifier is the final dispatch point: it decides from committed state immediately before each
// send, signs what it sends, sends opaque references only, and never sends an event twice.
func TestNotifierDecidesFromCurrentStateBeforeEverySend(t *testing.T) {
	r := newReceiver(t, 202, 500, 400)
	s := setupActivation(t, r)
	ctx := context.Background()
	record := s.viewRecord(tokenA, "notify-ada", `{"tier":"gold"}`)
	event := events.Event{Schema: events.Schema, ID: platform.UUID(), Type: "profile.created.v1", Tenant: "acme", Source: "console",
		ProfileID: record, ProfileVersion: 1, OccurredAt: time.Now().UTC()}
	store := platform.Store{Pool: s.pool}

	// Not registered, then registered on a host that is not allowed: suppressed without a request.
	if got, err := platform.NewNotifier(store, s.config.WebhookHosts, "changes").Notify(ctx, event); err != nil || got.Reason != "destination_not_registered" {
		t.Fatalf("unregistered destination: %+v %v", got, err)
	}
	secret := s.destination("changes", r)
	if got, err := platform.NewNotifier(store, []string{"elsewhere.invalid:443"}, "changes").Notify(ctx, event); err != nil || got.Reason != "destination_not_allowed" {
		t.Fatalf("destination not allowed: %+v %v", got, err)
	}
	n := platform.NewNotifier(store, s.config.WebhookHosts, "changes")

	got, err := n.Notify(ctx, event)
	if err != nil || got.Outcome != platform.NotificationDelivered || got.StatusCode != 202 {
		t.Fatalf("first send: %+v %v", got, err)
	}
	sent := r.deliveries()
	if len(sent) != 1 || sent[0].key != event.ID || strings.Contains(sent[0].body, "gold") {
		t.Fatalf("sent %+v", sent)
	}
	var body events.Event
	if err = json.Unmarshal([]byte(sent[0].body), &body); err != nil || body.ID != event.ID || body.ProfileID != record {
		t.Fatalf("body %s", sent[0].body)
	}
	timestamp, mac, _ := strings.Cut(strings.TrimPrefix(sent[0].signature, "t="), ",v1=")
	key, _ := hex.DecodeString(secret)
	expected := hmac.New(sha256.New, key)
	expected.Write([]byte(timestamp + "." + sent[0].body))
	if !hmac.Equal([]byte(mac), []byte(hex.EncodeToString(expected.Sum(nil)))) {
		t.Fatal("the signature does not verify")
	}
	if err = n.Record(ctx, event, got); err != nil {
		t.Fatal(err)
	}
	// A recorded event is never sent again, and recording it again changes nothing.
	if again, err := n.Notify(ctx, event); err != nil || again.Reason != "already_recorded" || again.Outcome != platform.NotificationDelivered || len(r.deliveries()) != 1 {
		t.Fatalf("a recorded event: %+v %v, %d sends", again, err, len(r.deliveries()))
	}
	if err = n.Record(ctx, event, platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "rejected"}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err = s.pool.QueryRow(ctx, `SELECT outcome FROM profile_change_notifications WHERE event_id = $1`, event.ID).Scan(&outcome); err != nil || outcome != "delivered" {
		t.Fatalf("recorded outcome %q %v", outcome, err)
	}

	// A 500 may clear on retry; a 400 will not.
	next := event
	next.ID, next.ProfileVersion = platform.UUID(), 2
	if _, err = n.Notify(ctx, next); !errors.Is(err, platform.ErrNotificationUnavailable) {
		t.Fatalf("a 500: %v", err)
	}
	if _, err = n.Notify(ctx, next); !errors.Is(err, platform.ErrNotificationRejected) {
		t.Fatalf("a 400: %v", err)
	}

	// A record restricted or no longer active since the event is fenced at the moment of sending.
	before := len(r.deliveries())
	if _, err = s.pool.Exec(ctx, `UPDATE profiles SET restricted = true WHERE id = $1`, record); err != nil {
		t.Fatal(err)
	}
	next.ID = platform.UUID()
	if got, err := n.Notify(ctx, next); err != nil || got.Outcome != platform.NotificationSuppressed || got.Reason != "record_restricted" {
		t.Fatalf("restricted: %+v %v", got, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE profiles SET restricted = false, lifecycle_state = 'erasure_pending' WHERE id = $1`, record); err != nil {
		t.Fatal(err)
	}
	if got, err := n.Notify(ctx, next); err != nil || got.Outcome != platform.NotificationSuppressed || got.Reason != "record_unavailable" {
		t.Fatalf("erasure pending: %+v %v", got, err)
	}
	unknown := next
	unknown.ProfileID = platform.UUID()
	if got, err := n.Notify(ctx, unknown); err != nil || got.Reason != "record_unavailable" {
		t.Fatalf("unknown record: %+v %v", got, err)
	}
	if len(r.deliveries()) != before {
		t.Fatal("a fenced record was sent")
	}
}
